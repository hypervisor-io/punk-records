package hookcli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPlanSkillPerAgent(t *testing.T) {
	cases := []struct {
		opts SkillOpts
		want []string
		not  []string
	}{
		{SkillOpts{Agent: "claude-code", ToolPrefix: "mcp__punk__", ServerURL: "http://localhost:9090"},
			[]string{"name: punk-plan", "description:", SkillMarker, "`mcp__punk__register`", "`mcp__punk__remember_many`", "`mcp__punk__list_tasks`", "`mcp__punk__await_tasks`", "`mcp__punk__set_task_status`", "`mcp__punk__claim_work`", "/plan/summary", "/plan/current", "/tasks/_where", "/conventions/live-server", "depends_on", "/answers/<id>", "/plan/review", "detached, throwaway worktree", "Server: http://localhost:9090"},
			[]string{"version:", "metadata:", "?wait=55"}},
		{SkillOpts{Agent: "opencode", ToolPrefix: "punk_"},
			[]string{"`punk_list_tasks`", "`punk_await_tasks`"}, nil},
		{SkillOpts{Agent: "pi", ToolPrefix: "punk_", Pi: true},
			[]string{"`punk_whoami`", "`punk_remember`", "/tasks?wait=55", "/tasks/<id>/status", "HTTP API"},
			[]string{"`punk_register`", "`punk_remember_many`", "`punk_list_tasks`", "`punk_await_tasks`", "`punk_claim_work`", "`punk_set_task_status`"}},
		{SkillOpts{Agent: "hermes", Hermes: true},
			[]string{"version: 1.0.0", "metadata:", "hermes:", "category: memory", "`list_tasks`", "plain names"}, []string{"prefixed ``"}},
	}
	for _, c := range cases {
		got := RenderPlanSkill(c.opts)
		if !strings.HasPrefix(got, "---\nname: punk-plan\n") {
			t.Fatalf("%s: frontmatter must start with name: %q", c.opts.Agent, got[:60])
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s: missing %q", c.opts.Agent, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Fatalf("%s: must not contain %q", c.opts.Agent, n)
			}
		}
		if strings.Contains(got, "—") || strings.Contains(got, "–") {
			t.Fatalf("%s: dash characters are not allowed", c.opts.Agent)
		}
		if strings.Contains(got, "{{") || strings.Contains(got, "<no value>") {
			t.Fatalf("%s: template leaked: %q", c.opts.Agent, got)
		}
	}
	desc := strings.SplitN(RenderPlanSkill(SkillOpts{Agent: "codex"}), "\n", 3)[2]
	if !strings.HasPrefix(desc, "description: ") || len(planDescription) > 1024 {
		t.Fatalf("description line malformed or over 1024 chars (%d)", len(planDescription))
	}
}

func TestPlanSkillTargetsSitBesideMemory(t *testing.T) {
	env := func(string) string { return "" }
	for _, agent := range []string{"claude-code", "codex", "opencode", "cursor", "copilot", "antigravity", "hermes", "openclaw", "pi"} {
		for _, project := range []bool{false, true} {
			if agent == "hermes" && project {
				continue
			}
			mem, err := SkillTargets(agent, project, "/home/u", env)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := PlanSkillTargets(agent, project, "/home/u", env)
			if err != nil || len(plan) != len(mem) {
				t.Fatalf("%s project=%v: plan targets %+v %v", agent, project, plan, err)
			}
			for i := range mem {
				wantDir := filepath.Dir(filepath.Dir(mem[i].Path))
				if plan[i].Path != filepath.Join(wantDir, "punk-plan", "SKILL.md") || plan[i].Name != PlanSkillName || mem[i].Name != SkillName {
					t.Fatalf("%s project=%v: mem %s plan %s names %q %q", agent, project, mem[i].Path, plan[i].Path, mem[i].Name, plan[i].Name)
				}
				if plan[i].Opts != mem[i].Opts {
					t.Fatalf("%s: plan opts %+v differ from memory opts %+v", agent, plan[i].Opts, mem[i].Opts)
				}
			}
			all, err := AllSkillTargets(agent, project, "/home/u", env)
			if err != nil || len(all) != 2*len(mem) || all[0].Name != SkillName || all[len(mem)].Name != PlanSkillName {
				t.Fatalf("%s: all targets %+v %v", agent, all, err)
			}
			for _, tg := range all {
				if !strings.Contains(Render(tg), "name: "+tg.Name+"\n") {
					t.Fatalf("%s: Render(%s) rendered the wrong skill", agent, tg.Name)
				}
			}
		}
	}
	if _, err := AllSkillTargets("emacs", false, "/home/u", env); err == nil {
		t.Fatal("unknown agent must error")
	}
}
