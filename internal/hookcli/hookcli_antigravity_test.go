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

// TestRunFromAntigravityPostToolUseForwardsAndRepliesEmptyObject verifies
// PostToolUse forwards the translated envelope and prints the documented
// "{}" reply (antigravity.google/docs/hooks: "Returns an empty JSON
// object {}").
func TestRunFromAntigravityPostToolUseForwardsAndRepliesEmptyObject(t *testing.T) {
	var gotHook []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/hooks" {
			t.Errorf("unexpected path %s", r.URL.Path)
			return
		}
		gotHook, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"stepIdx":7,"error":"","conversationId":"conv-rt","workspacePaths":["/repo"]}`
	var out, errw bytes.Buffer
	if err := RunFromAntigravity("PostToolUse", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{}\n" {
		t.Fatalf("PostToolUse stdout = %q, want exactly {}\\n", out.String())
	}
	var env struct {
		HookEventName string `json:"hook_event_name"`
		ToolUseID     string `json:"tool_use_id"`
		ToolName      string `json:"tool_name"`
	}
	if err := json.Unmarshal(gotHook, &env); err != nil {
		t.Fatalf("forwarded body not valid JSON: %v: %s", err, gotHook)
	}
	if env.HookEventName != "PostToolUse" || env.ToolUseID != "step7" || env.ToolName != "AntigravityStep" {
		t.Fatalf("forwarded envelope: %+v", env)
	}
}

// TestRunFromAntigravityStopAlwaysRepliesAllow verifies the Stop event
// always prints {"decision":"allow"} on the success path - and,
// critically, never "continue" (which would force Antigravity's execution
// loop to keep running).
func TestRunFromAntigravityStopAlwaysRepliesAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"executionNum":1,"terminationReason":"model_stop","fullyIdle":true,"conversationId":"conv-1","workspacePaths":["/p"]}`
	var out, errw bytes.Buffer
	if err := RunFromAntigravity("Stop", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"decision":"allow"}`+"\n" {
		t.Fatalf("Stop stdout = %q, want exactly {\"decision\":\"allow\"}\\n", out.String())
	}
}

// TestRunFromAntigravityStopRepliesAllowEvenWhenServerDown mirrors
// TestRunFromCursorBeforeSubmitPromptContinuesEvenWhenServerDown: the
// reply must not depend on the forward succeeding - a dead memory server
// must never leave Antigravity's execution loop hanging on a Stop hook.
func TestRunFromAntigravityStopRepliesAllowEvenWhenServerDown(t *testing.T) {
	const stdinBody = `{"terminationReason":"model_stop","fullyIdle":true,"conversationId":"conv-1","workspacePaths":["/p"]}`
	var out, errw bytes.Buffer
	if err := RunFromAntigravity("Stop", strings.NewReader(stdinBody), "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"decision":"allow"}`+"\n" {
		t.Fatalf("Stop stdout with server down = %q, want exactly {\"decision\":\"allow\"}\\n", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the forward failure should still be noted on stderr")
	}
}

