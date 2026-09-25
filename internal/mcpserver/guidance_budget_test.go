package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// The C07 guidance budget measures, separately, the three pieces of memory
// guidance punk puts in front of a model:
//
//  1. the initialize instructions (Instructions, served once per session in
//     the initialize result),
//  2. the tools/list entries (each tool's wire JSON: name, description and
//     inferred schemas, served once per session), and
//  3. the installed skill text (hookcli.RenderSkill's output, a file the host
//     loads on demand; measured in internal/hookcli/skill_test.go).
//
// Every number goes through estTokens, the same deterministic estimator
// before and after any content change, and every byte is taken from the real
// server constructors over a real in-memory MCP transport, never a mock, so
// the snapshots measure what a connected host actually receives.

// estTokens estimates model tokens as bytes/4, rounded up. Deliberately
// crude but deterministic: the budget tests compare the same payload family
// against itself across commits, so a stable estimator matters more than an
// accurate one. internal/hookcli's skill budget test uses the same formula;
// the two must not drift.
func estTokens(s string) int { return (len(s) + 3) / 4 }

// toolWireJSON renders one tools/list entry as a host receives it on the
// wire: name, description and inferred input/output schemas. This is the
// conservative measure of what a host can inject into context per tool;
// the description alone is reported beside it in the snapshot.
func toolWireJSON(t *testing.T, tool *mcp.Tool) string {
	t.Helper()
	raw, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal tool %s: %v", tool.Name, err)
	}
	return string(raw)
}

// guidanceProbe is one measured session-open payload: the initialize
// instructions plus the tools/list the session actually exposes.
type guidanceProbe struct {
	instructions string
	tools        []*mcp.Tool
	wire         string // toolWireJSON of every tool, one line each
}

// probeSession connects a real client to a real New(deps) server through the
// package's sessionOpts helper and captures what the host receives at
// session open: InitializeResult().Instructions and ListTools.
func probeSession(t *testing.T, tweaks ...func(*Deps)) guidanceProbe {
	t.Helper()
	cs := sessionOpts(t, nil, tweaks...)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := guidanceProbe{instructions: cs.InitializeResult().Instructions, tools: res.Tools}
	var sb strings.Builder
	for _, tool := range res.Tools {
		sb.WriteString(toolWireJSON(t, tool))
		sb.WriteByte('\n')
	}
	p.wire = sb.String()
	return p
}

// skillTexts are the installed-skill renders whose size the C07 accounting
// reports next to the server payloads: the widest MCP-agent render
// (claude-code, prefixed tools plus server URL), a plain-name render, and
// the pi render whose routing prose is separate (routingBodyPi).
func skillTexts() []struct {
	name string
	text string
} {
	claude := hookcli.SkillOpts{Agent: "claude-code", ToolPrefix: "mcp__punk__", ServerURL: "http://localhost:9090"}
	return []struct {
		name string
		text string
	}{
		{"punk-memory claude-code", hookcli.RenderSkill(claude)},
		{"punk-memory codex", hookcli.RenderSkill(hookcli.SkillOpts{Agent: "codex"})},
		{"punk-memory opencode", hookcli.RenderSkill(hookcli.SkillOpts{Agent: "opencode", ToolPrefix: "punk_"})},
		{"punk-memory pi", hookcli.RenderSkill(hookcli.SkillOpts{Agent: "pi", ToolPrefix: "punk_", Pi: true})},
		{"punk-plan claude-code", hookcli.RenderPlanSkill(claude)},
	}
}

