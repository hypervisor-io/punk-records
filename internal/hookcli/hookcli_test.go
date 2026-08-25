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

func TestRunForwardsAndInjectsContext(t *testing.T) {
	var gotHook []byte
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			auth = r.Header.Get("Authorization")
			gotHook, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			if r.URL.Query().Get("cwd") != "/home/u/myproj" {
				t.Errorf("cwd param: %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"namespace":"agent-myproj","context":"## Project memory\n- [/a] x","fact_ids":["1"]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/home/u/myproj","source":"startup"}`
	stdin := strings.NewReader(stdinBody)
	var out, errw bytes.Buffer
	if err := Run(stdin, srv.URL, "tok123", &out, &errw); err != nil {
		t.Fatal(err)
	}
	// The forwarded body must be byte-identical to stdin: the server parses
	// an 11-field struct (including session_id, used to key captured
	// facts), while the client's hookPayload decode struct only carries 2
	// fields. If forwardHook ever re-marshaled the decoded client struct
	// instead of relaying the raw bytes, this would still pass a loose
	// "contains SessionStart" check while silently dropping session_id and
	// no-op'ing all server-side capture.
	if string(gotHook) != stdinBody {
		t.Fatalf("forwarded body must equal stdin exactly:\n got:  %s\n want: %s", gotHook, stdinBody)
	}
	if auth != "Bearer tok123" {
		t.Fatalf("auth header: %q", auth)
	}
	var env struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout not valid hook JSON: %v: %s", err, out.String())
	}
	if env.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(env.HookSpecificOutput.AdditionalContext, "Project memory") {
		t.Fatalf("injection payload: %+v", env)
	}
}

func TestRunNonSessionStartPrintsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()
	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1","cwd":"/p","last_assistant_message":"m"}`)
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("non-SessionStart must print nothing: %s", out.String())
	}
}

func TestRunServerDownIsSilentSuccess(t *testing.T) {
	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1","cwd":"/p"}`)
	if err := Run(stdin, "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatalf("server-down must not error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatal("must print nothing on failure")
	}
	if errw.Len() == 0 {
		t.Fatal("failure should be noted on stderr")
	}
}

// TestRunSessionStartContextFetchServerDown: the hook forward succeeds but
// the context GET fails (server shuts down between requests) - SessionStart
// must still print nothing and note the failure on stderr, never error.
func TestRunSessionStartContextFetchServerDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/hooks" {
			w.Write([]byte(`{"status":"stored"}`))
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	}))
	defer srv.Close()
	baseURL := srv.URL
	origClient := httpClient
	// Force the context GET (the second call Run makes) to fail while the
	// hook forward (the first call) still succeeds, by closing srv right
	// after it has served exactly one request.
	var n int32
	httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if atomic.AddInt32(&n, 1) == 1 {
				return http.DefaultTransport.RoundTrip(req)
			}
			srv.Close()
			return http.DefaultTransport.RoundTrip(req)
		}),
		Timeout: origClient.Timeout,
	}
	defer func() { httpClient = origClient }()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/home/u/myproj"}`)
	if err := Run(stdin, baseURL, "", &out, &errw); err != nil {
		t.Fatalf("server-down must not error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("context fetch failure must print nothing: %s", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("context fetch failure should be noted on stderr")
	}
}

// TestRunFromEmptyIsByteExactPassthrough pins that RunFrom("", ...) behaves
// identically to Run: it never reads stdin itself and delegates straight
// through, so the forwarded body is still byte-identical to stdin (the
// same guarantee TestRunForwardsAndInjectsContext pins for Run directly)
// and SessionStart injection still reaches stdout. This is the "existing
// callers keep working" contract cmdHook now relies on for --from="".
func TestRunFromEmptyIsByteExactPassthrough(t *testing.T) {
	var gotHook []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			gotHook, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			w.Write([]byte(`{"namespace":"agent-p","context":"## Project memory\n- [/a] x","fact_ids":["1"]}`))
		}
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/home/u/myproj","source":"startup"}`
	var out, errw bytes.Buffer
	if err := RunFrom("", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if string(gotHook) != stdinBody {
		t.Fatalf("RunFrom(\"\", ...) must forward stdin byte-exact:\n got:  %s\n want: %s", gotHook, stdinBody)
	}
	if !strings.Contains(out.String(), "additionalContext") {
		t.Fatalf("RunFrom(\"\", ...) must still inject context on SessionStart: %s", out.String())
	}
}

