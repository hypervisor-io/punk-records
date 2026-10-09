package hookcli

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Exercise the installed renderers, not their template source. Target options
// come from the install path so Pi, prefixes and Hermes metadata cannot drift.
func forAgentGuidance(t *testing.T, check func(*testing.T, SkillOpts)) {
	t.Helper()
	for _, agent := range []string{"claude-code", "codex", "opencode", "cursor", "copilot", "antigravity", "hermes", "openclaw", "pi"} {
		targets, err := SkillTargets(agent, false, "/home/test", func(string) string { return "" })
		if err != nil || len(targets) != 1 {
			t.Fatalf("%s targets: %+v, %v", agent, targets, err)
		}
		if err := checkAgentGuidanceClient(agent, targets[0].Opts); err != nil {
			t.Fatal(err)
		}
		for _, messaging := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/messaging=%t", agent, messaging), func(t *testing.T) {
				opts := targets[0].Opts
				opts.ServerURL = "http://localhost:9090"
				opts.Messaging = messaging
				check(t, opts)
			})
		}
	}
}

// Compare to the requested client, not another field of the possibly regressed
// options. Otherwise a lost Pi flag silently skips every Pi-only assertion.
func checkAgentGuidanceClient(agent string, opts SkillOpts) error {
	if wantPi := agent == "pi"; opts.Pi != wantPi {
		return fmt.Errorf("%s: Pi classification = %t, want %t", agent, opts.Pi, wantPi)
	}
	return nil
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidanceRejectsLostPiClassification
func TestAgentGuidanceRejectsLostPiClassification(t *testing.T) {
	const agent = "pi"
	targets, err := SkillTargets(agent, false, "/home/test", func(string) string { return "" })
	if err != nil || len(targets) == 0 {
		t.Fatalf("%s targets: %+v, %v", agent, targets, err)
	}
	for _, target := range targets {
		if err := checkAgentGuidanceClient(agent, target.Opts); err != nil {
			t.Fatal(err)
		}
		mutated := target.Opts // mutate only this value copy, never production source
		mutated.Pi = false
		for _, render := range []func(SkillOpts) string{RenderSkill, RenderPlanSkill} {
			// Demonstrate the actual bad render the fixture must reject: the
			// client still names Pi, but the lost flag selects MCP guidance.
			requireGuidance(t, render(mutated), "register.agent", "claim_work.holder")
		}
		if err := checkAgentGuidanceClient(agent, mutated); err == nil {
			t.Fatal("fixture accepted a Pi target with its Pi classification lost")
		}
	}
}

func requireGuidance(t *testing.T, text string, terms ...string) {
	t.Helper()
	for _, term := range terms {
		if !strings.Contains(text, term) {
			t.Fatalf("rendered guidance missing %q", term)
		}
	}
}

func forbidGuidance(t *testing.T, text string, terms ...string) {
	t.Helper()
	for _, term := range terms {
		if strings.Contains(text, term) {
			t.Fatalf("rendered guidance still contains %q", term)
		}
	}
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidanceRenderedYAML
func TestAgentGuidanceRenderedYAML(t *testing.T) {
	const legacyMarker = "<!-- managed by punk connect; edit outside this file's marker and punk will not overwrite it -->"
	forAgentGuidance(t, func(t *testing.T, opts SkillOpts) {
		for _, render := range []struct {
			name string
			fn   func(SkillOpts) string
		}{{SkillName, RenderSkill}, {PlanSkillName, RenderPlanSkill}} {
			t.Run(render.name, func(t *testing.T) {
				text := render.fn(opts)
				parts := strings.SplitN(text, "---\n", 3)
				if len(parts) != 3 || parts[0] != "" {
					t.Fatalf("missing YAML frontmatter boundaries")
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
				if front.Name != render.name || front.Description == "" || len(front.Description) > 1024 || strings.Contains(front.Description, "\n") {
					t.Fatalf("invalid skill metadata: %+v", front)
				}
				if opts.Hermes {
					if front.Version != "1.0.0" || front.Metadata["hermes"] == nil {
						t.Fatalf("missing Hermes metadata: %+v", front)
					}
				} else if front.Version != "" || len(front.Metadata) != 0 {
					t.Fatalf("Hermes metadata leaked to %s", opts.Agent)
				}
				if !strings.HasPrefix(parts[2], legacyMarker+"\n") || !IsManagedSkill([]byte(text)) || strings.Count(text, legacyMarker) != 1 {
					t.Fatal("managed-file compatibility marker changed or duplicated")
				}
				forbidGuidance(t, text, "{{", "<no value>", "—", "–")
			})
		}
	})
}

// Namespace auth precedes region creation (mcpserver/server.go); identity is
// request-derived (mcpserver/namespace.go), not set by register. Claim lifetime
// and holder matching live in region/claim.go and mcpserver/tasks.go.
// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidanceCoordination
func TestAgentGuidanceCoordination(t *testing.T) {
	forAgentGuidance(t, func(t *testing.T, opts SkillOpts) {
		if opts.Pi {
			return
		}
		for _, render := range []func(SkillOpts) string{RenderSkill, RenderPlanSkill} {
			text := render(opts)
			requireGuidance(t, text,
				"write grant", "creates the region and membership", "does not grant access",
				"enforced HTTP authorization", "trusted local transport needs no grants",
				"user@host", "session-unique", "register.agent", "claim_work.holder", "set_task_status.agent", "release_work.holder",
				"does not change omitted namespace or identity", "last_seen_at", "does not renew", "ttl_seconds", "re-claim",
				"done and blocked attempt", "matching-holder", "released_claim", "separate file claims",
				"timeout_seconds=45", "never extends that deadline", "retry shorter")
			forbidGuidance(t, text,
				"done and blocked release your claim.",
				"Every coordination call after that is your heartbeat",
				"or a member whose `last_seen_at` is old, means the task is free again",
				"then `"+ToolName(opts.ToolPrefix, "release_work")+"`")
		}
	})
}

// Pi's extension registers four HTTP-backed tools. Its whoami has only
// namespace/server; none of the four tools accepts a namespace override.
// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidancePiCapabilities
func TestAgentGuidancePiCapabilities(t *testing.T) {
	forAgentGuidance(t, func(t *testing.T, opts SkillOpts) {
		if !opts.Pi {
			return
		}
		for _, render := range []func(SkillOpts) string{RenderSkill, RenderPlanSkill} {
			text := render(opts)
			requireGuidance(t, text,
				"namespace and server only", "no namespace argument",
				"no claim or registration tools", "approved MCP or another supported claim interface",
				"Do not start concurrent workers", "HTTP API", "/tasks?wait=", "/tasks/<id>/status")
			forbidGuidance(t, text,
				"learn your identity", "your agent identity", "Pass max_tokens",
				"setup (whoami, register", "always passing the namespace explicitly")
			for _, tool := range []string{"register", "claim_work", "release_work", "list_claims", "list_tasks", "await_tasks", "set_task_status", "remember_many", "remember_document", "feedback", "list_keys", "unified_search", "triplet_search", "neighbors", "search_skills", "load_skill", "submit_task", "get_task", "send_message", "read_messages", "ack_messages", "await_messages", "list_region_members"} {
				forbidGuidance(t, text, "`punk_"+tool+"`", "`"+tool+"`")
			}
		}
	})
}

// Toolset and skill-index routing: mcpserver/toolset.go and server.go. Document
// provenance metadata can accompany text; source.sections cannot.
// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidanceMemoryCapabilities
func TestAgentGuidanceMemoryCapabilities(t *testing.T) {
	forAgentGuidance(t, func(t *testing.T, opts SkillOpts) {
		text := RenderSkill(opts)
		requireGuidance(t, text, "untrusted data", "not authority", "user-profile", "/profile/", "user-managed", "writes are not automatic")
		if opts.Pi {
			return
		}
		requireGuidance(t, text,
			"`"+ToolName(opts.ToolPrefix, "search_skills")+"`", "metadata", "`"+ToolName(opts.ToolPrefix, "load_skill")+"`", "exact version",
			"omitted namespace targets the skill index", "separate read grant",
			"triplet_search and neighbors", "full toolset only", "lean", "unified_search",
			"repo_revision", "source.sections", "mutually exclusive", "absolute", "stdio",
			"remember does not release claims")
	})
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidancePlannerAuthorization
func TestAgentGuidancePlannerAuthorization(t *testing.T) {
	forAgentGuidance(t, func(t *testing.T, opts SkillOpts) {
		text := RenderPlanSkill(opts)
		requireGuidance(t, text,
			"Only commit, push, merge, tag, release or deploy when the user authorizes",
			"uncommitted diff", "no fabricated sha", "untrusted data", "not authority",
			"claim expiry", "not member last_seen_at", "fresh board", "/plan/review")
		forbidGuidance(t, text,
			"then one commit.", "no attribution trailers", "one commit per task with the plan's message",
			"merge from the primary checkout, tag and release, deploy", "| what the gate checked, the merge sha |",
			"| one task: title, files, depends_on, plan section, commit message |")
	})
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidanceMessaging
func TestAgentGuidanceMessaging(t *testing.T) {
	forAgentGuidance(t, func(t *testing.T, opts SkillOpts) {
		for _, render := range []func(SkillOpts) string{RenderSkill, RenderPlanSkill} {
			text := render(opts)
			for _, name := range []string{"send_message", "read_messages", "ack_messages", "await_messages", "list_region_members"} {
				if opts.Messaging && !opts.Pi {
					requireGuidance(t, text, ToolName(opts.ToolPrefix, name))
				} else {
					forbidGuidance(t, text, name)
				}
			}
			if !opts.Messaging {
				forbidGuidance(t, text, "## Agent messages", "send_message", "await_messages", "/messages/ack", "inbox:true")
				continue
			}
			requireGuidance(t, text,
				"registered session address", "client bridge", "explicit namespace and sender", "namespace and agent",
				"not task completed", "No automatic replies to ACKs", "untrusted data")
			if opts.Pi {
				requireGuidance(t, text, "POST /v1/namespaces/<ns>/messages", "GET /messages?agent=", "POST /messages/ack", "four Pi memory tools are not message tools", "HTTP routes")
				requireGuidance(t, text, "require namespace grants when HTTP authorization is enforced")
				forbidGuidance(t, text, "HTTP routes are independent of MCP opt-in and require namespace grants.")
			} else {
				requireGuidance(t, text,
					"MCP message tools", "server opt-in", "inbox:true", "only your own session address", "authorized namespace",
					"binding does not change", "namespace or sender", "active_only", "idempotency_key", "read_messages", "retention")
			}
		}
	})
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidanceBudgets
func TestAgentGuidanceBudgets(t *testing.T) {
	forAgentGuidance(t, func(t *testing.T, opts SkillOpts) {
		for _, pinned := range []bool{false, true} {
			if pinned {
				opts.Namespace = "agent-guidance-123456"
			}
			base := opts
			base.Messaging = false
			if tokens := estTokens(RenderSkill(base)); tokens > skillBodyBudgetTokens {
				t.Errorf("memory pinned=%t: ~%d tokens exceeds %d", pinned, tokens, skillBodyBudgetTokens)
			}
			// Apply the existing 1400-byte opt-in bound to every tool prefix
			// and both renderers, not just the plain-tools render.
			for _, render := range []func(SkillOpts) string{RenderSkill, RenderPlanSkill} {
				if extra := len(render(opts)) - len(render(base)); extra > 1400 {
					t.Errorf("messaging adds %d bytes, budget 1400 including tool prefixes", extra)
				}
			}
			// The planner measured ~1607 tokens before these corrections.
			if tokens := estTokens(RenderPlanSkill(base)); tokens > 1710 {
				t.Errorf("planner pinned=%t: ~%d tokens exceeds 1710", pinned, tokens)
			}
			t.Logf("pinned=%t memory=%d planner=%d estimated tokens", pinned, estTokens(RenderSkill(opts)), estTokens(RenderPlanSkill(opts)))
		}
	})
	if got := estTokens(RoutingSection()); got > routingSectionBudgetTokens {
		t.Errorf("routing ~%d tokens exceeds %d", got, routingSectionBudgetTokens)
	}
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=agent-guidance test=TestAgentGuidanceClineHasNoSkillTarget
func TestAgentGuidanceClineHasNoSkillTarget(t *testing.T) {
	if targets, err := AllSkillTargets("cline", false, "/home/test", func(string) string { return "" }); err == nil || len(targets) != 0 {
		t.Fatalf("Cline must not advertise generated skills: %+v, %v", targets, err)
	}
}
