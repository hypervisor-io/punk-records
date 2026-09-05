package hookcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file pin the round-6 cross-layer contract: Codex
// recursively merges MCP table FIELDS across config layers rather than
// replacing whole entries (upstream config/src/merge.rs:58-132,
// state.rs:343), so the entry Codex actually opens is the field-wise
// merge of every contributing scope - a field the project entry lacks
// inherits from the global layer, a field the project entry defines
// wins, and a header-subtable fragment in a layer without its own
// [mcp_servers.punk] table still contributes. MergeCodexMCPScopes
// computes that effective entry; a top-entry-only view is never it.

// writeMergeScope writes one config.toml scope fixture and returns its
// path.
func writeMergeScope(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMergeCodexMCPScopesInheritsLowerLayerFields is the reviewer's
// round-6 red proof at the merge layer: a project entry lacking
// enabled, http_headers_helper and bearer_token_env_var INHERITS those
// fields from the global layer's entry. An inherited enabled = false
// is not a working connection, an inherited helper makes the merged
// headers unverified, and an inherited bearer var names the credential
// the merged entry actually reads - a top-entry-only view reported a
// false alignment for all three.
func TestMergeCodexMCPScopesInheritsLowerLayerFields(t *testing.T) {
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-proj\" }\n")
	global := writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenabled = false\nhttp_headers_helper = 'printf x'\nbearer_token_env_var = \"PUNK_TEST_C05_INHERITED_TOKEN\"\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown != "" {
		t.Fatalf("the merged entry must be inspectable: %s", m.Unknown)
	}
	if !m.Defined || len(m.Contributing) != 2 {
		t.Fatalf("both layers contribute to the merged entry: %+v", m)
	}
	if !m.Disabled || m.DisabledFrom != global {
		t.Fatalf("a global-only enabled = false must disable the merged entry: %+v", m)
	}
	if !m.HeadersHelper || m.HeadersHelperFrom != global {
		t.Fatalf("a global-only http_headers_helper must mark the merged entry's headers unverified: %+v", m)
	}
	if m.BearerEnv != "PUNK_TEST_C05_INHERITED_TOKEN" || m.BearerEnvFrom != global {
		t.Fatalf("a global-only bearer_token_env_var must be the merged entry's credential source: %+v", m)
	}
	if m.Pin != "agent-proj" || m.Endpoint != "http://localhost:9090/mcp?toolset=agent" || m.EndpointFrom != project {
		t.Fatalf("the project layer's own fields must stay effective: %+v", m)
	}
}

// TestMergeCodexMCPScopesHigherLayerFieldWins pins the other half of
// field-wise merge: a field the higher layer DEFINES overrides the
// inherited value - a project enabled = true re-enables a globally
// disabled entry and a project url replaces the global url. Absent
// inherits, defined overrides; absent must never default to
// false/empty.
func TestMergeCodexMCPScopesHigherLayerFieldWins(t *testing.T) {
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://project:9090/mcp?toolset=agent\"\nenabled = true\n")
	global := writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk]\nurl = \"http://global:9090/mcp?toolset=agent\"\nenabled = false\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown != "" {
		t.Fatalf("the merged entry must be inspectable: %s", m.Unknown)
	}
	if m.Disabled {
		t.Fatalf("the project enabled = true must override the global enabled = false: %+v", m)
	}
	if m.Endpoint != "http://project:9090/mcp?toolset=agent" || m.EndpointFrom != project {
		t.Fatalf("the project url must win: %+v", m)
	}
}

