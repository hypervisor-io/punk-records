package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/route"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

const agentSpec = `---
name: database
version: 0.1.0
description: db specialist
triggers:
  - source: acme
    labels: {domain: database}
---
prompt
`

// session connects a real in-memory client to the real server, so tests
// exercise the actual protocol (Susanoo's pattern).
func session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "mcps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	specDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(specDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "agents", "database.md"), []byte(agentSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(specDir, db, slog.New(slog.DiscardHandler))
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	ledger := task.NewLedger(db, now)

	srv := New(Deps{
		Ledger:        ledger,
		Router:        route.New(db, reg, ledger, nil, now),
		Reg:           reg,
		Mem:           memory.New(db, now),
		DefaultBudget: task.Budget{Tokens: 1000, ToolCalls: 10},
	})

	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		return "<nil result>"
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if sb.Len() == 0 && res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		return string(raw)
	}
	return sb.String()
}

func TestMCPToolsEndToEnd(t *testing.T) {
	cs := session(t)
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 21 {
		t.Fatalf("tools = %d, want 21", len(tools.Tools))
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "submit_task", Arguments: map[string]any{
		"source": "acme", "external_ref": "incident:5",
		"labels": map[string]string{"domain": "database"},
	}})
	if err != nil || res.IsError {
		t.Fatalf("submit_task: err=%v res=%s", err, text(t, res))
	}
	var out struct {
		TaskID string `json:"task_id"`
		Agent  string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Agent != "database" || out.TaskID == "" {
		t.Fatalf("out = %+v", out)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_task", Arguments: map[string]any{"id": out.TaskID}})
	if err != nil || res.IsError {
		t.Fatalf("get_task: %v %s", err, text(t, res))
	}
	if !strings.Contains(text(t, res), `"routed"`) {
		t.Fatalf("get_task missing events: %s", text(t, res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), "database") {
		t.Fatalf("list_agents: %v %s", err, text(t, res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
		"namespace": "acme", "key": "/svc/db", "body": "primary is pg-1",
	}})
	if err != nil || res.IsError {
		t.Fatalf("remember: %v %s", err, text(t, res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{
		"namespace": "acme", "prefix": "/svc",
	}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), "pg-1") {
		t.Fatalf("recall: %v %s", err, text(t, res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_keys", Arguments: map[string]any{
		"namespace": "acme",
	}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), "/svc/db") {
		t.Fatalf("list_keys: %v %s", err, text(t, res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
		"namespace": "acme", "query": "primary", "hybrid": true, "scored": true,
	}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), `"score_components"`) {
		t.Fatalf("scored search: %v %s", err, text(t, res))
	}

	// tool-level error surfaces as IsError, not protocol failure
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "submit_task", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("missing source did not IsError")
	}
}

func TestTaskResourceSubscription(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "sub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	specDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(specDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "agents", "database.md"), []byte(agentSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(specDir, db, slog.New(slog.DiscardHandler))
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	ledger := task.NewLedger(db, nil)
	b := bus.New()
	ledger.Notify = func(taskID, status string) {
		b.Publish(bus.Event{Kind: "task_status", Key: taskID, Data: map[string]string{"status": status}})
	}
	srv := New(Deps{
		Ledger: ledger, Router: route.New(db, reg, ledger, nil, nil),
		Reg: reg, Mem: memory.New(db, nil), Bus: b,
		DefaultBudget: task.Budget{Tokens: 100, ToolCalls: 5},
	})

	updated := make(chan string, 8)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	tk, _, err := ledger.Submit(ctx, task.SubmitInput{Source: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	uri := "punk://tasks/" + tk.ID
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatal(err)
	}
	// status change fires the notification
	if err := ledger.SetStatus(ctx, tk.ID, task.StatusCanceled, "test", "sub test"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-updated:
		if got != uri {
			t.Fatalf("updated uri = %s, want %s", got, uri)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no resources/updated notification")
	}
	// resource read returns the task with events
	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, `"canceled"`) {
		t.Fatalf("read resource: %+v", res.Contents)
	}
}

func TestRegionTools(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "regmcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	specDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(specDir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(specDir, "agents", "database.md"), []byte(agentSpec), 0o644)
	reg := registry.New(specDir, db, slog.New(slog.DiscardHandler))
	_ = reg.Load(ctx)
	ledger := task.NewLedger(db, nil)
	srv := New(Deps{
		Ledger: ledger, Router: route.New(db, reg, ledger, nil, nil), Reg: reg,
		Mem: memory.New(db, nil), Region: region.New(db, nil),
		DefaultBudget: task.Budget{Tokens: 100, ToolCalls: 5},
	})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "register", Arguments: map[string]any{
		"namespace": "incident-42", "agent": "acme", "role": "lead",
	}})
	if err != nil || res.IsError {
		t.Fatalf("register: %v %s", err, text(t, res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_region_members", Arguments: map[string]any{"namespace": "incident-42"}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), "acme") {
		t.Fatalf("members: %v %s", err, text(t, res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agent_regions", Arguments: map[string]any{"agent": "acme"}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), "incident-42") {
		t.Fatalf("agent regions: %v %s", err, text(t, res))
	}
}

func TestMemoryV2Tools(t *testing.T) {
	cs := session(t)
	ctx := context.Background()
	for _, kv := range [][2]string{{"/svc/db", "replication lag on the primary"}, {"/svc/net", "packet loss on edge"}} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
			"namespace": "ns", "key": kv[0], "body": kv[1],
		}})
		if err != nil || res.IsError {
			t.Fatalf("remember: %v %s", err, text(t, res))
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
		"namespace": "ns", "query": "replication",
	}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), "/svc/db") {
		t.Fatalf("search: %v %s", err, text(t, res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "forget", Arguments: map[string]any{
		"namespace": "ns", "key": "/svc/db",
	}})
	if err != nil || res.IsError {
		t.Fatalf("forget: %v %s", err, text(t, res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "recall_as_of", Arguments: map[string]any{
		"namespace": "ns", "prefix": "/svc", "as_of": "2030-01-01T00:00:00Z",
	}})
	// after forget, /svc/db is gone; /svc/net remains
	if err != nil || res.IsError || strings.Contains(text(t, res), "/svc/db") || !strings.Contains(text(t, res), "/svc/net") {
		t.Fatalf("recall_as_of: %v %s", err, text(t, res))
	}
}

// TestSearchTemporalDoesNotHijackHybrid mirrors the API-level regression:
// Temporal=true on a hybrid/scored request must not divert to plain
// WindowedSearch just because the query text contains a recognized
// temporal phrase (a bare year) — Hybrid wins and Temporal is ignored.
func TestSearchTemporalDoesNotHijackHybrid(t *testing.T) {
	cs := session(t)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
		"namespace": "ns", "key": "/svc/db", "body": "replication lag on the primary in 2026",
	}})
	if err != nil || res.IsError {
		t.Fatalf("remember: %v %s", err, text(t, res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
		"namespace": "ns", "query": "replication 2026", "temporal": true, "hybrid": true, "scored": true,
	}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), `"score_components"`) {
		t.Fatalf("hybrid+temporal search: %v %s (want scored hybrid path, not plain WindowedSearch)", err, text(t, res))
	}
}

