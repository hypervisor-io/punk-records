package mcpserver

import "github.com/modelcontextprotocol/go-sdk/mcp"

// agentToolset is what a coding agent needs in a session: orient, read,
// write, coordinate. Everything else (task ledger, admin, curation,
// history) stays in the full set so the agent's tool list stays short
// and the routing rules in Instructions stay true.
var agentToolset = []string{
	"whoami", "recall", "search", "unified_search", "list_keys",
	"remember", "remember_many", "remember_document", "feedback",
	"claim_work", "release_work", "list_claims", "set_task_status",
}

// fullOnlyTools are removed when Deps.Toolset is "agent". Keep in sync
// with the AddTool calls in server.go; TestAgentToolsetIsLean guards it.
var fullOnlyTools = []string{
	"submit_task", "get_task", "list_agents", "delegate", "reflect",
	"recall_as_of", "forget", "link", "unlink", "triplet_search", "neighbors",
	"remember_model", "list_models", "list_entities", "profile", "diagnose",
	"register", "list_region_members", "list_agent_regions",
}

// applyToolset trims the server to the named set. Unknown or empty means full.
func applyToolset(s *mcp.Server, toolset string) {
	if toolset == "agent" {
		s.RemoveTools(fullOnlyTools...)
	}
}
