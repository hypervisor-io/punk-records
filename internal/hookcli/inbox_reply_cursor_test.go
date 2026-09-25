package hookcli

// Cursor inbox adapter tests (M7). Every reply shape asserted here is
// the documented contract from cursor.com/docs/agent/hooks, fetched
// 2026-09-25: sessionStart accepts additional_context; stop accepts
// followup_message with the client's own loop_limit (default 5);
// beforeSubmitPrompt's output is only continue+user_message, so it is
// never an inbox surface. The tests run the real init()-registered
// adapter through Inbox against the fake M3/M10 server (inbox_test.go).

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const cursorSessionStartPayload = `{"conversation_id":"c1","generation_id":"g1","hook_event_name":"sessionStart","cursor_version":"2.0","workspace_roots":["/work/proj"],"session_id":"c1"}`
const cursorStopPayload = `{"conversation_id":"c1","generation_id":"g2","hook_event_name":"stop","cursor_version":"2.0","workspace_roots":["/work/proj"],"status":"completed","loop_count":0}`

func cursorMsg(id string) InboxMessage {
	return InboxMessage{ID: id, Namespace: "ns1", Sender: "worker-a", Recipient: "cursor:c1",
		Body: "cursor body " + id, CreatedAt: "2026-09-25T10:00:00Z"}
}

func TestInboxCursorSessionStartPrintsAdditionalContextAndAcks(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(cursorMsg("m1"))
	out, errw := runInbox(t, InboxOpts{Client: "cursor", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"}, cursorSessionStartPayload)

	var reply map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &reply); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
	}
	got, ok := reply["additional_context"].(string)
	if !ok || got == "" {
		t.Fatalf("sessionStart reply must carry additional_context: %q", out)
	}
	if want := RenderInbox("ns1", "cursor:c1", []InboxMessage{cursorMsg("m1")}); got != want {
		t.Fatalf("additional_context mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	if _, forbidden := reply["continue"]; forbidden {
		t.Fatalf("sessionStart reply must not carry continue: %q", out)
	}
	if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("m1 must be acked after print, acked=%v stderr=%s", ids, errw)
	}
	if role := f.members["cursor:c1"]; !strings.HasPrefix(role, "cursor session /work/proj") {
		t.Fatalf("self-registration role = %q", role)
	}
}

func TestInboxCursorStopPrintsFollowupMessageAndAcks(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(cursorMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "cursor", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, cursorStopPayload)

	var reply map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &reply); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
	}
	got, ok := reply["followup_message"].(string)
	if !ok || got == "" {
		t.Fatalf("stop reply must carry followup_message: %q", out)
	}
	if want := RenderInbox("ns1", "cursor:c1", []InboxMessage{cursorMsg("m1")}); got != want {
		t.Fatalf("followup_message mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("m1 must be acked after print, acked=%v", ids)
	}
}

