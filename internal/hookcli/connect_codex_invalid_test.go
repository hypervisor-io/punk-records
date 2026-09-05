package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file pin the round-8 invalid-document contract for
// the Codex MCP inspector. Two classes of config Codex cannot load were
// previously read as valid entries and reported aligned:
//
//   - duplicate definitions: TOML v1.0.0 rejects a key or table defined
//     more than once (toml.io/v1.0.0 keys: duplicate definitions are
//     rejected, and quoted/bare spellings of the same key are the same
//     key), so a document carrying enabled = false followed by
//     enabled = true in the same table never loads at all. The old
//     inspector read the pair last-wins and reported the entry enabled
//     and aligned. Every duplicate of a recognized
//     [mcp_servers.punk] key, a header/env entry or a punk subtable -
//     including a field declared both as an inline table and as a
//     subtable, which is the same redefinition - must fail the scope as
//     unknown instead of reading a value Codex never sees.
//   - an auth kind outside the upstream enum: Codex's McpServerAuth
//     (config/src/mcp_types.rs:177-187) supports exactly oauth and
//     chatgpt, so an explicit auth value outside the enum is an auth
//     identity Codex cannot load and this inspector cannot establish -
//     conservatively unknown, never accepted presence.
//
// Diagnostics name the file, the line and the key identity only; config
// values (a bearer var name aside, header values above all) are never
// echoed.

// writeInvalidScope writes one config.toml fixture and returns its path.
func writeInvalidScope(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestInspectCodexMCPScopeDuplicatePunkFieldIsInvalid pins the
// duplicate-key half of the round-8 contract at the inspector layer: a
// recognized [mcp_servers.punk] key defined twice in the table is
// invalid TOML Codex refuses to load, so the scope must be unknown -
// never the last value read as effective. Quoted and bare spellings of
// the same key are the same key and count as duplicates.
func TestInspectCodexMCPScopeDuplicatePunkFieldIsInvalid(t *testing.T) {
	cases := map[string]string{
		"duplicate enabled":                "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nenabled = false\nenabled = true\n",
		"duplicate enabled reverse":        "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nenabled = true\nenabled = false\n",
		"duplicate quoted then bare":       "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n\"enabled\" = false\nenabled = true\n",
		"duplicate literal then bare":      "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n'enabled' = false\nenabled = true\n",
		"duplicate url":                    "[mcp_servers.punk]\nurl = 'http://reviewer-marker-value'\nurl = 'http://localhost:9090/mcp?toolset=agent'\n",
		"duplicate inline http_headers":    "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nhttp_headers = { 'X-Punk-Namespace' = 'agent-a' }\nhttp_headers = { 'X-Punk-Namespace' = 'agent-b' }\n",
		"duplicate bearer_token_env_var":   "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nbearer_token_env_var = 'REVIEWER_MARKER_ENV'\nbearer_token_env_var = 'PUNK_API_KEY'\n",
		"duplicate benign knob":            "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nstartup_timeout_sec = 5\nstartup_timeout_sec = 10\n",
		"duplicate auth":                   "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nauth = 'oauth'\nauth = 'chatgpt'\n",
		"duplicate command":                "[mcp_servers.punk]\ncommand = 'prog'\ncommand = 'other'\n",
		"duplicate key after other table":  "[features]\nhooks = true\n\n[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nenabled = false\nenabled = true\n",
		"duplicate across commented lines": "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nenabled = false\n# enabled = false\nenabled = true\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeInvalidScope(t, t.TempDir(), "config.toml", doc)
			sc := EnumerateCodexMCPScopes(p)[0]
			if sc.Err == nil {
				t.Fatalf("a duplicate definition is invalid TOML Codex cannot load and must be unknown: %+v", sc)
			}
			if !strings.Contains(sc.Err.Error(), p) {
				t.Fatalf("diagnostics must name the file: %v", sc.Err)
			}
			if strings.Contains(sc.Err.Error(), "REVIEWER_MARKER_ENV") || strings.Contains(sc.Err.Error(), "reviewer-marker-value") {
				t.Fatalf("diagnostics must never echo config values: %v", sc.Err)
			}
		})
	}
}

