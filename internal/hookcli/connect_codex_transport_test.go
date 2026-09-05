package hookcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file pin the round-7 transport contract for the
// Codex MCP inspector and the cross-layer merge. Upstream Codex
// recursively merges MCP table FIELDS across config layers and then
// validates the merged entry's transport shape
// (config/src/mcp_types.rs RawMcpServerConfig::try_from): a stdio
// entry (command) rejects every HTTP field (url, bearer_token_env_var,
// bearer_token, http_headers_helper, http_headers, env_http_headers,
// oauth, oauth_resource, auth), a streamable-HTTP entry (url) rejects
// every stdio field (args, env, env_vars, cwd) and bearer_token, and
// an entry Codex refuses to load is never a connection verify can
// prove. Because the merge is field-wise, the invalid mix arises
// precisely when NO single layer looks wrong - a global command
// inherited next to a project url - so the transport shape is
// validated on the merged effective entry, never per file. The second
// half pins the dotted-key audit as a class: punk identity fields
// written through dotted-key paths (parent-table
// [mcp_servers] punk.X = ..., root-level mcp_servers.punk.X = ...) or
// inline tables (mcp_servers = { punk = {...} }) are representations
// this inspector does not parse, and it must answer unknown rather
// than treat what it skipped as absent.

// writeTransportScope writes one config.toml scope fixture and returns
// its path.
func writeTransportScope(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMergeCodexMCPScopesMixedTransportIsUnknown is the reviewer's
// round-7 red proof at the merge layer: a clean-looking project HTTP
// entry (url plus a namespace pin) inherits command from the global
// layer, and the merged entry - both command and url - is one upstream
// Codex REJECTS as a mixed transport. A per-file view called this
// aligned; the merge must call the effective entry unknown, naming the
// conflicting fields and the layers they came from, and must never
// render the command's value (a field value is not diagnostic
// material).
func TestMergeCodexMCPScopesMixedTransportIsUnknown(t *testing.T) {
	dir := t.TempDir()
	project := writeTransportScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-proj\" }\n")
	global := writeTransportScope(t, dir, "global.toml", "[mcp_servers.punk]\ncommand = 'reviewer-synthetic-program'\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown == "" {
		t.Fatalf("a merged entry carrying both command and url is rejected by upstream Codex and must be unknown: %+v", m)
	}
	for _, want := range []string{"url", "command", project, global} {
		if !strings.Contains(m.Unknown, want) {
			t.Fatalf("the mixed-transport unknown must name %q: %s", want, m.Unknown)
		}
	}
	if strings.Contains(m.Unknown, "reviewer-synthetic-program") {
		t.Fatalf("the unknown must name fields and files, never values: %s", m.Unknown)
	}
}

// TestMergeCodexMCPScopesTransportConflicts covers the rest of the
// upstream rejection matrix (config/src/mcp_types.rs
// RawMcpServerConfig::try_from) as inherited field combinations: with
// a command present, every HTTP field is rejected; with a url present,
// every stdio field and bearer_token are rejected. Each row puts the
// poison in the GLOBAL layer only, the way cross-layer inheritance
// actually produces the mix.
func TestMergeCodexMCPScopesTransportConflicts(t *testing.T) {
	cases := map[string]struct {
		project, global, field string
	}{
		"command with inherited bearer_token_env_var": {"[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n", "[mcp_servers.punk]\ncommand = 'prog'\n", ""},
		"command with inherited http_headers":         {"[mcp_servers.punk]\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-a\" }\n", "[mcp_servers.punk]\ncommand = 'prog'\n", "http_headers"},
		"command with inherited http_headers_helper":  {"[mcp_servers.punk]\nhttp_headers_helper = 'false'\n", "[mcp_servers.punk]\ncommand = 'prog'\n", "http_headers_helper"},
		"url with inherited args":                     {"[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n", "[mcp_servers.punk]\nargs = ['--flag']\n", "args"},
		"url with inherited env inline":               {"[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n", "[mcp_servers.punk]\nenv = { FOO = 'bar' }\n", "env"},
		"url with inherited env subtable":             {"[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n", "[mcp_servers.punk.env]\nFOO = 'bar'\n", "env"},
		"url with inherited env_vars":                 {"[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n", "[mcp_servers.punk]\nenv_vars = ['FOO']\n", "env_vars"},
		"url with inherited cwd":                      {"[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n", "[mcp_servers.punk]\ncwd = '/tmp'\n", "cwd"},
		"url with inherited bearer_token":             {"[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\n", "[mcp_servers.punk]\nbearer_token = 'reviewer-synthetic-token'\n", "bearer_token"},
		"single-layer url with env":                   {"[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nenv = { FOO = 'bar' }\n", "", "env"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			project := writeTransportScope(t, dir, "project.toml", tc.project)
			global := filepath.Join(dir, "global.toml")
			if tc.global != "" {
				global = writeTransportScope(t, dir, "global.toml", tc.global)
			}
			m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
			if m.Unknown == "" {
				t.Fatalf("the merged entry mixes transports upstream Codex rejects and must be unknown: %+v", m)
			}
			if tc.field != "" && !strings.Contains(m.Unknown, tc.field) {
				t.Fatalf("the unknown must name the rejected field %s: %s", tc.field, m.Unknown)
			}
			if strings.Contains(m.Unknown, "reviewer-synthetic-token") {
				t.Fatalf("a bearer_token value must never appear in diagnostics: %s", m.Unknown)
			}
		})
	}
}

