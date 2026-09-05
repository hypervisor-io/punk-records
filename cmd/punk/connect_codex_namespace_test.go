package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tests in this file pin the connect half of the C05 namespace
// contract: a project connection must bake the repository's
// remote-derived namespace into BOTH the hook command and the MCP
// entry's X-Punk-Namespace header (so hooks and whoami agree even when
// the MCP client cannot advertise workspace roots), and a global
// connection must pin nothing (it serves many repositories, so pinning
// would force every repo into the connector's cwd namespace). Fixtures
// use a custom CODEX_HOME and sanitized acme remotes; nothing touches
// the operator's real Codex configuration.

// codexRepoFixture is a temporary git checkout with origin set, plus a
// custom CODEX_HOME and an isolated credentials path, chdir'd into.
func codexRepoFixture(t *testing.T, remote string) (repo, codexHome string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", remote)
	codexHome = filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv("PUNK_API_KEY", "")
	t.Chdir(repo)
	return repo, codexHome
}

func TestConnectCodexProjectPinsRemoteNamespace(t *testing.T) {
	repo, codexHome := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	wantNS, src := hookcli.ProjectNamespace(repo)
	if src != "remote" {
		t.Fatalf("fixture must derive from the remote: %s (%s)", wantNS, src)
	}
	if err := cmdConnectCodex([]string{"--project", "--no-skill", "--url", "http://127.0.0.1:19393"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(repo, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `"X-Punk-Namespace" = "`+wantNS+`"`) {
		t.Fatalf("project MCP entry must pin the remote-derived namespace %s:\n%s", wantNS, cfg)
	}
	hooks, err := os.ReadFile(filepath.Join(repo, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hooks), "--ns "+wantNS) {
		t.Fatalf("project hooks must pin the same namespace %s:\n%s", wantNS, hooks)
	}
	// A project connect must not leak the pin into the global files.
	if raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml")); err == nil && strings.Contains(string(raw), wantNS) {
		t.Fatalf("global config.toml must stay unpinned:\n%s", raw)
	}
}

func TestConnectCodexGlobalPinsNothing(t *testing.T) {
	_, codexHome := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	if err := cmdConnectCodex([]string{"--no-skill", "--url", "http://127.0.0.1:19393"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "X-Punk-Namespace") {
		t.Fatalf("a global connection serves many repos and must not pin one namespace:\n%s", cfg)
	}
	hooks, err := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hooks), "--ns") {
		t.Fatalf("global hooks must not pin the connector's cwd namespace:\n%s", hooks)
	}
}

// verifyWhoamiServer fakes the MCP server's namespace resolution with
// the same precedence internal/mcpserver implements (an X-Punk-Namespace
// header wins, then the first file:// workspace root mapped with the
// hook-side basename rule, then the server default), so the --verify
// tests below run the real client path - roots advertisement and all -
// without a database.
func verifyWhoamiServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "punk-records", Version: "test"}, &mcp.ServerOptions{Instructions: "inst"})
	type out struct {
		Namespace string `json:"namespace"`
		Source    string `json:"source"`
		Root      string `json:"root,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "whoami", Description: "who"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, out, error) {
			if req.Extra != nil && req.Extra.Header != nil {
				if h := strings.TrimSpace(req.Extra.Header.Get("X-Punk-Namespace")); h != "" {
					return nil, out{Namespace: h, Source: "header"}, nil
				}
			}
			if res, err := req.Session.ListRoots(ctx, nil); err == nil {
				for _, r := range res.Roots {
					if u, perr := url.Parse(r.URI); perr == nil && u.Scheme == "file" && u.Path != "" {
						p := filepath.Clean(u.Path)
						return nil, out{Namespace: "agent-" + strings.ToLower(filepath.Base(p)), Source: "roots", Root: p}, nil
					}
				}
			}
			return nil, out{Namespace: "agent-default", Source: "default"}, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts
}

// captureStdout is shared with authz_cli_test.go.

// TestConnectCodexNoHooksVerifyDoesNotInventHookPin is the C05 review
// fix at the CLI boundary: `punk connect codex --project --no-hooks
// --verify` updates only the project MCP entry, so the hooks that
// actually drive capture remain the previously installed GLOBAL
// UNPINNED ones. Verify must compare the pins it inspects from the
// installed files - here unpinned global hooks against the project MCP
// pin, a mismatch it must warn about - and must never print "namespace
// alignment ok" for a hook config this invocation did not write.
func TestConnectCodexNoHooksVerifyDoesNotInventHookPin(t *testing.T) {
	_, codexHome := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	// The installed hooks remain global and unpinned; only the MCP entry
	// is about to receive the project pin.
	if _, err := hookcli.ConnectCodexHooks(filepath.Join(codexHome, "hooks.json"), "/bin/punk", ts.URL, ""); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-hooks", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "namespace alignment ok") {
		t.Fatalf("verify falsely reports aligned hooks despite --no-hooks retaining unpinned hooks:\n%s", out)
	}
	if !strings.Contains(out, "punk: warning") {
		t.Fatalf("the retained unpinned hooks capture path-derived while the MCP entry pins remote-derived - verify must warn:\n%s", out)
	}
}

