package hookcli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// codexHookSrv is a minimal capture+context double for the RunFrom("codex")
// tests below: it records the last forwarded /v1/agent/hooks body and
// answers /v1/agent/context with a fixed non-empty context.
type codexHookSrv struct {
	gotHook []byte
	srv     *httptest.Server
}

func newCodexHookSrv(t *testing.T, contextBody string) *codexHookSrv {
	t.Helper()
	h := &codexHookSrv{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/hooks":
			h.gotHook, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"status":"stored"}`))
		case "/v1/agent/context":
			w.Write([]byte(`{"namespace":"agent-p","context":` + strconv.Quote(contextBody) + `,"fact_ids":["1"]}`))
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// TestRunFromCodexNormalizesSessionStart replaces the old byte-exact
// passthrough contract (TestRunFromCodexIsPassthrough, removed by C01):
// RunFrom("codex", ...) now routes through the explicit Codex normalizer,
// so the forwarded body is the TRANSLATED envelope (source hardcoded
// "codex", never Codex's own session-start "source" reason), not stdin
// verbatim. SessionStart remains an injection event: the nested
// hookSpecificOutput envelope Codex 0.153.4's session-start command output
// schema accepts (same shape as Claude Code's) is printed when the server
// has context to inject.
func TestRunFromCodexNormalizesSessionStart(t *testing.T) {
	h := newCodexHookSrv(t, "## Project memory\n- [/a] x")

	var out, errw strings.Builder
	if err := RunFrom("codex", strings.NewReader(string(codexFixture(t, "session_start.json"))), h.srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, h.gotHook)
	if env.HookEventName != "SessionStart" || env.SessionID != "sess-codex-0153-1" {
		t.Fatalf("forwarded envelope = %s", h.gotHook)
	}
	if env.Source != "codex" {
		t.Fatalf("forwarded source = %q, want codex (not the native startup reason)", env.Source)
	}
	if !strings.Contains(out.String(), `"hookSpecificOutput"`) || !strings.Contains(out.String(), "## Project memory") {
		t.Fatalf("codex SessionStart must inject the nested additionalContext envelope: %s", out.String())
	}
}

// TestRunFromCodexUserPromptSubmitMapsTurnID is the C01 regression test at
// the RunFrom boundary: a native 0.153.4 UserPromptSubmit carries turn_id
// and no prompt_id, and the forwarded envelope must carry that turn_id
// verbatim as prompt_id so the server actually retains the capture instead
// of answering "ignored". UserPromptSubmit is also an injection event, so
// a non-empty turn context produces the nested envelope here too.
func TestRunFromCodexUserPromptSubmitMapsTurnID(t *testing.T) {
	h := newCodexHookSrv(t, "- [/a] relevant turn fact")

	var out, errw strings.Builder
	if err := RunFrom("codex", strings.NewReader(string(codexFixture(t, "user_prompt_submit.json"))), h.srv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, h.gotHook)
	if env.HookEventName != "UserPromptSubmit" {
		t.Fatalf("forwarded envelope = %s", h.gotHook)
	}
	if env.PromptID != "turn-codex-0001" {
		t.Fatalf("forwarded prompt_id = %q, want native turn_id verbatim", env.PromptID)
	}
	if env.Prompt != "normalize native codex hook events" {
		t.Fatalf("forwarded prompt = %q", env.Prompt)
	}
	if !strings.Contains(out.String(), `"hookSpecificOutput"`) || !strings.Contains(out.String(), "relevant turn fact") {
		t.Fatalf("codex UserPromptSubmit must inject the turn context envelope: %s", out.String())
	}
}

// TestRunFromCodexCaptureOnlyEventsPrintNothing pins stdout semantics for
// the capture-only events: PostToolUse and Stop forward their translated
// envelopes but must leave stdout completely empty - neither event has an
// additionalContext contract upstream (their output schemas carry no such
// field for punk's use).
func TestRunFromCodexCaptureOnlyEventsPrintNothing(t *testing.T) {
	for _, fixture := range []string{"post_tool_use.json", "stop.json"} {
		h := newCodexHookSrv(t, "context that must never print")
		var out, errw strings.Builder
		if err := RunFrom("codex", strings.NewReader(string(codexFixture(t, fixture))), h.srv.URL, "", &out, &errw); err != nil {
			t.Fatal(err)
		}
		if len(h.gotHook) == 0 {
			t.Fatalf("%s: nothing forwarded", fixture)
		}
		if out.Len() != 0 {
			t.Fatalf("%s: capture-only hook must print nothing, got %s", fixture, out.String())
		}
	}
}

// TestRunFromCodexFailOpen mirrors the fail-open contract every other
// entry point documents: a dead server must not error and must not print
// anything, and malformed stdin must not error either.
func TestRunFromCodexFailOpen(t *testing.T) {
	var out, errw strings.Builder
	if err := RunFrom("codex", strings.NewReader(string(codexFixture(t, "session_start.json"))), "http://127.0.0.1:1", "", &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("dead server must print nothing: %s", out.String())
	}
	var out2, errw2 strings.Builder
	if err := RunFrom("codex", strings.NewReader(`{not json`), "http://127.0.0.1:1", "", &out2, &errw2); err != nil {
		t.Fatal(err)
	}
	if out2.Len() != 0 {
		t.Fatalf("malformed stdin must print nothing: %s", out2.String())
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