// TestMergeCodexMCPScopesHeaderFragmentContributes is the reviewer's
// round-6 red proof for the fragment case: a global
// [mcp_servers.punk.http_headers] fragment with NO
// [mcp_servers.punk] table of its own is not an installed entry, but
// it still merges into the entry the project layer defines - its
// Authorization header makes the merged connection authenticate from a
// credential verify cannot compare, and its namespace pin is inherited
// when the project entry defines none. Header values are never stored
// or rendered.
func TestMergeCodexMCPScopesHeaderFragmentContributes(t *testing.T) {
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n")
	global := writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk.http_headers]\nAuthorization = 'Bearer synthetic-fragment-secret'\nx-punk-namespace = 'agent-fragment'\n")
	scopes := EnumerateCodexMCPScopes(project, global)
	if scopes[1].Installed {
		t.Fatal("a header fragment without a [mcp_servers.punk] table is not an installed entry")
	}
	m := MergeCodexMCPScopes(scopes)
	if m.Unknown != "" {
		t.Fatalf("the fragment merges into the project-defined entry: %s", m.Unknown)
	}
	if !m.Defined {
		t.Fatalf("the project layer defines the table: %+v", m)
	}
	if !m.HeaderAuth || m.HeaderAuthFrom != global {
		t.Fatalf("the fragment's Authorization must mark the merged entry and name its layer: %+v", m)
	}
	if m.Pin != "agent-fragment" {
		t.Fatalf("the fragment's namespace pin is inherited into the merged entry: %+v", m)
	}
	if strings.Contains(fmt.Sprintf("%+v", m), "synthetic-fragment-secret") ||
		strings.Contains(fmt.Sprintf("%+v", scopes[1]), "synthetic-fragment-secret") {
		t.Fatal("a header value must never be stored in the scope or the merged entry")
	}
}

// TestMergeCodexMCPScopesFragmentOnlyIsUnknown is the honest half of
// the fragment case: when NO config layer defines
// [mcp_servers.punk] itself, the fragment fields merge into an entry
// that does not exist - whether Codex opens a punk connection cannot
// be established and must be reported, never silently treated as
// absent punk config.
func TestMergeCodexMCPScopesFragmentOnlyIsUnknown(t *testing.T) {
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk.http_headers]\nAuthorization = 'Bearer synthetic-fragment-secret'\n")
	global := writeMergeScope(t, dir, "global.toml", "# no punk config here\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Defined {
		t.Fatal("no layer defines the punk table")
	}
	if m.Unknown == "" || !strings.Contains(m.Unknown, "[mcp_servers.punk]") || !strings.Contains(m.Unknown, project) {
		t.Fatalf("a fragment without any table must be reported honestly, naming the fragment: %+v", m)
	}
	if strings.Contains(m.Unknown, "synthetic-fragment-secret") {
		t.Fatalf("the unknown reason must stay secret-safe: %s", m.Unknown)
	}
}

// TestMergeCodexMCPScopesNoPunkAnywhereIsUnknown: no punk table and no
// punk fragment in any layer is the honest "nothing found" unknown,
// naming every inspected file.
func TestMergeCodexMCPScopesNoPunkAnywhereIsUnknown(t *testing.T) {
	dir := t.TempDir()
	a := writeMergeScope(t, dir, "a.toml", "[mcp_servers.other]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n")
	b := filepath.Join(dir, "missing.toml")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(a, b))
	if m.Defined || len(m.Contributing) != 0 {
		t.Fatalf("foreign tables and missing files contribute nothing: %+v", m)
	}
	if !strings.Contains(m.Unknown, "no [mcp_servers.punk] entry found") ||
		!strings.Contains(m.Unknown, a) || !strings.Contains(m.Unknown, b) {
		t.Fatalf("missing punk config must report the honest unknown naming every file: %+v", m)
	}
}

// TestMergeCodexMCPScopesCrossLayerCaseConflictIsUnknown: the merged
// header table can hold spellings whose names differ only by case from
// DIFFERENT layers - valid TOML per layer, indeterminate once merged,
// because Codex inserts headers from an unordered map. A disagreeing
// pair leaves the effective pin unestablishable: unknown, never a
// guessed one.
func TestMergeCodexMCPScopesCrossLayerCaseConflictIsUnknown(t *testing.T) {
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-a\" }\n")
	global := writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers = { \"x-punk-namespace\" = \"agent-b\" }\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown == "" {
		t.Fatalf("a cross-layer case-variant pin conflict must be unknown: %+v", m)
	}

	// The same spelling in both layers is a clean override, not a
	// conflict: the higher layer's value wins.
	global = writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-b\" }\n")
	m = MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown != "" || m.Pin != "agent-a" {
		t.Fatalf("the same spelling merges deterministically, higher layer wins: %+v (unknown %s)", m, m.Unknown)
	}
}

