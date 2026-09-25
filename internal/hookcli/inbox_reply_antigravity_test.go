package hookcli

// Antigravity inbox adapter tests (M7). Reply shapes are the documented
// contract from antigravity.google/docs/hooks, fetched 2026-09-25:
// PreInvocation accepts optional injectSteps[].ephemeralMessage; Stop
// REQUIRES a decision on every invocation, "continue" re-enters the loop
// and reason is the only field injected as a system message, so the
// continuation reply must be {"decision":"continue","reason":<rendered>}
// and every other path answers {"decision":"allow"} (the docs' "any
// other value allows the stop" branch).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const antigravityPreInvocationPayload = `{"invocationNum":3,"initialNumSteps":10,"conversationId":"a1","workspacePaths":["/work/proj"],"transcriptPath":"/t","artifactDirectoryPath":"/a","modelName":"gemini-3.6-flash-medium"}`
const antigravityStopPayload = `{"executionNum":1,"terminationReason":"model_stop","error":"","fullyIdle":true,"conversationId":"a1","workspacePaths":["/work/proj"],"transcriptPath":"/t","artifactDirectoryPath":"/a","modelName":"gemini-3.6-flash-medium"}`

func antigravityMsg(id string) InboxMessage {
	return InboxMessage{ID: id, Namespace: "ns1", Sender: "worker-a", Recipient: "antigravity:a1",
		Body: "antigravity body " + id, CreatedAt: "2026-09-25T10:00:00Z"}
}

func TestInboxAntigravityPreInvocationInjectsEveryInvocationAndAcks(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(antigravityMsg("m1"))
	// invocationNum=3 pins the difference from the capture path's
	// invocationNum==0 gate: the inbox injects on EVERY invocation.
	out, _ := runInbox(t, InboxOpts{Client: "antigravity", Mode: "context", Event: "PreInvocation", BaseURL: srv.URL, Namespace: "ns1"}, antigravityPreInvocationPayload)

	var reply struct {
		InjectSteps []map[string]string `json:"injectSteps"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &reply); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
	}
	if len(reply.InjectSteps) != 1 {
		t.Fatalf("PreInvocation reply must carry exactly one injected step: %q", out)
	}
	got := reply.InjectSteps[0]["ephemeralMessage"]
	if want := RenderInbox("ns1", "antigravity:a1", []InboxMessage{antigravityMsg("m1")}); got != want {
		t.Fatalf("ephemeralMessage mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("m1 must be acked after print, acked=%v", ids)
	}
	if _, ok := f.members["antigravity:a1"]; !ok {
		t.Fatalf("address must resolve from conversationId, members=%v", f.members)
	}
}

func TestInboxAntigravityStopContinueCarriesReasonAndAcks(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(antigravityMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "antigravity", Mode: "continue", Event: "Stop", BaseURL: srv.URL, Namespace: "ns1"}, antigravityStopPayload)

	var reply map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &reply); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
	}
	if reply["decision"] != "continue" {
		t.Fatalf("Stop with unread messages and a granted continuation must continue: %q", out)
	}
	reason, ok := reply["reason"].(string)
	if !ok || reason == "" {
		t.Fatalf("decision:continue without reason re-enters the loop with NO message (docs); reason must carry the rendered inbox: %q", out)
	}
	if want := RenderInbox("ns1", "antigravity:a1", []InboxMessage{antigravityMsg("m1")}); reason != want {
		t.Fatalf("reason mismatch:\ngot:  %q\nwant: %q", reason, want)
	}
	if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("m1 must be acked after print, acked=%v", ids)
	}
}

func TestInboxAntigravityStopEmptyInboxAllows(t *testing.T) {
	inboxTestEnv(t)
	_, srv := newFakeInbox(t)
	out, _ := runInbox(t, InboxOpts{Client: "antigravity", Mode: "continue", Event: "Stop", BaseURL: srv.URL, Namespace: "ns1"}, antigravityStopPayload)
	if strings.TrimSpace(out) != `{"decision":"allow"}` {
		t.Fatalf("Stop requires a decision on every invocation; empty inbox must allow, got %q", out)
	}
}

func TestInboxAntigravityStopDisabledAllowsWithoutRequests(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING", "")
	f, srv := newFakeInbox(t)
	f.add(antigravityMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "antigravity", Mode: "continue", Event: "Stop", BaseURL: srv.URL, Namespace: "ns1"}, antigravityStopPayload)
	if strings.TrimSpace(out) != `{"decision":"allow"}` {
		t.Fatalf("disabled Stop must answer the required minimum reply, got %q", out)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("disabled messaging must make zero requests, got %d", n)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("disabled messaging acks nothing, acked=%v", ids)
	}
}

func TestInboxAntigravityStopServerDownAllows(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.down = true
	f.add(antigravityMsg("m1"))
	out, errw := runInbox(t, InboxOpts{Client: "antigravity", Mode: "continue", Event: "Stop", BaseURL: srv.URL, Namespace: "ns1"}, antigravityStopPayload)
	if strings.TrimSpace(out) != `{"decision":"allow"}` {
		t.Fatalf("a dead server must never leave Stop hanging; want allow, got %q", out)
	}
	if !strings.Contains(errw, "punk hook inbox:") {
		t.Fatalf("failure must be noted on stderr, got %q", errw)
	}
}

func TestInboxAntigravityStopCapHitAllowsAndAcksNothing(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(antigravityMsg("m1"))
	state := inboxStatePath("antigravity", srv.URL, "ns1", "antigravity:a1")
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
	out, _ := runInbox(t, InboxOpts{Client: "antigravity", Mode: "continue", Event: "Stop", BaseURL: srv.URL, Namespace: "ns1"}, antigravityStopPayload)
	if strings.TrimSpace(out) != `{"decision":"allow"}` {
		t.Fatalf("cap-hit Stop must allow (Antigravity documents no client cap; punk's own cap is the only guard), got %q", out)
	}
	f.mu.Lock()
	reads := len(f.reads)
	f.mu.Unlock()
	if reads != 0 {
		t.Fatalf("cap-hit Stop must not lease messages it cannot carry, GET /messages count=%d", reads)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("cap-hit Stop acks nothing, acked=%v", ids)
	}
}

func TestInboxAntigravityPreInvocationSilentWhenEmpty(t *testing.T) {
	inboxTestEnv(t)
	_, srv := newFakeInbox(t)
	out, _ := runInbox(t, InboxOpts{Client: "antigravity", Mode: "context", Event: "PreInvocation", BaseURL: srv.URL, Namespace: "ns1"}, antigravityPreInvocationPayload)
	if out != "" {
		t.Fatalf("injectSteps is optional; an empty inbox stays silent, got %q", out)
	}
}