// TestConnectCodexNoMCPVerifyDoesNotInventMCPPin is the mirror case:
// --no-mcp leaves a previously installed MCP entry untouched, so verify
// must inspect that entry's actual X-Punk-Namespace header instead of
// assuming it carries this invocation's namespace. A stale entry pinning
// something else is a conflicting-pins mismatch, not an aligned
// connection.
func TestConnectCodexNoMCPVerifyDoesNotInventMCPPin(t *testing.T) {
	repo, _ := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	// A stale project MCP entry from an earlier install pins a namespace
	// this invocation is NOT writing.
	if _, err := hookcli.ConnectCodexConfig(filepath.Join(repo, ".codex", "config.toml"),
		hookcli.MCPEntryOpts{ServerURL: ts.URL, Namespace: "agent-stale-000001"}, true, false); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "namespace alignment ok") {
		t.Fatalf("verify must not claim alignment against an unwritten MCP entry:\n%s", out)
	}
	if !strings.Contains(out, "agent-stale-000001") {
		t.Fatalf("verify must name the stale pin it inspected:\n%s", out)
	}
}

// TestConnectCodexVerifyReportsUninspectedSides: with --no-hooks,
// --no-mcp and nothing installed at either scope, verify has no hook pin
// and no MCP pin to compare - it must report the alignment as not
// verified rather than claim ok or invent equal pins.
func TestConnectCodexVerifyReportsUninspectedSides(t *testing.T) {
	codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-hooks", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "namespace alignment ok") {
		t.Fatalf("nothing was installed or inspected; verify must not claim alignment:\n%s", out)
	}
	if !strings.Contains(out, "namespace alignment not verified") ||
		!strings.Contains(out, "hook side is unknown") ||
		!strings.Contains(out, "MCP side is unknown") {
		t.Fatalf("verify must report both sides unverified with their reasons:\n%s", out)
	}
}

// TestConnectCodexProjectVerifyInspectsInstalledPins: a full project
// connect pins the same remote-derived namespace into the hook command
// and the MCP entry, and verify - reading both back from the installed
// files, not from the invocation's flags - reports the alignment it
// actually proved.
func TestConnectCodexProjectVerifyInspectsInstalledPins(t *testing.T) {
	repo, _ := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	wantNS, src := hookcli.ProjectNamespace(repo)
	if src != "remote" {
		t.Fatalf("fixture must derive from the remote: %s (%s)", wantNS, src)
	}
	if !strings.Contains(out, "namespace alignment ok: hooks and MCP sessions both resolve "+wantNS) {
		t.Fatalf("verify must report the inspected pins aligned at %s:\n%s", wantNS, out)
	}
}