// TestMergeCodexMCPScopesBlankEnvOverlayKeepsInheritedStaticPin: an
// inherited global static pin stays effective when the project layer's
// env_http_headers spelling resolves blank - Codex SKIPS an
// env-resolved header whose variable is empty or whitespace-only
// (upstream codex-rs rmcp-client/src/utils.rs), so the merged entry
// sends the inherited static pin and the reported pin must be the one
// Codex actually sends.
func TestMergeCodexMCPScopesBlankEnvOverlayKeepsInheritedStaticPin(t *testing.T) {
	t.Setenv("PUNK_TEST_C05_BLANK_PIN", "  ")
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenv_http_headers = { \"X-Punk-Namespace\" = \"PUNK_TEST_C05_BLANK_PIN\" }\n")
	global := writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-global\" }\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown != "" || m.Pin != "agent-global" {
		t.Fatalf("a blank env overlay must leave the inherited static pin effective: %+v (unknown %s)", m, m.Unknown)
	}
}

// TestMergeCodexMCPScopesOverriddenEnvSpellingIsNotInherited: the
// project layer redefining the SAME env_http_headers spelling replaces
// the global variable name field-wise - when the project's variable
// resolves blank, Codex skips the header entirely and the merged entry
// is UNPINNED. Falling back to the global layer's resolved pin here
// would report a pin Codex never sends: false evidence of alignment.
func TestMergeCodexMCPScopesOverriddenEnvSpellingIsNotInherited(t *testing.T) {
	t.Setenv("PUNK_TEST_C05_GLOBAL_PIN", "agent-global")
	t.Setenv("PUNK_TEST_C05_BLANK_PIN", "")
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenv_http_headers = { \"X-Punk-Namespace\" = \"PUNK_TEST_C05_BLANK_PIN\" }\n")
	global := writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenv_http_headers = { \"X-Punk-Namespace\" = \"PUNK_TEST_C05_GLOBAL_PIN\" }\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown != "" {
		t.Fatalf("the merged entry must be inspectable: %s", m.Unknown)
	}
	if m.Pin != "" {
		t.Fatalf("the overridden blank env spelling must leave the merged entry unpinned, not inherit the global pin: %+v", m)
	}
}

// TestMergeCodexMCPScopesEnvAuthBlankIsNotHeaderAuth: an inherited
// env-resolved Authorization whose variable is missing or blank is
// skipped by Codex, so the merged entry does not authenticate through
// a header - and a set one does, naming the layer that contributes it.
func TestMergeCodexMCPScopesEnvAuthBlankIsNotHeaderAuth(t *testing.T) {
	t.Setenv("PUNK_TEST_C05_AUTH_BLANK", "")
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n")
	global := writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk]\nenv_http_headers = { \"Authorization\" = \"PUNK_TEST_C05_AUTH_BLANK\" }\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown != "" || m.HeaderAuth {
		t.Fatalf("a blank env-resolved Authorization is skipped by Codex: %+v (unknown %s)", m, m.Unknown)
	}

	t.Setenv("PUNK_TEST_C05_AUTH_BLANK", "Bearer synthetic-env-secret")
	m = MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if !m.HeaderAuth || m.HeaderAuthFrom != global {
		t.Fatalf("a set env-resolved Authorization marks the merged entry: %+v", m)
	}
	if strings.Contains(fmt.Sprintf("%+v", m), "synthetic-env-secret") {
		t.Fatalf("the env var's value must never be stored in the merged entry: %+v", m)
	}
}

// TestMergeCodexMCPScopesUninspectableScopeIsUnknown: one layer this
// inspector cannot parse faithfully poisons the whole merged entry -
// the fields it would contribute are unknown, so the effective
// connection is unknown, never the other layers' view alone.
func TestMergeCodexMCPScopesUninspectableScopeIsUnknown(t *testing.T) {
	dir := t.TempDir()
	project := writeMergeScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n")
	global := writeMergeScope(t, dir, "global.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenabled = \"false\"\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown == "" || !strings.Contains(m.Unknown, "could not inspect") || !strings.Contains(m.Unknown, global) {
		t.Fatalf("an uninspectable lower layer must make the merged entry unknown, naming the file: %+v", m)
	}
}
