package hookcli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectCodexHooksWritesClaudeShapedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(p, []byte(`{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-lint.sh"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectCodexHooks(p, "/usr/local/bin/punk", "https://punk.example.com", "agent-x-abcdef")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, p)
	hooks := m["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"} {
		raw, _ := json.Marshal(hooks[ev])
		if !strings.Contains(string(raw), "/usr/local/bin/punk hook --url https://punk.example.com --from codex --ns agent-x-abcdef") {
			t.Fatalf("%s missing punk entry: %s", ev, raw)
		}
	}
	if raw, _ := json.Marshal(hooks["SessionStart"]); !strings.Contains(string(raw), `"matcher":"startup|resume"`) {
		t.Fatalf("SessionStart must match startup|resume only: %s", raw)
	}
	if raw, _ := json.Marshal(hooks["PostToolUse"]); !strings.Contains(string(raw), "my-lint.sh") {
		t.Fatal("user PostToolUse entry must survive")
	}
	if changed, err := ConnectCodexHooks(p, "/usr/local/bin/punk", "https://punk.example.com", "agent-x-abcdef"); err != nil || changed {
		t.Fatalf("idempotent: changed=%v err=%v", changed, err)
	}
}

func TestRunFromCodexIsPassthrough(t *testing.T) {
	// Mirrors the Claude passthrough contract (TestRunFromClaudeIsByteExactPassthrough
	// and the from="" test in hookcli_test.go): Codex's hook payloads share
	// Claude Code's field names, so Run must forward stdin byte-exact and
	// SessionStart must still inject the context envelope.
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

	var out, errw strings.Builder
	in := `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/w","source":"startup","model":"gpt-5","permission_mode":"default"}`
	if err := RunFrom("codex", strings.NewReader(in), srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if string(gotHook) != in {
		t.Fatalf("RunFrom(\"codex\", ...) must forward stdin byte-exact:\n got:  %s\n want: %s", gotHook, in)
	}
	if !strings.Contains(out.String(), `"hookSpecificOutput"`) {
		t.Fatalf("codex SessionStart must inject context: %s", out.String())
	}

	var out2, errw2 strings.Builder
	gotHook = nil
	in2 := `{"hook_event_name":"Stop","session_id":"s1","cwd":"/p","stop_hook_active":false,"last_assistant_message":"m"}`
	if err := RunFrom("codex", strings.NewReader(in2), srv.URL, "", &out2, &errw2); err != nil {
		t.Fatal(err)
	}
	if string(gotHook) != in2 {
		t.Fatalf("RunFrom(\"codex\", ...) must forward stdin byte-exact:\n got:  %s\n want: %s", gotHook, in2)
	}
	if out2.Len() != 0 {
		t.Fatalf("non-SessionStart must print nothing: %s", out2.String())
	}
}