// TestProfileAndDiagnoseTools exercises the profile and diagnose tools
// end to end through the real MCP protocol (Task 1.7), mirroring
// TestMemoryV2Tools's session/CallTool/text shape.
func TestProfileAndDiagnoseTools(t *testing.T) {
	cs := session(t)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
		"namespace": "ns", "key": "/a", "body": "hello",
	}})
	if err != nil || res.IsError {
		t.Fatalf("remember: %v %s", err, text(t, res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "profile", Arguments: map[string]any{
		"namespace": "ns",
	}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), `"facts":1`) {
		t.Fatalf("profile: %v %s", err, text(t, res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "diagnose", Arguments: map[string]any{
		"namespace": "ns",
	}})
	// "namespace":"ns" is discriminating (echoes the fixture); "orphan_links":0
	// alone holds for any input and would pass even if diagnose ignored ns.
	if err != nil || res.IsError || !strings.Contains(text(t, res), `"namespace":"ns"`) || !strings.Contains(text(t, res), `"orphan_links":0`) {
		t.Fatalf("diagnose: %v %s", err, text(t, res))
	}
}

// doneLLM is a stub llm.Client that immediately answers via the reflect
// loop's done tool, with no citations. Enough to prove the wiring: the
// reflect tool is registered and callable end to end only when Deps.LLM
// is set.
type doneLLM struct{}

func (doneLLM) Chat(context.Context, []llm.Turn, []llm.Tool) (*llm.Result, error) {
	args, _ := json.Marshal(map[string]any{"answer": "stub answer"})
	return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "d1", Name: "done", Args: args}}}, nil
}
func (doneLLM) Model() string { return "stub" }