// TestRunFromAntigravityStopRepliesAllowOnMalformedStdin verifies bad JSON
// on stdin still gets the unconditional Stop reply - translation failing
// must not leave the hook hanging.
func TestRunFromAntigravityStopRepliesAllowOnMalformedStdin(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	if err := RunFromAntigravity("Stop", strings.NewReader(`{not valid`), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"decision":"allow"}`+"\n" {
		t.Fatalf("malformed stdin Stop reply = %q, want exactly {\"decision\":\"allow\"}\\n", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the normalize failure should still be noted on stderr")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("malformed stdin must make no HTTP calls, got %d", calls)
	}
}

// TestRunFromAntigravityStopRepliesAllowOnStdinReadError mirrors
// TestRunFromCursorStdinReadErrorPrintsContinueTrue: a stdin read error
// leaves nothing to translate, but event is already known from the
// --event flag (unlike Cursor, which must sniff hook_event_name out of
// raw), so the Stop reply still fires unconditionally.
func TestRunFromAntigravityStopRepliesAllowOnStdinReadError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := errReader{err: io.ErrClosedPipe}
	if err := RunFromAntigravity("Stop", stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatalf("stdin read error must not error: %v", err)
	}
	if out.String() != `{"decision":"allow"}`+"\n" {
		t.Fatalf("stdin read error Stop reply = %q, want exactly {\"decision\":\"allow\"}\\n", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the read failure should still be noted on stderr")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("stdin read error must make no HTTP calls, got %d", calls)
	}
}

// TestRunFromAntigravityPostToolUseAlwaysRepliesEmptyObjectEvenOnFailure
// verifies PostToolUse prints its documented "{}" reply on EVERY path -
// including a translation failure - mirroring the Stop reply's
// unconditional printAntigravityStopReply discipline (see
// printAntigravityPostToolUseReply's own doc comment in hookcli.go for the
// "why": nothing in Antigravity's docs excuses a failed/skipped
// PostToolUse invocation from the Required "{}" reply).
func TestRunFromAntigravityPostToolUseAlwaysRepliesEmptyObjectEvenOnFailure(t *testing.T) {
	var out, errw bytes.Buffer
	if err := RunFromAntigravity("PostToolUse", strings.NewReader(`{not valid`), "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{}\n" {
		t.Fatalf("PostToolUse malformed-stdin reply = %q, want exactly {}\\n", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the normalize failure should still be noted on stderr")
	}
}

// TestRunFromAntigravityPreInvocationStaysQuietOnFailure verifies
// PreInvocation prints NOTHING when its own translation fails - unlike
// PostToolUse and Stop, PreInvocation's injectSteps output is optional, so
// there is no Required reply to print unconditionally, mirroring how
// Cursor's five non-blocking events never get a reply at all.
func TestRunFromAntigravityPreInvocationStaysQuietOnFailure(t *testing.T) {
	var out, errw bytes.Buffer
	if err := RunFromAntigravity("PreInvocation", strings.NewReader(`{not valid`), "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("PreInvocation malformed stdin must print nothing, got: %s", out.String())
	}
}

// TestRunFromAntigravityPreInvocationInjectsOnFirstCallOnly verifies
// context injection fires (injectSteps with an ephemeralMessage) on
// invocationNum==0 and stays silent on a later invocation in the same
// conversation - the stateless once-per-conversation gate.
func TestRunFromAntigravityPreInvocationInjectsOnFirstCallOnly(t *testing.T) {
	var hookCalls, ctxCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			atomic.AddInt32(&hookCalls, 1)
			_, _ = w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			atomic.AddInt32(&ctxCalls, 1)
			if r.URL.Query().Get("cwd") != "/home/u/agproj" {
				t.Errorf("cwd param: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"namespace":"agent-agproj","context":"## Project memory\n- [/a] x","fact_ids":["1"]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// First invocation of the conversation: must forward SessionStart and
	// inject context.
	first := `{"invocationNum":0,"conversationId":"conv-inj-1","workspacePaths":["/home/u/agproj"]}`
	var out, errw bytes.Buffer
	if err := RunFromAntigravity("PreInvocation", strings.NewReader(first), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	var result struct {
		InjectSteps []struct {
			EphemeralMessage string `json:"ephemeralMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("stdout not valid injectSteps JSON: %v: %s", err, out.String())
	}
	if len(result.InjectSteps) != 1 || !strings.Contains(result.InjectSteps[0].EphemeralMessage, "Project memory") {
		t.Fatalf("expected injected context, got: %+v", result)
	}
	if atomic.LoadInt32(&hookCalls) != 1 || atomic.LoadInt32(&ctxCalls) != 1 {
		t.Fatalf("expected exactly one hook call and one context call, got hook=%d ctx=%d", hookCalls, ctxCalls)
	}

	// Later invocation, same conversation: must forward nothing and inject
	// nothing.
	later := `{"invocationNum":1,"conversationId":"conv-inj-1","workspacePaths":["/home/u/agproj"]}`
	var out2, errw2 bytes.Buffer
	if err := RunFromAntigravity("PreInvocation", strings.NewReader(later), srv.URL, "", &out2, &errw2); err != nil {
		t.Fatal(err)
	}
	if out2.Len() != 0 {
		t.Fatalf("later invocation must print nothing, got: %s", out2.String())
	}
	if atomic.LoadInt32(&hookCalls) != 1 || atomic.LoadInt32(&ctxCalls) != 1 {
		t.Fatalf("later invocation must make no additional HTTP calls, got hook=%d ctx=%d", hookCalls, ctxCalls)
	}
}

// TestRunFromAntigravityPreInvocationEmptyContextPrintsNothing mirrors
// TestRunEmptyContextPrintsNothing: an empty context string from the
// server must not produce an injectSteps envelope with an empty
// ephemeralMessage.
func TestRunFromAntigravityPreInvocationEmptyContextPrintsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			w.Write([]byte(`{"namespace":"agent-empty","context":"","fact_ids":[]}`))
		}
	}))
	defer srv.Close()

	const stdinBody = `{"invocationNum":0,"conversationId":"conv-empty","workspacePaths":["/empty"]}`
	var out, errw bytes.Buffer
	if err := RunFromAntigravity("PreInvocation", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("empty context must not produce an injectSteps payload: %s", out.String())
	}
}

