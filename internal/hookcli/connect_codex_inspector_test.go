package hookcli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file pin the round-4 inspector contract for
// Codex config.toml scopes: diagnostics never echo config VALUES (a
// header value can carry an Authorization secret), env_http_headers
// overlay static http_headers the way Codex's rmcp client applies
// them (env wins, header names case-insensitive) so the reported pin
// is the one Codex actually sends, and a static or env-resolved
// Authorization header is surfaced as credential provenance verify
// cannot compare. Round 5 adds: a blank env-resolved header value is
// SKIPPED by Codex and must never overwrite the static pin, string
// content (a [mcp_servers.punk] example inside a multi-line
// developer_instructions string) is never document structure, an
// enabled = false entry is installed but not a working connection,
// and http_headers_helper marks the entry's headers unverified without
// the helper ever being executed.

// TestInspectCodexMCPScopeDiagnosticsNeverEchoHeaderValues: a punk
// entry whose Authorization header uses a TOML representation this
// inspector does not support (a multi-line string) fails as unknown,
// and the error carries the file, line and field identity only - the
// secret value must never appear in it.
func TestInspectCodexMCPScopeDiagnosticsNeverEchoHeaderValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	content := `[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
[mcp_servers.punk.http_headers]
Authorization = """Bearer reviewer-synthetic-marker"""
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err == nil {
		t.Fatal("a multi-line header value must be reported as unsupported, not inspected silently")
	}
	if strings.Contains(sc.Err.Error(), "reviewer-synthetic-marker") {
		t.Fatalf("diagnostic exposes synthetic header secret: %v", sc.Err)
	}
}

// TestInspectCodexMCPScopeEnvHTTPHeadersOverrideStatic: Codex 0.153.4
// inserts env_http_headers AFTER static http_headers into a
// case-insensitive HeaderMap (upstream codex-rs
// rmcp-client/src/utils.rs), so an env-resolved X-Punk-Namespace is
// the pin Codex actually sends. Reporting the static value here would
// be false evidence of alignment.
func TestInspectCodexMCPScopeEnvHTTPHeadersOverrideStatic(t *testing.T) {
	t.Setenv("REVIEWER_C05_NAMESPACE", "agent-effective")
	p := filepath.Join(t.TempDir(), "config.toml")
	content := `[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
http_headers = { "X-Punk-Namespace" = "agent-static" }
env_http_headers = { "X-Punk-Namespace" = "REVIEWER_C05_NAMESPACE" }
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil {
		t.Fatalf("env_http_headers must be inspected faithfully: %v", sc.Err)
	}
	if !sc.Installed || sc.Pin != "agent-effective" {
		t.Fatalf("effective pin must come from the env header: installed=%v pin=%q", sc.Installed, sc.Pin)
	}
}