// TestMergeCodexMCPScopesValidTransportsStayKnown: the control rows.
// A command-only stdio entry is a VALID transport (upstream rejects
// nothing), so the merge does not call it unknown - verify's
// HTTP-only probe reports the missing url downstream. A url-only
// entry and a full HTTP entry stay known too.
func TestMergeCodexMCPScopesValidTransportsStayKnown(t *testing.T) {
	dir := t.TempDir()
	stdio := writeTransportScope(t, dir, "stdio.toml", "[mcp_servers.punk]\ncommand = 'prog'\nargs = ['--flag']\nenv = { FOO = 'bar' }\ncwd = '/tmp'\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(stdio))
	if m.Unknown != "" {
		t.Fatalf("a stdio-only entry is a valid transport: %s", m.Unknown)
	}
	if m.Endpoint != "" {
		t.Fatalf("a stdio entry has no url for verify to probe: %+v", m)
	}

	http := writeTransportScope(t, dir, "http.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nbearer_token_env_var = 'PUNK_API_KEY'\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-a\" }\n")
	m = MergeCodexMCPScopes(EnumerateCodexMCPScopes(http))
	if m.Unknown != "" {
		t.Fatalf("a full HTTP entry is a valid transport: %s", m.Unknown)
	}
}

// TestInspectCodexMCPScopeDottedPunkRepresentationsAreUnknown is the
// round-7 audit AS A CLASS at the inspector layer: punk identity
// fields written through dotted-key paths - parent-table
// ([mcp_servers] punk.X = ...), root-level (mcp_servers.punk.X = ...),
// quoted segments, or the punk entry nested in an inline
// mcp_servers table - are representations this inspector does not
// parse, and every one must fail the scope as unknown. Treating a
// skipped representation as absent was the false-alignment defect.
// Errors name the file, the line and the representation only; the
// marker values must never be echoed.
func TestInspectCodexMCPScopeDottedPunkRepresentationsAreUnknown(t *testing.T) {
	cases := map[string]string{
		"parent-table dotted bearer":   "[mcp_servers]\npunk.bearer_token_env_var = 'REVIEWER_MARKER_VALUE'\n",
		"parent-table dotted url":      "[mcp_servers]\npunk.url = 'http://reviewer-marker-value'\n",
		"parent-table quoted segment":  "[mcp_servers]\n\"punk\".url = 'http://reviewer-marker-value'\n",
		"parent-table inline entry":    "[mcp_servers]\npunk = { url = 'http://reviewer-marker-value' }\n",
		"root dotted bearer":           "mcp_servers.punk.bearer_token_env_var = 'REVIEWER_MARKER_VALUE'\n",
		"root dotted header":           "mcp_servers.punk.http_headers.X-Punk-Namespace = 'agent-reviewer-marker'\n",
		"root inline punk":             "mcp_servers = { punk = { url = 'http://reviewer-marker-value' } }\n",
		"root multiline inline punk":   "mcp_servers = {\n other = { url = 'http://localhost:1' },\n punk = { url = 'http://reviewer-marker-value' }\n}\n",
		"punk-table dotted stdio":      "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nenv.FOO = 'REVIEWER_MARKER_VALUE'\n",
		"header subtable dotted key":   "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.http_headers]\nX-Punk-Namespace.sub = 'agent-reviewer-marker'\n",
		"env subtable dotted key":      "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\n[mcp_servers.punk.env]\nFOO.sub = 'REVIEWER_MARKER_VALUE'\n",
		"punk-table dotted enabled":    "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nenabled.value = false\n",
		"punk-table dotted bearer tok": "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nbearer_token.x = 'REVIEWER_MARKER_VALUE'\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeTransportScope(t, t.TempDir(), "config.toml", doc)
			sc := EnumerateCodexMCPScopes(p)[0]
			if sc.Err == nil {
				t.Fatalf("a punk-identity dotted/inline representation must be unknown, not silently skipped: %+v", sc)
			}
			if strings.Contains(sc.Err.Error(), "REVIEWER_MARKER_VALUE") || strings.Contains(sc.Err.Error(), "reviewer-marker-value") || strings.Contains(sc.Err.Error(), "agent-reviewer-marker") {
				t.Fatalf("diagnostics must never echo config values: %v", sc.Err)
			}
			if !strings.Contains(sc.Err.Error(), p) {
				t.Fatalf("diagnostics must name the file: %v", sc.Err)
			}
		})
	}
}

