package hookcli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkillTarget is one SKILL.md punk writes for an agent. Name is the
// skill it carries (SkillName or PlanSkillName); Render picks the text.
type SkillTarget struct {
	Name string
	Path string
	Opts SkillOpts
}

func skillFile(dir string) string { return filepath.Join(dir, SkillName, "SKILL.md") }

// PlanSkillTargets mirrors SkillTargets for the punk-plan skill: the same
// per-agent skill directory, a sibling folder named punk-plan.
func PlanSkillTargets(agent string, project bool, home string, env func(string) string) ([]SkillTarget, error) {
	base, err := SkillTargets(agent, project, home, env)
	if err != nil {
		return nil, err
	}
	out := make([]SkillTarget, 0, len(base))
	for _, tg := range base {
		dir := filepath.Dir(filepath.Dir(tg.Path))
		out = append(out, SkillTarget{Name: PlanSkillName, Path: filepath.Join(dir, PlanSkillName, "SKILL.md"), Opts: tg.Opts})
	}
	return out, nil
}

// AllSkillTargets is every skill punk installs for an agent: punk-memory
// then punk-plan. punk connect and punk skill install walk this list.
func AllSkillTargets(agent string, project bool, home string, env func(string) string) ([]SkillTarget, error) {
	mem, err := SkillTargets(agent, project, home, env)
	if err != nil {
		return nil, err
	}
	plan, err := PlanSkillTargets(agent, project, home, env)
	if err != nil {
		return nil, err
	}
	return append(mem, plan...), nil
}

// SkillTargets lists where an agent loads skills from, global or project,
// with the rendering options for that agent. Paths come from each
// vendor's documentation; agents that read the shared ~/.agents/skills
// location get that single file.
func SkillTargets(agent string, project bool, home string, env func(string) string) ([]SkillTarget, error) {
	if env == nil {
		env = os.Getenv
	}
	agents := filepath.Join(home, ".agents", "skills")
	projAgents := filepath.Join(".agents", "skills")
	one := func(p string, o SkillOpts) []SkillTarget {
		o.Agent = agent
		return []SkillTarget{{Name: SkillName, Path: skillFile(p), Opts: o}}
	}
	switch agent {
	case "claude-code":
		o := SkillOpts{ToolPrefix: "mcp__punk__"}
		if project {
			return one(filepath.Join(".claude", "skills"), o), nil
		}
		return one(filepath.Join(home, ".claude", "skills"), o), nil
	case "codex":
		if project {
			return one(projAgents, SkillOpts{}), nil
		}
		ch := env("CODEX_HOME")
		if ch == "" {
			ch = filepath.Join(home, ".codex")
		}
		return one(filepath.Join(ch, "skills"), SkillOpts{}), nil
	case "opencode":
		if project {
			return one(projAgents, SkillOpts{ToolPrefix: "punk_"}), nil
		}
		return one(agents, SkillOpts{ToolPrefix: "punk_"}), nil
	case "cursor", "copilot", "openclaw":
		if project {
			return one(projAgents, SkillOpts{}), nil
		}
		return one(agents, SkillOpts{}), nil
	case "antigravity":
		if project {
			return one(projAgents, SkillOpts{}), nil
		}
		return one(filepath.Join(home, ".gemini", "config", "skills"), SkillOpts{}), nil
	case "hermes":
		// Hermes documents no project location; the global category tree is the only target.
		return []SkillTarget{{Name: SkillName, Path: filepath.Join(home, ".hermes", "skills", "memory", SkillName, "SKILL.md"), Opts: SkillOpts{Agent: agent, Hermes: true}}}, nil
	case "pi":
		if project {
			return one(projAgents, SkillOpts{ToolPrefix: "punk_", Pi: true}), nil
		}
		return one(filepath.Join(home, ".pi", "agent", "skills"), SkillOpts{ToolPrefix: "punk_", Pi: true}), nil
	}
	return nil, fmt.Errorf("no skill location known for agent %q", agent)
}

// WriteSkill writes content to path unless a file punk did not write is
// already there. Parent directories are created.
func WriteSkill(path, content string) (changed bool, err error) {
	existing, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if !strings.Contains(string(existing), SkillMarker) {
			return false, fmt.Errorf("%s exists and is not managed by punk (missing marker); leaving it alone", path)
		}
		if string(existing) == content {
			return false, nil
		}
	case errors.Is(readErr, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
	default:
		return false, readErr
	}
	if err := writePreservingSymlinkAndMode(path, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
