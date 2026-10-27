package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// projectKeyToken is a synthetic token with no other meaning. The
// project-scope guard is proven by finding - or not finding - these exact
// bytes anywhere under the temp repository after a connect.
const projectKeyToken = "synthetic-project-scope-key-do-not-embed"

// connectKeyFixture gives one connect a clean cwd plus an isolated HOME and
// pins every PUNK_* the connectors read, so nothing leaks in from the
// operator's environment (or from the PUNK_MESSAGING=1 gate run).
func connectKeyFixture(t *testing.T, inRepo bool) (cwd, home string) {
	t.Helper()
	home = t.TempDir()
	cwd = t.TempDir()
	if inRepo {
		cwd = t.TempDir()
		if _, err := exec.LookPath("git"); err == nil {
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			git("init", "-q")
			git("remote", "add", "origin", "git@github.com:acme/punk-records.git")
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(home, "missing-credentials.json"))
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", projectKeyToken)
	t.Setenv("PUNK_NAMESPACE", "")
	t.Setenv("PUNK_MESSAGING", "")
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("CLINE_MCP_SETTINGS_PATH", "")
	t.Setenv("CLINE_DATA_DIR", "")
	t.Setenv("CLINE_DIR", "")
	t.Chdir(cwd)
	return cwd, home
}

// treeContainsKey reports the first file under root whose bytes contain the
// token, or "" when the whole tree is clean.
func treeContainsKey(t *testing.T, root string) string {
	t.Helper()
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not a leak
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(raw), projectKeyToken) {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// TestConnectProjectScopeNeverEmbedsLiteralKey is the acceptance case for the
// planner defect: with --project the config lives inside the repository, so a
// selected literal key must NOT be written anywhere under it. Every
// project-capable connector is exercised; the whole temp repo tree is
// grepped for the token bytes.
func TestConnectProjectScopeNeverEmbedsLiteralKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"claude-code", []string{"connect", "claude-code", "--project", "--no-skill"}},
		{"cursor", []string{"connect", "cursor", "--project", "--no-skill"}},
		{"opencode", []string{"connect", "opencode", "--project", "--no-skill"}},
		{"antigravity", []string{"connect", "antigravity", "--project", "--no-skill"}},
		{"copilot", []string{"connect", "copilot", "--project", "--no-skill"}},
		{"codex", []string{"connect", "codex", "--project", "--no-skill"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := connectKeyFixture(t, true)
			out, err := captureStdout(t, func() error { return run(tc.args) })
			if err != nil {
				t.Fatalf("connect %s: %v\n%s", tc.name, err, out)
			}
			if leaked := treeContainsKey(t, repo); leaked != "" {
				t.Fatalf("the literal key was written into a repo-scoped file: %s", leaked)
			}
			if !strings.Contains(out, "was NOT embedded") {
				t.Fatalf("connect must print the not-embedded note:\n%s", out)
			}
		})
	}
}

// TestConnectGlobalScopeStillEmbedsLiteralKeyAt0600 is the other half: the
// guard must not weaken the global (Desktop/GUI-readable) path, which still
// embeds the token and writes the file owner-only.
func TestConnectGlobalScopeStillEmbedsLiteralKeyAt0600(t *testing.T) {
	for _, tc := range []struct {
		name, note string
		args       []string
		path       func(home string) string
	}{
		{
			name: "claude-code",
			args: []string{"connect", "claude-code", "--no-skill"},
			path: func(home string) string { return filepath.Join(home, ".claude.json") },
			note: "keep that file private",
		},
		{
			name: "codex",
			args: []string{"connect", "codex", "--no-skill"},
			path: func(home string) string { return filepath.Join(home, "codex-home", "config.toml") },
			note: "written into the punk block",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, home := connectKeyFixture(t, false)
			out, err := captureStdout(t, func() error { return run(tc.args) })
			if err != nil {
				t.Fatalf("connect %s: %v\n%s", tc.name, err, out)
			}
			p := tc.path(home)
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("global config not written at %s: %v\n%s", p, err, out)
			}
			if !strings.Contains(string(raw), projectKeyToken) {
				t.Fatalf("the global config must embed the literal key for a GUI launch:\n%s", raw)
			}
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, want 0600: the file holds a literal token", info.Mode().Perm())
			}
			if !strings.Contains(out, tc.note) {
				t.Fatalf("global connect note missing (%q):\n%s", tc.note, out)
			}
			if strings.Contains(out, "was NOT embedded") {
				t.Fatalf("the global path must not print the project note:\n%s", out)
			}
		})
	}
}

// TestConnectApiKeyEnvRespectedInBothScopes pins that an explicit
// --api-key-env keeps working unchanged: the host's env reference is
// written and the key is never embedded, in project and global scope alike.
func TestConnectApiKeyEnvRespectedInBothScopes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		path func(cwd, home string) string
	}{
		{
			name: "project",
			args: []string{"connect", "claude-code", "--project", "--no-skill", "--api-key-env", "PUNK_CUSTOM_TOKEN"},
			path: func(cwd, _ string) string { return filepath.Join(cwd, ".mcp.json") },
		},
		{
			name: "global",
			args: []string{"connect", "claude-code", "--no-skill", "--api-key-env", "PUNK_CUSTOM_TOKEN"},
			path: func(_, home string) string { return filepath.Join(home, ".claude.json") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd, home := connectKeyFixture(t, true)
			out, err := captureStdout(t, func() error { return run(tc.args) })
			if err != nil {
				t.Fatalf("connect %s: %v\n%s", tc.name, err, out)
			}
			p := tc.path(cwd, home)
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("config not written at %s: %v\n%s", p, err, out)
			}
			if !strings.Contains(string(raw), "Bearer ${PUNK_CUSTOM_TOKEN}") {
				t.Fatalf("an explicit --api-key-env must be written as the host env reference:\n%s", raw)
			}
			if strings.Contains(string(raw), projectKeyToken) {
				t.Fatalf("an explicit --api-key-env must not embed the key:\n%s", raw)
			}
			if leaked := treeContainsKey(t, cwd); leaked != "" {
				t.Fatalf("the key leaked into %s", leaked)
			}
		})
	}
}
