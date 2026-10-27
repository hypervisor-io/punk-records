package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// P2-B: the punk command workflows served as MCP prompts. The
// prompt text is the shared command body (hookcli.CommandBody) - the
// same single source the installed skill files render from - so the
// MCP surface cannot drift from them. The Description fields here are
// short picker labels only; the served text is never duplicated.
//
// Prompt names drop the "punk-" prefix so clients surface them
// naturally: Claude Code shows /mcp__punk__join, /mcp__punk__send, ...
// and Gemini CLI lists them natively.
//
// Dependency direction: mcpserver importing hookcli is the established
// direction (see instructions.go); hookcli's transitive closure does
// not include mcpserver, so there is no cycle.

// promptSkillOpts renders the shared command bodies for the MCP
// surface: no client tool prefix (a bare `send_message` is the
// server's own tool identity, and prefixed clients know their own
// prefix). Sibling-command references are client-neutral since the
// P2-A fix - hookcli's commandRef names every client form in one
// shared string - so no reference-form option is left to set here.
var promptSkillOpts = hookcli.SkillOpts{}

// commandPrompt binds one MCP prompt to the hookcli command whose
// picker label (CommandDescription) and body (CommandBody) it serves,
// plus the prompt's argument schema.
type commandPrompt struct {
	name    string
	command string
	args    []*mcp.PromptArgument
}

// description is the prompt's picker label: hookcli's own one-line
// command description, the same text the installed skill front matter
// carries, so prompts/list and the skills never drift apart.
func (cp commandPrompt) description() string {
	desc, _ := hookcli.CommandDescription(cp.command)
	return desc
}

// The three prompt arguments the commands need: namespace is optional
// everywhere (empty means the session's [PUNK ROUTING] namespace);
// recipient and text are required wherever the command carries a
// message to a specific peer.
var (
	promptArgNamespace = &mcp.PromptArgument{
		Name:        "namespace",
		Description: "punk namespace to run the command in; empty uses the session's [PUNK ROUTING] namespace",
	}
	promptArgRecipient = &mcp.PromptArgument{
		Name:        "recipient",
		Description: "exact punk address of the peer to message or hand off to (find it with the peers prompt)",
		Required:    true,
	}
	promptArgConsolidateNS = &mcp.PromptArgument{
		Name:        "namespace",
		Description: "punk namespace to consolidate; required, never inferred",
		Required:    true,
	}
	promptArgText = &mcp.PromptArgument{
		Name:        "text",
		Description: "the message body or work description, handed over verbatim",
		Required:    true,
	}
)

// commandPrompts is the prompt table in prompts/list order.
var commandPrompts = []commandPrompt{
	{name: "join", command: hookcli.CommandJoin},
	{name: "peers", command: hookcli.CommandPeers,
		args: []*mcp.PromptArgument{promptArgNamespace}},
	{name: "send", command: hookcli.CommandSend,
		args: []*mcp.PromptArgument{promptArgNamespace, promptArgRecipient, promptArgText}},
	{name: "inbox", command: hookcli.CommandInbox},
	{name: "handoff", command: hookcli.CommandHandoff,
		args: []*mcp.PromptArgument{promptArgNamespace, promptArgRecipient, promptArgText}},
	{name: "consolidate", command: hookcli.CommandConsolidate,
		args: []*mcp.PromptArgument{promptArgConsolidateNS}},
}

// registerCommandPrompts adds the punk command prompts to the
// server (prompts/list, prompts/get). Labels and bodies both come from
// hookcli's command table, and the bodies are client- and
// Deps-independent, so every toolset serves the same set.
func registerCommandPrompts(s *mcp.Server) {
	for _, cp := range commandPrompts {
		s.AddPrompt(&mcp.Prompt{
			Name:        cp.name,
			Description: cp.description(),
			Arguments:   cp.args,
		}, commandPromptHandler(cp))
	}
}

// commandPromptHandler serves prompts/get for one command prompt: the
// provided argument values render as a header, the shared command body
// rides below it, and required arguments are enforced here because the
// SDK does not validate them itself.
func commandPromptHandler(cp commandPrompt) mcp.PromptHandler {
	return func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		var missing []string
		for _, a := range cp.args {
			if a.Required && strings.TrimSpace(req.Params.Arguments[a.Name]) == "" {
				missing = append(missing, a.Name)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("prompt %s: missing required argument(s): %s", cp.name, strings.Join(missing, ", "))
		}
		body, ok := hookcli.CommandBody(cp.command, promptSkillOpts)
		if !ok {
			return nil, fmt.Errorf("prompt %s: no shared body for command %q", cp.name, cp.command)
		}
		return &mcp.GetPromptResult{
			Description: cp.description(),
			Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: promptArgumentHeader(cp, req.Params.Arguments) + body},
			}},
		}, nil
	}
}

// promptArgumentHeader renders the provided argument values as a
// "Name: value" block terminated by a blank line. With no arguments
// provided it is empty, and the prompt is exactly the shared body
// bytes.
func promptArgumentHeader(cp commandPrompt, args map[string]string) string {
	var b strings.Builder
	for _, a := range cp.args {
		if v := strings.TrimSpace(args[a.Name]); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", strings.ToUpper(a.Name[:1])+a.Name[1:], v)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String() + "\n"
}