// TestInspectCodexMCPScopeEnvHeaderUnsetKeepsStatic: an env header
// whose variable is not set is skipped by Codex (env::var errs), so
// the static header stays effective.
func TestInspectCodexMCPScopeEnvHeaderUnsetKeepsStatic(t *testing.T) {
	const name = "REVIEWER_C05_UNSET_NAMESPACE"
	prev, ok := os.LookupEnv(name)
	os.Unsetenv(name)
	t.Cleanup(func() {
		if ok {
			os.Setenv(name, prev)
		} else {
			os.Unsetenv(name)
		}
	})
	p := filepath.Join(t.TempDir(), "config.toml")
	content := `[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
http_headers = { "X-Punk-Namespace" = "agent-static" }
env_http_headers = { "X-Punk-Namespace" = "` + name + `" }
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Pin != "agent-static" {
		t.Fatalf("unset env header must leave the static pin effective: %+v (err %v)", sc, sc.Err)
	}
}

// TestInspectCodexMCPScopeHeaderNamesAreCaseInsensitive: Codex matches
// header names case-insensitively, so a lowercase x-punk-namespace
// static header is the pin just the same - and an env header written
// in a different case still overrides it.
func TestInspectCodexMCPScopeHeaderNamesAreCaseInsensitive(t *testing.T) {
	t.Setenv("REVIEWER_C05_NAMESPACE", "agent-env")
	p := filepath.Join(t.TempDir(), "config.toml")
	content := `[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
[mcp_servers.punk.http_headers]
x-punk-namespace = "agent-lower"
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Pin != "agent-lower" {
		t.Fatalf("lowercase namespace header must be the pin: %+v (err %v)", sc, sc.Err)
	}

	content = `[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
http_headers = { "x-punk-namespace" = "agent-lower" }
env_http_headers = { "X-PUNK-NAMESPACE" = "REVIEWER_C05_NAMESPACE" }
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sc = EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Pin != "agent-env" {
		t.Fatalf("env header must win across case: %+v (err %v)", sc, sc.Err)
	}
}

// TestInspectCodexMCPScopeCaseVariantConflictIsUnknown: two static
// headers whose names differ only by case carry different values;
// Codex inserts headers from an unordered map, so which one wins
// cannot be established faithfully and the entry must be unknown,
// never a guessed pin.
func TestInspectCodexMCPScopeCaseVariantConflictIsUnknown(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	content := `[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
http_headers = { "X-Punk-Namespace" = "agent-a" }
[mcp_servers.punk.http_headers]
"x-punk-namespace" = "agent-b"
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err == nil {
		t.Fatalf("case-variant headers with disagreeing values must be unknown, got pin %q", sc.Pin)
	}
}

// TestInspectCodexMCPScopeAuthorizationHeaderProvenance: a static or
// env-resolved Authorization header is a credential source verify
// cannot compare safely; the inspector surfaces its PRESENCE
// (HeaderAuth) and never its value.
func TestInspectCodexMCPScopeAuthorizationHeaderProvenance(t *testing.T) {
	t.Setenv("REVIEWER_C05_AUTH", "Bearer reviewer-synthetic-env-marker")
	p := filepath.Join(t.TempDir(), "config.toml")
	content := `[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
http_headers = { "Authorization" = "Bearer reviewer-synthetic-static-marker", "X-Punk-Namespace" = "agent-static" }
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || !sc.HeaderAuth || sc.Pin != "agent-static" {
		t.Fatalf("static Authorization must mark HeaderAuth: %+v (err %v)", sc, sc.Err)
	}

	content = `[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
env_http_headers = { "authorization" = "REVIEWER_C05_AUTH" }
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sc = EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || !sc.HeaderAuth {
		t.Fatalf("env-resolved Authorization must mark HeaderAuth: %+v (err %v)", sc, sc.Err)
	}
}

// TestEffectiveCodexHeadersBlankEnvKeepsStaticPin is the reviewer's
// round-5 red proof: upstream Codex SKIPS an env_http_headers entry
// whose variable resolves to an empty or whitespace-only value
// (rmcp-client/src/utils.rs), so a blank variable must leave the
// static pin effective - overwriting it would assert a blank pin Codex
// never sends, and with no static header at all the entry is simply
// unpinned, never blank-pinned.
func TestEffectiveCodexHeadersBlankEnvKeepsStaticPin(t *testing.T) {
	for _, v := range []string{"", "   "} {
		t.Setenv("REVIEW_C05_BLANK", v)
		eff, ok := effectiveCodexHeaders(
			map[string]string{"X-Punk-Namespace": "agent-static"},
			map[string]string{"X-Punk-Namespace": "REVIEW_C05_BLANK"})
		if !ok {
			t.Fatalf("a blank env overlay (%q) must not invalidate the static headers", v)
		}
		if eff["x-punk-namespace"] != "agent-static" {
			t.Fatalf("blank env value %q must not replace the static pin: %v", v, eff)
		}
		eff, ok = effectiveCodexHeaders(nil, map[string]string{"X-Punk-Namespace": "REVIEW_C05_BLANK"})
		if !ok {
			t.Fatalf("a blank env overlay (%q) alone must stay inspectable", v)
		}
		if _, present := eff["x-punk-namespace"]; present {
			t.Fatalf("a blank env value %q must resolve to NO header, not a blank one: %v", v, eff)
		}
	}
}

// TestInspectCodexMCPScopeMultilineStringIsNotStructure is the
// reviewer's round-5 red proof: a [mcp_servers.punk] example inside a
// multi-line developer_instructions string is documentation, not
// config. The inspector tracks basic AND literal multi-line strings
// across the whole document, so string content never counts as a table
// header - while a real punk table AFTER the string closes still
// parses, and an unterminated multi-line string is conservatively
// unknown.
func TestInspectCodexMCPScopeMultilineStringIsNotStructure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	doc := `developer_instructions = """
Documentation example:
[mcp_servers.punk]
url = 'http://localhost:9090/mcp?toolset=agent'
http_headers = { 'X-Punk-Namespace' = 'agent-example' }
[example]
"""
`
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err == nil && sc.Installed {
		t.Fatal("an MCP example inside a multi-line string is string content, not installed config")
	}

	// The literal (''') form, plus a REAL punk table after the string
	// closes: structure tracking must resume at the closing delimiter.
	doc = `notes = '''
[mcp_servers.punk]
'''
[mcp_servers.punk]
url = "http://localhost:9090/mcp?toolset=agent"
http_headers = { "X-Punk-Namespace" = "agent-real" }
`
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc = EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Pin != "agent-real" {
		t.Fatalf("a real punk table after a multi-line literal string must parse: %+v (err %v)", sc, sc.Err)
	}

	// An unterminated multi-line string swallows the rest of the
	// document in real TOML too; what follows can never be established
	// as structure, so the entry is conservatively unknown.
	doc = "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n[other]\ndoc = \"\"\"\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc = EnumerateCodexMCPScopes(p)[0]
	if sc.Err == nil {
		t.Fatal("an unterminated multi-line string must be unknown, not silently inspected")
	}
}

// TestInspectCodexMCPScopeEnabledFalseMarksDisabled: Codex filters
// enabled = false MCP entries (upstream codex-rs
// connection_manager.rs), so the inspector surfaces the flag - the
// entry is installed but not a working connection. A non-boolean
// enabled value cannot be read faithfully and is conservatively
// unknown.
func TestInspectCodexMCPScopeEnabledFalseMarksDisabled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	doc := "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenabled = false\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || !sc.Disabled {
		t.Fatalf("enabled = false must mark the entry disabled: %+v (err %v)", sc, sc.Err)
	}

	doc = "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenabled = true # still on\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc = EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Disabled {
		t.Fatalf("enabled = true must stay a working entry: %+v (err %v)", sc, sc.Err)
	}

	doc = "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenabled = \"false\"\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc = EnumerateCodexMCPScopes(p)[0]
	if sc.Err == nil {
		t.Fatal("a string enabled value is an activation state this inspector cannot establish")
	}
}