// TestRunFromClaudeIsByteExactPassthrough: from="claude" is the same
// contract as from="", spelled out explicitly.
func TestRunFromClaudeIsByteExactPassthrough(t *testing.T) {
	var gotHook []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHook, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"Stop","session_id":"s1","cwd":"/p","last_assistant_message":"m"}`
	var out, errw bytes.Buffer
	if err := RunFrom("claude", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if string(gotHook) != stdinBody {
		t.Fatalf("RunFrom(\"claude\", ...) must forward stdin byte-exact:\n got:  %s\n want: %s", gotHook, stdinBody)
	}
	if out.Len() != 0 {
		t.Fatalf("non-SessionStart must print nothing: %s", out.String())
	}
}

// TestRunFromClaudeCodeAliasIsPassthrough: --from claude-code must behave
// exactly like --from "" or --from claude - byte-exact passthrough - for
// callers that prefer to name every agent explicitly rather than rely on
// the empty-string default meaning Claude Code.
func TestRunFromClaudeCodeAliasIsPassthrough(t *testing.T) {
	var gotHook []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHook, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"Stop","session_id":"s1","cwd":"/p","last_assistant_message":"m"}`
	var out, errw bytes.Buffer
	if err := RunFrom("claude-code", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if string(gotHook) != stdinBody {
		t.Fatalf("RunFrom(\"claude-code\", ...) must forward stdin byte-exact:\n got:  %s\n want: %s", gotHook, stdinBody)
	}
	if out.Len() != 0 {
		t.Fatalf("non-SessionStart must print nothing: %s", out.String())
	}
}

// TestRunFromIsCaseInsensitive: --from Cursor / --from CLAUDE-CODE must
// resolve the same as their lowercase spellings, both for the passthrough
// alias check and for the translator registry lookup Normalize performs.
func TestRunFromIsCaseInsensitive(t *testing.T) {
	var gotHook []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHook, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"sessionStart","conversation_id":"conv-ci","workspace_roots":["/p"]}`
	var out, errw bytes.Buffer
	if err := RunFrom("Cursor", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if errw.Len() != 0 {
		t.Fatalf("--from Cursor must resolve to the cursor translator, got stderr: %s", errw.String())
	}
	var env struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(gotHook, &env); err != nil {
		t.Fatalf("forwarded body not valid JSON: %v: %s", err, gotHook)
	}
	if env.Source != "cursor" {
		t.Fatalf("source = %q, want cursor", env.Source)
	}

	// --from CLAUDE-CODE (uppercase alias) must also passthrough, byte-exact.
	var out2, errw2 bytes.Buffer
	const claudeBody = `{"hook_event_name":"Stop","session_id":"s1","cwd":"/p"}`
	if err := RunFrom("CLAUDE-CODE", strings.NewReader(claudeBody), srv.URL, "", &out2, &errw2); err != nil {
		t.Fatal(err)
	}
	if string(gotHook) != claudeBody {
		t.Fatalf("--from CLAUDE-CODE must forward stdin byte-exact:\n got:  %s\n want: %s", gotHook, claudeBody)
	}
	if errw2.Len() != 0 {
		t.Fatalf("--from CLAUDE-CODE must passthrough cleanly, got stderr: %s", errw2.String())
	}
}

// TestRunFromCursorTranslatesAndForwards: cursor mode must translate the
// native payload before forwarding, and - critically - must never print an
// injection envelope to stdout, even for a translated SessionStart. Cursor
// has no additionalContext contract; only Claude Code's hook runner reads
// that stdout convention.
func TestRunFromCursorTranslatesAndForwards(t *testing.T) {
	var gotHook []byte
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/v1/agent/hooks" {
			t.Errorf("unexpected path %s (cursor mode must never fetch /v1/agent/context)", r.URL.Path)
			return
		}
		gotHook, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"sessionStart","conversation_id":"conv-9","workspace_roots":["/home/u/myproj"]}`
	var out, errw bytes.Buffer
	if err := RunFrom("cursor", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("cursor mode must never print an injection envelope, even on SessionStart: %s", out.String())
	}
	var env struct {
		HookEventName string `json:"hook_event_name"`
		SessionID     string `json:"session_id"`
		CWD           string `json:"cwd"`
		Source        string `json:"source"`
	}
	if err := json.Unmarshal(gotHook, &env); err != nil {
		t.Fatalf("forwarded body not valid JSON: %v: %s", err, gotHook)
	}
	if env.HookEventName != "SessionStart" || env.SessionID != "conv-9" ||
		env.CWD != "/home/u/myproj" || env.Source != "cursor" {
		t.Fatalf("forwarded envelope: %+v", env)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("cursor mode must make exactly one HTTP call (hooks only, no context), got %d", calls)
	}
}