// TestReflectToolOnlyWiredWithLLM confirms the conditional registration
// in New(): with Deps.LLM nil, reflect must be absent (existing tool
// count of 21, unaffected by this change); with it set, reflect appears
// and answers end to end through the real MCP protocol.
func TestReflectToolOnlyWiredWithLLM(t *testing.T) {
	cs := session(t) // Deps.LLM unset
	ctx := context.Background()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 21 {
		t.Fatalf("tools = %d, want 21 (reflect must stay off without an LLM)", len(tools.Tools))
	}

	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "reflectmcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{Mem: memory.New(db, nil), LLM: doneLLM{}})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	wired, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wired.Close() })

	wtools, err := wired.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tl := range wtools.Tools {
		if tl.Name == "reflect" {
			found = true
		}
	}
	if !found {
		t.Fatal("reflect tool missing when Deps.LLM is set")
	}
	res, err := wired.CallTool(ctx, &mcp.CallToolParams{Name: "reflect", Arguments: map[string]any{
		"namespace": "acme", "query": "anything?",
	}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), "stub answer") {
		t.Fatalf("reflect: %v %s", err, text(t, res))
	}
}

// TestUnlinkAndNeighborsAsOf exercises the unlink tool's soft-delete
// semantics end to end through the real MCP protocol: a live edge
// disappears from neighbors after unlink but stays visible via
// neighbors' as_of an instant before the unlink; unlinking an edge with
// no live row returns a tool error.
//
// Builds its own server/session with an exposed clock closure, rather
// than session(t): session(t)'s fakeClock is private to that helper, and
// the store's now() is bound to it, not to time.Now() - the as_of window
// here needs an instant taken from that same clock, precisely between
// the link and unlink calls (mirroring TestInvalidateLinkAndAsOf at the
// memory layer), which session(t) has no way to hand back.
func TestUnlinkAndNeighborsAsOf(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "unlinkmcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	srv := New(Deps{Mem: memory.New(db, now)})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	for _, kv := range [][2]string{{"/a", "x"}, {"/b", "y"}} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
			"namespace": "ns", "key": kv[0], "body": kv[1],
		}})
		if err != nil || res.IsError {
			t.Fatalf("remember: %v %s", err, text(t, res))
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "link", Arguments: map[string]any{
		"namespace": "ns", "from_key": "/a", "to_key": "/b", "link_type": "leads_to",
	}})
	if err != nil || res.IsError {
		t.Fatalf("link: %v %s", err, text(t, res))
	}
	// mid ticks the same clock the store uses one step past the link's
	// stamp and strictly before unlink's - guaranteed inside the edge's
	// open validity window regardless of how many now() calls preceded it.
	mid := now()

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "unlink", Arguments: map[string]any{
		"namespace": "ns", "from_key": "/a", "to_key": "/b", "link_type": "leads_to",
	}})
	if err != nil || res.IsError {
		t.Fatalf("unlink: %v %s", err, text(t, res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "neighbors", Arguments: map[string]any{
		"namespace": "ns", "key": "/a", "direction": "out",
	}})
	if err != nil || res.IsError || strings.Contains(text(t, res), "/b") {
		t.Fatalf("neighbors after unlink still shows the edge: %v %s", err, text(t, res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "neighbors", Arguments: map[string]any{
		"namespace": "ns", "key": "/a", "direction": "out", "as_of": mid.Format(time.RFC3339Nano),
	}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), "/b") {
		t.Fatalf("neighbors as_of before unlink should still show the edge: %v %s", err, text(t, res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "unlink", Arguments: map[string]any{
		"namespace": "ns", "from_key": "/x", "to_key": "/y", "link_type": "leads_to",
	}})
	if err == nil && !res.IsError {
		t.Fatal("unlink on a nonexistent edge should return a tool error")
	}
}

// TestUnlinkOmittedLinkTypeDefaultsToRelatesTo proves the unlink tool's
// link_type default (relates_to, applied by memory.InvalidateLink) closes
// only the relates_to edge of a pair that has both a relates_to and a
// leads_to edge live - the omitted-field default must not blanket-close
// every edge between the two keys.
func TestUnlinkOmittedLinkTypeDefaultsToRelatesTo(t *testing.T) {
	cs := session(t)
	ctx := context.Background()

	for _, kv := range [][2]string{{"/a", "x"}, {"/b", "y"}} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
			"namespace": "ns", "key": kv[0], "body": kv[1],
		}})
		if err != nil || res.IsError {
			t.Fatalf("remember: %v %s", err, text(t, res))
		}
	}
	for _, lt := range []string{"relates_to", "leads_to"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "link", Arguments: map[string]any{
			"namespace": "ns", "from_key": "/a", "to_key": "/b", "link_type": lt,
		}})
		if err != nil || res.IsError {
			t.Fatalf("link %s: %v %s", lt, err, text(t, res))
		}
	}

	// link_type deliberately omitted: must default to relates_to, not
	// close every edge between /a and /b.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "unlink", Arguments: map[string]any{
		"namespace": "ns", "from_key": "/a", "to_key": "/b",
	}})
	if err != nil || res.IsError {
		t.Fatalf("unlink: %v %s", err, text(t, res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "neighbors", Arguments: map[string]any{
		"namespace": "ns", "key": "/a", "direction": "out",
	}})
	if err != nil || res.IsError {
		t.Fatalf("neighbors: %v %s", err, text(t, res))
	}
	got := text(t, res)
	if !strings.Contains(got, "leads_to") {
		t.Fatalf("neighbors after default-type unlink = %s, want leads_to still live", got)
	}
	if strings.Contains(got, "relates_to") {
		t.Fatalf("neighbors after default-type unlink = %s, want relates_to closed", got)
	}
}