// TestConnectCodexMixedScopesVerifyReportsLayeredCaptures covers the
// mixed install: a global unpinned connection followed by a project
// connection leaves punk config in both scopes, and native Codex
// APPENDS hook registrations from every applicable config layer - the
// global unpinned registration keeps firing alongside the pinned
// project one, so captures for this repository land in TWO namespaces
// (the path-derived one and the remote-derived pin). Verify must
// report every capture destination and refuse the alignment claim;
// claiming "aligned" from the project pins alone was the round-3
// review finding (the old test here wrongly pinned the override
// assumption).
func TestConnectCodexMixedScopesVerifyReportsLayeredCaptures(t *testing.T) {
	repo, codexHome := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	if _, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--no-skill", "--url", ts.URL})
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "namespace alignment ok") {
		t.Fatalf("the global unpinned registration still fires alongside the pinned project one, so captures land in two namespaces; verify must not claim alignment:\n%s", out)
	}
	if !strings.Contains(out, "punk: warning") {
		t.Fatalf("verify must warn about the layered capture destinations:\n%s", out)
	}
	wantNS, src := hookcli.ProjectNamespace(repo)
	if src != "remote" {
		t.Fatalf("fixture must derive from the remote: %s (%s)", wantNS, src)
	}
	if !strings.Contains(out, wantNS) {
		t.Fatalf("verify must name the project-scope pin among the capture destinations:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(codexHome, "hooks.json")) {
		t.Fatalf("verify must name the global scope that still captures:\n%s", out)
	}
}

// TestConnectCodexVerifySeesBothHookScopes is the reviewer's round-3
// red proof (TestReviewerC05BothHookScopesRemainEffective): native
// Codex appends hooks from all applicable config layers
// (codex-rs/hooks discovery loops layers_low_to_high with
// append_hook_events), so after a global install a later project
// install leaves the global UNPINNED registrations firing alongside
// the pinned project ones. Verifying only the project-scope pin would
// claim alignment while part of the capture actually lands in the
// global scope's path-derived namespace.
func TestConnectCodexVerifySeesBothHookScopes(t *testing.T) {
	codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	if _, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--no-skill", "--url", ts.URL})
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "namespace alignment ok:") {
		t.Fatalf("global unpinned hooks remain active alongside pinned project hooks, but verify claims all aligned:\n%s", out)
	}
}

// TestConnectCodexVerifyRejectsForeignMCPEndpoint is the reviewer's
// round-3 red proof (TestReviewerC05MustNotVerifyDifferentServer): an
// old project MCP entry pointing at a dead server with the RIGHT
// namespace pin, combined with --project --no-mcp --verify --url <new
// server>, must not yield an alignment claim - the old code probed the
// NEW server using the OLD config's pin and declared alignment while
// real retrieval points at the dead old server. Verify must compare
// the inspected endpoint provenance against the endpoint it actually
// probed and report the alignment unverified on mismatch.
func TestConnectCodexVerifyRejectsForeignMCPEndpoint(t *testing.T) {
	repo, _ := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	ns, _ := hookcli.ProjectNamespace(repo)
	if _, err := hookcli.ConnectCodexConfig(filepath.Join(repo, ".codex", "config.toml"),
		hookcli.MCPEntryOpts{ServerURL: "http://127.0.0.1:1", Namespace: ns}, true, false); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err == nil && strings.Contains(out, "namespace alignment ok:") {
		t.Fatalf("installed MCP points to unreachable old server, but verify probes new hook server and claims alignment:\n%s", out)
	}
	if err == nil && !strings.Contains(out, "namespace alignment not verified") {
		t.Fatalf("verify must report the endpoint provenance mismatch as unverified:\n%s", out)
	}
}

// TestCodexAuthProblemMissingBearerEnvIsUnverified is the reviewer's
// round-4 red proof: a configured bearer_token_env_var that is missing
// or empty is REJECTED by Codex (upstream codex-rs
// codex-mcp/src/rmcp_client.rs resolve_bearer_token), so verify must
// never accept it as "consistent unauthenticated" - the installed
// connection's credential is unestablished regardless of the probe
// key.
func TestCodexAuthProblemMissingBearerEnvIsUnverified(t *testing.T) {
	const name = "REVIEWER_C05_MISSING_BEARER"
	prev, ok := os.LookupEnv(name)
	os.Unsetenv(name)
	t.Cleanup(func() {
		if ok {
			os.Setenv(name, prev)
		} else {
			os.Unsetenv(name)
		}
	})
	sc := hookcli.CodexMCPEffective{BearerEnv: name, BearerEnvFrom: "fixture.toml"}
	if reason := codexAuthProblem(&sc, ""); reason == "" {
		t.Fatal("missing configured bearer env incorrectly accepted as unauthenticated")
	}
	if reason := codexAuthProblem(&sc, "probe-key"); reason == "" {
		t.Fatal("missing configured bearer env must be unverified even when the probe carried a key")
	}
	t.Setenv(name, "")
	if reason := codexAuthProblem(&sc, ""); reason == "" {
		t.Fatal("empty configured bearer env incorrectly accepted as unauthenticated")
	}
}

