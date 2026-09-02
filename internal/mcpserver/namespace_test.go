package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessionWithRoots is session(t) with client roots advertised, the way
// Claude Code and Cursor do for the open workspace.
func sessionWithRoots(t *testing.T, rootURIs ...string) *mcp.ClientSession {
	t.Helper()
	return sessionOpts(t, func(c *mcp.Client) {
		for _, u := range rootURIs {
			c.AddRoots(&mcp.Root{URI: u, Name: "ws"})
		}
	})
}

func TestWhoamiResolvesNamespaceFromRoots(t *testing.T) {
	cs := sessionWithRoots(t, "file:///home/dev/My_Project")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Namespace, Source, Root string
	}
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Namespace != "agent-my-project" || out.Source != "roots" || out.Root != "/home/dev/My_Project" {
		t.Fatalf("whoami = %+v", out)
	}
}

func TestWhoamiFallsBackToDefault(t *testing.T) {
	cs := session(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); !strings.Contains(got, `"namespace":"agent-default"`) || !strings.Contains(got, `"source":"default"`) {
		t.Fatalf("whoami = %s", got)
	}
}

func TestRememberWithoutNamespaceUsesRoots(t *testing.T) {
	cs := sessionWithRoots(t, "file:///srv/repos/billing")
	ctx := context.Background()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember",
		Arguments: map[string]any{"key": "/k", "body": "v"}}); err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recall",
		Arguments: map[string]any{"namespace": "agent-billing", "prefix": "/k"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text(t, res), `"body":"v"`) {
		t.Fatalf("fact not stored under resolved namespace: %s", text(t, res))
	}
}
