package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// nativeCommandsEnv is the env stub for the native-command target
// tests: XDG_CONFIG_HOME pinned to a writable temp root (created once,
// outside the returned closure - t.TempDir returns a fresh directory
// on every call) so the OpenCode global path is deterministic.
func nativeCommandsEnv(t *testing.T) func(string) string {
	t.Helper()
	xdg := filepath.Join(t.TempDir(), "xdg")
	return func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return xdg
		}
		return ""
	}
}

// writeNativeCommands installs every target with render and returns a
// map of path -> bytes for reconnect comparisons.
func writeNativeCommands(t *testing.T, ts []SkillTarget, render func(SkillTarget) string) map[string]string {
	t.Helper()
	written := map[string]string{}
	for _, tg := range ts {
		changed, err := WriteSkill(tg.Path, render(tg))
		if err != nil {
			t.Fatalf("write %s: %v", tg.Path, err)
		}
		if !changed {
			t.Fatalf("first install of %s reported no change", tg.Path)
		}
		raw, err := os.ReadFile(tg.Path)
		if err != nil {
			t.Fatal(err)
		}
		written[tg.Path] = string(raw)
	}
	return written
}

// reconnectNativeCommands re-runs the install and proves idempotency:
// every WriteSkill reports no change and the bytes stay identical.
func reconnectNativeCommands(t *testing.T, ts []SkillTarget, render func(SkillTarget) string, before map[string]string) {
	t.Helper()
	for _, tg := range ts {
		changed, err := WriteSkill(tg.Path, render(tg))
		if err != nil {
			t.Fatalf("reconnect %s: %v", tg.Path, err)
		}
		if changed {
			t.Fatalf("reconnect of %s rewrote the file; must be byte-identical no-op", tg.Path)
		}
		raw, err := os.ReadFile(tg.Path)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != before[tg.Path] {
			t.Fatalf("reconnect of %s changed bytes", tg.Path)
		}
	}
}

