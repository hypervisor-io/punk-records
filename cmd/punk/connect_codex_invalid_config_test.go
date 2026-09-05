package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// The tests in this file are the reviewer's round-8 red proofs folded
// permanent at the CLI boundary: a config.toml Codex cannot load must
// never yield a "namespace alignment ok" claim. Both fixtures look
// aligned to a last-wins reader - the pin and the endpoint match the
// probe - but upstream rejects the document, so the installed
// connection is not a working one and the alignment is unverified.

// TestConnectCodexVerifyDuplicateKeyConfigIsUnverified: the punk entry
// defines enabled twice in the same table (enabled = false then
// enabled = true). TOML v1.0.0 rejects duplicate definitions of a key
// (toml.io/v1.0.0 keys - quoted/bare spellings of the same key
// included), so Codex cannot load this config.toml at all. The old
// inspector read the pair last-wins, reported the entry enabled and
// printed a false alignment; verify must report the MCP side unknown
// and the alignment unverified.
func TestConnectCodexVerifyDuplicateKeyConfigIsUnverified(t *testing.T) {
	repo, _ := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	ns, _ := hookcli.ProjectNamespace(repo)
	p := filepath.Join(repo, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[mcp_servers.punk]\nurl='" + ts.URL + "/mcp?toolset=agent'\nhttp_headers={'X-Punk-Namespace'='" + ns + "'}\nenabled=false\nenabled=true\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err == nil && strings.Contains(out, "namespace alignment ok:") {
		t.Fatalf("invalid config reported as verified working alignment:\n%s", out)
	}
	if err == nil && (!strings.Contains(out, "namespace alignment not verified") || !strings.Contains(out, "MCP side is unknown")) {
		t.Fatalf("verify must report the duplicate-key config through an unknown MCP side:\n%s", out)
	}
}

// TestConnectCodexVerifyUnsupportedAuthKindIsUnverified: the punk entry
// carries auth = 'reviewer-invalid-auth-kind'. Upstream Codex's
// McpServerAuth enum (config/src/mcp_types.rs:177-187) supports exactly
// oauth and chatgpt, so an explicit auth value outside the enum is an
// entry Codex cannot load and an auth identity verify cannot establish
// - conservatively unknown. The old inspector accepted any auth string
// as presence-only and printed a false alignment; verify must report
// the MCP side unknown, and the unsupported value must never be echoed
// into the output.
func TestConnectCodexVerifyUnsupportedAuthKindIsUnverified(t *testing.T) {
	repo, _ := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	ns, _ := hookcli.ProjectNamespace(repo)
	p := filepath.Join(repo, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[mcp_servers.punk]\nurl='" + ts.URL + "/mcp?toolset=agent'\nhttp_headers={'X-Punk-Namespace'='" + ns + "'}\nauth='reviewer-invalid-auth-kind'\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err == nil && strings.Contains(out, "namespace alignment ok:") {
		t.Fatalf("invalid config reported as verified working alignment:\n%s", out)
	}
	if err == nil && (!strings.Contains(out, "namespace alignment not verified") || !strings.Contains(out, "MCP side is unknown")) {
		t.Fatalf("verify must report the unsupported auth kind through an unknown MCP side:\n%s", out)
	}
	if strings.Contains(out, "reviewer-invalid-auth-kind") {
		t.Fatalf("verify output must never echo the auth value:\n%s", out)
	}
}