// TestInstructionsPayloadSnapshot reports the guidance payload sizes with no
// bounds, so the exact numbers stay visible even while
// TestInstructionsPayloadWithinBudget is red. The bounds test pins the
// contract; this one is the measuring tape: run it before and after any
// guidance change and diff the log.
func TestInstructionsPayloadSnapshot(t *testing.T) {
	full := probeSession(t)
	agent := probeSession(t, func(d *Deps) { d.Toolset = "agent" })

	if agent.instructions != full.instructions {
		t.Fatalf("instructions differ between toolsets; they are one string served to both")
	}
	t.Logf("initialize instructions: %d bytes, ~%d tokens (served once per session)",
		len(full.instructions), estTokens(full.instructions))
	t.Logf("full tools/list: %d tools, %d bytes, ~%d tokens",
		len(full.tools), len(full.wire), estTokens(full.wire))
	t.Logf("agent tools/list: %d tools, %d bytes, ~%d tokens",
		len(agent.tools), len(agent.wire), estTokens(agent.wire))
	for _, tool := range agent.tools {
		raw := toolWireJSON(t, tool)
		t.Logf("  agent tool %-18s wire %5d bytes ~%4d tokens (description %d bytes)",
			tool.Name, len(raw), estTokens(raw), len(tool.Description))
	}
	t.Logf("session open, agent toolset: instructions + tools/list = %d bytes, ~%d tokens",
		len(agent.instructions)+len(agent.wire), estTokens(agent.instructions)+estTokens(agent.wire))

	for _, s := range skillTexts() {
		t.Logf("installed skill %-28s %5d bytes ~%4d tokens (loaded on demand)",
			s.name, len(s.text), estTokens(s.text))
	}
	t.Logf("shared routing section: %d bytes, ~%d tokens",
		len(hookcli.RoutingSection()), estTokens(hookcli.RoutingSection()))

	// Duplication accounting: the routing section is embedded verbatim in
	// both the instructions and every MCP-agent skill render, so a session
	// that connects the server AND loads the skill carries it twice. The
	// compact-hit etiquette sentence additionally repeats inside the skill
	// itself (routing section + Etiquette), which the snapshot counts so
	// the redundancy is measured, not assumed.
	claude := hookcli.RenderSkill(hookcli.SkillOpts{Agent: "claude-code", ToolPrefix: "mcp__punk__", ServerURL: "http://localhost:9090"})
	t.Logf("RoutingSection occurrences: instructions=%d skill=%d",
		strings.Count(full.instructions, hookcli.RoutingSection()), strings.Count(claude, hookcli.RoutingSection()))
	t.Logf("compact-hit etiquette occurrences: instructions=%d skill=%d",
		strings.Count(full.instructions, "A compact hit is already-read evidence"),
		strings.Count(claude, "compact hit as already-read evidence")+strings.Count(claude, "A compact hit is already-read evidence"))
}

// TestInstructionsNotRepeatedPerTool pins the accounting premise C07 rests
// on: the server serves Instructions exactly once, in the initialize result,
// and tools/list entries carry only their own name, description and schemas.
// This was verified against go-sdk v1.7.0-pre.1 (InitializeResult takes
// ServerOptions.Instructions; listTools returns the registered *Tool values
// verbatim) and is asserted here on real responses for both toolsets. The
// consequence for the budget: instructions cost once per session, not once
// per tool. If a future change starts embedding the instructions (or the
// shared routing section) into tool entries, this test fails before the
// payload silently grows by instructions x tool count. Repetition a user
// sees in a host's UI is host-side rendering, a different payload than the
// one measured here.
func TestInstructionsNotRepeatedPerTool(t *testing.T) {
	full := probeSession(t)
	agent := probeSession(t, func(d *Deps) { d.Toolset = "agent" })

	if full.instructions != Instructions {
		t.Fatalf("initialize result does not carry Instructions verbatim")
	}
	if n := strings.Count(Instructions, hookcli.RoutingSection()); n != 1 {
		t.Fatalf("Instructions embed the routing section %d times, want exactly 1", n)
	}
	fragments := []string{Instructions, hookcli.RoutingSection(), "Punk Records is the shared memory plane"}
	for _, probe := range []guidanceProbe{full, agent} {
		for _, tool := range probe.tools {
			raw := toolWireJSON(t, tool)
			for _, frag := range fragments {
				if strings.Contains(raw, frag) {
					t.Fatalf("tool %s embeds server-level guidance (%d bytes of it); tools/list entries must stay self-contained", tool.Name, len(frag))
				}
			}
		}
	}
}

