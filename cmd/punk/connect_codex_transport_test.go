package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// TestConnectCodexVerifyUnsupportedInheritedIdentityIsUnverified is
// the reviewer's round-7 red proof folded permanent. The project layer
// carries a normal HTTP punk entry (url plus the namespace pin) while
// the GLOBAL layer contributes connection identity through fields and
// TOML representations the old inspector skipped:
//
//   - command only in the global layer: the field-wise merged entry
//     carries BOTH command and url, a mixed transport upstream Codex
//     rejects (config/src/mcp_types.rs
//     RawMcpServerConfig::try_from) - an entry Codex refuses to load
//     is not a connection whose alignment verify can prove;
//   - [mcp_servers] punk.bearer_token_env_var = ... (a dotted key
//     under the parent table) and mcp_servers.punk.bearer_token_env_var
//     = ... (a dotted key at the document root): valid TOML Codex
//     merges into the punk entry, but representations the inspector
//     does not parse - the scope is conservatively unknown.
//
// Verify comparing only the top entry's parsed fields reported a false
// alignment for all three; it must report the alignment unverified and
// never print "namespace alignment ok".
func TestConnectCodexVerifyUnsupportedInheritedIdentityIsUnverified(t *testing.T) {
	cases := map[string]string{
		"stdio command mixed with HTTP": "[mcp_servers.punk]\ncommand='reviewer-nonexistent-command'\n",
		"parent dotted bearer":          "[mcp_servers]\npunk.bearer_token_env_var='REVIEW_C05_MISSING_TOKEN'\n",
		"root dotted bearer":            "mcp_servers.punk.bearer_token_env_var='REVIEW_C05_MISSING_TOKEN'\n",
	}
	for name, global := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("REVIEW_C05_MISSING_TOKEN", "")
			repo, home := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
			ts := verifyWhoamiServer(t)
			ns, _ := hookcli.ProjectNamespace(repo)
			project := "[mcp_servers.punk]\nurl='" + ts.URL + "/mcp?toolset=agent'\nhttp_headers={'X-Punk-Namespace'='" + ns + "'}\n"
			for p, body := range map[string]string{
				filepath.Join(repo, ".codex", "config.toml"): project,
				filepath.Join(home, "config.toml"):           global,
			} {
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := captureStdout(t, func() error {
				return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
			})
			if err == nil && strings.Contains(out, "namespace alignment ok:") {
				t.Fatalf("ignored inherited connection identity; false alignment success:\n%s", out)
			}
			if err == nil && !strings.Contains(out, "namespace alignment not verified") {
				t.Fatalf("verify must report the unsupported inherited identity as unverified:\n%s", out)
			}
		})
	}
}