// TestRunFromAntigravityUnwiredEventMakesNoHTTPCallsAndPrintsNothing
// verifies PreToolUse and PostInvocation (never wired into hooks.json, but
// exercised here as if a stale/hand-edited config invoked them) forward
// nothing, inject nothing, and print nothing.
func TestRunFromAntigravityUnwiredEventMakesNoHTTPCallsAndPrintsNothing(t *testing.T) {
	for _, event := range []string{"PreToolUse", "PostInvocation", ""} {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.Write([]byte(`{"status":"stored"}`))
		}))
		const stdinBody = `{"conversationId":"conv-x","workspacePaths":["/p"],"invocationNum":0}`
		var out, errw bytes.Buffer
		if err := RunFromAntigravity(event, strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
			t.Fatalf("event=%q: %v", event, err)
		}
		if out.Len() != 0 {
			t.Fatalf("event=%q: must print nothing, got: %s", event, out.String())
		}
		if atomic.LoadInt32(&calls) != 0 {
			t.Fatalf("event=%q: must make no HTTP calls, got %d", event, calls)
		}
		srv.Close()
	}
}

// TestRunFromAntigravityTrailingSlashBaseURLIsTrimmed mirrors
// TestRunTrailingSlashBaseURLIsTrimmed.
func TestRunFromAntigravityTrailingSlashBaseURLIsTrimmed(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/v1/agent/hooks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"stepIdx":1,"conversationId":"conv-1","workspacePaths":["/p"]}`
	var out, errw bytes.Buffer
	if err := RunFromAntigravity("PostToolUse", strings.NewReader(stdinBody), srv.URL+"/", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/agent/hooks" {
		t.Fatalf("trailing-slash baseURL produced path %q, want single-slash /v1/agent/hooks", gotPath)
	}
}

// TestRunFromAntigravityStdinOverCapIsBounded mirrors
// TestRunStdinOverCapIsRejectedWithNoHTTPCalls: stdin past maxStdinBytes
// is truncated, which turns otherwise well-formed JSON into an incomplete
// document that fails to parse - noted on stderr, no HTTP calls, and (for
// the Stop event) the reply still fires since event is known independent
// of parsing raw at all.
func TestRunFromAntigravityStdinOverCapIsBounded(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const prefix = `{"terminationReason":"model_stop","fullyIdle":true,"conversationId":"conv-1","workspacePaths":["/p"],"pad":"`
	const suffix = `"}`
	pad := strings.Repeat("x", maxStdinBytes+4096-len(prefix)-len(suffix))
	body := prefix + pad + suffix
	if len(body) != maxStdinBytes+4096 {
		t.Fatalf("test setup: stdin body is %d bytes, want %d", len(body), maxStdinBytes+4096)
	}

	var out, errw bytes.Buffer
	if err := RunFromAntigravity("Stop", strings.NewReader(body), srv.URL, "", &out, &errw); err != nil {
		t.Fatalf("oversized stdin must not error: %v", err)
	}
	if out.String() != `{"decision":"allow"}`+"\n" {
		t.Fatalf("oversized stdin Stop reply = %q, want exactly {\"decision\":\"allow\"}\\n", out.String())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("oversized stdin must make no HTTP calls, got %d", calls)
	}
}