// TestRunFromCursorBeforeSubmitPromptPrintsContinueTrue: Cursor's
// beforeSubmitPrompt hook is documented (cursor.com/docs/agent/hooks) as
// BLOCKING - the IDE waits for {"continue": true|false} on stdout before
// letting the prompt through. Prior to this fix, punk printed nothing at
// all for any cursor-mode event, which would stall or cancel every prompt
// the moment a beforeSubmitPrompt hook was wired up. punk only observes
// hook traffic and must never block a prompt, so this must always print
// exactly {"continue":true} - never false - after forwarding.
func TestRunFromCursorBeforeSubmitPromptPrintsContinueTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"conv-1","generation_id":"gen-1","prompt":"hello","workspace_roots":["/p"]}`
	var out, errw bytes.Buffer
	if err := RunFrom("cursor", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"continue":true}`+"\n" {
		t.Fatalf("beforeSubmitPrompt stdout = %q, want exactly {\"continue\":true}\\n", out.String())
	}
}

// TestRunFromCursorBeforeSubmitPromptContinuesEvenWhenServerDown: the
// {"continue":true} print must not depend on the forward to
// /v1/agent/hooks succeeding - a dead memory server must still let the
// user's prompt through (fail-open), never silently stall Cursor waiting
// for a stdout response that never comes.
func TestRunFromCursorBeforeSubmitPromptContinuesEvenWhenServerDown(t *testing.T) {
	const stdinBody = `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"conv-1","generation_id":"gen-1","prompt":"hello","workspace_roots":["/p"]}`
	var out, errw bytes.Buffer
	if err := RunFrom("cursor", strings.NewReader(stdinBody), "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"continue":true}`+"\n" {
		t.Fatalf("beforeSubmitPrompt stdout with server down = %q, want exactly {\"continue\":true}\\n (fail-open must still permit the prompt)", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the forward failure should still be noted on stderr")
	}
}

// TestRunFromCursorMalformedBeforeSubmitPromptStillContinues: a payload
// whose hook_event_name is genuinely "beforeSubmitPrompt" but that fails
// Normalize's stricter cursorPayload decode on some OTHER field's shape
// (here workspace_roots sent as a bare string instead of an array, which
// json.Unmarshal rejects as a type mismatch) must still print
// {"continue":true} and exit clean (err == nil). Before the hoist, RunFrom
// returned as soon as Normalize reported an error, never reaching the
// isCursorBeforeSubmitPrompt check at all - which would leave Cursor
// hanging on a blocking beforeSubmitPrompt hook whenever translation
// failed for any reason, exactly the case where fail-open matters most.
func TestRunFromCursorMalformedBeforeSubmitPromptStillContinues(t *testing.T) {
	// workspace_roots is typed []string in cursorPayload; a bare string
	// here makes json.Unmarshal fail with a type error, so
	// Normalize("cursor", raw) returns a non-nil err - while
	// isCursorBeforeSubmitPrompt(raw), which only decodes hook_event_name/
	// cwd as plain strings, still parses raw cleanly and correctly reports
	// this as a beforeSubmitPrompt event.
	const stdinBody = `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"conv-1","generation_id":"gen-1","prompt":"hello","workspace_roots":"not-an-array"}`
	var out, errw bytes.Buffer
	if err := RunFrom("cursor", strings.NewReader(stdinBody), "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"continue":true}`+"\n" {
		t.Fatalf("malformed beforeSubmitPrompt stdout = %q, want exactly {\"continue\":true}\\n", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the normalize failure should still be noted on stderr")
	}
}

// TestRunFromCursorNonPromptEventsStdoutStaysEmpty: every other mapped
// cursor event (not beforeSubmitPrompt) has no stdout contract at all and
// must print nothing - only beforeSubmitPrompt is a blocking Cursor hook.
func TestRunFromCursorNonPromptEventsStdoutStaysEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	payloads := map[string]string{
		"sessionStart":  `{"hook_event_name":"sessionStart","conversation_id":"conv-1","workspace_roots":["/p"]}`,
		"postToolUse":   `{"hook_event_name":"postToolUse","conversation_id":"conv-1","tool_name":"read_file","tool_use_id":"t1","workspace_roots":["/p"]}`,
		"afterFileEdit": `{"hook_event_name":"afterFileEdit","conversation_id":"conv-1","file_path":"/p/a.go","workspace_roots":["/p"]}`,
		"stop":          `{"hook_event_name":"stop","conversation_id":"conv-1","status":"completed","workspace_roots":["/p"]}`,
		"sessionEnd":    `{"hook_event_name":"sessionEnd","conversation_id":"conv-1","reason":"completed","workspace_roots":["/p"]}`,
	}
	for name, body := range payloads {
		var out, errw bytes.Buffer
		if err := RunFrom("cursor", strings.NewReader(body), srv.URL, "", &out, &errw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Len() != 0 {
			t.Fatalf("%s: non-beforeSubmitPrompt cursor event must print nothing, got: %s", name, out.String())
		}
	}
}

// TestRunFromCursorSkippedEventForwardsNothing: an unmapped native cursor
// event must forward nothing at all - no HTTP call, clean exit, empty
// stdout - rather than forwarding garbage or an empty envelope.
func TestRunFromCursorSkippedEventForwardsNothing(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const stdinBody = `{"hook_event_name":"preToolUse","conversation_id":"conv-1","tool_name":"read_file"}`
	var out, errw bytes.Buffer
	if err := RunFrom("cursor", strings.NewReader(stdinBody), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("skipped event must print nothing: %s", out.String())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("skipped event must make no HTTP calls, got %d", calls)
	}
}

// errReader is an io.Reader whose Read always fails, simulating a stdin
// read error (e.g. a broken pipe) independent of any content.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// TestRunFromCursorStdinReadErrorPrintsContinueTrue: a stdin read error
// leaves RunFrom with no hook_event_name to recover at all, so it cannot
// tell whether the in-flight event was Cursor's blocking beforeSubmitPrompt.
// Before this fix, RunFrom printed nothing on this path, which would hang
// Cursor forever on a beforeSubmitPrompt hook that happened to hit a read
// error. Printing {"continue":true} unconditionally for cursor mode here is
// safe since beforeSubmitPrompt is the only one of the six wired events
// that reads stdout at all.
func TestRunFromCursorStdinReadErrorPrintsContinueTrue(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := errReader{err: io.ErrClosedPipe}
	if err := RunFrom("cursor", stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatalf("stdin read error must not error: %v", err)
	}
	if out.String() != `{"continue":true}`+"\n" {
		t.Fatalf("stdin read error stdout = %q, want exactly {\"continue\":true}\\n", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the read failure should still be noted on stderr")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("stdin read error must make no HTTP calls, got %d", calls)
	}
}

// TestRunFromCursorOverCapStdinPrintsContinueTrueWithNoHTTPCalls mirrors
// TestRunStdinOverCapIsRejectedWithNoHTTPCalls, but for cursor mode: stdin
// past maxStdinBytes is truncated by RunFrom's own LimitReader mid-value,
// which turns an otherwise well-formed beforeSubmitPrompt payload into
// incomplete JSON that isCursorBeforeSubmitPrompt cannot parse. Before this
// fix, that meant RunFrom printed nothing at all - exactly the truncated
// payload case where Cursor would be left hanging on its blocking hook.
// Nothing must reach the network either: the payload never parses, so there
// is nothing valid to forward.
func TestRunFromCursorOverCapStdinPrintsContinueTrueWithNoHTTPCalls(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	// A well-formed beforeSubmitPrompt JSON object padded to exactly
	// maxStdinBytes+4096 bytes total, so RunFrom's LimitReader truncates it
	// mid-value and the resulting bytes never parse as complete JSON.
	const prefix = `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"conv-1","generation_id":"gen-1","prompt":"`
	const suffix = `","workspace_roots":["/p"]}`
	pad := strings.Repeat("x", maxStdinBytes+4096-len(prefix)-len(suffix))
	body := prefix + pad + suffix
	if len(body) != maxStdinBytes+4096 {
		t.Fatalf("test setup: stdin body is %d bytes, want %d", len(body), maxStdinBytes+4096)
	}
	stdin := strings.NewReader(body)

	var out, errw bytes.Buffer
	if err := RunFrom("cursor", stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatalf("oversized cursor stdin must not error: %v", err)
	}
	if out.String() != `{"continue":true}`+"\n" {
		t.Fatalf("oversized cursor stdin stdout = %q, want exactly {\"continue\":true}\\n", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("oversized stdin should be noted on stderr")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("oversized cursor stdin must make no HTTP calls, got %d", calls)
	}
}

// TestRunFromUnknownAgentIsFailOpen: an unrecognized --from value must be
// noted on stderr and otherwise swallowed - same fail-open contract as
// every other failure mode in this package - never a returned error, never
// stdout garbage, never an HTTP call with nothing valid to send.
func TestRunFromUnknownAgentIsFailOpen(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"sessionStart"}`)
	if err := RunFrom("some-made-up-agent", stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatalf("unknown agent must not error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unknown agent must print nothing: %s", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("unknown agent should be noted on stderr")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("unknown agent must make no HTTP calls, got %d", calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestRunEmptyContextPrintsNothing: the server responds 200 with an empty
// context string (e.g. a brand new namespace with no facts yet) - Run must
// not print an injection envelope with an empty additionalContext.
func TestRunEmptyContextPrintsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			w.Write([]byte(`{"namespace":"agent-empty","context":"","fact_ids":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/empty"}`)
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("empty context must not produce an injection payload: %s", out.String())
	}
}

// TestRunFromCursorAtCapButParseableStdoutStaysEmpty: a well-formed cursor
// sessionStart payload padded (via a filler field) to land at EXACTLY
// maxStdinBytes total hits RunFrom's len(raw)==maxStdinBytes guard, but the
// JSON is complete and parses cleanly - unlike
// TestRunFromCursorOverCapStdinPrintsContinueTrueWithNoHTTPCalls, where
// LimitReader truncates a longer payload mid-value. The guard exists to
// recover from truncation, not to treat every at-cap payload as suspect: a
// genuinely complete sessionStart at the cap must not be treated as a
// possibly-truncated beforeSubmitPrompt, must not print {"continue":true},
// and must still forward normally like any other mapped, non-blocking
// cursor event.
func TestRunFromCursorAtCapButParseableStdoutStaysEmpty(t *testing.T) {
	var calls int32
	var gotHook []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotHook, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	const prefix = `{"hook_event_name":"sessionStart","conversation_id":"conv-cap","workspace_roots":["/p"],"filler":"`
	const suffix = `"}`
	pad := strings.Repeat("x", maxStdinBytes-len(prefix)-len(suffix))
	body := prefix + pad + suffix
	if len(body) != maxStdinBytes {
		t.Fatalf("test setup: stdin body is %d bytes, want %d", len(body), maxStdinBytes)
	}
	stdin := strings.NewReader(body)

	var out, errw bytes.Buffer
	if err := RunFrom("cursor", stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("at-cap-but-parseable sessionStart must not print {\"continue\":true}: %s", out.String())
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("at-cap-but-parseable payload should still forward normally, got %d calls", calls)
	}
	var env struct {
		HookEventName string `json:"hook_event_name"`
		SessionID     string `json:"session_id"`
	}
	if err := json.Unmarshal(gotHook, &env); err != nil {
		t.Fatalf("forwarded body not valid JSON: %v: %s", err, gotHook)
	}
	if env.HookEventName != "SessionStart" || env.SessionID != "conv-cap" {
		t.Fatalf("forwarded envelope: %+v", env)
	}
}

// TestRunFromNonCursorStdinReadErrorPrintsNothing: a stdin read error under a
// non-cursor --from must leave stdout completely empty - the
// {"continue":true} reply on a read error exists only because Cursor's
// beforeSubmitPrompt is a blocking hook that reads stdout; every other
// agent (or an unrecognized one) has no stdout contract at all, so printing
// {"continue":true} there would be a bare, meaningless leak onto that
// agent's stdout. Mirrors TestRunFromCursorStdinReadErrorPrintsContinueTrue,
// asserting the opposite outcome for a non-cursor from.
func TestRunFromNonCursorStdinReadErrorPrintsNothing(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := errReader{err: io.ErrClosedPipe}
	if err := RunFrom("some-made-up-agent", stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatalf("stdin read error must not error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("non-cursor stdin read error must print nothing (no {\"continue\":true} leak), got: %s", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("the read failure should still be noted on stderr")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("stdin read error must make no HTTP calls, got %d", calls)
	}
}

// TestRunMalformedStdinMakesNoHTTPCalls: bad JSON on stdin must return nil
// (never break the hook process) and must not reach the network at all -
// there is nothing valid to forward.
func TestRunMalformedStdinMakesNoHTTPCalls(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{not valid json`)
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatalf("malformed stdin must not error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("malformed stdin must print nothing: %s", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("malformed stdin should be noted on stderr")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("malformed stdin must not make any HTTP calls, got %d", calls)
	}
}

// TestRunForwardNon200IsNotedOnStderr: a non-200 from /v1/agent/hooks (e.g.
// a bad/expired API key returning 401) currently vanishes completely -
// forwardHook drains and discards the body without ever looking at
// resp.StatusCode. Capture failures must leave one diagnostic line on
// stderr, mirroring fetchContext's non-200 handling, while staying
// fail-open (nil error, no stdout).
func TestRunForwardNon200IsNotedOnStderr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"missing api key"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1","cwd":"/p"}`)
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("forward failure must print nothing: %s", out.String())
	}
	if !strings.Contains(errw.String(), "401") {
		t.Fatalf("forward non-200 status should be noted on stderr, got: %s", errw.String())
	}
}

// TestRunTrailingSlashBaseURLIsTrimmed: a baseURL with a trailing slash
// (e.g. from a user-supplied PUNK_URL env var) must not turn into a
// double-slash path against the real chi router, which 404s on
// "//v1/agent/hooks" with zero diagnostics since chi doesn't CleanPath by
// default. Run itself must trim the trailing slash, not just cmdHook.
func TestRunTrailingSlashBaseURLIsTrimmed(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/v1/agent/hooks" {
			// Simulate chi's default (no CleanPath) behavior: a
			// double-slash path is a 404, not a route match.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1","cwd":"/p"}`)
	if err := Run(stdin, srv.URL+"/", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/agent/hooks" {
		t.Fatalf("trailing-slash baseURL produced path %q, want single-slash /v1/agent/hooks", gotPath)
	}
	if errw.Len() != 0 {
		t.Fatalf("trailing-slash baseURL should forward cleanly with no stderr note, got: %s", errw.String())
	}
}

// TestRunStdinOverCapIsRejectedWithNoHTTPCalls: stdin past maxStdinBytes
// must be truncated by the LimitReader inside Run, which turns otherwise
// well-formed JSON into an incomplete document. That must fail the
// json.Unmarshal (noted on stderr as "bad payload"), return nil, and never
// reach the network - there is nothing valid to forward or key context on.
func TestRunStdinOverCapIsRejectedWithNoHTTPCalls(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	// A well-formed JSON object padded to exactly maxStdinBytes+4096 bytes
	// total, so Run's LimitReader truncates it mid-value and the resulting
	// bytes never parse as complete JSON.
	const prefix = `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/p","pad":"`
	const suffix = `"}`
	pad := strings.Repeat("x", maxStdinBytes+4096-len(prefix)-len(suffix))
	body := prefix + pad + suffix
	if len(body) != maxStdinBytes+4096 {
		t.Fatalf("test setup: stdin body is %d bytes, want %d", len(body), maxStdinBytes+4096)
	}
	stdin := strings.NewReader(body)

	var out, errw bytes.Buffer
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatalf("oversized stdin must not error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversized stdin must print nothing: %s", out.String())
	}
	if !strings.Contains(errw.String(), "bad payload") {
		t.Fatalf("oversized stdin should be noted on stderr as a bad payload, got: %s", errw.String())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("oversized stdin must not make any HTTP calls, got %d", calls)
	}
}

// TestRunNoAuthHeaderWhenAPIKeyEmpty: an empty apiKey must omit the
// Authorization header entirely (not send "Bearer ") on both the hook
// forward and the context fetch.
func TestRunNoAuthHeaderWhenAPIKeyEmpty(t *testing.T) {
	var hookAuth, ctxAuth string
	var hookAuthSet, ctxAuthSet bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			hookAuth = r.Header.Get("Authorization")
			_, hookAuthSet = r.Header["Authorization"]
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			ctxAuth = r.Header.Get("Authorization")
			_, ctxAuthSet = r.Header["Authorization"]
			w.Write([]byte(`{"namespace":"agent-p","context":"## Project memory\n- [/a] x","fact_ids":["1"]}`))
		}
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/p"}`)
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if hookAuthSet || hookAuth != "" {
		t.Fatalf("hook forward must send no Authorization header when apiKey is empty, got %q", hookAuth)
	}
	if ctxAuthSet || ctxAuth != "" {
		t.Fatalf("context fetch must send no Authorization header when apiKey is empty, got %q", ctxAuth)
	}
}

// TestRunContextFetchNon200SkipsInjection: a non-200 from the context
// endpoint (e.g. 401 with no/expired API key) must skip injection rather
// than decoding an error body as if it were {"context":"..."}.
func TestRunContextFetchNon200SkipsInjection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"missing api key"}`))
		}
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/p"}`)
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("non-200 context response must not produce an injection payload: %s", out.String())
	}
	if errw.Len() == 0 {
		t.Fatal("non-200 context response should be noted on stderr")
	}
}

