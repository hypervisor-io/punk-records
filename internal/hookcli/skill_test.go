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
			[]string{"name: punk-memory", "description:", SkillMarker, "`mcp__punk__whoami`", "`mcp__punk__unified_search`", "format: compact", "/tasks/<id>/status", "claim_work", "punk://memory/", "Never invent a namespace or a key", "`mcp__punk__list_tasks`", "`mcp__punk__await_tasks`", "`mcp__punk__set_task_status`", "timeout_seconds=45", "never extends that deadline", "retry shorter"},
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
	for _, w := range []string{"recall", "search", "unified_search", "compact", "remember"} {
		if !strings.Contains(r, w) {
			t.Fatalf("routing section missing %q", w)
		}
	}
	if strings.Contains(r, "\n#") {
		t.Fatal("routing section must be plain prose without markdown headers")
	}
	// C07 trimmed the routing section to the routing decisions; the guidance
	// it no longer repeats must still be discoverable in the rendered skill,
	// in the sections that own it (Namespaces, Reading memory, Writing memory).
	for _, w := range []string{"Never invent a namespace or a key", "list_keys", "remember_many", "remember_document", "feedback"} {
		if !strings.Contains(got, w) {
			t.Fatalf("rendered skill lost guidance %q that the trimmed routing section no longer carries", w)
		}
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

// estTokens estimates model tokens as bytes/4, rounded up. It mirrors
// mcpserver's estimator in guidance_budget_test.go exactly: the C07 budget
// compares the initialize instructions, the tools/list payload and the
// installed skill text with one estimator, so the three measurements stay
// comparable and the two helpers must not drift.
func estTokens(s string) int { return (len(s) + 3) / 4 }

// widestSkillOpts is the render the skill budget pins: claude-code carries
// the longest tool prefix (mcp__punk__) and a server URL, so it is the
// largest punk-memory text any agent installs.
var widestSkillOpts = SkillOpts{Agent: "claude-code", ToolPrefix: "mcp__punk__", ServerURL: "http://localhost:9090"}

// TestSkillTextPayloadSnapshot measures the installed skill text separately
// from the server payloads (mcpserver's TestInstructionsPayloadSnapshot
// reports the other two pieces): the skill is a file on disk that hosts load
// on demand, so its size is paid per load, not per session. The duplication
// counts are the C07 evidence: the routing section is embedded verbatim once
// per render, and the compact-hit etiquette sentence used to appear twice
// inside one render (routing section + Etiquette).
func TestSkillTextPayloadSnapshot(t *testing.T) {
	for _, o := range []SkillOpts{
		widestSkillOpts,
		{Agent: "codex"},
		{Agent: "opencode", ToolPrefix: "punk_"},
		{Agent: "pi", ToolPrefix: "punk_", Pi: true},
		{Agent: "hermes", Hermes: true},
	} {
		s := RenderSkill(o)
		t.Logf("punk-memory %-12s %5d bytes ~%4d tokens (routing section x%d, compact-hit etiquette x%d)",
			o.Agent, len(s), estTokens(s),
			strings.Count(s, RoutingSection()),
			strings.Count(s, "already-read evidence"))
	}
	p := RenderPlanSkill(widestSkillOpts)
	t.Logf("punk-plan   %-12s %5d bytes ~%4d tokens", widestSkillOpts.Agent, len(p), estTokens(p))
	t.Logf("shared routing section: %d bytes ~%d tokens", len(RoutingSection()), estTokens(RoutingSection()))
}

// Bounds derived from the measured baseline at 08b0afe (the log of
// TestSkillTextPayloadSnapshot at that commit):
//
//	shared routing section:      1477 bytes, ~370 tokens
//	punk-memory claude-code:     7322 bytes, ~1831 tokens
//
// C07 removed 598 bytes (~150 tokens) of routing prose verified redundant
// with the MCP tools/list descriptions and input schemas (which every
// MCP-agent skill render is installed beside), plus the 107-byte (~27
// token) Etiquette duplicate of the routing section's compact-hit line.
// pi keeps its own routingBodyPi and is only touched by the Etiquette dedup.
const (
	// routingSectionBudgetTokens = 370 baseline - 150 verified redundancy
	// + 25 slack. Red below the change (370), green above it (~220).
	routingSectionBudgetTokens = 245
	// skillBodyBudgetTokens = 1831 baseline - 150 routing - 27 etiquette
	// + 55 slack. Red below the change (1831), green above it (~1655).
	skillBodyBudgetTokens = 1710
)

// TestSkillTextWithinBudget bounds the installed skill text with the same
// estimator the server payloads use. The bounds sit strictly below the
// 08b0afe baseline (red before the C07 dedup) and strictly above the
// trimmed sizes (green after), so the skill cannot silently regrow the
// prose the tools/list payload already carries.
func TestSkillTextWithinBudget(t *testing.T) {
	if got := estTokens(RoutingSection()); got > routingSectionBudgetTokens {
		t.Errorf("routing section = ~%d tokens, budget %d (baseline at 08b0afe was ~370)", got, routingSectionBudgetTokens)
	}
	if got := estTokens(RenderSkill(widestSkillOpts)); got > skillBodyBudgetTokens {
		t.Errorf("punk-memory claude-code = ~%d tokens, budget %d (baseline at 08b0afe was ~1831)", got, skillBodyBudgetTokens)
	}
	// The dedup, not a rewrite: the routing section stays embedded exactly
	// once per render, and the compact-hit etiquette exists exactly once
	// (inside the routing section) instead of once per section.
	s := RenderSkill(widestSkillOpts)
	if n := strings.Count(s, RoutingSection()); n != 1 {
		t.Errorf("routing section occurs %d times in the render, want exactly 1", n)
	}
	if n := strings.Count(s, "already-read evidence"); n != 1 {
		t.Errorf("compact-hit etiquette occurs %d times in the render, want exactly 1 (the routing section's copy)", n)
	}
}
