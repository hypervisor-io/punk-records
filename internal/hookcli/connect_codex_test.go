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

func TestConnectCodexConfigWritesManagedBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	orig := "model = \"gpt-5\"\n\n[projects.\"/x\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	o := MCPEntryOpts{ServerURL: "https://punk.example.com", APIKey: "prk_secret", Namespace: "agent-x-abcdef", Agent: "alice@laptop"}
	changed, err := ConnectCodexConfig(p, o, true, false)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	raw, _ := os.ReadFile(p)
	s := string(raw)
	if !strings.HasPrefix(s, orig) {
		t.Fatalf("user content must be preserved verbatim at the top:\n%s", s)
	}
	for _, want := range []string{
		"# punk-managed-start", "# punk-managed-end",
		"[mcp_servers.punk]", `url = "https://punk.example.com/mcp?toolset=agent"`,
		`bearer_token_env_var = "PUNK_API_KEY"`,
		`"X-Punk-Namespace" = "agent-x-abcdef"`, `"X-Punk-Agent" = "alice@laptop"`,
		`default_tools_approval_mode = "approve"`, "[features]", "hooks = true",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "prk_secret") {
		t.Fatal("literal API key must never be written into config.toml")
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if changed, _ := ConnectCodexConfig(p, o, true, false); changed {
		t.Fatal("idempotent")
	}
	// URL change rewrites only the block.
	o.ServerURL = "https://punk2.example.com"
	if changed, _ := ConnectCodexConfig(p, o, true, false); !changed {
		t.Fatal("changed URL must rewrite the block")
	}
	raw, _ = os.ReadFile(p)
	if strings.Count(string(raw), "# punk-managed-start") != 1 || !strings.Contains(string(raw), "punk2.example.com") || strings.Contains(string(raw), "punk.example.com/mcp") {
		t.Fatalf("block not replaced in place:\n%s", raw)
	}
}

func TestConnectCodexConfigFeaturesTableOutsideBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[features]\nweb_search = true\n\n[tui]\ntheme = \"dark\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCodexConfig(p, MCPEntryOpts{ServerURL: "http://localhost:9090"}, true, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	s := string(raw)
	if strings.Count(s, "[features]") != 1 {
		t.Fatalf("must not create a second [features] table:\n%s", s)
	}
	if !strings.Contains(s, "[features]\nhooks = true\nweb_search = true\n") {
		t.Fatalf("hooks = true must be inserted into the existing [features] table:\n%s", s)
	}
	if !strings.Contains(s, "[tui]\ntheme = \"dark\"\n") {
		t.Fatal("other tables must be untouched")
	}
	// Existing hooks = false is left alone (user's choice) but reported.
	if err := os.WriteFile(p, []byte("[features]\nhooks = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCodexConfig(p, MCPEntryOpts{ServerURL: "http://localhost:9090"}, true, false); err == nil {
		t.Fatal("hooks = false set by the user must be reported as an error naming the line, not silently flipped")
	}
}

func TestConnectCodexConfigRefusesForeignPunkTable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[mcp_servers.punk]\ncommand = \"something-else\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := MCPEntryOpts{ServerURL: "http://localhost:9090"}
	if _, err := ConnectCodexConfig(p, o, false, false); err == nil {
		t.Fatal("foreign [mcp_servers.punk] must be refused without force")
	}
	if _, err := ConnectCodexConfig(p, o, false, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "something-else") || strings.Count(string(raw), "[mcp_servers.punk]") != 1 {
		t.Fatalf("force must replace the foreign table:\n%s", raw)
	}
}

func TestConnectCodexConfigKeepsCRLF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	orig := "model = \"gpt-5\"\r\n\r\n[features]\r\nweb_search = true\r\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	o := MCPEntryOpts{ServerURL: "http://localhost:9090"}
	if _, err := ConnectCodexConfig(p, o, true, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	s := string(raw)
	if strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") {
		t.Fatalf("mixed line endings:\n%q", s)
	}
	if !strings.Contains(s, "[features]\r\nhooks = true\r\nweb_search = true\r\n") {
		t.Fatalf("hooks flag not inserted with CRLF:\n%q", s)
	}
	if changed, _ := ConnectCodexConfig(p, o, true, false); changed {
		t.Fatal("idempotent on CRLF files")
	}
}
