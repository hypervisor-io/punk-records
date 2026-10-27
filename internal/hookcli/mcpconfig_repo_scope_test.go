package hookcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRepoScopedTarget pins the path-based project-scope probe Hermes
// (--config) and OpenClaw (--home) use: inside a git work tree and not
// gitignored is the only combination that flips to project scope. Every
// other outcome - no repo, an ignored file, no git - stays global, which
// keeps the ordinary embed-the-key behaviour untouched.
func TestRepoScopedTarget(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; the repo-scope probe requires it")
	}

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.yaml\nbuild/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"untracked file in the work tree", filepath.Join(repo, "config.yaml"), true},
		{"nonexistent file in the work tree", filepath.Join(repo, "fresh", "config.yaml"), true},
		{"repo root itself", filepath.Join(repo, "opencode.json"), true},
		{"gitignored file", filepath.Join(repo, "ignored.yaml"), false},
		{"file under a gitignored directory", filepath.Join(repo, "build", "config.json"), false},
		{"outside any repository", filepath.Join(t.TempDir(), "config.yaml"), false},
		{"empty path", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoScopedTarget(tc.path); got != tc.want {
				t.Fatalf("RepoScopedTarget(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}

	// The probe is read-only: it must not create, modify or remove anything.
	before, err := os.ReadDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	_ = RepoScopedTarget(filepath.Join(repo, "config.yaml"))
	after, err := os.ReadDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("the probe changed the work tree: %d entries before, %d after", len(before), len(after))
	}
}

// TestProjectScopedEntryUsesEnvReference is the rule the probe feeds: a
// project-scoped entry never carries the literal token, in any host's
// header shape, and is never treated as holding a secret.
func TestProjectScopedEntryUsesEnvReference(t *testing.T) {
	o := MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: "synthetic-literal", ProjectScoped: true}
	if hasLiteralMCPToken(o) {
		t.Fatal("a project-scoped entry must never count as holding a literal token")
	}
	h := mcpHeaders(o)
	if h["Authorization"] != "Bearer ${PUNK_API_KEY}" {
		t.Fatalf("Authorization = %v, want the env reference", h["Authorization"])
	}

	// Global scope is unchanged.
	global := MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: "synthetic-literal"}
	if !hasLiteralMCPToken(global) {
		t.Fatal("a global entry with a literal key must still count as holding one")
	}
	if got := mcpHeaders(global)["Authorization"]; got != "Bearer synthetic-literal" {
		t.Fatalf("global Authorization = %v, want the literal", got)
	}

	// An explicit --api-key-env wins in either scope.
	explicit := MCPEntryOpts{APIKey: "synthetic-literal", APIKeyEnv: "PUNK_CUSTOM", ProjectScoped: true}
	if got := mcpHeaders(explicit)["Authorization"]; got != "Bearer ${PUNK_CUSTOM}" {
		t.Fatalf("explicit --api-key-env Authorization = %v", got)
	}
}
