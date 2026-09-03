package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestReadToolsDefaultBudget: a recall with no max_tokens is capped at
// defaultMaxTokens and says so; -1 lifts the cap; list_keys is never
// budgeted so a busy prefix can always be enumerated.
func TestReadToolsDefaultBudget(t *testing.T) {
	cs := session(t)
	ctx := context.Background()

	body := strings.Repeat("x", 4*4000) // ~4000 tokens each
	for i := 0; i < 8; i++ {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
			"namespace": "ns", "key": fmt.Sprintf("/tasks/T%d", i), "body": body,
		}})
		if err != nil || res.IsError {
			t.Fatalf("remember %d: %v %s", i, err, text(t, res))
		}
	}

	var out struct {
		Facts     []json.RawMessage `json:"facts"`
		Truncated bool              `json:"truncated"`
		Total     int               `json:"total"`
		Note      string            `json:"note"`
	}
	call := func(args map[string]any) {
		out.Facts, out.Truncated, out.Total, out.Note = nil, false, 0, ""
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("recall %v: %v %s", args, err, text(t, res))
		}
		if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
			t.Fatal(err)
		}
	}

	call(map[string]any{"namespace": "ns", "prefix": "/tasks"})
	if len(out.Facts) != 2 || !out.Truncated || out.Total != 8 || !strings.Contains(out.Note, "2 of 8") {
		t.Fatalf("default budget: facts=%d truncated=%v total=%d note=%q", len(out.Facts), out.Truncated, out.Total, out.Note)
	}

	call(map[string]any{"namespace": "ns", "prefix": "/tasks", "max_tokens": -1})
	if len(out.Facts) != 8 || out.Truncated || out.Total != 0 || out.Note != "" {
		t.Fatalf("uncapped: facts=%d truncated=%v total=%d note=%q", len(out.Facts), out.Truncated, out.Total, out.Note)
	}

	call(map[string]any{"namespace": "ns", "prefix": "/tasks", "max_tokens": 4000})
	if len(out.Facts) != 1 || !out.Truncated || out.Total != 8 {
		t.Fatalf("explicit cap: facts=%d truncated=%v total=%d", len(out.Facts), out.Truncated, out.Total)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_keys", Arguments: map[string]any{"namespace": "ns", "prefix": "/tasks"}})
	if err != nil || res.IsError {
		t.Fatalf("list_keys: %v %s", err, text(t, res))
	}
	var keys struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys.Keys) != 8 {
		t.Fatalf("list_keys = %v, want 8 keys", keys.Keys)
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "list_keys" {
			continue
		}
		schema, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(schema), "max_tokens") {
			t.Fatalf("list_keys schema advertises max_tokens it ignores: %s", schema)
		}
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
		"namespace": "ns", "query": "xxxx", "hybrid": true, "scored": true, "limit": 8,
	}})
	if err != nil || res.IsError {
		t.Fatalf("search: %v %s", err, text(t, res))
	}
	var sout struct {
		Results   []json.RawMessage `json:"results"`
		Truncated bool              `json:"truncated"`
		Total     int               `json:"total"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &sout); err != nil {
		t.Fatal(err)
	}
	if len(sout.Results) > 0 && (len(sout.Results) > 2 || (sout.Total > 2 && !sout.Truncated)) {
		t.Fatalf("search budget: results=%d truncated=%v total=%d", len(sout.Results), sout.Truncated, sout.Total)
	}
}
