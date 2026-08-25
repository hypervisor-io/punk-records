package hookcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// hermesTestServer stands in for a punk server, recording the last hook
// body it received and counting calls to each endpoint. ctxBody is what
// GET /v1/agent/context answers with; an empty ctxBody answers with a
// context field holding an empty string, which callers must treat exactly
// like "nothing to inject".
type hermesTestServer struct {
	srv          *httptest.Server
	hookCalls    int32
	ctxCalls     int32
	lastHook     []byte
	lastCtxQuery url.Values
}

func newHermesTestServer(t *testing.T, ctxBody string) *hermesTestServer {
	t.Helper()
	h := &hermesTestServer{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			atomic.AddInt32(&h.hookCalls, 1)
			body, _ := io.ReadAll(r.Body)
			h.lastHook = body
			_, _ = w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			atomic.AddInt32(&h.ctxCalls, 1)
			h.lastCtxQuery = r.URL.Query()
			payload, _ := json.Marshal(map[string]any{"context": ctxBody})
			_, _ = w.Write(payload)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// TestRunFromHermesFirstTurnInjectsContext pins the whole injection path:
// the prompt is captured, context is fetched for the payload's cwd, and the
// reply is Hermes' OWN flat {"context":...} shape - not Claude Code's
// nested hookSpecificOutput envelope, and not Copilot's additionalContext.
func TestRunFromHermesFirstTurnInjectsContext(t *testing.T) {
	h := newHermesTestServer(t, "## Project memory\n- migrations run on boot")
	stdin := `{"hook_event_name":"pre_llm_call","session_id":"s1","cwd":"/home/u/proj",
		"extra":{"user_message":"what broke","is_first_turn":true}}`

	var out, errw bytes.Buffer
	if err := RunFromHermes(strings.NewReader(stdin), h.srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if h.hookCalls != 1 {
		t.Fatalf("hook forwards = %d, want 1", h.hookCalls)
	}
	if h.ctxCalls != 1 {
		t.Fatalf("context fetches = %d, want 1", h.ctxCalls)
	}

	var reply map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &reply); err != nil {
		t.Fatalf("stdout is not valid JSON: %v: %s", err, out.String())
	}
	if len(reply) != 1 {
		t.Fatalf("reply must carry exactly the context key, got %v", reply)
	}
	got, _ := reply["context"].(string)
	if got != "## Project memory\n- migrations run on boot" {
		t.Fatalf("reply context = %q", got)
	}
}

// TestRunFromHermesLaterTurnFetchesTurnContext pins the per-turn path:
// a non-first pre_llm_call still captures the prompt,
// and now fetches a mode=turn context scoped to the prompt text instead
// of skipping injection entirely. A non-empty answer is printed in
// Hermes' own flat {"context":...} shape.
func TestRunFromHermesLaterTurnFetchesTurnContext(t *testing.T) {
	h := newHermesTestServer(t, "## Relevant memory\n- [k] fact")
	stdin := `{"hook_event_name":"pre_llm_call","session_id":"s1","cwd":"/home/u/proj",
		"extra":{"user_message":"and now","is_first_turn":false}}`

	var out, errw bytes.Buffer
	if err := RunFromHermes(strings.NewReader(stdin), h.srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if h.hookCalls != 1 {
		t.Fatalf("hook forwards = %d, want 1 (capture must not be gated)", h.hookCalls)
	}
	if h.ctxCalls != 1 {
		t.Fatalf("context fetches = %d, want 1 on a non-first turn", h.ctxCalls)
	}
	if got := h.lastCtxQuery.Get("mode"); got != "turn" {
		t.Fatalf("context fetch mode = %q, want turn", got)
	}
	if got := h.lastCtxQuery.Get("q"); got != "and now" {
		t.Fatalf("context fetch q = %q, want the prompt text", got)
	}
	if got := h.lastCtxQuery.Get("sid"); got != "s1" {
		t.Fatalf("context fetch sid = %q, want s1", got)
	}

	var reply map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &reply); err != nil {
		t.Fatalf("stdout is not valid JSON: %v: %s", err, out.String())
	}
	if got, _ := reply["context"].(string); got != "## Relevant memory\n- [k] fact" {
		t.Fatalf("reply context = %q", got)
	}

	var env claudeEnvelope
	if err := json.Unmarshal(h.lastHook, &env); err != nil {
		t.Fatalf("forwarded body not valid JSON: %v", err)
	}
	if env.HookEventName != "UserPromptSubmit" || env.Prompt != "and now" || env.PromptID == "" {
		t.Fatalf("forwarded envelope: %+v", env)
	}
}

// TestRunFromHermesLaterTurnEmptyContextPrintsNothing pins the quiet-turn
// contract: when the server answers an empty context (per-turn injection
// disabled, or nothing new matches the prompt), a non-first pre_llm_call
// prints nothing at all - exactly the pre-per-turn behavior.
func TestRunFromHermesLaterTurnEmptyContextPrintsNothing(t *testing.T) {
	h := newHermesTestServer(t, "")
	stdin := `{"hook_event_name":"pre_llm_call","session_id":"s1","cwd":"/home/u/proj",
		"extra":{"user_message":"and now","is_first_turn":false}}`

	var out, errw bytes.Buffer
	if err := RunFromHermes(strings.NewReader(stdin), h.srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if h.hookCalls != 1 {
		t.Fatalf("hook forwards = %d, want 1", h.hookCalls)
	}
	if h.ctxCalls != 1 {
		t.Fatalf("context fetches = %d, want 1", h.ctxCalls)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must stay empty on an empty context, got: %s", out.String())
	}
}

// TestRunFromHermesNonPromptEventsPrintNothing pins that the three
// observation events forward their envelope and keep stdout completely
// empty - none of them has a documented reply contract, and printing an
// unexpected object into a hook Hermes parses as JSON would be garbage.
func TestRunFromHermesNonPromptEventsPrintNothing(t *testing.T) {
	cases := map[string]string{
		"on_session_start": `{"hook_event_name":"on_session_start","session_id":"s","cwd":"/r"}`,
		"post_tool_call": `{"hook_event_name":"post_tool_call","session_id":"s","cwd":"/r","tool_name":"terminal",
			"tool_input":{"command":"ls"},"extra":{"tool_call_id":"tc1","result":"ok"}}`,
		"post_llm_call": `{"hook_event_name":"post_llm_call","session_id":"s","cwd":"/r",
			"extra":{"assistant_response":"done"}}`,
	}
	for name, stdin := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHermesTestServer(t, "context that must not be fetched")
			var out, errw bytes.Buffer
			if err := RunFromHermes(strings.NewReader(stdin), h.srv.URL, "", &out, &errw); err != nil {
				t.Fatal(err)
			}
			if h.hookCalls != 1 {
				t.Fatalf("hook forwards = %d, want 1", h.hookCalls)
			}
			if h.ctxCalls != 0 {
				t.Fatalf("context fetches = %d, want 0", h.ctxCalls)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout must be empty, got: %s", out.String())
			}
		})
	}
}