// TestCodexAuthProblemHeaderAuthIsUnverified: an entry authenticating
// through an Authorization header (static or env-resolved) holds a
// credential verify cannot compare against its probe key without
// handling the header value, so the alignment must report unverified
// rather than assume the header matches.
func TestCodexAuthProblemHeaderAuthIsUnverified(t *testing.T) {
	sc := hookcli.CodexMCPEffective{HeaderAuth: true, HeaderAuthFrom: "fixture.toml"}
	if reason := codexAuthProblem(&sc, ""); reason == "" {
		t.Fatal("an Authorization-header credential must be unverified without a probe key")
	}
	if reason := codexAuthProblem(&sc, "probe-key"); reason == "" {
		t.Fatal("an Authorization-header credential must be unverified even when the probe carried a key")
	}
}

// TestConnectCodexVerifyDisabledMCPEntryIsUnverified is the reviewer's
// round-5 red proof: Codex filters disabled MCP entries (upstream
// codex-rs connection_manager.rs), so a preserved enabled = false punk
// entry is not a working connection. Verify must report the alignment
// unverified - never ok - even when the disabled entry's pin, endpoint
// and credentials all match the probe.
func TestConnectCodexVerifyDisabledMCPEntryIsUnverified(t *testing.T) {
	repo, _ := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	ns, _ := hookcli.ProjectNamespace(repo)
	config := filepath.Join(repo, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	data := "[mcp_servers.punk]\nurl = '" + ts.URL + "/mcp?toolset=agent'\nenabled = false\nhttp_headers = { 'X-Punk-Namespace' = '" + ns + "' }\n"
	if err := os.WriteFile(config, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err == nil && strings.Contains(out, "namespace alignment ok:") {
		t.Fatalf("a disabled MCP entry is not a working connection; verify must not claim alignment:\n%s", out)
	}
	if err == nil && (!strings.Contains(out, "namespace alignment not verified") || !strings.Contains(out, "enabled = false")) {
		t.Fatalf("verify must report the disabled entry as the reason it cannot prove alignment:\n%s", out)
	}
}

// TestConnectCodexVerifyHeaderHelperIsUnverified is the reviewer's
// round-5 red proof: http_headers_helper resolves part of the entry's
// headers by running an external command at connect time (upstream
// codex-rs config/src/mcp_types.rs), so the entry's effective headers -
// namespace pin and credentials alike - are unknown to verify. Verify
// never executes the helper and must report the alignment unverified
// rather than claim ok from the static headers alone.
func TestConnectCodexVerifyHeaderHelperIsUnverified(t *testing.T) {
	repo, _ := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	ns, _ := hookcli.ProjectNamespace(repo)
	config := filepath.Join(repo, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	data := "[mcp_servers.punk]\nurl = '" + ts.URL + "/mcp?toolset=agent'\nhttp_headers_helper = 'false'\nhttp_headers = { 'X-Punk-Namespace' = '" + ns + "' }\n"
	if err := os.WriteFile(config, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err == nil && strings.Contains(out, "namespace alignment ok:") {
		t.Fatalf("helper-supplied headers are unknown to verify; verify must not claim alignment:\n%s", out)
	}
	if err == nil && (!strings.Contains(out, "namespace alignment not verified") || !strings.Contains(out, "http_headers_helper")) {
		t.Fatalf("verify must report http_headers_helper as the reason it cannot prove alignment:\n%s", out)
	}
}