// Bounds derived from the measured baseline at 08b0afe (the log of
// TestInstructionsPayloadSnapshot at that commit):
//
//	initialize instructions:   2130 bytes, ~533 tokens
//	agent tools/list wire:    22055 bytes, ~5514 tokens (16 tools)
//	session open (agent):     24185 bytes, ~6047 tokens
//
// C07 removed 598 bytes (~150 tokens) of routing prose from the shared
// routing section after verifying each sentence is carried near-verbatim by
// the tools/list payload or runtime responses: recall's budget sentence (the
// max_tokens schema of recall/search/recall_as_of plus the truncation note),
// recall's list_keys enumeration advice and the list_keys line (list_keys'
// own description), search's anchors and format sentences (search's anchors
// and format schema descriptions), the recall_as_of line (its description),
// and the write-tool selection clauses (remember_many/remember_document/
// feedback descriptions). The tool entries themselves are untouched, so
// their bound is a ratchet, not a cut.
//
// Re-measured for S01 round 2 (search_skills/load_skill admitted to the
// agent toolset): agent tools/list wire is now 24380 bytes, ~6095 tokens
// (18 tools). The tool entries were otherwise untouched.
//
// Re-measured for R01 (strategy field on search/unified-search plus the
// routed-meta output and UnifiedHit's skill variant): agent tools/list
// wire is now 25707 bytes, ~6427 tokens. The 1327-byte growth is the
// inspectable-routing surface itself; the routed hits deliberately ride
// the existing compact-hit schema instead of a new RouteResult subtree,
// which is what keeps the growth to the meta block.
//
// M2 messaging re-measurement (2026-09-25): 23 agent tools, 30255 bytes,
// ~7564 tokens, up 4548 bytes / ~1137 tokens from 25707 / ~6427. Added
// wire entries: send_message 1173, read_messages 974, ack_messages 619,
// await_messages 1073, list_region_members 704 bytes, plus five newlines.
// Existing lean entries and initialize instructions are unchanged.
const (
	// instructionsBudgetTokens = 533 baseline - 150 verified redundancy
	// + 37 slack (~10% of the trimmed size). Red below the change (533),
	// green above it (~383).
	instructionsBudgetTokens = 420
	// agentToolsetBudgetTokens: 5514 baseline + 581 measured for the S01
	// round-2 admission of search_skills (1251 bytes, ~313 tokens) and
	// load_skill (1072 bytes, ~268 tokens) to the lean toolset - the
	// deliberate, measured client path for procedural discovery - + 16
	// slack. The re-measurement is the budgeted ratchet step the task
	// sanctions; this still forbids any further growth of the per-tool
	// payload through this package.
	// R01 ratchet step: 6095 + 332 measured for the strategy field on
	// search/unified_search, the routed-meta output schema and
	// UnifiedHit's skill variant (see the re-measurement note above)
	// + 16 slack.
	// M11 restores the pre-messaging default; opt-in costs have a separate
	// measured bound below and cannot buy slack for any existing tool.
	agentToolsetBudgetTokens = 6443
	// sessionOpenBudgetTokens is the two parts summed: what a host pays
	// per session for punk's server-owned guidance with the lean toolset.
	sessionOpenBudgetTokens = instructionsBudgetTokens + agentToolsetBudgetTokens
)

// TestInstructionsPayloadWithinBudget is the enforcement half of the C07
// contract: the server-owned guidance a host pays for every session - the
// initialize instructions plus the agent tools/list - stays under bounds
// derived from the measured baseline. The bounds sit strictly below the
// 08b0afe baseline (so the test was red before the trim) and strictly above
// the trimmed sizes (so it is green after), which is what makes it a bound
// rather than a snapshot.
func TestInstructionsPayloadWithinBudget(t *testing.T) {
	full := probeSession(t)
	agent := probeSession(t, func(d *Deps) { d.Toolset = "agent" })

	if got := estTokens(full.instructions); got > instructionsBudgetTokens {
		t.Errorf("initialize instructions = ~%d tokens, budget %d (baseline at 08b0afe was ~533; see the derivation on the bound constants)", got, instructionsBudgetTokens)
	}
	if got := estTokens(agent.wire); got > agentToolsetBudgetTokens {
		t.Errorf("agent tools/list = ~%d tokens, budget %d (ratchet over the 22055-byte baseline at 08b0afe)", got, agentToolsetBudgetTokens)
	}
	if got := estTokens(full.instructions) + estTokens(agent.wire); got > sessionOpenBudgetTokens {
		t.Errorf("session open payload = ~%d tokens, budget %d", got, sessionOpenBudgetTokens)
	}
}