// TestInspectCodexMCPScopeUnrecognizedKeyMultilineArrayStaysInspectable
// is the 3d(i) red proof: an unrecognized [mcp_servers.punk] key (tool
// filters and similar future fields) with a multi-line array value used
// to fail the whole scope as unknown, because only recognized keys
// folded multi-line values - the continuation lines were then read as
// fresh (unparseable) lines inside the punk table. The value carries no
// punk identity, so it must be folded and ignored, leaving the rest of
// the entry inspectable.
func TestInspectCodexMCPScopeUnrecognizedKeyMultilineArrayStaysInspectable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	doc := "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenabled_tools = [\n  \"recall\",\n  \"search\",\n]\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Endpoint != "http://localhost:9090/mcp?toolset=agent" {
		t.Fatalf("an unrecognized key's multi-line array value must be folded and ignored: %+v (err %v)", sc, sc.Err)
	}
}

// TestInspectCodexMCPScopeForeignMultilineNestedArrayStaysInspectable is
// the 3d(ii) red proof: a root-level or other-table multi-line array
// whose continuation line begins with '[' (a nested array element) used
// to be misread by the strings.HasPrefix(trim, "[") table-header check,
// failing the whole document as unparseable. auditCodexForeignLine must
// fold such an array so its continuation lines are consumed and never
// examined as headers.
func TestInspectCodexMCPScopeForeignMultilineNestedArrayStaysInspectable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	doc := "[other]\nmatrix = [\n  [\"a\", \"b\"],\n  [\"c\", \"d\"],\n]\n\n[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || sc.Endpoint != "http://localhost:9090/mcp?toolset=agent" {
		t.Fatalf("a foreign table's multi-line array with a bracket-leading continuation line must not be misread as a table header: %+v (err %v)", sc, sc.Err)
	}
}

// TestInspectCodexMCPScopeHeaderHelperMarksUnverified: an entry
// carrying http_headers_helper resolves part of its headers by running
// an external command at connect time (upstream codex-rs
// config/src/mcp_types.rs). The inspector records the helper's
// PRESENCE (HeadersHelper) so verify reports the headers unverified -
// and NEVER executes the helper, proven here by a helper command that
// would leave a marker file behind.
func TestInspectCodexMCPScopeHeaderHelperMarksUnverified(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "helper-executed")
	p := filepath.Join(t.TempDir(), "config.toml")
	doc := "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers_helper = 'touch " + marker + "'\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-static\" }\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed || !sc.HeadersHelper {
		t.Fatalf("http_headers_helper must mark the entry's headers unverified: %+v (err %v)", sc, sc.Err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the inspector must NEVER execute http_headers_helper")
	}
}
