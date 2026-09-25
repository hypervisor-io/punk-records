package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMessagingToolGate(t *testing.T) {
	for _, set := range []string{"agent", "full"} {
		for _, enabled := range []bool{false, true} {
			cs := sessionOpts(t, nil, func(d *Deps) { d.Toolset = set; d.MessagingEnabled = enabled })
			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			tools := map[string]*mcp.Tool{}
			if !enabled {
				for _, name := range messagingToolset {
					if strings.Contains(cs.InitializeResult().Instructions, name) {
						t.Fatalf("disabled initialize references %s", name)
					}
				}
			}
			for _, tool := range res.Tools {
				tools[tool.Name] = tool
			}
			if set == "agent" {
				want := len(agentToolset)
				if enabled {
					want += len(messagingToolset)
				}
				if len(tools) != want {
					t.Fatalf("agent tool count=%d want=%d", len(tools), want)
				}
			}
			for _, name := range []string{"send_message", "read_messages", "ack_messages", "await_messages"} {
				if (tools[name] != nil) != enabled {
					t.Fatalf("%s enabled=%v tool %s presence=%v", set, enabled, name, tools[name] != nil)
				}
				if !enabled {
					r, e := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
					if e == nil && (r == nil || !r.IsError) {
						t.Fatalf("disabled tool callable: %s", name)
					}
				}
			}
			if (tools["list_region_members"] != nil) != (set == "full" || enabled) {
				t.Fatalf("member discovery gate %s %v", set, enabled)
			}
			if tools["register"] == nil || tools["claim_work"] == nil {
				t.Fatal("existing coordination disabled")
			}
			if !enabled && set == "full" && tools["list_region_members"].Description != "List the satellites registered to a brain region." {
				t.Fatal("legacy full member description changed")
			}
		}
	}
}

func TestMessagingEnabledPreservesDefaultToolSchemas(t *testing.T) {
	for _, set := range []string{"agent", "full"} {
		off := probeSession(t, func(d *Deps) { d.Toolset = set })
		on := probeSession(t, func(d *Deps) { d.Toolset = set; d.MessagingEnabled = true })
		if off.instructions != on.instructions {
			t.Fatal("default instructions grew with message switch")
		}
		byName := map[string]*mcp.Tool{}
		for _, tool := range on.tools {
			byName[tool.Name] = tool
		}
		for _, tool := range off.tools {
			if got := byName[tool.Name]; got == nil || toolWireJSON(t, got) != toolWireJSON(t, tool) {
				t.Fatalf("existing schema changed %s", tool.Name)
			}
		}
	}
}