// The messaging admission must not buy extra budget for existing tools.
func TestMessagingAdmissionPreservesExistingToolBudget(t *testing.T) {
	agent := probeSession(t, func(d *Deps) { d.Toolset = "agent"; d.MessagingEnabled = true })
	var existing, messaging strings.Builder
	for _, tool := range agent.tools {
		switch tool.Name {
		case "send_message", "read_messages", "ack_messages", "await_messages", "list_region_members":
			messaging.WriteString(toolWireJSON(t, tool) + "\n")
		default:
			existing.WriteString(toolWireJSON(t, tool) + "\n")
		}
	}
	t.Logf("existing lean tools: %d bytes, ~%d tokens; messaging admission: %d bytes, ~%d tokens",
		existing.Len(), estTokens(existing.String()), messaging.Len(), estTokens(messaging.String()))
	if got := estTokens(existing.String()); got > 6443 {
		t.Errorf("existing lean tools = ~%d tokens, pre-messaging budget 6443", got)
	}
	// M10/M11: lease/owner, sent view, count and full-ID recovery, plus
	// message lease metadata add 883 bytes to M2's 4548-byte admission.
	// Current measured admission 5431 bytes = 1358 tokens; 16 tokens slack.
	if got := estTokens(messaging.String()); got > 1374 {
		t.Errorf("messaging admission = ~%d tokens, budget 1374 (1358 measured + 16 slack)", got)
	}
	if got := estTokens(agent.wire); got > agentToolsetBudgetTokens+1374 {
		t.Errorf("enabled tools budget = %d", got)
	}
}

// TestInstructionsAndToolsetRoutingDiscoverable is the preservation half of
// the C07 contract: trimming duplicated prose must never make routing
// guidance undiscoverable. A host that connects the punk server sees the
// union of the initialize instructions and the tools/list text, so every
// retrieval and coordination rule must be findable in that union - either in
// the instructions (the canonical routing explanation, exactly once) or in
// the owning tool's own entry - and every exposed tool must keep a non-empty
// description of its own.
func TestInstructionsAndToolsetRoutingDiscoverable(t *testing.T) {
	agent := probeSession(t, func(d *Deps) { d.Toolset = "agent" })
	union := agent.instructions + "\n" + agent.wire

	// The canonical routing explanation appears exactly once in the union:
	// duplication between the instructions and a tool entry would mean the
	// trim went the wrong way (re-per-tool instead of once-per-session).
	if n := strings.Count(union, hookcli.RoutingSection()); n != 1 {
		t.Fatalf("routing section occurs %d times in instructions+tools/list, want exactly 1 (in the instructions)", n)
	}

	seen := map[string]bool{}
	for _, tool := range agent.tools {
		seen[tool.Name] = true
		if strings.TrimSpace(tool.Description) == "" {
			t.Fatalf("tool %s lost its description; tool-specific docs must survive the trim", tool.Name)
		}
	}
	for _, name := range agentToolset {
		if !seen[name] {
			t.Fatalf("agent toolset lost tool %s; trimming prose must not remove capabilities", name)
		}
	}

	cases := []struct {
		fragment string
		why      string
	}{
		{"you know the key prefix", "recall's when-to-use rule (instructions routing section)"},
		{"hybrid", "search's ranked-fusion route (instructions or search's own entry)"},
		{"unified_search", "the wording-unknown route stays named"},
		{"compact", "compact-format guidance (instructions or format schemas)"},
		{"list_keys", "the enumeration route stays named"},
		{"never invent them", "list_keys' key-discovery rule, owned by its description"},
		{"remember", "the write route stays named"},
		{"feedback", "the ranking-feedback route stays named (its own description)"},
		{"claim_work", "coordination: claim before working"},
		{"release_work", "coordination: release after working"},
		{"register", "coordination: register once per session"},
		{"list_tasks", "coordination: the task board"},
		{"await_tasks", "coordination: wait instead of polling"},
		{"set_task_status", "coordination: report state"},
		{"/tasks/<id>", "coordination key convention"},
		{"/questions/<id>", "blocked-work question convention"},
		{"/answers/<id>", "blocked-work answer convention"},
		{"punk-memory", "the full usage skill stays referenced from the instructions"},
		{"Call whoami once at session start", "session-start orientation"},
		{"resolved from the client's workspace root (see whoami) when empty", "namespace resolution stays unambiguous in every namespace parameter"},
	}
	for _, c := range cases {
		if !strings.Contains(union, c.fragment) {
			t.Errorf("union of instructions+tools/list lost %q (%s)", c.fragment, c.why)
		}
	}
}

func TestEnabledMessagingRoutingDiscoverable(t *testing.T) {
	p := probeSession(t, func(d *Deps) { d.Toolset = "agent"; d.MessagingEnabled = true })
	for _, term := range []string{"list_region_members", "send_message", "read_messages", "await_messages", "ack_messages", "not task completed", "untrusted agent text", "lease_seconds", "count_only", "sent"} {
		if !strings.Contains(p.wire, term) {
			t.Errorf("enabled surface lost %q", term)
		}
	}
}
