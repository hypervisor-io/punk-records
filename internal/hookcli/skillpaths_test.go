package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillTargetsTable(t *testing.T) {
	env := func(k string) string {
		if k == "CODEX_HOME" {
			return "/ch"
		}
		return ""
	}
	cases := map[string]string{
		"claude-code": "/home/u/.claude/skills/punk-memory/SKILL.md",
		"codex":       "/ch/skills/punk-memory/SKILL.md",
		"opencode":    "/home/u/.agents/skills/punk-memory/SKILL.md",
		"cursor":      "/home/u/.agents/skills/punk-memory/SKILL.md",
		"copilot":     "/home/u/.agents/skills/punk-memory/SKILL.md",
		"antigravity": "/home/u/.gemini/config/skills/punk-memory/SKILL.md",
		"hermes":      "/home/u/.hermes/skills/memory/punk-memory/SKILL.md",
		"openclaw":    "/home/u/.agents/skills/punk-memory/SKILL.md",
		"pi":          "/home/u/.pi/agent/skills/punk-memory/SKILL.md",
	}
	for agent, want := range cases {
		ts, err := SkillTargets(agent, false, "/home/u", env)
		if err != nil || len(ts) != 1 || ts[0].Path != want {
			t.Fatalf("%s global: %+v %v", agent, ts, err)
		}
		if ts[0].Opts.Agent != agent {
			t.Fatalf("%s: opts.Agent = %q", agent, ts[0].Opts.Agent)
		}
	}
	if ts, _ := SkillTargets("claude-code", true, "/home/u", env); ts[0].Path != filepath.Join(".claude", "skills", "punk-memory", "SKILL.md") {
		t.Fatalf("claude project: %s", ts[0].Path)
	}
	if ts, _ := SkillTargets("codex", true, "/home/u", env); ts[0].Path != filepath.Join(".agents", "skills", "punk-memory", "SKILL.md") {
		t.Fatalf("codex project: %s", ts[0].Path)
	}
	if ts, _ := SkillTargets("pi", false, "/home/u", env); ts[0].Opts.ToolPrefix != "punk_" || !ts[0].Opts.Pi {
		t.Fatalf("pi opts: %+v", ts[0].Opts)
	}
	if ts, _ := SkillTargets("claude-code", false, "/home/u", env); ts[0].Opts.ToolPrefix != "mcp__punk__" {
		t.Fatalf("claude prefix: %+v", ts[0].Opts)
	}
	if ts, _ := SkillTargets("hermes", false, "/home/u", env); !ts[0].Opts.Hermes {
		t.Fatal("hermes opts")
	}
	if _, err := SkillTargets("emacs", false, "/home/u", env); err == nil {
		t.Fatal("unknown agent must error")
	}
}

func TestWriteSkillMarkerGated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "skills", "punk-memory", "SKILL.md")
	content := RenderSkill(SkillOpts{Agent: "cursor"})
	changed, err := WriteSkill(p, content)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, _ := WriteSkill(p, content); changed {
		t.Fatal("idempotent")
	}
	if err := os.WriteFile(p, []byte("---\nname: punk-memory\ndescription: mine\n---\nhand written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteSkill(p, content); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("hand-written file must be refused with the path in the error: %v", err)
	}
}
