package hookcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// commandTestEnv is the SkillTargets env stub used by the command golden
// tests: CODEX_HOME pinned so the Codex path is deterministic.
func commandTestEnv(k string) string {
	if k == "CODEX_HOME" {
		return "/ch"
	}
	return ""
}

// prefixOf returns the bytes of s up to n, for failure messages that must
// not slice past a short string.
func prefixOf(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// TestCommandSkillTargetsPerClientPath is the golden location test: every
// client that loads punk skills gets all five command skills as a sibling
// folder of punk-memory/punk-plan, and pi gets none (its four HTTP tools
// cannot run them).
func TestCommandSkillTargetsPerClientPath(t *testing.T) {
	global := map[string]string{
		"claude-code": "/home/u/.claude/skills",
		"codex":       "/ch/skills",
		"opencode":    "/home/u/.agents/skills",
		"cursor":      "/home/u/.agents/skills",
		"copilot":     "/home/u/.agents/skills",
		"antigravity": "/home/u/.gemini/config/skills",
		"hermes":      "/home/u/.hermes/skills/memory",
		"openclaw":    "/home/u/.agents/skills",
	}
	for agent, dir := range global {
		ts, err := CommandSkillTargets(agent, false, "/home/u", commandTestEnv)
		if err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
		if len(ts) != len(CommandNames) {
			t.Fatalf("%s: %d command targets, want %d: %+v", agent, len(ts), len(CommandNames), ts)
		}
		for i, name := range CommandNames {
			tg := ts[i]
			want := filepath.Join(dir, name, "SKILL.md")
			if tg.Name != name || tg.Path != want {
				t.Fatalf("%s: target %d = %s at %s, want %s at %s", agent, i, tg.Name, tg.Path, name, want)
			}
			if tg.Opts.Agent != agent || tg.Opts.Command != name {
				t.Fatalf("%s/%s: opts = %+v", agent, name, tg.Opts)
			}
		}
	}

	// Project-local locations, mirroring SkillTargets.
	project := map[string]string{
		"claude-code": filepath.Join(".claude", "skills"),
		"codex":       filepath.Join(".agents", "skills"),
		"cursor":      filepath.Join(".agents", "skills"),
		"copilot":     filepath.Join(".agents", "skills"),
		"opencode":    filepath.Join(".agents", "skills"),
		"antigravity": filepath.Join(".agents", "skills"),
		"openclaw":    filepath.Join(".agents", "skills"),
	}
	for agent, dir := range project {
		ts, err := CommandSkillTargets(agent, true, "/home/u", commandTestEnv)
		if err != nil || len(ts) != len(CommandNames) {
			t.Fatalf("%s project: %+v %v", agent, ts, err)
		}
		for i, name := range CommandNames {
			if want := filepath.Join(dir, name, "SKILL.md"); ts[i].Path != want {
				t.Fatalf("%s project: %s at %s, want %s", agent, name, ts[i].Path, want)
			}
		}
	}
	// Hermes documents no project location: its commands stay global.
	if ts, _ := CommandSkillTargets("hermes", true, "/home/u", commandTestEnv); ts[0].Path != "/home/u/.hermes/skills/memory/punk-join/SKILL.md" {
		t.Fatalf("hermes project: %s", ts[0].Path)
	}

	if ts, err := CommandSkillTargets("pi", false, "/home/u", commandTestEnv); err != nil || len(ts) != 0 {
		t.Fatalf("pi must have no command targets: %+v %v", ts, err)
	}
	if _, err := CommandSkillTargets("emacs", false, "/home/u", commandTestEnv); err == nil {
		t.Fatal("unknown agent must error")
	}
}

// TestCommandFrontmatterValid parses the generated frontmatter of every
// command for every client with KnownFields, exactly as the skill guidance
// test does for punk-memory/punk-plan.
func TestCommandFrontmatterValid(t *testing.T) {
	for _, agent := range []string{"claude-code", "codex", "opencode", "cursor", "copilot", "antigravity", "hermes", "openclaw"} {
		ts, err := CommandSkillTargets(agent, false, "/home/u", commandTestEnv)
		if err != nil {
			t.Fatal(err)
		}
		for _, tg := range ts {
			t.Run(agent+"/"+tg.Name, func(t *testing.T) {
				text := Render(tg)
				if !strings.HasPrefix(text, "---\nname: "+tg.Name+"\n") {
					t.Fatalf("frontmatter must start with the command name: %q", prefixOf(text, 80))
				}
				parts := strings.SplitN(text, "---\n", 3)
				if len(parts) != 3 || parts[0] != "" {
					t.Fatal("missing YAML frontmatter boundaries")
				}
				var front struct {
					Name        string         `yaml:"name"`
					Description string         `yaml:"description"`
					Version     string         `yaml:"version"`
					Metadata    map[string]any `yaml:"metadata"`
				}
				decoder := yaml.NewDecoder(strings.NewReader(parts[1]))
				decoder.KnownFields(true)
				if err := decoder.Decode(&front); err != nil {
					t.Fatalf("generated frontmatter is not valid YAML: %v", err)
				}
				if front.Name != tg.Name || front.Description == "" || len(front.Description) > 1024 || strings.Contains(front.Description, "\n") {
					t.Fatalf("invalid command metadata: %+v", front)
				}
				if tg.Opts.Hermes {
					if front.Version != "1.0.0" || front.Metadata["hermes"] == nil {
						t.Fatalf("missing Hermes metadata: %+v", front)
					}
				} else if front.Version != "" || len(front.Metadata) != 0 {
					t.Fatalf("Hermes metadata leaked to %s", agent)
				}
				if !strings.HasPrefix(parts[2], SkillMarker+"\n") || !IsManagedSkill([]byte(text)) || strings.Count(text, SkillMarker) != 1 {
					t.Fatal("managed-file compatibility marker changed or duplicated")
				}
				forbidGuidance(t, text, "{{", "<no value>", "—", "–")
			})
		}
	}
}

// TestCommandBodiesCallMCPTools pins the workflow each command wraps and the
// per-client tool prefix, so a body cannot silently stop calling the tool it
// documents.
func TestCommandBodiesCallMCPTools(t *testing.T) {
	tools := map[string][]string{
		CommandJoin:        {"join"},
		CommandPeers:       {"list_region_members", "send_message"},
		CommandSend:        {"send_message"},
		CommandInbox:       {"read_messages", "ack_messages", "await_messages"},
		CommandHandoff:     {"remember", "send_message", "claim_work", "set_task_status", "release_work"},
		CommandConsolidate: {"consolidate"},
	}
	for _, agent := range []string{"claude-code", "codex", "opencode", "cursor", "copilot", "antigravity", "hermes", "openclaw"} {
		opts := SkillOpts{Agent: agent}
		switch agent {
		case "claude-code":
			opts.ToolPrefix = "mcp__punk__"
		case "opencode":
			opts.ToolPrefix = "punk_"
		case "hermes":
			opts.Hermes = true
		}
		for _, name := range CommandNames {
			opts.Command = name
			body, ok := CommandBody(name, opts)
			if !ok {
				t.Fatalf("%s: CommandBody(%s) not found", agent, name)
			}
			for _, tool := range tools[name] {
				if ref := "`" + ToolName(opts.ToolPrefix, tool) + "`"; !strings.Contains(body, ref) {
					t.Fatalf("%s/%s: body must call %s", agent, name, ref)
				}
			}
			// Every command except punk-join sends the model to punk-join
			// when the [PUNK ROUTING] block is absent, in the client-neutral
			// form every render shares.
			if name != CommandJoin {
				if want := commandRef(CommandJoin); !strings.Contains(body, want) {
					t.Fatalf("%s/%s: body must name the join command as %s", agent, name, want)
				}
			}
			if !strings.Contains(body, "[PUNK ROUTING]") {
				t.Fatalf("%s/%s: body must point at the [PUNK ROUTING] block", agent, name)
			}
			// The neutral reference names both client forms, so a client
			// still learns its own syntax from the shared text.
			if name != CommandJoin && !strings.Contains(body, "/"+CommandJoin) {
				t.Fatalf("%s/%s: body must name the slash form", agent, name)
			}
			if name != CommandJoin && !strings.Contains(body, "$"+CommandJoin+" in Codex") {
				t.Fatalf("%s/%s: body must name the Codex form", agent, name)
			}
		}
	}
	// P2-A-fix: the reference is client-neutral, not per-client.
	if got := commandRef(CommandJoin); got != "punk-join (/punk-join; $punk-join in Codex)" {
		t.Fatalf("commandRef(punk-join) = %q", got)
	}
}

// TestCommandTextSingleSharedSource proves the client files are one source:
// the body CommandBody returns (the text P2-B reuses) appears exactly once
// inside the rendered file, and two clients differ only by the tool prefix
// (the invocation text is shared, not per-client - P2-A-fix).
func TestCommandTextSingleSharedSource(t *testing.T) {
	for _, name := range CommandNames {
		o := SkillOpts{Agent: "claude-code", ToolPrefix: "mcp__punk__", Command: name}
		body, ok := CommandBody(name, o)
		if !ok {
			t.Fatalf("CommandBody(%s) not found", name)
		}
		if !strings.HasPrefix(body, "# "+name+"\n") {
			t.Fatalf("%s: body must open with its heading: %q", name, prefixOf(body, 40))
		}
		file := RenderCommand(o)
		if n := strings.Count(file, body); n != 1 {
			t.Fatalf("%s: shared body occurs %d times in the file, want 1", name, n)
		}
		if _, ok := CommandBody("no-such-command", o); ok {
			t.Fatal("unknown command must not render")
		}
		if desc, ok := CommandDescription(name); !ok || desc == "" {
			t.Fatalf("%s: CommandDescription = %q, %t", name, desc, ok)
		}
	}
	if _, ok := CommandDescription("no-such-command"); ok {
		t.Fatal("unknown command must not have a description")
	}
	// Cross-client: identical bytes once the tool prefix is normalized away,
	// so nothing else is duplicated per client.
	for _, name := range CommandNames {
		claude, _ := CommandBody(name, SkillOpts{Agent: "claude-code", ToolPrefix: "mcp__punk__", Command: name})
		codex, _ := CommandBody(name, SkillOpts{Agent: "codex", Command: name})
		norm := strings.NewReplacer("`mcp__punk__", "`")
		if norm.Replace(claude) != norm.Replace(codex) {
			t.Fatalf("%s: bodies differ by more than the tool prefix", name)
		}
	}
}

// TestCommandTargetsInstalledThroughAllSkillTargets drives the real install
// list: punk connect and punk skill install walk AllSkillTargets, so the
// command files must appear there and WriteSkill must land them on disk.
func TestCommandTargetsInstalledThroughAllSkillTargets(t *testing.T) {
	// A fresh home per agent: several clients share the ~/.agents/skills
	// root, so a reused home would make the second install a no-op.
	for _, agent := range []string{"claude-code", "codex", "cursor", "copilot", "hermes"} {
		home := t.TempDir()
		env := func(k string) string {
			if k == "CODEX_HOME" {
				return filepath.Join(home, ".codex")
			}
			return ""
		}
		all, err := AllSkillTargets(agent, false, home, env)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, tg := range all {
			if tg.Opts.Command == "" {
				continue
			}
			found++
			content := Render(tg)
			if !strings.Contains(content, "name: "+tg.Name+"\n") {
				t.Fatalf("%s: Render(%s) rendered the wrong skill", agent, tg.Name)
			}
			changed, err := WriteSkill(tg.Path, content)
			if err != nil || !changed {
				t.Fatalf("%s: install %s: %v", agent, tg.Name, err)
			}
			raw, err := os.ReadFile(tg.Path)
			if err != nil || string(raw) != content {
				t.Fatalf("%s: installed %s does not match its render: %v", agent, tg.Name, err)
			}
		}
		if found != len(CommandNames) {
			t.Fatalf("%s: %d command targets in AllSkillTargets, want %d", agent, found, len(CommandNames))
		}
	}
	// pi is the documented exception: it has no command targets.
	all, err := AllSkillTargets("pi", false, t.TempDir(), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range all {
		if tg.Opts.Command != "" {
			t.Fatalf("pi must not install command %s", tg.Name)
		}
	}
}

// TestCodexRenderMatchesSharedRootForEverySkill is the P2-A-fix regression:
// Codex discovers both CODEX_HOME/skills and the shared ~/.agents/skills
// tree, so every skill punk installs must render byte-identically for codex
// and for a shared-root client, with messaging guidance on and off.
// Otherwise ReconcileCodexSkillTargets cannot dedupe and Codex's catalog
// lists each punk skill twice.
func TestCodexRenderMatchesSharedRootForEverySkill(t *testing.T) {
	const serverURL = "http://127.0.0.1:19393"
	for _, messaging := range []bool{false, true} {
		t.Run(fmt.Sprintf("messaging=%t", messaging), func(t *testing.T) {
			home := t.TempDir()
			env := func(k string) string {
				if k == "CODEX_HOME" {
					return filepath.Join(home, ".codex")
				}
				return ""
			}
			// Populate effective options exactly as installSkillFor does,
			// before any comparison or write.
			populate := func(agent string) []SkillTarget {
				ts, err := AllSkillTargets(agent, false, home, env)
				if err != nil {
					t.Fatal(err)
				}
				for i := range ts {
					ts[i].Opts.ServerURL, ts[i].Opts.Messaging = serverURL, messaging
				}
				return ts
			}
			codex := populate("codex")
			shared := populate("cursor") // cursor renders the shared-root bytes
			if len(codex) != 2+len(CommandNames) || len(shared) != len(codex) {
				t.Fatalf("codex=%d shared=%d skills, want %d", len(codex), len(shared), 2+len(CommandNames))
			}
			byName := make(map[string]SkillTarget, len(shared))
			for _, tg := range shared {
				byName[tg.Name] = tg
				writeSkillFile(t, filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md"), Render(tg))
			}
			for _, tg := range codex {
				sh, ok := byName[tg.Name]
				if !ok {
					t.Fatalf("codex skill %s has no shared counterpart", tg.Name)
				}
				if got, want := Render(tg), Render(sh); got != want {
					t.Fatalf("codex %s render differs from the shared root", tg.Name)
				}
			}
			// End to end: with the shared copies installed, every codex
			// target is redundant and the reconcile keeps none.
			kept, notes := ReconcileCodexSkillTargets(codex, home, env)
			if len(kept) != 0 {
				t.Fatalf("codex must dedupe against the shared root, kept=%+v notes=%v", kept, notes)
			}
		})
	}
}