// fakeExpander is a stub memory.QueryExpander for TestSearchExpandFlag: it
// always returns the fixed reformulation list it was built with.
type fakeExpander struct{ refs []string }

func (f fakeExpander) Expand(context.Context, string) ([]string, error) { return f.refs, nil }

// newExpandSession mirrors TestUnlinkAndNeighborsAsOf's custom-Deps session
// pattern (session(t) hardcodes Deps and has no way to inject an Expander),
// wiring exp (possibly nil) as Deps.Expander, then seeding the two-fact
// fixture shared with memory.TestHybridSearchExpanded: /a only matches the
// direct query "authentication", /b only matches the reformulation "login
// tokens".
func newExpandSession(t *testing.T, exp memory.QueryExpander) *mcp.ClientSession {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "expandmcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{Mem: memory.New(db, nil), Expander: exp})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	for _, kv := range [][2]string{
		{"/a", "authentication uses jwt"},
		{"/b", "login tokens rotate hourly"},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
			"namespace": "ns", "key": kv[0], "body": kv[1],
		}})
		if err != nil || res.IsError {
			t.Fatalf("remember %s: %v %s", kv[0], err, text(t, res))
		}
	}
	return cs
}

// TestSearchExpandFlag proves the search tool's expand field end to end:
// with hybrid+scored+expand and a wired Deps.Expander, a reformulation-only
// match (/b) joins the direct-query match (/a); expand:false restricts to
// the direct match even with an expander wired; expand:true with
// Deps.Expander nil is ignored (no error), same as expand:false.
func TestSearchExpandFlag(t *testing.T) {
	ctx := context.Background()

	cs := newExpandSession(t, fakeExpander{refs: []string{"login tokens"}})
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
		"namespace": "ns", "query": "authentication", "hybrid": true, "scored": true, "expand": true,
	}})
	if err != nil || res.IsError {
		t.Fatalf("search expand=true: %v %s", err, text(t, res))
	}
	if got := text(t, res); !strings.Contains(got, "/a") || !strings.Contains(got, "/b") {
		t.Fatalf("search expand=true = %s, want both /a and /b", got)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
		"namespace": "ns", "query": "authentication", "hybrid": true, "scored": true, "expand": false,
	}})
	if err != nil || res.IsError {
		t.Fatalf("search expand=false: %v %s", err, text(t, res))
	}
	if got := text(t, res); !strings.Contains(got, "/a") || strings.Contains(got, "/b") {
		t.Fatalf("search expand=false = %s, want only /a", got)
	}

	csNoExpander := newExpandSession(t, nil)
	res, err = csNoExpander.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
		"namespace": "ns", "query": "authentication", "hybrid": true, "scored": true, "expand": true,
	}})
	if err != nil || res.IsError {
		t.Fatalf("search expand=true no expander: %v %s", err, text(t, res))
	}
	if got := text(t, res); !strings.Contains(got, "/a") || strings.Contains(got, "/b") {
		t.Fatalf("search expand=true no expander = %s, want only /a (flag ignored, no error)", got)
	}
}