// TestRunContextFetchBodyIsSizeLimited: a hostile/misconfigured server that
// streams an unbounded response body must not balloon the hook process -
// Run should cap what it reads and, since the JSON won't parse as a
// complete document, skip injection rather than hang or OOM.
func TestRunContextFetchBodyIsSizeLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			// Write a JSON object whose "context" value alone is far larger
			// than any sane hook-injection cap, followed by proper closing,
			// to prove a limited reader truncates it into invalid/unusable
			// JSON rather than a giant string being fully buffered.
			w.Write([]byte(`{"namespace":"agent-p","context":"`))
			chunk := strings.Repeat("x", 1<<20)
			for i := 0; i < 20; i++ { // 20MB, well past any 1MB-class cap
				w.Write([]byte(chunk))
			}
			w.Write([]byte(`","fact_ids":[]}`))
		}
	}))
	defer srv.Close()

	var out, errw bytes.Buffer
	stdin := strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/p"}`)
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	// The truncated body is invalid JSON, so decode must fail and injection
	// must be skipped entirely - not merely bounded to "some sane cap".
	if out.Len() != 0 {
		t.Fatalf("truncated/undecodable context body must not produce an injection payload: %d bytes: %s", out.Len(), out.String())
	}
	if !strings.Contains(errw.String(), "context response decode") {
		t.Fatalf("oversized body should be noted on stderr as a decode failure, got: %s", errw.String())
	}
}

