package hookcli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// VerifyReport is what a real MCP session against punk looked like.
type VerifyReport struct {
	Tools        []string
	Namespace    string
	Instructions bool
}

// VerifyMCP connects to endpoint as an MCP client, lists tools and calls
// whoami. An agent whose first punk call fails tends to abandon the tool
// for the rest of the session, so connect ends by proving the round trip.
func VerifyMCP(ctx context.Context, endpoint string) (VerifyReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "punk-connect-verify", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("connect %s: %w", endpoint, err)
	}
	defer func() { _ = cs.Close() }()
	rep := VerifyReport{Instructions: cs.InitializeResult().Instructions != ""}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		return rep, fmt.Errorf("tools/list: %w", err)
	}
	for _, t := range tools.Tools {
		rep.Tools = append(rep.Tools, t.Name)
	}
	sort.Strings(rep.Tools)
	if len(rep.Tools) == 0 {
		return rep, fmt.Errorf("server at %s exposes no tools", endpoint)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		return rep, fmt.Errorf("whoami: %w", err)
	}
	var out struct {
		Namespace string `json:"namespace"`
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &out)
		}
	}
	if out.Namespace == "" {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			_ = json.Unmarshal(raw, &out)
		}
	}
	rep.Namespace = out.Namespace
	return rep, nil
}
