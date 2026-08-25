package hookcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRunFromCopilotPostToolUseForwardsAndPrintsNothing verifies PostToolUse
// forwards the translated envelope and - unlike Antigravity's PostToolUse -
// prints nothing at all: docs.github.com/en/copilot/reference/
// hooks-reference documents postToolUse's output as optional ("Return {}
// or empty output to keep the original successful result"), not required.
func TestRunFromCopilotPostToolUseForwardsAndPrintsNothing(t *testing.T) {
	var gotHook []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/hooks" {
			t.Errorf("unexpected path %s", r.URL.Path)
			return
		}
		gotHook, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"PostToolUse","session_id":"sess-rt","timestamp":"2026-08-01T12:00:00Z","cwd":"/repo","tool_name":"bash","tool_input":{"command":"ls"},"tool_result":{"result_type":"success","text_result_for_llm":"file1"}}`
	var out, errw bytes.Buffer
	if err := RunFromCopilot(strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("PostToolUse stdout must be empty (no required reply), got: %s", out.String())
	}
	var env struct {
		HookEventName string `json:"hook_event_name"`
		ToolUseID     string `json:"tool_use_id"`
		ToolName      string `json:"tool_name"`
	}
	if err := json.Unmarshal(gotHook, &env); err != nil {
		t.Fatalf("forwarded body not valid JSON: %v: %s", err, gotHook)
	}
	if env.HookEventName != "PostToolUse" || env.ToolName != "bash" || env.ToolUseID == "" {
		t.Fatalf("forwarded envelope: %+v", env)
	}
}

// TestRunFromCopilotSessionStartInjectsContext verifies SessionStart
// forwards the translated envelope, fetches context, and prints Copilot's
// OWN flat {"additionalContext":...} envelope - not Claude Code's nested
// hookSpecificOutput shape.
func TestRunFromCopilotSessionStartInjectsContext(t *testing.T) {
	var hookCalls, ctxCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			atomic.AddInt32(&hookCalls, 1)
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			atomic.AddInt32(&ctxCalls, 1)
			if r.URL.Query().Get("cwd") != "/home/u/cpproj" {
				t.Errorf("cwd param: %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"namespace":"agent-cpproj","context":"## Project memory\n- [/a] x","fact_ids":["1"]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"SessionStart","session_id":"sess-inj-1","cwd":"/home/u/cpproj","source":"startup"}`
	var out, errw bytes.Buffer
	if err := RunFromCopilot(strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	var result struct {
		AdditionalContext string `json:"additionalContext"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("stdout not valid additionalContext JSON: %v: %s", err, out.String())
	}
	if !strings.Contains(result.AdditionalContext, "Project memory") {
		t.Fatalf("expected injected context, got: %+v", result)
	}
	// Must NOT be Claude Code's nested hookSpecificOutput envelope.
	if strings.Contains(out.String(), "hookSpecificOutput") {
		t.Fatalf("must print Copilot's own flat envelope, not Claude Code's nested one: %s", out.String())
	}
	if atomic.LoadInt32(&hookCalls) != 1 || atomic.LoadInt32(&ctxCalls) != 1 {
		t.Fatalf("expected exactly one hook call and one context call, got hook=%d ctx=%d", hookCalls, ctxCalls)
	}
}

// TestRunFromCopilotSessionStartEmptyContextPrintsNothing mirrors
// TestRunEmptyContextPrintsNothing: an empty context string from the
// server must not produce an additionalContext payload.
func TestRunFromCopilotSessionStartEmptyContextPrintsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			w.Write([]byte(`{"namespace":"agent-empty","context":"","fact_ids":[]}`))
		}
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"SessionStart","session_id":"sess-empty","cwd":"/empty"}`
	var out, errw bytes.Buffer
	if err := RunFromCopilot(strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("empty context must not produce an additionalContext payload: %s", out.String())
	}
}

// TestRunFromCopilotStopPrintsNothingEvenWhenServerDown verifies Stop
// forwards a best-effort capture and stays fail-open: a dead memory server
// must never surface as output or an error, and - unlike Antigravity's
// Stop - Copilot's agentStop has no required stdout reply at all, so
// nothing is ever printed for it.
func TestRunFromCopilotStopPrintsNothingEvenWhenServerDown(t *testing.T) {
	const stdinBody = `{"hook_event_name":"Stop","session_id":"sess-down","cwd":"/p","stop_reason":"end_turn","stop_hook_active":false}`
	var out, errw bytes.Buffer
	if err := RunFromCopilot(strings.NewReader(stdinBody), "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("Stop must print nothing even with the server down, got: %s", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the forward failure should still be noted on stderr")
	}
}

// TestRunFromCopilotUnmappedEventMakesNoHTTPCallsAndPrintsNothing verifies
// an event with no Claude Code mapping (e.g. PreToolUse) forwards nothing
// and prints nothing.
func TestRunFromCopilotUnmappedEventMakesNoHTTPCallsAndPrintsNothing(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"PreToolUse","session_id":"sess-x","tool_name":"bash","tool_input":{"command":"ls"}}`
	var out, errw bytes.Buffer
	if err := RunFromCopilot(strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("unmapped event must print nothing, got: %s", out.String())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("unmapped event must make no HTTP calls, got %d", calls)
	}
}

// TestRunFromCopilotMalformedStdinPrintsNothing verifies bad JSON on stdin
// is noted on stderr and makes no HTTP calls, printing nothing - fail-open,
// not a crash or a garbage reply.
func TestRunFromCopilotMalformedStdinPrintsNothing(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	if err := RunFromCopilot(strings.NewReader(`{not valid`), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("malformed stdin must print nothing, got: %s", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the normalize failure should still be noted on stderr")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("malformed stdin must make no HTTP calls, got %d", calls)
	}
}

// TestRunFromCopilotStdinReadErrorPrintsNothing verifies a stdin read
// error is noted on stderr and produces no output, always returning nil
// (fail-open).
func TestRunFromCopilotStdinReadErrorPrintsNothing(t *testing.T) {
	var out, errw bytes.Buffer
	stdin := errReader{err: io.ErrClosedPipe}
	if err := RunFromCopilot(stdin, "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdin read error must print nothing, got: %s", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the read failure should still be noted on stderr")
	}
}

// TestRunFromCopilotTrailingSlashBaseURLIsTrimmed verifies a trailing
// slash on baseURL never produces a double-slash request path.
func TestRunFromCopilotTrailingSlashBaseURLIsTrimmed(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"UserPromptSubmit","session_id":"sess-slash","cwd":"/p","prompt":"hi"}`
	var out, errw bytes.Buffer
	if err := RunFromCopilot(strings.NewReader(stdinBody), srv.URL+"/", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/agent/hooks" {
		t.Fatalf("path = %q, want /v1/agent/hooks (no double slash)", gotPath)
	}
}
