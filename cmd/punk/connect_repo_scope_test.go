package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConnectRepoScopedTargetNeverEmbedsLiteralKey covers the two
// connectors that take an arbitrary path instead of a --project flag
// (Hermes --config, OpenClaw --home). The repo-scope rule is derived from
// the path itself: a config inside a git work tree that is not gitignored
// gets the env reference and the not-embedded note; a gitignored target
// and a target outside any repository keep the ordinary embedding.
func TestConnectRepoScopedTargetNeverEmbedsLiteralKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     func(repo string) []string
		cfgPath  func(repo string) string
		noteLine string
	}{
		{
			name: "hermes --config",
			args: func(repo string) []string {
				return []string{"connect", "hermes", "--config", filepath.Join(repo, ".hermes", "config.yaml"), "--no-skill"}
			},
			cfgPath:  func(repo string) string { return filepath.Join(repo, ".hermes", "config.yaml") },
			noteLine: "was NOT embedded",
		},
		{
			name:     "openclaw --home",
			args:     func(repo string) []string { return []string{"connect", "openclaw", "--home", repo, "--no-skill"} },
			cfgPath:  func(repo string) string { return filepath.Join(repo, "config.json") },
			noteLine: "was NOT embedded",
		},
	} {
		t.Run(tc.name+"/inside a work tree", func(t *testing.T) {
			repo, _ := connectKeyFixture(t, true)
			out, err := captureStdout(t, func() error { return run(tc.args(repo)) })
			if err != nil {
				t.Fatalf("%s: %v\n%s", tc.name, err, out)
			}
			if leaked := treeContainsKey(t, repo); leaked != "" {
				t.Fatalf("the literal key was written into a repo-scoped file: %s", leaked)
			}
			if !strings.Contains(out, tc.noteLine) {
				t.Fatalf("%s must print the not-embedded note:\n%s", tc.name, out)
			}
			raw, err := os.ReadFile(tc.cfgPath(repo))
			if err != nil {
				t.Fatalf("config not written at %s: %v\n%s", tc.cfgPath(repo), err, out)
			}
			if !strings.Contains(string(raw), "PUNK_API_KEY") {
				t.Fatalf("%s must reference PUNK_API_KEY instead of embedding the key:\n%s", tc.name, raw)
			}
		})

		t.Run(tc.name+"/gitignored target still embeds", func(t *testing.T) {
			repo, _ := connectKeyFixture(t, true)
			// Ignore the whole tree, so the target is never committed.
			if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := captureStdout(t, func() error { return run(tc.args(repo)) })
			if err != nil {
				t.Fatalf("%s: %v\n%s", tc.name, err, out)
			}
			p := tc.cfgPath(repo)
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("config not written at %s: %v\n%s", p, err, out)
			}
			if !strings.Contains(string(raw), projectKeyToken) {
				t.Fatalf("a gitignored target is never committed, so it keeps the embedding:\n%s", raw)
			}
			if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, want 0600: the file holds a literal token", info.Mode().Perm())
			}
			if strings.Contains(out, "was NOT embedded") {
				t.Fatalf("%s must not print the project note for a gitignored target:\n%s", tc.name, out)
			}
		})

		t.Run(tc.name+"/outside a repository still embeds", func(t *testing.T) {
			repo, _ := connectKeyFixture(t, true)
			// A plain directory that is not a git work tree.
			outside := t.TempDir()
			args := tc.args(repo)
			// Re-point the connector at the non-repo directory.
			for i, a := range args {
				if a == repo {
					args[i] = outside
				}
				if strings.Contains(a, filepath.Join(repo, ".hermes")) {
					args[i] = filepath.Join(outside, ".hermes", "config.yaml")
				}
			}
			out, err := captureStdout(t, func() error { return run(args) })
			if err != nil {
				t.Fatalf("%s: %v\n%s", tc.name, err, out)
			}
			p := strings.Replace(tc.cfgPath(repo), repo, outside, 1)
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("config not written at %s: %v\n%s", p, err, out)
			}
			if !strings.Contains(string(raw), projectKeyToken) {
				t.Fatalf("a target outside any repository keeps the embedding:\n%s", raw)
			}
			if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
			}
		})
	}
}