// TestInspectCodexMCPScopeForeignDottedKeysStayIgnored: the audit's
// limits. Dotted keys that cannot reach the punk entry - another
// server's fields under [mcp_servers], a root-level path into another
// server, or an inline mcp_servers table with no punk key - are not
// punk identity and must not poison an otherwise inspectable punk
// table.
func TestInspectCodexMCPScopeForeignDottedKeysStayIgnored(t *testing.T) {
	cases := map[string]string{
		"parent-table other server":  "[mcp_servers]\nother.url = 'http://localhost:1'\n\n[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nhttp_headers = { 'X-Punk-Namespace' = 'agent-real' }\n",
		"root dotted other server":   "mcp_servers.other.url = 'http://localhost:1'\n\n[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nhttp_headers = { 'X-Punk-Namespace' = 'agent-real' }\n",
		"root inline without punk":   "mcp_servers = { other = { url = 'http://localhost:1' } }\n\n[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nhttp_headers = { 'X-Punk-Namespace' = 'agent-real' }\n",
		"foreign table dotted":       "[tui]\ntheme.dark = 'yes'\n\n[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nhttp_headers = { 'X-Punk-Namespace' = 'agent-real' }\n",
		"untracked punk dotted tool": "[mcp_servers.punk]\nurl = 'http://localhost:9090/mcp?toolset=agent'\nhttp_headers = { 'X-Punk-Namespace' = 'agent-real' }\ntools.inspect.output_token_limit = 100\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeTransportScope(t, t.TempDir(), "config.toml", doc)
			sc := EnumerateCodexMCPScopes(p)[0]
			if sc.Err != nil || !sc.Installed || sc.Pin != "agent-real" {
				t.Fatalf("foreign dotted keys are not punk identity and must stay ignored: %+v (err %v)", sc, sc.Err)
			}
		})
	}
}

// TestMergeCodexMCPScopesDottedLayerPoisonsMerge is the reviewer's
// round-7 red proof for the parent-table dotted case at the merge
// layer: the global layer's [mcp_servers] punk.bearer_token_env_var is
// valid TOML Codex would merge into the entry, the inspector cannot
// parse it faithfully, and one uninspectable layer poisons the whole
// merged entry - unknown, naming the file, never a false clean bill
// from the project layer alone.
func TestMergeCodexMCPScopesDottedLayerPoisonsMerge(t *testing.T) {
	dir := t.TempDir()
	project := writeTransportScope(t, dir, "project.toml", "[mcp_servers.punk]\nurl = \"http://localhost:9090/mcp?toolset=agent\"\nhttp_headers = { \"X-Punk-Namespace\" = \"agent-proj\" }\n")
	global := writeTransportScope(t, dir, "global.toml", "[mcp_servers]\npunk.bearer_token_env_var = 'REVIEWER_MARKER_ABSENT'\n")
	m := MergeCodexMCPScopes(EnumerateCodexMCPScopes(project, global))
	if m.Unknown == "" || !strings.Contains(m.Unknown, "could not inspect") || !strings.Contains(m.Unknown, global) {
		t.Fatalf("an uninspectable dotted lower layer must poison the merged entry, naming the file: %+v", m)
	}
	if strings.Contains(fmt.Sprintf("%+v", m), "REVIEWER_MARKER_ABSENT") {
		t.Fatalf("the merged entry must never carry config values: %+v", m)
	}
}

// TestInspectCodexMCPScopeTransportFieldsArePresenceOnly: the
// transport-shaped fields are validated for well-formedness and
// recorded as presence. A non-string command, a non-string args
// element or an env table with a non-string value are configs Codex
// cannot decode at all - conservatively unknown. A multi-line args
// array with a comment is legal TOML and must parse.
func TestInspectCodexMCPScopeTransportFieldsArePresenceOnly(t *testing.T) {
	dir := t.TempDir()
	bad := map[string]string{
		"non-string command":      "[mcp_servers.punk]\ncommand = 5\n",
		"non-string args element": "[mcp_servers.punk]\nargs = ['--flag', 5]\n",
		"non-array args":          "[mcp_servers.punk]\nargs = '--flag'\n",
		"non-string env value":    "[mcp_servers.punk]\nenv = { FOO = 5 }\n",
		"non-string env subtable": "[mcp_servers.punk.env]\nFOO = 5\n",
		"unterminated args":       "[mcp_servers.punk]\nargs = ['--flag'\n",
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			p := writeTransportScope(t, t.TempDir(), "config.toml", doc)
			if sc := EnumerateCodexMCPScopes(p)[0]; sc.Err == nil {
				t.Fatalf("a transport field Codex cannot decode must be unknown: %+v", sc)
			}
		})
	}

	p := writeTransportScope(t, dir, "config.toml", "[mcp_servers.punk]\ncommand = 'prog'\nargs = [\n '--flag', # a note\n '--other',\n]\nenv_vars = [ 'FOO', { name = 'BAR' } ]\n")
	sc := EnumerateCodexMCPScopes(p)[0]
	if sc.Err != nil || !sc.Installed {
		t.Fatalf("a well-formed multi-line args array must parse: %+v (err %v)", sc, sc.Err)
	}
	if strings.Contains(fmt.Sprintf("%+v", sc), "--flag") || strings.Contains(fmt.Sprintf("%+v", sc), "prog") {
		t.Fatalf("transport field values are presence-only and must never be stored: %+v", sc)
	}
}
