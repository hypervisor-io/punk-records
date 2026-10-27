package mcpserver

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// promptText extracts the single user-message text from a prompts/get
// result, failing the test on any other shape.
func promptText(t *testing.T, res *mcp.GetPromptResult) string {
	t.Helper()
	if res == nil {
		t.Fatal("prompts/get result is nil")
	}
	if len(res.Messages) != 1 {
		t.Fatalf("prompt rendered %d messages, want 1", len(res.Messages))
	}
	msg := res.Messages[0]
	if msg.Role != "user" {
		t.Fatalf("prompt message role = %q, want \"user\"", msg.Role)
	}
	tc, ok := msg.Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("prompt message content = %T, want *mcp.TextContent", msg.Content)
	}
	return tc.Text
}

// TestCommandPromptsListed is the P2-B acceptance, list half: an
// in-memory MCP client sees exactly the five command prompts, in
// order, with the agreed argument schemas.
func TestCommandPromptsListed(t *testing.T) {
	cs := session(t)
	res, err := cs.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatalf("prompts/list: %v", err)
	}
	want := []string{"join", "peers", "send", "inbox", "handoff"}
	var got []string
	for _, p := range res.Prompts {
		got = append(got, p.Name)
	}
	// The SDK lists prompts sorted by name; assert the exact set.
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	sort.Strings(want)
	if strings.Join(sorted, ",") != strings.Join(want, ",") {
		t.Fatalf("prompts/list = %v, want %v", got, want)
	}

	// argument schemas: namespace optional where relevant, recipient
	// and text required for send/handoff, nothing for join/inbox.
	required := map[string]map[string]string{} // prompt -> arg -> "required"|"optional"
	for _, p := range res.Prompts {
		if p.Description == "" {
			t.Errorf("prompt %s: empty description (client pickers need it)", p.Name)
		}
		m := map[string]string{}
		for _, a := range p.Arguments {
			if a.Description == "" {
				t.Errorf("prompt %s argument %s: empty description", p.Name, a.Name)
			}
			if a.Required {
				m[a.Name] = "required"
			} else {
				m[a.Name] = "optional"
			}
		}
		required[p.Name] = m
	}
	if len(required["join"]) != 0 || len(required["inbox"]) != 0 {
		t.Errorf("join/inbox take no arguments, got %v / %v", required["join"], required["inbox"])
	}
	if want := map[string]string{"namespace": "optional"}; !equalArgMap(required["peers"], want) {
		t.Errorf("peers arguments = %v, want %v", required["peers"], want)
	}
	sendArgs := map[string]string{"namespace": "optional", "recipient": "required", "text": "required"}
	if !equalArgMap(required["send"], sendArgs) {
		t.Errorf("send arguments = %v, want %v", required["send"], sendArgs)
	}
	if !equalArgMap(required["handoff"], sendArgs) {
		t.Errorf("handoff arguments = %v, want %v", required["handoff"], sendArgs)
	}
}

func equalArgMap(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestCommandPromptsRenderSharedBody is the P2-B acceptance, render
// half: every prompt renders, and the shared command body bytes from
// hookcli.CommandBody ride verbatim - prompts that take no arguments
// render exactly those bytes, prompts with required arguments render
// them below the argument header. No duplicated prose can drift.
func TestCommandPromptsRenderSharedBody(t *testing.T) {
	cs := session(t)
	ctx := context.Background()
	cases := []struct {
		name string
		args map[string]string
	}{
		{name: "join"},
		{name: "peers"},
		// send/handoff carry required recipient+text, so a render must
		// supply them (clients prompt for required arguments).
		{name: "send", args: map[string]string{"recipient": "claude-code:s9", "text": "status check"}},
		{name: "inbox"},
		{name: "handoff", args: map[string]string{"recipient": "claude-code:s9", "text": "own the migration"}},
	}
	for _, c := range cases {
		body, ok := hookcli.CommandBody("punk-"+c.name, hookcli.SkillOpts{})
		if !ok {
			t.Fatalf("hookcli.CommandBody(punk-%s): command not found", c.name)
		}
		res, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: c.name, Arguments: c.args})
		if err != nil {
			t.Fatalf("prompts/get %s: %v", c.name, err)
		}
		got := promptText(t, res)
		if c.args == nil {
			if got != body {
				t.Fatalf("prompt %s (no arguments): rendered text must be exactly the shared body;\ngot:\n%s\nwant:\n%s", c.name, got, body)
			}
			continue
		}
		if !strings.Contains(got, body) {
			t.Fatalf("prompt %s: rendered text must carry the shared body verbatim;\ngot:\n%s", c.name, got)
		}
		for arg, val := range c.args {
			label := strings.ToUpper(arg[:1]) + arg[1:]
			if !strings.Contains(got, label+": "+val) {
				t.Errorf("prompt %s: header line for %s missing;\ngot:\n%s", c.name, arg, got)
			}
		}
	}
}

// TestCommandSendPromptRendersArguments: provided arguments become a
// header above the shared body - the concrete ask first, the workflow
// (still the single-source bytes) below it.
func TestCommandSendPromptRendersArguments(t *testing.T) {
	cs := session(t)
	res, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name: "send",
		Arguments: map[string]string{
			"namespace": "ns-a",
			"recipient": "claude-code:s2",
			"text":      "please rerun the failing gate",
		},
	})
	if err != nil {
		t.Fatalf("prompts/get send: %v", err)
	}
	got := promptText(t, res)
	for _, want := range []string{
		"Namespace: ns-a",
		"Recipient: claude-code:s2",
		"Text: please rerun the failing gate",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("send prompt missing header line %q;\ngot:\n%s", want, got)
		}
	}
	body, _ := hookcli.CommandBody(hookcli.CommandSend, hookcli.SkillOpts{})
	if !strings.Contains(got, body) {
		t.Fatalf("send prompt must carry the shared command body verbatim;\ngot:\n%s", got)
	}
	if strings.Index(got, "Namespace: ns-a") > strings.Index(got, body) {
		t.Fatalf("argument header must precede the shared body;\ngot:\n%s", got)
	}
}

// TestCommandPromptMissingRequiredArgument: the SDK does not enforce
// required prompt arguments, so the handler must - send without a
// recipient is an error naming what is missing.
func TestCommandPromptMissingRequiredArgument(t *testing.T) {
	cs := session(t)
	_, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name:      "send",
		Arguments: map[string]string{"text": "hi"},
	})
	if err == nil || !strings.Contains(err.Error(), "recipient") {
		t.Fatalf("send without recipient: err = %v, want an error naming recipient", err)
	}
}

// TestCommandPromptsOnAgentToolset: prompts are Deps-independent and
// registered before applyToolset, so the lean agent toolset serves the
// same five.
func TestCommandPromptsOnAgentToolset(t *testing.T) {
	cs := sessionOpts(t, nil, func(d *Deps) { d.Toolset = "agent" })
	res, err := cs.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatalf("prompts/list (agent toolset): %v", err)
	}
	if len(res.Prompts) != 5 {
		t.Fatalf("agent toolset prompts = %d, want 5", len(res.Prompts))
	}
}