// TestOpenCodeCommandTargets pins the native locations (P2-C):
// global ~/.config/opencode/commands (XDG_CONFIG_HOME honored, the
// same base the plugin/config paths use) and per-project
// .opencode/commands; one punk-<command>.md per command, invoked as
// /punk-<command>.
func TestOpenCodeCommandTargets(t *testing.T) {
	home := t.TempDir()
	env := nativeCommandsEnv(t)
	ts, err := OpenCodeCommandTargets(false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != len(CommandNames) {
		t.Fatalf("%d command targets, want %d", len(ts), len(CommandNames))
	}
	xdg := env("XDG_CONFIG_HOME")
	for i, name := range CommandNames {
		want := filepath.Join(xdg, "opencode", "commands", name+".md")
		if ts[i].Name != name || ts[i].Path != want {
			t.Fatalf("target %d = %s at %s, want %s at %s", i, ts[i].Name, ts[i].Path, name, want)
		}
		if ts[i].Opts.Agent != "opencode" || ts[i].Opts.Command != name || ts[i].Opts.ToolPrefix != "punk_" {
			t.Fatalf("%s: opts = %+v", name, ts[i].Opts)
		}
	}
	// Without XDG_CONFIG_HOME the global root falls back to ~/.config.
	ts, err = OpenCodeCommandTargets(false, home, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "opencode", "commands", "punk-join.md")
	if ts[0].Path != want {
		t.Fatalf("no-XDG global path = %s, want %s", ts[0].Path, want)
	}
	// Project mode is workspace-relative, like every other --project path.
	ts, err = OpenCodeCommandTargets(true, home, nativeCommandsEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(".opencode", "commands", "punk-join.md"); ts[0].Path != want {
		t.Fatalf("project path = %s, want %s", ts[0].Path, want)
	}
}

// TestOpenCodeCommandRender pins the command-file shape: OpenCode takes
// the command name from the FILE name, so the frontmatter carries only
// the description; the body is the shared CommandBody bytes (OpenCode
// tool prefix, /punk-* invocations) behind the managed marker.
func TestOpenCodeCommandRender(t *testing.T) {
	for _, name := range CommandNames {
		o := SkillOpts{Agent: "opencode", ToolPrefix: "punk_", Command: name}
		text := RenderOpenCodeCommand(o)
		if !strings.HasPrefix(text, "---\ndescription: ") {
			t.Fatalf("%s: frontmatter must carry only the description: %q", name, prefixOf(text, 80))
		}
		parts := strings.SplitN(text, "---\n", 3)
		if len(parts) != 3 || parts[0] != "" {
			t.Fatalf("%s: missing frontmatter boundaries", name)
		}
		var front struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		decoder := yaml.NewDecoder(strings.NewReader(parts[1]))
		decoder.KnownFields(true)
		if err := decoder.Decode(&front); err != nil {
			t.Fatalf("%s: frontmatter is not valid YAML: %v", name, err)
		}
		if front.Name != "" {
			t.Fatalf("%s: OpenCode commands take their name from the file name; frontmatter name key must stay absent", name)
		}
		if front.Description == "" || len(front.Description) > 1024 {
			t.Fatalf("%s: bad description %q", name, front.Description)
		}
		if !strings.HasPrefix(parts[2], SkillMarker+"\n") || strings.Count(text, SkillMarker) != 1 {
			t.Fatalf("%s: managed marker missing or duplicated", name)
		}
		body, ok := CommandBody(name, o)
		if !ok || !strings.Contains(text, body) {
			t.Fatalf("%s: rendered file must carry the shared CommandBody verbatim", name)
		}
		forbidGuidance(t, text, "{{", "<no value>", "—", "–")
	}
}

// TestOpenCodeCommandsInstallReconnect is the P2-C acceptance for
// OpenCode: a temp HOME install of the five command files, a reconnect
// that is byte-identical, and a foreign file punk refuses to touch.
func TestOpenCodeCommandsInstallReconnect(t *testing.T) {
	home := t.TempDir()
	ts, err := OpenCodeCommandTargets(false, home, nativeCommandsEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	before := writeNativeCommands(t, ts, func(tg SkillTarget) string { return RenderOpenCodeCommand(tg.Opts) })
	reconnectNativeCommands(t, ts, func(tg SkillTarget) string { return RenderOpenCodeCommand(tg.Opts) }, before)

	// A foreign file at a punk command path is refused, never overwritten.
	foreign := filepath.Join(filepath.Dir(ts[0].Path), CommandPeers+".md")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("# my own command\n\ndo not touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tg := range ts {
		if tg.Path != foreign {
			continue
		}
		if _, err := WriteSkill(tg.Path, RenderOpenCodeCommand(tg.Opts)); err == nil {
			t.Fatal("WriteSkill must refuse a foreign command file")
		}
		raw, _ := os.ReadFile(foreign)
		if string(raw) != "# my own command\n\ndo not touch\n" {
			t.Fatal("foreign command file was modified")
		}
	}
}

// TestClineCommandSkills is the P2-C acceptance for Cline. Cline has no
// separate "workflows" format (checked 2026-10-27): its native command
// surface is Agent Skills - <name>/SKILL.md under ~/.cline/skills
// (global) or .cline/skills (workspace), frontmatter name must match
// the directory name, and every enabled skill is slash-invocable as
// /<name>. The five punk commands therefore install as ordinary Cline
// skills carrying the shared P2-A bodies, and reconnect is idempotent.
func TestClineCommandSkills(t *testing.T) {
	home := t.TempDir()
	ts, err := ClineCommandSkillTargets(false, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != len(CommandNames) {
		t.Fatalf("%d targets, want %d", len(ts), len(CommandNames))
	}
	for i, name := range CommandNames {
		want := filepath.Join(home, ".cline", "skills", name, "SKILL.md")
		if ts[i].Name != name || ts[i].Path != want {
			t.Fatalf("target %d = %s at %s, want %s at %s", i, ts[i].Name, ts[i].Path, name, want)
		}
		if ts[i].Opts.Agent != "cline" || ts[i].Opts.Command != name {
			t.Fatalf("%s: opts = %+v", name, ts[i].Opts)
		}
	}
	// Cline requires frontmatter name == directory name; the shared
	// renderer already guarantees that, and the body is byte-identical
	// to RenderCommand for cline options.
	for _, tg := range ts {
		text := Render(tg)
		if !strings.HasPrefix(text, "---\nname: "+tg.Name+"\n") {
			t.Fatalf("%s: frontmatter name must match the directory name: %q", tg.Name, prefixOf(text, 80))
		}
		if want := RenderCommand(tg.Opts); text != want {
			t.Fatalf("%s: cline rendering must be the shared command rendering", tg.Name)
		}
		forbidGuidance(t, text, "{{", "<no value>", "—", "–")
	}
	before := writeNativeCommands(t, ts, Render)
	reconnectNativeCommands(t, ts, Render, before)

	// A foreign SKILL.md is refused, never overwritten.
	foreign := filepath.Join(home, ".cline", "skills", CommandInbox, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("---\nname: mine\ndescription: mine\n---\n\nmy own skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteSkill(foreign, Render(ts[3])); err == nil {
		t.Fatal("WriteSkill must refuse a foreign skill file")
	}
	if raw, _ := os.ReadFile(foreign); string(raw) != "---\nname: mine\ndescription: mine\n---\n\nmy own skill\n" {
		t.Fatal("foreign skill file was modified")
	}

	// Workspace root is .cline/skills, relative to the project.
	ts, err = ClineCommandSkillTargets(true, home)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(".cline", "skills", "punk-join", "SKILL.md"); ts[0].Path != want {
		t.Fatalf("project path = %s, want %s", ts[0].Path, want)
	}
}

// TestAntigravityCommandSkillsCurrentNativeLocations pins where the
// five punk commands land for Antigravity (P2-C). Antigravity's legacy
// "workflows" files (.agents/workflows/<name>.md,
// ~/.gemini/config/workflows/<name>.md) stopped being indexed or
// slash-executable on October 19, 2026 (antigravity.google/docs/ide/workflows
// and /docs/migration/workflows-to-skills, fetched 2026-10-27): Agent
// Skills are the current native format, keep the same /<name> slash
// invocation, and take precedence over same-named workflows. punk
// installs the commands as Antigravity skills - the behavior P2-A
// already ships through CommandSkillTargets - and this test proves the
// locations and reconnect idempotency so no future change silently
// drops the command surface or revives the dead workflow format.
func TestAntigravityCommandSkillsCurrentNativeLocations(t *testing.T) {
	home := t.TempDir()
	// Global root: ~/.gemini/config/skills.
	ts, err := CommandSkillTargets("antigravity", false, home, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != len(CommandNames) {
		t.Fatalf("antigravity global: %d targets, want %d", len(ts), len(CommandNames))
	}
	for i, name := range CommandNames {
		if want := filepath.Join(home, ".gemini", "config", "skills", name, "SKILL.md"); ts[i].Path != want {
			t.Fatalf("antigravity global: %s at %s, want %s", name, ts[i].Path, want)
		}
	}
	before := writeNativeCommands(t, ts, Render)
	reconnectNativeCommands(t, ts, Render, before)

	// Workspace root (.agents/skills) is cwd-relative like every other
	// --project path: isolate the working directory before writing.
	t.Chdir(t.TempDir())
	ts, err = CommandSkillTargets("antigravity", true, home, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range CommandNames {
		if want := filepath.Join(".agents", "skills", name, "SKILL.md"); ts[i].Path != want {
			t.Fatalf("antigravity project: %s at %s, want %s", name, ts[i].Path, want)
		}
	}
	before = writeNativeCommands(t, ts, Render)
	reconnectNativeCommands(t, ts, Render, before)
}