// TestInspectCodexMCPScopeDuplicateSubtableIsInvalid pins the
// duplicate-subtable half: a punk subtable declared twice, a header or
// env entry defined twice inside its subtable, or a field declared BOTH
// as an inline table and as a subtable (the same redefinition in two
// spellings) are invalid TOML Codex refuses to load - unknown, never
// last-wins. Quoted and bare table spellings normalize to the same
// table and count as duplicates.
func TestInspectCodexMCPScopeDuplicateSubtableIsInvalid(t *testing.T) {
	cases := map[string]string{
		"duplicate header subtable":        "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.http_headers]\nX-Punk-Namespace = 'agent-a'\n[mcp_servers.punk.http_headers]\nX-Punk-Agent = 'agent-b'\n",
		"duplicate quoted header subtable": "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.http_headers]\nX-Punk-Namespace = 'agent-a'\n[mcp_servers.punk.\"http_headers\"]\nX-Punk-Agent = 'agent-b'\n",
		"duplicate header key":             "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.http_headers]\nX-Punk-Namespace = 'agent-a'\nX-Punk-Namespace = 'agent-b'\n",
		"duplicate quoted header key":      "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.http_headers]\n\"X-Punk-Namespace\" = 'agent-a'\n'X-Punk-Namespace' = 'agent-b'\n",
		"duplicate env_http_headers table": "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.env_http_headers]\nX-Punk-Namespace = 'A'\n[mcp_servers.punk.env_http_headers]\nX-Punk-Agent = 'B'\n",
		"duplicate env_http_headers key":   "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.env_http_headers]\nX-Punk-Namespace = 'A'\nX-Punk-Namespace = 'B'\n",
		"duplicate env subtable":           "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.env]\nFOO = 'a'\n[mcp_servers.punk.env]\nBAR = 'b'\n",
		"duplicate env key":                "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.env]\nFOO = 'a'\nFOO = 'b'\n",
		"env inline plus subtable":         "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nenv = { FOO = 'bar' }\n[mcp_servers.punk.env]\nBAR = 'x'\n",
		"oauth inline plus subtable":       "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\noauth = { }\n[mcp_servers.punk.oauth]\n",
		"duplicate unknown punk subtable":  "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.tools]\nx = 1\n[mcp_servers.punk.tools]\ny = 2\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeInvalidScope(t, t.TempDir(), "config.toml", doc)
			sc := EnumerateCodexMCPScopes(p)[0]
			if sc.Err == nil {
				t.Fatalf("a duplicated subtable or entry is invalid TOML Codex cannot load and must be unknown: %+v", sc)
			}
			if !strings.Contains(sc.Err.Error(), p) {
				t.Fatalf("diagnostics must name the file: %v", sc.Err)
			}
		})
	}
}

// TestInspectCodexMCPScopeAuthKindOutsideEnumIsInvalid pins the auth
// half of the round-8 contract: upstream Codex's McpServerAuth enum
// (config/src/mcp_types.rs:177-187) supports exactly oauth and chatgpt.
// An explicit auth value outside the enum is an entry Codex cannot load
// and an auth identity verify cannot establish - unknown, and the value
// is never echoed. The two supported kinds stay inspectable.
func TestInspectCodexMCPScopeAuthKindOutsideEnumIsInvalid(t *testing.T) {
	for _, kind := range []string{"oauth", "chatgpt"} {
		doc := "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nauth = '" + kind + "'\n"
		p := writeInvalidScope(t, t.TempDir(), "config.toml", doc)
		sc := EnumerateCodexMCPScopes(p)[0]
		if sc.Err != nil || !sc.Installed {
			t.Fatalf("auth = %q is inside the upstream enum and must stay inspectable: %+v (err %v)", kind, sc, sc.Err)
		}
		if m := MergeCodexMCPScopes([]CodexMCPScope{sc}); m.Unknown != "" {
			t.Fatalf("auth = %q alongside url is a valid HTTP-shaped entry: %s", kind, m.Unknown)
		}
	}

	bad := map[string]string{
		"unsupported kind":  "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nauth = 'reviewer-invalid-auth-kind'\n",
		"empty kind":        "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nauth = ''\n",
		"case-variant kind": "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nauth = 'OAuth'\n",
		"non-string kind":   "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nauth = 5\n",
		"basic-string kind": "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nauth = \"reviewer-invalid-auth-kind\"\n",
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			p := writeInvalidScope(t, t.TempDir(), "config.toml", doc)
			sc := EnumerateCodexMCPScopes(p)[0]
			if sc.Err == nil {
				t.Fatalf("an auth kind outside the upstream enum must be unknown: %+v", sc)
			}
			if !strings.Contains(sc.Err.Error(), p) {
				t.Fatalf("diagnostics must name the file: %v", sc.Err)
			}
			if strings.Contains(sc.Err.Error(), "reviewer-invalid-auth-kind") {
				t.Fatalf("diagnostics must never echo the auth value: %v", sc.Err)
			}
			if m := MergeCodexMCPScopes([]CodexMCPScope{sc}); m.Unknown == "" {
				t.Fatalf("an uninspectable auth kind must poison the merged entry: %+v", m)
			}
		})
	}
}

// TestInspectCodexMCPScopeRecognizedEntryShapeStaysInspectable is the
// control for the narrowed boundary: a punk entry using exactly the
// recognized keys - the shape punk connect codex itself writes - stays
// inspectable, and a foreign table with its own repeated-looking keys
// outside the punk subtree does not poison it.
func TestInspectCodexMCPScopeRecognizedEntryShapeStaysInspectable(t *testing.T) {
	doc := "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nbearer_token_env_var = \"PUNK_API_KEY\"\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-x\" }\nstartup_timeout_sec = 10\ntool_timeout_sec = 60\ndefault_tools_approval_mode = \"approve\"\n"
	p := writeInvalidScope(t, t.TempDir(), "config.toml", doc)
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Pin != "agent-x" {
		t.Fatalf("the recognized punk-entry shape must stay inspectable: %+v (err %v)", sc, sc.Err)
	}

	doc = "[tui]\ntheme = 'dark'\n\n[mcp_servers.other]\nurl = 'http://localhost:1'\n\n[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-x\" }\n"
	p = writeInvalidScope(t, t.TempDir(), "config.toml", doc)
	sc = EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Pin != "agent-x" {
		t.Fatalf("foreign tables must not poison an inspectable punk entry: %+v (err %v)", sc, sc.Err)
	}
}
