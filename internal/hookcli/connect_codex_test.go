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

// TestDedupeCodexHookScopesRemovesByteEquivalentProjectGroups is the C03
// red proof for hook scopes: the same punk-managed group registered in
// BOTH the global ~/.codex/hooks.json and the project ./.codex/hooks.json
// executes every wired event twice for that repo. The project copy of a
// byte-equivalent group is the redundant one (the global registration
// already fires here) and must be removed; user entries and non-punk
// groups are untouched, and a second pass reports no change.
func TestDedupeCodexHookScopesRemovesByteEquivalentProjectGroups(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "global", "hooks.json")
	projectPath := filepath.Join(dir, "project", "hooks.json")

	if _, err := ConnectCodexHooks(globalPath, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCodexHooks(projectPath, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil {
		t.Fatal(err)
	}
	// A user entry in the project file must survive the dedupe.
	settings, _, err := loadSettings(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	hooks := settings["hooks"].(map[string]any)
	hooks["PostToolUse"] = append(hooks["PostToolUse"].([]any), map[string]any{
		"matcher": "Bash",
		"hooks":   []any{map[string]any{"type": "command", "command": "my-lint.sh"}},
	})
	raw, _ := encodeSettings(settings)
	if err := os.WriteFile(projectPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	notes, changed, err := DedupeCodexHookScopes(globalPath, projectPath, "/usr/local/bin/punk")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	settings, _, err = loadSettings(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	hooks = settings["hooks"].(map[string]any)
	for _, ev := range codexHookEvents {
		groups, _ := hooks[ev].([]any)
		for _, g := range groups {
			if isPunkManagedGroup(g, "/usr/local/bin/punk") {
				t.Fatalf("%s: byte-equivalent punk group must be removed from the project file", ev)
			}
		}
	}
	raw, _ = json.Marshal(hooks["PostToolUse"])
	if !strings.Contains(string(raw), "my-lint.sh") {
		t.Fatalf("user entry must survive: %s", raw)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "UserPromptSubmit") {
		t.Fatalf("notes must name the deduped events: %s", joined)
	}

	// Global file untouched, and a rerun is a no-op.
	settings, _, err = loadSettings(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	if g := settings["hooks"].(map[string]any)["Stop"].([]any); !isPunkManagedGroup(g[len(g)-1], "/usr/local/bin/punk") {
		t.Fatal("global registration must be preserved")
	}
	if _, changed, err := DedupeCodexHookScopes(globalPath, projectPath, "/usr/local/bin/punk"); err != nil || changed {
		t.Fatalf("idempotent: changed=%v err=%v", changed, err)
	}
}

// TestDedupeCodexHookScopesKeepsNamespacePinnedProjectGroups: a project
// registration whose command differs from the global one (a --ns pin from
// punk connect codex --project) is NOT a byte-equivalent duplicate - it is
// kept, and the note explains the double execution so the user can act.
func TestDedupeCodexHookScopesKeepsNamespacePinnedProjectGroups(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "global", "hooks.json")
	projectPath := filepath.Join(dir, "project", "hooks.json")

	if _, err := ConnectCodexHooks(globalPath, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCodexHooks(projectPath, "/usr/local/bin/punk", "http://localhost:9090", "agent-proj-1234"); err != nil {
		t.Fatal(err)
	}

	notes, changed, err := DedupeCodexHookScopes(globalPath, projectPath, "/usr/local/bin/punk")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("namespace-pinned project groups are not byte-equivalent; nothing may be removed")
	}
	settings, _, err := loadSettings(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	groups := settings["hooks"].(map[string]any)["Stop"].([]any)
	if !isPunkManagedGroup(groups[len(groups)-1], "/usr/local/bin/punk") {
		t.Fatal("pinned project group must be kept")
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "both") || !strings.Contains(joined, "Stop") {
		t.Fatalf("double-execution diagnostic must name the event and both scopes: %s", joined)
	}
}

// TestConnectCodexConfigStripsAllManagedBlocks: a config.toml carrying TWO
// punk-managed blocks (a paste accident or a pre-stripManagedBlock bug)
// must converge to exactly one after connect - the MCP alias is
// punk-managed twice, which is precisely a duplicate punk can reconcile.
func TestConnectCodexConfigStripsAllManagedBlocks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	o := MCPEntryOpts{ServerURL: "http://localhost:9090"}
	if _, err := ConnectCodexConfig(p, o, false, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	// Duplicate the managed block by hand, then reconnect.
	if err := os.WriteFile(p, append(raw, raw...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCodexConfig(p, o, false, false); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(p)
	if strings.Count(string(raw), codexBlockStart) != 1 || strings.Count(string(raw), codexPunkTable) != 1 {
		t.Fatalf("all stale managed blocks must be stripped, leaving exactly one:\n%s", raw)
	}
}

// TestDetectCodexMCPAliases: two mcp_servers tables pointing at the same
// punk /mcp endpoint are duplicate aliases Codex would connect twice.
// Punk-managed duplicates are reconciled elsewhere (the marker block);
// this detector must report the aliases so the user can resolve a foreign
// one - it never edits the file.
func TestDetectCodexMCPAliases(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	content := "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n\n" +
		"[mcp_servers.punk_old]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n\n" +
		"[mcp_servers.other]\nurl = \"http://example.com/mcp\"\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	aliases, err := DetectCodexMCPAliases(p, "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 2 || aliases[0] != "punk" || aliases[1] != "punk_old" {
		t.Fatalf("aliases = %v, want [punk punk_old]", aliases)
	}

	// One alias only: no duplicate, empty result.
	p2 := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p2, []byte("[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliases, err = DetectCodexMCPAliases(p2, "http://localhost:9090")
	if err != nil || len(aliases) != 0 {
		t.Fatalf("aliases = %v err=%v, want none", aliases, err)
	}

	// CRLF content parses the same.
	p3 := filepath.Join(t.TempDir(), "config.toml")
	crlf := strings.ReplaceAll(content, "\n", "\r\n")
	if err := os.WriteFile(p3, []byte(crlf), 0o600); err != nil {
		t.Fatal(err)
	}
	aliases, err = DetectCodexMCPAliases(p3, "http://localhost:9090")
	if err != nil || len(aliases) != 2 {
		t.Fatalf("CRLF: aliases = %v err=%v", aliases, err)
	}
}

// TestDedupeCodexHookScopesPreservesDifferentMatchers is the review
// regression for matcher-blind deletion: a global SessionStart group with
// matcher "startup" and a project group with the same command but matcher
// "resume" are NOT equivalent - the project registration covers a case the
// global one does not, so nothing may be removed and the note must name
// the difference. (Modeled on the reviewer's
// TestReviewerC03PreserveDifferentHookMatchers.)
func TestDedupeCodexHookScopesPreservesDifferentMatchers(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "global.json")
	projectPath := filepath.Join(dir, "project.json")
	for path, matcher := range map[string]string{globalPath: "startup", projectPath: "resume"} {
		payload := `{"hooks":{"SessionStart":[{"matcher":"` + matcher + `","hooks":[{"type":"command","command":"/bin/punk hook --url http://localhost:9090 --from codex"}]}]}}`
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(projectPath)
	_, changed, err := DedupeCodexHookScopes(globalPath, projectPath, "/bin/punk")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(projectPath)
	if changed || string(before) != string(after) {
		t.Fatal("resume-only project hook must not be removed against a startup-only global hook")
	}
}

// TestDedupeCodexHookScopesPreservesDifferentHookMetadata: same command,
// same matcher, but a different timeout - conservative preservation: the
// project group stays and the diagnostic fires.
func TestDedupeCodexHookScopesPreservesDifferentHookMetadata(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "global.json")
	projectPath := filepath.Join(dir, "project.json")
	global := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/bin/punk hook --url http://localhost:9090 --from codex","timeout":10}]}]}}`
	project := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/bin/punk hook --url http://localhost:9090 --from codex","timeout":30}]}]}}`
	os.WriteFile(globalPath, []byte(global), 0o600)
	os.WriteFile(projectPath, []byte(project), 0o600)

	_, changed, err := DedupeCodexHookScopes(globalPath, projectPath, "/bin/punk")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("different hook timeout must preserve the project group")
	}
}

// TestDedupeCodexHookScopesRemovesExactlyIdenticalGroups pins the positive
// case at group granularity: same command, same matcher, same timeout -
// removed from the project file.
func TestDedupeCodexHookScopesRemovesExactlyIdenticalGroups(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "global.json")
	projectPath := filepath.Join(dir, "project.json")
	payload := `{"hooks":{"SessionStart":[{"matcher":"startup|resume","hooks":[{"type":"command","command":"/bin/punk hook --url http://localhost:9090 --from codex","timeout":10}]}]}}`
	os.WriteFile(globalPath, []byte(payload), 0o600)
	os.WriteFile(projectPath, []byte(payload), 0o600)

	_, changed, err := DedupeCodexHookScopes(globalPath, projectPath, "/bin/punk")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	settings, _, err := loadSettings(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if groups, ok := settings["hooks"].(map[string]any)["SessionStart"]; ok && len(groups.([]any)) != 0 {
		t.Fatalf("identical project group must be removed: %v", groups)
	}
}

// TestDedupeCodexHookScopesSameFileNoOp is the same-file boundary from
// review round 3: global and project paths naming ONE file (directly or
// through aliases) must no-op - deduping a file against itself would
// delete its own punk registrations. (Reviewer's
// TestReviewerC03SameHookFileMustNotDedupeItself, plus a symlink alias.)
func TestDedupeCodexHookScopesSameFileNoOp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hooks.json")
	if _, err := ConnectCodexHooks(p, "/bin/punk", "http://localhost:19393", ""); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)

	// Direct: identical path twice.
	if _, changed, err := DedupeCodexHookScopes(p, p, "/bin/punk"); err != nil || changed {
		t.Fatalf("identical paths must no-op: changed=%v err=%v", changed, err)
	}
	// Aliased: a symlink to the same file.
	link := filepath.Join(dir, "linked-hooks.json")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := DedupeCodexHookScopes(p, link, "/bin/punk"); err != nil || changed {
		t.Fatalf("symlinked same file must no-op: changed=%v err=%v", changed, err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("one hooks file must never be deduped against itself")
	}
}