func TestSearchCompactFormat(t *testing.T) {
	cs := session(t)
	ctx := context.Background()
	long := strings.Repeat("z", 800)
	for _, k := range []string{"/svc/a", "/svc/b"} {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember",
			Arguments: map[string]any{"namespace": "ns", "key": k, "body": "outage " + long}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search",
		Arguments: map[string]any{"namespace": "ns", "query": "outage", "format": "compact"}})
	if err != nil || res.IsError {
		t.Fatalf("search compact: %v %s", err, text(t, res))
	}
	var out struct {
		Facts []json.RawMessage `json:"facts"`
		Hits  []struct {
			Key  string `json:"key"`
			Body string `json:"body"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Facts) != 0 {
		t.Fatalf("compact must not also return facts, got %d", len(out.Facts))
	}
	if len(out.Hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(out.Hits))
	}
	if len([]rune(out.Hits[0].Body)) != 603 {
		t.Fatalf("body not clipped: %d runes", len([]rune(out.Hits[0].Body)))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "unified_search",
		Arguments: map[string]any{"namespace": "ns", "query": "outage", "format": "compact"}})
	if err != nil || res.IsError {
		t.Fatalf("unified_search compact: %v %s", err, text(t, res))
	}
	var uo struct {
		Hits    []json.RawMessage `json:"hits"`
		Compact []struct {
			Key string `json:"key"`
		} `json:"compact"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &uo); err != nil {
		t.Fatal(err)
	}
	if len(uo.Hits) != 0 || len(uo.Compact) != 2 {
		t.Fatalf("unified compact: hits=%d compact=%d", len(uo.Hits), len(uo.Compact))
	}
}

func TestServerInstructionsAdvertised(t *testing.T) {
	cs := session(t)
	got := cs.InitializeResult().Instructions
	for _, want := range []string{"unified_search", "recall", "compact", "list_keys", "remember"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions missing %q:\n%s", want, got)
		}
	}
}

func TestSearchAnchorsInput(t *testing.T) {
	cs := session(t)
	ctx := context.Background()
	for k, b := range map[string]string{
		"/incident/1":   "database outage traced to connection saturation",
		"/runbook/pool": "when ERR_POOL_EXHAUSTED appears, raise max_connections",
	} {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember",
			Arguments: map[string]any{"namespace": "ns", "key": k, "body": b}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search",
		Arguments: map[string]any{"namespace": "ns", "query": "database outage", "hybrid": true, "scored": true,
			"anchors": []string{"ERR_POOL_EXHAUSTED"}, "format": "compact"}})
	if err != nil || res.IsError {
		t.Fatalf("search anchors: %v %s", err, text(t, res))
	}
	var out struct {
		Hits []struct {
			Key string `json:"key"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range out.Hits {
		found = found || h.Key == "/runbook/pool"
	}
	if !found {
		t.Fatalf("anchored runbook missing: %+v", out.Hits)
	}
}
