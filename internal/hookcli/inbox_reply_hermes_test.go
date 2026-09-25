package hookcli

// Hermes inbox adapter tests (M7 addendum). The reply shape is the
// documented pre_llm_call shell-hook contract from hermes-agent.
// nousresearch.com/docs/user-guide/features/hooks: a flat {"context":
// <text>} appended to the user message. pre_llm_call fires before EVERY
// model call, giving per-turn catch-up; Hermes has no continuation or
// wake contract, so continue/wait modes downgrade to context in
// inbox.go and no other event carries the inbox.

import (
	"encoding/json"
	"strings"
	"testing"
)

const hermesPreLLMCallPayload = `{"hook_event_name":"pre_llm_call","session_id":"h1","cwd":"/work/proj","extra":{"is_first_turn":false,"user_message":"status?"}}`

func hermesMsg(id string) InboxMessage {
	return InboxMessage{ID: id, Namespace: "ns1", Sender: "worker-a", Recipient: "hermes:h1",
		Body: "hermes body " + id, CreatedAt: "2026-09-25T10:00:00Z"}
}

func TestInboxHermesPreLLMCallPrintsFlatContextAndAcks(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(hermesMsg("m1"))
	// is_first_turn=false pins the per-turn catch-up: unlike the memory
	// injection path (RunFromHermes gates the session block to the first
	// turn), the inbox injects on any turn while unread messages exist.
	out, _ := runInbox(t, InboxOpts{Client: "hermes", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"}, hermesPreLLMCallPayload)

	var reply map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &reply); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
	}
	got, ok := reply["context"].(string)
	if !ok || got == "" {
		t.Fatalf("pre_llm_call reply must carry flat context: %q", out)
	}
	if want := RenderInbox("ns1", "hermes:h1", []InboxMessage{hermesMsg("m1")}); got != want {
		t.Fatalf("context mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("m1 must be acked after print, acked=%v", ids)
	}
	if _, ok := f.members["hermes:h1"]; !ok {
		t.Fatalf("address must resolve from session_id, members=%v", f.members)
	}
}

func TestInboxHermesContinueModeDowngradesToContext(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(hermesMsg("m1"))
	// Hermes has no continuation contract: --mode continue downgrades to
	// context (inbox.go), and pre_llm_call still delivers via context.
	out, errw := runInbox(t, InboxOpts{Client: "hermes", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, hermesPreLLMCallPayload)
	if !strings.Contains(out, `"context"`) {
		t.Fatalf("downgraded continue must still deliver as context, got %q", out)
	}
	if !strings.Contains(errw, "no continuation contract") {
		t.Fatalf("the downgrade must be noted on stderr, got %q", errw)
	}
	if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("m1 must be acked after print, acked=%v", ids)
	}
}

func TestInboxHermesOtherEventsAreNotInboxSurfaces(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	f.add(hermesMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "hermes", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"},
		`{"hook_event_name":"post_llm_call","session_id":"h1","cwd":"/work/proj"}`)
	if out != "" {
		t.Fatalf("post_llm_call has no content-carrying reply; got %q", out)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("an event that cannot carry content must not fetch or register, requests=%d", n)
	}
	if ids := f.ackedIDs(); len(ids) != 0 {
		t.Fatalf("nothing may be acked on an observation event, acked=%v", ids)
	}
}

func TestInboxHermesDisabledPrintsNothingAndMakesNoRequests(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING", "")
	f, srv := newFakeInbox(t)
	f.add(hermesMsg("m1"))
	out, _ := runInbox(t, InboxOpts{Client: "hermes", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"}, hermesPreLLMCallPayload)
	if out != "" {
		t.Fatalf("disabled messaging must print nothing (no Hermes event requires a reply), got %q", out)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("disabled messaging must make zero requests, got %d", n)
	}
}
