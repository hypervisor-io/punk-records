package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// connectCommandFiles returns the bytes of every punk-* command file the
// given connect target installed under root, keyed by relative path.
func connectCommandFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range hookcli.CommandNames {
		p := filepath.Join(root, name+".md")
		if _, err := os.Stat(p); err == nil {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			files[name+".md"] = string(raw)
			continue
		}
		p = filepath.Join(root, name, "SKILL.md")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("command %s installed nowhere under %s", name, root)
		}
		files[name+"/SKILL.md"] = string(raw)
	}
	if len(files) != len(hookcli.CommandNames) {
		t.Fatalf("found %d command files under %s, want %d", len(files), root, len(hookcli.CommandNames))
	}
	return files
}

// assertReconnectIdempotent runs the same connect command again and
// proves every command file is byte-identical afterwards.
func assertReconnectIdempotent(t *testing.T, args []string, root string, before map[string]string) {
	t.Helper()
	if _, err := captureStdout(t, func() error { return cmdConnect(args) }); err != nil {
		t.Fatalf("reconnect %v: %v", args, err)
	}
	after := connectCommandFiles(t, root)
	if len(after) != len(before) {
		t.Fatalf("reconnect changed the file set: %d -> %d files", len(before), len(after))
	}
	for name, raw := range before {
		if after[name] != raw {
			t.Fatalf("reconnect changed %s", name)
		}
	}
}

// TestConnectOpenCodeInstallsCommands drives the real `punk connect
// opencode` against a temp HOME/XDG root and proves the five punk
// commands land as native OpenCode command files (P2-C), with an
// idempotent reconnect.
func TestConnectOpenCodeInstallsCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PUNK_API_KEY", "")
	if _, err := captureStdout(t, func() error {
		return cmdConnect([]string{"opencode", "--url", "http://127.0.0.1:9090"})
	}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config", "opencode", "commands")
	before := connectCommandFiles(t, root)
	for name, raw := range before {
		if !strings.HasPrefix(raw, "---\ndescription: ") {
			t.Fatalf("%s: OpenCode command frontmatter must carry the description: %q", name, raw[:min(len(raw), 60)])
		}
		if !strings.Contains(raw, hookcli.SkillMarker) {
			t.Fatalf("%s: managed marker missing", name)
		}
	}
	assertReconnectIdempotent(t, []string{"opencode", "--url", "http://127.0.0.1:9090"}, root, before)
}

// TestConnectClineInstallsCommandSkills drives the real `punk connect
// cline` against a temp HOME and proves the five punk commands land as
// native Cline skills (~/.cline/skills/punk-<name>/SKILL.md, Cline's
// slash-invocable command surface), with an idempotent reconnect.
func TestConnectClineInstallsCommandSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{"CLINE_MCP_SETTINGS_PATH", "CLINE_DATA_DIR", "CLINE_DIR", "PUNK_API_KEY"} {
		t.Setenv(k, "")
	}
	if _, err := captureStdout(t, func() error {
		return cmdConnect([]string{"cline", "--url", "http://127.0.0.1:9090"})
	}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".cline", "skills")
	before := connectCommandFiles(t, root)
	for name, raw := range before {
		if !strings.HasPrefix(raw, "---\nname: "+strings.TrimSuffix(name, "/SKILL.md")+"\n") {
			t.Fatalf("%s: Cline requires frontmatter name == directory name: %q", name, raw[:min(len(raw), 60)])
		}
	}
	assertReconnectIdempotent(t, []string{"cline", "--url", "http://127.0.0.1:9090"}, root, before)

	// Project mode writes the same skills under .cline/skills in the
	// workspace.
	t.Chdir(t.TempDir())
	if _, err := captureStdout(t, func() error {
		return cmdConnect([]string{"cline", "--project", "--no-mcp", "--url", "http://127.0.0.1:9090"})
	}); err != nil {
		t.Fatal(err)
	}
	proj := connectCommandFiles(t, filepath.Join(".cline", "skills"))
	for name, raw := range before {
		if proj[name] != raw {
			t.Fatalf("project rendering of %s differs from the global one", name)
		}
	}
}

// TestConnectAntigravityInstallsCommandSkills drives the real `punk
// connect antigravity` against a temp HOME and proves the five punk
// commands land as Antigravity Agent Skills
// (~/.gemini/config/skills/punk-<name>/SKILL.md) - the client's
// current native command surface since legacy workflows stopped working
// on 2026-10-19 - with an idempotent reconnect.
func TestConnectAntigravityInstallsCommandSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PUNK_API_KEY", "")
	if _, err := captureStdout(t, func() error {
		return cmdConnect([]string{"antigravity", "--no-mcp", "--url", "http://127.0.0.1:9090"})
	}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".gemini", "config", "skills")
	before := connectCommandFiles(t, root)
	assertReconnectIdempotent(t, []string{"antigravity", "--no-mcp", "--url", "http://127.0.0.1:9090"}, root, before)
}