// TestRunUserPromptSubmitFetchesTurnContext pins the Claude Code per-turn
// path: a UserPromptSubmit event captures as before AND
// fetches mode=turn context scoped to the prompt, printing the same
// hookSpecificOutput envelope with hookEventName UserPromptSubmit. An
// empty server answer prints nothing (see the empty-context guard).
func TestRunUserPromptSubmitFetchesTurnContext(t *testing.T) {
	var ctxQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			ctxQuery = r.URL.RawQuery
			w.Write([]byte(`{"namespace":"agent-myproj","context":"## Relevant memory\n- [/a] x","fact_ids":["1"]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdin := strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/home/u/myproj","prompt":"why did auth break","prompt_id":"p1"}`)
	var out, errw bytes.Buffer
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mode=turn", "q=why+did+auth+break", "sid=s1"} {
		if !strings.Contains(ctxQuery, want) {
			t.Fatalf("context query %q missing %q", ctxQuery, want)
		}
	}
	var env struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout not valid hook JSON: %v: %s", err, out.String())
	}
	if env.HookSpecificOutput.HookEventName != "UserPromptSubmit" ||
		!strings.Contains(env.HookSpecificOutput.AdditionalContext, "Relevant memory") {
		t.Fatalf("injection payload: %+v", env)
	}
}

// TestRunUserPromptSubmitEmptyContextPrintsNothing pins the quiet-turn
// contract for Claude Code: an empty per-turn context (feature disabled
// server-side, or everything already injected) prints nothing at all.
func TestRunUserPromptSubmitEmptyContextPrintsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			w.Write([]byte(`{"namespace":"agent-myproj","context":"","fact_ids":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdin := strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/p","prompt":"hi","prompt_id":"p1"}`)
	var out, errw bytes.Buffer
	if err := Run(stdin, srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("empty turn context must print nothing: %s", out.String())
	}
}