func TestInboxCursorStopEmptyInboxPrintsNothing(t *testing.T) {
	inboxTestEnv(t)
	_, srv := newFakeInbox(t)
	out, _ := runInbox(t, InboxOpts{Client: "cursor", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, cursorStopPayload)
	if out != "" {
		t.Fatalf("stop with an empty inbox must print nothing (no event wired for the inbox requires a reply), got %q", out)
	}
}

func TestInboxCursorStopCapHitAcksNothingAndNeverFetches(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(cursorMsg("m1"))
	// Exhaust the continuation window for this address: stop carries
	// content only as a followup_message, so with the cap spent the hook
	// must not even lease m1 - it waits for the next sessionStart.
	state := inboxStatePath("cursor", srv.URL, "ns1", "cursor:c1")
	clock := time.Unix(1_800_000_000, 0)
	restore := inboxNow
	inboxNow = func() time.Time { return clock }
	t.Cleanup(func() { inboxNow = restore })
	for i := 0; i < 5; i++ {
		granted, err := reserveContinuation(state, clock, 5, 10*time.Minute)
		if err != nil || !granted {
			t.Fatalf("setup: reserve %d: granted=%v err=%v", i, granted, err)
		}
	}
	out, _ := runInbox(t, InboxOpts{Client: "cursor", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, cursorStopPayload)
	if out != "" {
		t.Fatalf("cap-hit stop must print nothing, got %q", out)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("cap-hit stop must ack nothing (nothing was delivered), acked=%v", ids)
	}
	f.mu.Lock()
	reads := len(f.reads)
	f.mu.Unlock()
	if reads != 0 {
		t.Fatalf("cap-hit stop must not lease messages it cannot carry, GET /messages count=%d", reads)
	}
}

func TestInboxCursorBeforeSubmitPromptIsNotAnInboxSurface(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(cursorMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "cursor", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"},
		`{"conversation_id":"c1","hook_event_name":"beforeSubmitPrompt","cursor_version":"2.0","workspace_roots":["/work/proj"],"prompt":"hi"}`)
	if out != "" {
		t.Fatalf("beforeSubmitPrompt has no context field in its reply contract; the inbox must print nothing, got %q", out)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("beforeSubmitPrompt must not fetch or register (no way to deliver), requests=%d", n)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("nothing may be acked on an event that cannot carry content, acked=%v", ids)
	}
}

func TestInboxCursorDisabledPrintsNothingAndMakesNoRequests(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING", "")
	f, srv := newFakeInbox(t)
	f.add(cursorMsg("m1"))
	for name, stdin := range map[string]string{"sessionStart": cursorSessionStartPayload, "stop": cursorStopPayload} {
		out, _ := runInbox(t, InboxOpts{Client: "cursor", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"}, stdin)
		if out != "" {
			t.Fatalf("%s: disabled messaging must print nothing, got %q", name, out)
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("disabled messaging must make zero requests, got %d", n)
	}
}

func TestInboxCursorServerDownFailsOpenSilent(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.down = true
	f.add(cursorMsg("m1"))
	out, errw := runInbox(t, InboxOpts{Client: "cursor", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, cursorStopPayload)
	if out != "" {
		t.Fatalf("a dead server must never print a garbage reply on stop, got %q", out)
	}
	if !strings.Contains(errw, "punk hook inbox:") {
		t.Fatalf("failure must be noted on stderr, got %q", errw)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("a dead server acks nothing, acked=%v", ids)
	}
}

func TestInboxCursorAddressUsesConversationID(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(cursorMsg("m1"))
	// session_id absent from the payload shape stop carries: the address
	// must come from conversation_id (cursor.com/docs/agent/hooks common
	// input fields).
	runInbox(t, InboxOpts{Client: "cursor", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, cursorStopPayload)
	if _, ok := f.members["cursor:c1"]; !ok {
		t.Fatalf("expected registration as cursor:c1, members=%v", f.members)
	}
	if ids := f.ackedIDs(); len(ids) != 1 {
		t.Fatalf("delivery to cursor:c1 must ack m1, acked=%v", ids)
	}
}

// The init()-registered adapter must be complete: base inbox.go init
// runs before this file's init (lexical file order), and a regression in
// that ordering would silently leave Reply nil (the "client has no reply
// writer yet" inert path). See /answers/phase2-adapter-decisions.
func TestInboxAdapterRegistrationsComplete(t *testing.T) {
	for _, tc := range []struct {
		name          string
		allowContinue bool
	}{
		{"cursor", true},
		{"copilot", true},
		{"antigravity", true},
		{"hermes", false},
	} {
		c, ok := lookupInboxClient(tc.name)
		if !ok {
			t.Fatalf("%s: not registered", tc.name)
		}
		if c.Reply == nil || c.CanCarry == nil {
			t.Fatalf("%s: adapter registration incomplete (Reply=%v CanCarry=%v)", tc.name, c.Reply != nil, c.CanCarry != nil)
		}
		if c.Parse == nil {
			t.Fatalf("%s: built-in parser lost in adapter registration", tc.name)
		}
		if c.AllowContinue != tc.allowContinue {
			t.Fatalf("%s: AllowContinue=%v, want %v", tc.name, c.AllowContinue, tc.allowContinue)
		}
	}
}

// The 6th cursor stop inside the window falls back, and the follow-up
// after the window continues again - the client-side loop_limit (5)
// and punk's own cap stay aligned by construction.
func TestInboxCursorStopContinuationWindowReopens(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	clock := time.Unix(1_800_000_000, 0)
	restore := inboxNow
	inboxNow = func() time.Time { return clock }
	t.Cleanup(func() { inboxNow = restore })

	for i := 1; i <= 6; i++ {
		f.add(cursorMsg(fmt.Sprintf("m%d", i)))
		out, _ := runInbox(t, InboxOpts{Client: "cursor", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, cursorStopPayload)
		wantFollowup := i <= 5
		gotFollowup := strings.Contains(out, "followup_message")
		if gotFollowup != wantFollowup {
			t.Fatalf("continuation %d: followup printed=%v, want %v (out=%q)", i, gotFollowup, wantFollowup, out)
		}
		clock = clock.Add(time.Second)
	}
	clock = clock.Add(10 * time.Minute)
	f.add(cursorMsg("m7"))
	out, _ := runInbox(t, InboxOpts{Client: "cursor", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, cursorStopPayload)
	if !strings.Contains(out, "followup_message") {
		t.Fatalf("first continuation after the window must print followup_message, got %q", out)
	}
}
