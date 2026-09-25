package mcpserver

import "github.com/modelcontextprotocol/go-sdk/mcp"

// agentToolset is what a coding agent needs in a session: orient, read,
// write, coordinate. Everything else (task ledger, admin, curation,
// history) stays in the full set so the agent's tool list stays short
// and the routing rules in Instructions stay true.
var agentToolset = []string{
	"whoami", "recall", "search", "unified_search", "list_keys",
	"remember", "remember_many", "remember_document", "feedback",
	"claim_work", "release_work", "list_claims", "register", "set_task_status",
	"list_tasks", "await_tasks", "search_skills", "load_skill",
}

// Only opt-in sessions pay the schema cost of messaging and lean discovery.
var messagingToolset = []string{"send_message", "read_messages", "ack_messages", "await_messages", "list_region_members"}

// fullOnlyTools are removed when Deps.Toolset is "agent". Keep in sync
// with the AddTool calls in server.go; TestAgentToolsetIsLean guards it.
// search_skills/load_skill are admitted to the lean set (S01 round 2:
// procedural discovery must be reachable by the clients it is for); the
// added wire bytes are measured and budgeted in guidance_budget_test.go
// (C07 ratchet re-measurement, see agentToolsetBudgetTokens).
var fullOnlyTools = []string{
	"submit_task", "get_task", "list_agents", "delegate", "reflect",
	"recall_as_of", "forget", "link", "unlink", "triplet_search", "neighbors",
	"remember_model", "list_models", "list_entities", "profile", "diagnose",
	"list_agent_regions",
}

// applyToolset trims the server to the named set. Unknown or empty means full.
func applyToolset(s *mcp.Server, toolset string, messaging bool) {
	if toolset == "agent" {
		s.RemoveTools(fullOnlyTools...)
		if !messaging {
			s.RemoveTools("list_region_members")
		}
	}
}
