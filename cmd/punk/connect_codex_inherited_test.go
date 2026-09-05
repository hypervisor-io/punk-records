package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// TestConnectCodexVerifyInheritedConnectionIdentity is the reviewer's
// round-6 red proof folded permanent: Codex recursively merges MCP
// table FIELDS across config layers rather than replacing whole
// entries (upstream config/src/merge.rs:58-132, state.rs:343), so a
// project [mcp_servers.punk] entry that looks clean on its own can
// still inherit connection identity from the global layer:
//
//   - enabled = false only in the global layer: the project entry
//     lacks the field, the merged entry is disabled and is not a
//     working connection;
//   - http_headers_helper only in the global layer: the merged entry's
//     headers are resolved by an external command verify never
//     executes, so pin and credentials are unverified;
//   - bearer_token_env_var only in the global layer, missing or empty
//     in this environment: inherited into the merged entry, and Codex
//     rejects a configured bearer variable that is not set;
//   - a header-only [mcp_servers.punk.http_headers] fragment carrying
//     an Authorization with NO [mcp_servers.punk] table of its own:
//     the fragment still contributes to the merged entry the project
//     layer defines, so the merged connection authenticates from a
//     header credential verify cannot compare.
//
// Verify comparing only the top entry's fields reported a false
// alignment for all four; it must compute the effective merged entry
// across the contributing scopes and report the alignment unverified.
func TestConnectCodexVerifyInheritedConnectionIdentity(t *testing.T) {
	for _, setting := range []string{
		"enabled = false",
		"http_headers_helper = 'false'",
		"bearer_token_env_var = 'PUNK_TEST_C05_INHERITED_MISSING_TOKEN'",
		"header-only fragment",
	} {
		t.Run(setting, func(t *testing.T) {
			t.Setenv("PUNK_TEST_C05_INHERITED_MISSING_TOKEN", "")
			repo, home := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
			ts := verifyWhoamiServer(t)
			ns, _ := hookcli.ProjectNamespace(repo)
			config := "[mcp_servers.punk]\nurl = '" + ts.URL + "/mcp?toolset=agent'\nhttp_headers = { 'X-Punk-Namespace' = '" + ns + "' }\n"
			for _, p := range []string{filepath.Join(repo, ".codex", "config.toml"), filepath.Join(home, "config.toml")} {
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				text := config
				if p == filepath.Join(home, "config.toml") {
					text += setting + "\n"
					if setting == "header-only fragment" {
						text = "[mcp_servers.punk.http_headers]\nAuthorization = 'Bearer reviewer-fake'\n"
					}
				}
				if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := captureStdout(t, func() error {
				return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
			})
			if err == nil && strings.Contains(out, "namespace alignment ok:") {
				t.Fatalf("lower config layer's inherited connection identity ignored; false alignment claim:\n%s", out)
			}
			if err == nil && !strings.Contains(out, "namespace alignment not verified") {
				t.Fatalf("verify must report the inherited connection identity as unverified:\n%s", out)
			}
		})
	}
}

// TestConnectCodexVerifyFragmentOnlyReportsHonestly: when NO config
// layer defines [mcp_servers.punk] itself and only a header fragment
// exists, the fragment merges into an entry that does not exist -
// verify must report the MCP side unknown (naming the fragment) rather
// than claim alignment or silently treat the fragment as absent.
func TestConnectCodexVerifyFragmentOnlyReportsHonestly(t *testing.T) {
	_, home := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	ts := verifyWhoamiServer(t)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	fragment := "[mcp_servers.punk.http_headers]\nAuthorization = 'Bearer reviewer-fake'\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(fragment), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-mcp", "--no-skill", "--verify", "--url", ts.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "namespace alignment ok") {
		t.Fatalf("a fragment without any [mcp_servers.punk] table proves no connection; verify must not claim alignment:\n%s", out)
	}
	if !strings.Contains(out, "namespace alignment not verified") || !strings.Contains(out, "MCP side is unknown") {
		t.Fatalf("verify must report the fragment-only MCP side as unknown:\n%s", out)
	}
	if strings.Contains(out, "reviewer-fake") {
		t.Fatalf("verify output must never carry a header value:\n%s", out)
	}
}
