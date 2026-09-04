package hookcli

import (
	"strings"
	"testing"
)

func TestRenderSkillPerAgent(t *testing.T) {
	cases := []struct {
		opts SkillOpts
		want []string
		not  []string
	}{
		{SkillOpts{Agent: "claude-code", ToolPrefix: "mcp__punk__", ServerURL: "http://localhost:9090"},
			[]string{"name: punk-memory", "description:", SkillMarker, "`mcp__punk__whoami`", "`mcp__punk__unified_search`", "format: compact", "/tasks/<id>/status", "claim_work", "punk://memory/", "Never invent a key", "`mcp__punk__list_tasks`", "`mcp__punk__await_tasks`", "`mcp__punk__set_task_status`", "timeout_seconds"},
			[]string{"version:", "metadata:"}},
		{SkillOpts{Agent: "opencode", ToolPrefix: "punk_"},
			[]string{"`punk_search`", "`punk_remember_many`"}, nil},
		{SkillOpts{Agent: "pi", ToolPrefix: "punk_", Pi: true},
			[]string{"`punk_whoami`", "`punk_recall`", "`punk_search`", "`punk_remember`", "HTTP API", "/tasks?wait=", "/status"},
			[]string{"`punk_unified_search`", "`punk_claim_work`", "unified_search", "remember_many", "remember_document", "feedback", "list_keys", "triplet_search", "prefixed ``"}},
		{SkillOpts{Agent: "hermes", Hermes: true},
			[]string{"version: 1.0.0", "metadata:", "hermes:", "category: memory", "`recall`", "plain names"}, []string{"prefixed ``"}},
		{SkillOpts{Agent: "codex", Namespace: "agent-billing-1a2b3c"},
			[]string{"agent-billing-1a2b3c"}, nil},
	}
	for _, c := range cases {
		got := RenderSkill(c.opts)
		if !strings.HasPrefix(got, "---\nname: punk-memory\n") {
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
	}
}

func TestSkillDescriptionLengthAndRouting(t *testing.T) {
	got := RenderSkill(SkillOpts{Agent: "cursor"})
	desc := ""
	for _, ln := range strings.Split(got, "\n") {
		if strings.HasPrefix(ln, "description: ") {
			desc = strings.TrimPrefix(ln, "description: ")
		}
	}
	if desc == "" || len(desc) > 1024 || strings.HasPrefix(desc, ">") {
		t.Fatalf("description must be a single line of at most 1024 chars, got %d", len(desc))
	}
	r := RoutingSection()
	for _, w := range []string{"recall", "search", "unified_search", "compact", "remember", "feedback", "Never invent"} {
		if !strings.Contains(r, w) {
			t.Fatalf("routing section missing %q", w)
		}
	}
	if strings.Contains(r, "\n#") {
		t.Fatal("routing section must be plain prose without markdown headers")
	}
}

func TestSkillNoLongerTellsAgentsToPoll(t *testing.T) {
	s := RenderSkill(SkillOpts{Agent: "claude", ToolPrefix: "mcp__punk__"})
	for _, banned := range []string{"Do not recall the whole", "recall only the ids you need"} {
		if strings.Contains(s, banned) {
			t.Fatalf("skill still carries the polling text %q", banned)
		}
	}
	for _, want := range []string{"list_tasks", "await_tasks", "set_task_status", "next", "in_progress"} {
		if !strings.Contains(s, want) {
			t.Fatalf("skill must mention %q", want)
		}
	}
}
