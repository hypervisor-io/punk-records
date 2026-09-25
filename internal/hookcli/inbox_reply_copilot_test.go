package hookcli

// Copilot CLI inbox adapter tests (M7). Reply shapes are the documented
// contract from docs.github.com/en/copilot/reference/hooks-reference,
// fetched 2026-09-25: sessionStart may inject a flat additionalContext;
// agentStop (wired as Stop) accepts {"decision":"block","reason":...}
// with stop_hook_active on the input and a client-side runaway guard of
// 8 consecutive blocks; userPromptSubmitted config-file hook output is
// DROPPED, so it is pinned here as a non-surface (no fetch, no ACK).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const copilotSessionStartPayload = `{"hook_event_name":"SessionStart","session_id":"s1","timestamp":"2026-09-25T10:00:00Z","cwd":"/work/proj","source":"startup"}`
const copilotStopPayload = `{"hook_event_name":"Stop","session_id":"s1","timestamp":"2026-09-25T10:01:00Z","cwd":"/work/proj","transcript_path":"/t","stop_reason":"end_turn","stop_hook_active":false}`

func copilotMsg(id string) InboxMessage {
	return InboxMessage{ID: id, Namespace: "ns1", Sender: "worker-a", Recipient: "copilot:s1",
		Body: "copilot body " + id, CreatedAt: "2026-09-25T10:00:00Z"}
}

func TestInboxCopilotSessionStartPrintsFlatAdditionalContextAndAcks(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(copilotMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "copilot", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"}, copilotSessionStartPayload)

	var reply map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &reply); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
	}
	got, ok := reply["additionalContext"].(string)
	if !ok || got == "" {
		t.Fatalf("SessionStart reply must carry flat additionalContext: %q", out)
	}
	if want := RenderInbox("ns1", "copilot:s1", []InboxMessage{copilotMsg("m1")}); got != want {
		t.Fatalf("additionalContext mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	// The Claude-shaped nested envelope is garbage on Copilot's stdout
	// contract: pin that the reply is the flat shape only.
	if _, nested := reply["hookSpecificOutput"]; nested {
		t.Fatalf("copilot reply must not use Claude's nested envelope: %q", out)
	}
	if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("m1 must be acked after print, acked=%v", ids)
	}
}

func TestInboxCopilotStopPrintsBlockWithReasonAndAcks(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(copilotMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "copilot", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, copilotStopPayload)

	var reply map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &reply); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
	}
	if reply["decision"] != "block" {
		t.Fatalf("Stop reply must block to force continuation: %q", out)
	}
	reason, ok := reply["reason"].(string)
	if !ok || reason == "" {
		t.Fatalf("block requires reason carrying the rendered inbox: %q", out)
	}
	if want := RenderInbox("ns1", "copilot:s1", []InboxMessage{copilotMsg("m1")}); reason != want {
		t.Fatalf("reason mismatch:\ngot:  %q\nwant: %q", reason, want)
	}
	if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("m1 must be acked after print, acked=%v", ids)
	}
}

func TestInboxCopilotStopHookActiveSuppressesFetchAndAck(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(copilotMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "copilot", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"},
		strings.Replace(copilotStopPayload, `"stop_hook_active":false`, `"stop_hook_active":true`, 1))
	if out != "" {
		t.Fatalf("stop_hook_active=true must print nothing (Stop cannot carry context), got %q", out)
	}
	f.mu.Lock()
	reads := len(f.reads)
	f.mu.Unlock()
	if reads != 0 {
		t.Fatalf("stop_hook_active=true must not lease messages it cannot carry, GET /messages count=%d", reads)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("nothing delivered, nothing acked; acked=%v", ids)
	}
}

func TestInboxCopilotUserPromptSubmittedIsNotAnInboxSurface(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(copilotMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "copilot", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"},
		`{"hook_event_name":"UserPromptSubmit","session_id":"s1","timestamp":"2026-09-25T10:02:00Z","cwd":"/work/proj","prompt":"hi"}`)
	if out != "" {
		t.Fatalf("userPromptSubmitted config-file hook output is dropped by Copilot; printing would ack undelivered content, got %q", out)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("userPromptSubmitted must not fetch or register, requests=%d", n)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("nothing may be acked on an event whose output is dropped, acked=%v", ids)
	}
}

func TestInboxCopilotStopEmptyInboxPrintsNothing(t *testing.T) {
	inboxTestEnv(t)
	_, srv := newFakeInbox(t)
	out, _ := runInbox(t, InboxOpts{Client: "copilot", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, copilotStopPayload)
	if out != "" {
		t.Fatalf("agentStop with an empty inbox must print nothing (empty output is not a block), got %q", out)
	}
}

func TestInboxCopilotDisabledPrintsNothingAndMakesNoRequests(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING", "")
	f, srv := newFakeInbox(t)
	f.add(copilotMsg("m1"))
	for name, tc := range map[string]struct {
		mode, stdin string
	}{"SessionStart": {"context", copilotSessionStartPayload}, "Stop": {"continue", copilotStopPayload}} {
		out, _ := runInbox(t, InboxOpts{Client: "copilot", Mode: tc.mode, BaseURL: srv.URL, Namespace: "ns1"}, tc.stdin)
		if out != "" {
			t.Fatalf("%s: disabled messaging must print nothing, got %q", name, out)
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("disabled messaging must make zero requests, got %d", n)
	}
}

func TestInboxCopilotCapHitStopPrintsNothing(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(copilotMsg("m1"))
	state := inboxStatePath("copilot", srv.URL, "ns1", "copilot:s1")
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
	out, _ := runInbox(t, InboxOpts{Client: "copilot", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, copilotStopPayload)
	if out != "" {
		t.Fatalf("cap-hit Stop must print nothing (punk's 5/10min cap sits under Copilot's documented 8-block runaway guard), got %q", out)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("cap-hit Stop acks nothing, acked=%v", ids)
	}
}
