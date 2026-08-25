package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

const (
	hermesRTCWD       = "/home/u/hermes-rt-project"
	hermesRTSessionID = "hermes-rt-session-1"
)

// hermesRTContextFactBody is the sentinel seeded before the run, so the
// first-turn injection has real project memory to fetch.
const hermesRTContextFactBody = "hermes round trip context injection sentinel fact 7b21ce"

// TestHermesHookRoundTripsThroughRealServer drives hookcli.RunFromHermes -
// the exact code path "punk hook --from hermes" runs - against a live
// httptest.Server wrapping this package's own Server.Router(), so the whole
// chain is exercised end to end: native Hermes shell payload -> translation
// -> POST /v1/agent/hooks -> handleAgentHook -> stored fact, and for the
// first turn also GET /v1/agent/context -> handleAgentContext -> the
// {"context":...} reply Hermes reads off stdout.
//
// Driving the entry point rather than Normalize-plus-a-hand-built-request
// is what makes the stdout contract testable at all: a translator test can
// prove the envelope is right, but only this can prove the reply Hermes
// actually consumes carries the recalled memory.
func TestHermesHookRoundTripsThroughRealServer(t *testing.T) {
	srv := testServer(t)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	if _, err := srv.mem.Write(context.Background(), memory.WriteInput{
		Namespace:  AgentNamespace(hermesRTCWD),
		Key:        "/decisions/hermes-rt-context",
		Body:       hermesRTContextFactBody,
		Author:     "test",
		Writer:     "test",
		Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, stdin string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if err := hookcli.RunFromHermes(strings.NewReader(stdin), httpSrv.URL, "", &out, &errw); err != nil {
			t.Fatalf("RunFromHermes: %v (stderr: %s)", err, errw.String())
		}
		return out.String()
	}

	sid := sanitizeID(hermesRTSessionID)
	recallOne := func(t *testing.T, prefix string) memory.Fact {
		t.Helper()
		facts, err := srv.mem.Recall(context.Background(), AgentNamespace(hermesRTCWD), prefix, 5)
		if err != nil {
			t.Fatalf("Recall(%s): %v", prefix, err)
		}
		if len(facts) != 1 {
			t.Fatalf("expected exactly one fact under %s, got %d: %+v", prefix, len(facts), facts)
		}
		return facts[0]
	}

	// on_session_start: stored, sourced as hermes (not the "claude-code"
	// default the server falls back to for an unrecognized source), and
	// silent on stdout.
	if reply := run(t, `{"hook_event_name":"on_session_start","session_id":"`+hermesRTSessionID+`","cwd":"`+hermesRTCWD+`",
		"extra":{"model":"hermes-4","platform":"cli"}}`); reply != "" {
		t.Fatalf("on_session_start printed %q, want nothing", reply)
	}
	start := recallOne(t, "/agent-sessions/"+sid+"/start")
	if start.SourceRef != "hermes" {
		t.Fatalf("SessionStart SourceRef = %q, want hermes", start.SourceRef)
	}

	// pre_llm_call, first turn: captured AND answered with Hermes' own
	// {"context":...} reply carrying the seeded fact.
	reply := run(t, `{"hook_event_name":"pre_llm_call","session_id":"`+hermesRTSessionID+`","cwd":"`+hermesRTCWD+`",
		"extra":{"user_message":"why did the migration fail","is_first_turn":true}}`)
	var injected map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(reply)), &injected); err != nil {
		t.Fatalf("first-turn reply is not JSON: %v: %q", err, reply)
	}
	ctxText, _ := injected["context"].(string)
	if !strings.Contains(ctxText, hermesRTContextFactBody) {
		t.Fatalf("injected context missing the seeded fact: %q", ctxText)
	}

	// pre_llm_call, later turn: still captured, no injection.
	if later := run(t, `{"hook_event_name":"pre_llm_call","session_id":"`+hermesRTSessionID+`","cwd":"`+hermesRTCWD+`",
		"extra":{"user_message":"and what about the index","is_first_turn":false}}`); later != "" {
		t.Fatalf("a later turn printed %q, want nothing", later)
	}
	prompts, err := srv.mem.Recall(context.Background(), AgentNamespace(hermesRTCWD), "/agent-sessions/"+sid+"/prompt-", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompt facts (capture must not be gated by the injection gate), got %d: %+v", len(prompts), prompts)
	}

	// post_tool_call: the payload's own tool_call_id becomes the capture
	// key, and both the arguments and the result reach the stored body.
	if out := run(t, `{"hook_event_name":"post_tool_call","session_id":"`+hermesRTSessionID+`","cwd":"`+hermesRTCWD+`",
		"tool_name":"terminal","tool_input":{"command":"ls -la"},
		"extra":{"tool_call_id":"tc-77","task_id":"task-1","result":"file1\nfile2","duration_ms":12}}`); out != "" {
		t.Fatalf("post_tool_call printed %q, want nothing", out)
	}
	tool := recallOne(t, "/agent-sessions/"+sid+"/tool-tc-77")
	if !strings.Contains(tool.Body, "terminal") || !strings.Contains(tool.Body, "file1") {
		t.Fatalf("PostToolUse body missing tool name or result: %q", tool.Body)
	}

	// post_llm_call: the assistant's real text lands in the Stop fact,
	// which is the whole reason on_session_end is left unwired.
	if out := run(t, `{"hook_event_name":"post_llm_call","session_id":"`+hermesRTSessionID+`","cwd":"`+hermesRTCWD+`",
		"extra":{"assistant_response":"the 0020 migration was never applied"}}`); out != "" {
		t.Fatalf("post_llm_call printed %q, want nothing", out)
	}
	stop := recallOne(t, "/agent-sessions/"+sid+"/stop")
	if stop.Body != "the 0020 migration was never applied" {
		t.Fatalf("Stop body = %q, want the verbatim assistant response", stop.Body)
	}
}

// TestHermesHostileSessionIDIsSanitized pins that a session id carrying
// path traversal or control characters cannot escape the
// /agent-sessions/<sid>/ subtree - the same hostile-payload discipline
// every ingest path in this package is held to.
func TestHermesHostileSessionIDIsSanitized(t *testing.T) {
	srv := testServer(t)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	const hostile = `../../etc/passwd`
	var out, errw bytes.Buffer
	stdin := `{"hook_event_name":"on_session_start","session_id":"` + hostile + `","cwd":"` + hermesRTCWD + `"}`
	if err := hookcli.RunFromHermes(strings.NewReader(stdin), httpSrv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}

	facts, err := srv.mem.Recall(context.Background(), AgentNamespace(hermesRTCWD), "/agent-sessions/", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if strings.Contains(f.Key, "..") || strings.Contains(f.Key, "etc/passwd") {
			t.Fatalf("hostile session id reached the keyspace: %q", f.Key)
		}
	}
}