// TestRunFromHermesUnmappedEventForwardsNothing pins that an event punk
// never wired (but a user might have pointed at the same command by hand)
// is a clean no-op rather than an error or a partial forward.
func TestRunFromHermesUnmappedEventForwardsNothing(t *testing.T) {
	h := newHermesTestServer(t, "")
	stdin := `{"hook_event_name":"on_session_end","session_id":"s","cwd":"/r","extra":{"completed":true}}`
	var out, errw bytes.Buffer
	if err := RunFromHermes(strings.NewReader(stdin), h.srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if h.hookCalls != 0 || h.ctxCalls != 0 || out.Len() != 0 {
		t.Fatalf("unmapped event: hooks=%d ctx=%d stdout=%q", h.hookCalls, h.ctxCalls, out.String())
	}
}

// TestRunFromHermesFailOpen pins the contract every entry point in this
// package shares: a dead server, a malformed payload, and an empty recalled
// context all leave stdout untouched and still return nil, so a hook
// failure can never break or stall a Hermes turn.
func TestRunFromHermesFailOpen(t *testing.T) {
	t.Run("dead server", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		dead.Close()
		stdin := `{"hook_event_name":"pre_llm_call","session_id":"s","cwd":"/r","extra":{"user_message":"x","is_first_turn":true}}`
		var out, errw bytes.Buffer
		if err := RunFromHermes(strings.NewReader(stdin), dead.URL, "", &out, &errw); err != nil {
			t.Fatalf("must stay fail-open, got %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout must stay empty when nothing could be fetched, got %q", out.String())
		}
		if errw.Len() == 0 {
			t.Fatal("failures must be noted on stderr")
		}
	})

	t.Run("malformed payload", func(t *testing.T) {
		h := newHermesTestServer(t, "x")
		var out, errw bytes.Buffer
		if err := RunFromHermes(strings.NewReader(`{"hook_event_name":`), h.srv.URL, "", &out, &errw); err != nil {
			t.Fatalf("must stay fail-open, got %v", err)
		}
		if h.hookCalls != 0 || out.Len() != 0 {
			t.Fatalf("malformed payload: hooks=%d stdout=%q", h.hookCalls, out.String())
		}
	})

	t.Run("empty context", func(t *testing.T) {
		h := newHermesTestServer(t, "")
		stdin := `{"hook_event_name":"pre_llm_call","session_id":"s","cwd":"/r","extra":{"user_message":"x","is_first_turn":true}}`
		var out, errw bytes.Buffer
		if err := RunFromHermes(strings.NewReader(stdin), h.srv.URL, "", &out, &errw); err != nil {
			t.Fatal(err)
		}
		if h.ctxCalls != 1 {
			t.Fatalf("context fetches = %d, want 1", h.ctxCalls)
		}
		if out.Len() != 0 {
			t.Fatalf("an empty recalled context must print nothing, got %q", out.String())
		}
	})
}

// TestRunFromHermesSendsBearerKey pins that the API key reaches BOTH
// requests (capture and context), not just the first - an injection path
// that silently 401s would look identical to "no memory yet".
func TestRunFromHermesSendsBearerKey(t *testing.T) {
	var authed int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k3y" {
			t.Errorf("%s missing bearer: %q", r.URL.Path, r.Header.Get("Authorization"))
		} else {
			atomic.AddInt32(&authed, 1)
		}
		if r.URL.Path == "/v1/agent/context" {
			_, _ = w.Write([]byte(`{"context":"memory"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()

	stdin := `{"hook_event_name":"pre_llm_call","session_id":"s","cwd":"/r","extra":{"user_message":"x","is_first_turn":true}}`
	var out, errw bytes.Buffer
	if err := RunFromHermes(strings.NewReader(stdin), srv.URL, "k3y", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if authed != 2 {
		t.Fatalf("authenticated requests = %d, want 2 (capture and context)", authed)
	}
}
