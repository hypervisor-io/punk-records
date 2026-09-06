package mcpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/api"
	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/route"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Task A02 tool/resource permission inventory and HTTP red proofs.
//
// Setup: the MCP server is mounted through the production path -
// api.Server.MountMCP behind the bearer auth middleware - with
// enforcement on and ONE valid credential for subject "alice" holding
// read+write on ns-a only. Every namespaced tool/resource attempt
// against namespace B must be denied; global (task-ledger, registry,
// A2A) tools stay reachable, matching their REST twins.
//
// Classes:
//   - authz.OpRead / authz.OpWrite: the tool touches namespace-scoped
//     memory or region state and requires that grant on the FINAL
//     resolved namespace. Explicit arguments, the X-Punk-Namespace
//     header and client roots only SELECT a namespace; they never
//     grant access to it.
//   - "" (global): no namespace-scoped memory is touched (task ledger,
//     spec registry, A2A delegation, diagnostics). list_agent_regions
//     is global but its cross-region enumeration is filtered to the
//     verified subject's readable namespaces (see authz_http_test.go).
//
// The stdio server (punk mcp) carries no HTTP credential and stays a
// trusted local transport (see internal/authz package docs); the gate
// that enforces here is injected per request by the HTTP boundary.

// toolPermissionInventory is the A02 MCP tool permission inventory:
// every registered tool must appear here, and every entry must match a
// registered tool.
var toolPermissionInventory = map[string]authz.Op{
	// global surfaces
	"whoami":             "",
	"submit_task":        "",
	"get_task":           "",
	"list_agents":        "",
	"delegate":           "",
	"list_agent_regions": "", // filtered enumeration, see authz_http_test.go
	// read surfaces
	"recall":              authz.OpRead,
	"list_keys":           authz.OpRead,
	"search":              authz.OpRead,
	"search_skills":       authz.OpRead,
	"load_skill":          authz.OpRead,
	"recall_as_of":        authz.OpRead,
	"triplet_search":      authz.OpRead,
	"unified_search":      authz.OpRead,
	"neighbors":           authz.OpRead,
	"list_models":         authz.OpRead,
	"list_entities":       authz.OpRead,
	"profile":             authz.OpRead,
	"diagnose":            authz.OpRead,
	"list_tasks":          authz.OpRead,
	"await_tasks":         authz.OpRead,
	"list_claims":         authz.OpRead,
	"list_region_members": authz.OpRead,
	"reflect":             authz.OpRead,
	// write surfaces
	"remember":          authz.OpWrite,
	"remember_many":     authz.OpWrite,
	"remember_document": authz.OpWrite,
	"forget":            authz.OpWrite,
	"link":              authz.OpWrite,
	"unlink":            authz.OpWrite,
	"remember_model":    authz.OpWrite,
	"feedback":          authz.OpWrite,
	"set_task_status":   authz.OpWrite,
	"claim_work":        authz.OpWrite,
	"release_work":      authz.OpWrite,
	"register":          authz.OpWrite,
}

// resourcePermissionInventory classifies the subscribable resource
// templates: punk://tasks is the global task ledger; punk://memory is
// namespace-scoped and requires read on the URI's namespace at
// subscribe time, at read time, and again at every delivery.
var resourcePermissionInventory = map[string]authz.Op{
	"task":   "",
	"memory": authz.OpRead,
}

// mcpAuthzRig is the HTTP enforcement rig: real MCP server behind the
// real api auth boundary.
type mcpAuthzRig struct {
	ts    *httptest.Server
	keys  *api.Keys
	az    *authz.Authorizer
	mem   *memory.Store
	bus   *bus.Bus
	reg   *region.Store
	token string // alice: read+write on ns-a only
}

func mcpAuthzRigNew(t *testing.T, enforce bool) *mcpAuthzRig {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "a02mcp.db"))
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
	// locked clock: the bus goroutine and HTTP handlers run concurrently
	var mu sync.Mutex
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	}
	ledger := task.NewLedger(db, now)
	mem := memory.New(db, now)
	b := bus.New()
	regStore := region.New(db, nil)
	srv := New(Deps{
		Ledger: ledger, Router: route.New(db, reg, ledger, nil, now),
		Reg: reg, Mem: mem, Region: regStore, Bus: b,
		LLM:              doneLLM{},
		A2ARemotes:       []A2ARemote{{Name: "remote-a", Endpoint: "http://127.0.0.1:9/a2a", Token: "t"}},
		NamespaceFor:     api.AgentNamespace,
		DefaultNamespace: "agent-default",
		DefaultBudget:    task.Budget{Tokens: 1000, ToolCalls: 10},
	})
	keys := api.NewKeys(db, now)
	az := authz.New(db, nil)
	if enforce {
		keys.SetAuthorizer(az)
	}
	apiSrv := api.New(slog.New(slog.DiscardHandler), api.Deps{Memory: mem, Keys: keys})
	apiSrv.MountMCP(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	ts := httptest.NewServer(apiSrv.Router())
	t.Cleanup(ts.Close)
	g := &mcpAuthzRig{ts: ts, keys: keys, az: az, mem: mem, bus: b, reg: regStore}
	if enforce {
		token, err := keys.Create(ctx, "alice-key", "alice")
		if err != nil {
			t.Fatal(err)
		}
		if err := az.Grant(ctx, "alice", "ns-a", authz.OpRead); err != nil {
			t.Fatal(err)
		}
		if err := az.Grant(ctx, "alice", "ns-a", authz.OpWrite); err != nil {
			t.Fatal(err)
		}
		g.token = token
	}
	return g
}

// connectOpts dials the rig over the real streamable-HTTP transport
// with per-request headers (auth + any forged/selection headers under
// test) and optional client roots.
func (g *mcpAuthzRig) connectOpts(t *testing.T, extra http.Header, roots []string, updated chan string) *mcp.ClientSession {
	t.Helper()
	hdr := http.Header{}
	if g.token != "" {
		hdr.Set("Authorization", "Bearer "+g.token)
	}
	for k, vs := range extra {
		for _, v := range vs {
			hdr.Add(k, v)
		}
	}
	var opts *mcp.ClientOptions
	if updated != nil {
		opts = &mcp.ClientOptions{
			ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
				select {
				case updated <- req.Params.URI:
				default:
				}
			},
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "a02", Version: "0"}, opts)
	for _, r := range roots {
		client.AddRoots(&mcp.Root{URI: r})
	}
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   g.ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: headerRT{h: hdr}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func (g *mcpAuthzRig) connect(t *testing.T) *mcp.ClientSession {
	return g.connectOpts(t, nil, nil, nil)
}

// call runs one tool and reports denied=true when the call carried a
// namespace-grant denial.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (out string, denied bool, otherErr bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	switch {
	case err != nil:
		return err.Error(), strings.Contains(err.Error(), "namespace grant required"), true
	case res.IsError:
		body := text(t, res)
		return body, strings.Contains(body, "namespace grant required"), true
	}
	return text(t, res), false, false
}

// bAttempt is one inventory entry's namespace-B attempt.
func bAttempt(name string) map[string]any {
	base := map[string]any{"namespace": "ns-b"}
	switch name {
	case "search", "triplet_search", "unified_search", "search_skills":
		base["query"] = "x"
	case "load_skill":
		base["name"] = "x"
	case "reflect":
		base["query"] = "x"
	case "recall_as_of":
		base["as_of"] = time.Now().UTC().Format(time.RFC3339)
	case "neighbors":
		base["key"] = "/k"
	case "remember":
		base["key"], base["body"] = "/k", "v"
	case "remember_many":
		base["facts"] = []map[string]any{{"key": "/k", "body": "v"}}
	case "remember_document":
		base["prefix"], base["text"] = "/d", "a\n\nb"
	case "forget":
		base["key"] = "/k"
	case "link", "unlink":
		base["from_key"], base["to_key"] = "/a", "/b"
	case "remember_model":
		base["slug"], base["body"] = "m", "b"
	case "feedback":
		base["ids"], base["rating"] = []string{"x"}, 0.5
	case "set_task_status":
		base["id"], base["state"], base["summary"] = "T1", "done", "x"
	case "claim_work", "release_work":
		base["key"] = "/tasks/T1"
	case "await_tasks":
		base["timeout_seconds"] = 1
	}
	return base
}

// TestToolPermissionInventory enumerates every registered tool and
// resource template and asserts each is classified, with no stale
// entries: an unclassified new tool fails this test (task A02: every
// external surface must be inventoried).
func TestToolPermissionInventory(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.Bus = bus.New()
	deps.LLM = doneLLM{}
	deps.A2ARemotes = []A2ARemote{{Name: "remote-a", Endpoint: "http://127.0.0.1:9/a2a", Token: "t"}}
	srv := New(deps)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "inv", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, tool := range tools.Tools {
		registered[tool.Name] = true
		if _, ok := toolPermissionInventory[tool.Name]; !ok {
			t.Errorf("tool %q is not classified in toolPermissionInventory (task A02: every external surface must be inventoried)", tool.Name)
		}
	}
	for name := range toolPermissionInventory {
		if !registered[name] {
			t.Errorf("inventory entry %q does not match any registered tool (stale inventory)", name)
		}
	}

	tmpls, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotTmpl := map[string]bool{}
	for _, tmpl := range tmpls.ResourceTemplates {
		gotTmpl[tmpl.Name] = true
		if _, ok := resourcePermissionInventory[tmpl.Name]; !ok {
			t.Errorf("resource template %q is not classified in resourcePermissionInventory", tmpl.Name)
		}
	}
	for name := range resourcePermissionInventory {
		if !gotTmpl[name] {
			t.Errorf("resource inventory entry %q does not match any registered template (stale inventory)", name)
		}
	}
}

// TestToolInventoryEnforcedOverHTTP is the table-driven red proof: a
// valid A-only credential attempts B access on every namespaced
// inventory entry over the real HTTP MCP transport. All must be denied
// in enforcement mode; global tools must not answer with a namespace
// denial.
func TestToolInventoryEnforcedOverHTTP(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	cs := g.connect(t)
	for name, op := range toolPermissionInventory {
		if op == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			out, denied, hadErr := call(t, cs, name, bAttempt(name))
			if !denied {
				t.Fatalf("%s on ns-b: denied=%v err=%v out=%s; want namespace-grant denial", name, denied, hadErr, out)
			}
		})
	}
	// global tools answer (or fail for non-authz reasons), never with a
	// namespace denial
	global := map[string]map[string]any{
		"whoami":             {},
		"list_agents":        {},
		"get_task":           {"id": "nope"},
		"submit_task":        {"source": "a02"},
		"delegate":           {"remote": "nope", "text": "x"},
		"list_agent_regions": {"agent": "alice"},
	}
	for name, args := range global {
		out, denied, _ := call(t, cs, name, args)
		if denied {
			t.Fatalf("global tool %s answered with a namespace denial: %s", name, out)
		}
	}
	// positive controls on the granted namespace
	if out, denied, err := call(t, cs, "remember", map[string]any{"namespace": "ns-a", "key": "/k", "body": "v"}); denied || err {
		t.Fatalf("remember on granted ns-a: denied=%v err=%v out=%s", denied, err, out)
	}
	if out, denied, err := call(t, cs, "recall", map[string]any{"namespace": "ns-a", "prefix": "/k"}); denied || err {
		t.Fatalf("recall on granted ns-a: denied=%v err=%v out=%s", denied, err, out)
	}
}

// TestResourceInventoryEnforcedOverHTTP: namespace-scoped resource
// reads and subscriptions are denied for B; the ledger resource stays
// reachable (its not-found is a non-authz error).
func TestResourceInventoryEnforcedOverHTTP(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	cs := g.connect(t)
	ctx := context.Background()

	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "punk://memory/ns-b/x"}); err == nil ||
		!strings.Contains(err.Error(), "namespace grant required") {
		t.Fatalf("read punk://memory/ns-b/x err = %v, want namespace-grant denial", err)
	}
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: "punk://memory/ns-b/tasks"}); err == nil ||
		!strings.Contains(err.Error(), "namespace grant required") {
		t.Fatalf("subscribe punk://memory/ns-b err = %v, want namespace-grant denial", err)
	}
	// ledger resource: global, reachable (not-found, not a denial)
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "punk://tasks/nope"}); err != nil &&
		strings.Contains(err.Error(), "namespace grant required") {
		t.Fatalf("ledger resource denied: %v", err)
	}
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: "punk://tasks/T1"}); err != nil {
		t.Fatalf("subscribe ledger resource = %v, want ok (global)", err)
	}
	// positive control: granted namespace read
	if _, err := g.mem.Write(ctx, memory.WriteInput{Namespace: "ns-a", Key: "/tasks/T1", Body: "v"}); err != nil {
		t.Fatal(err)
	}
	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "punk://memory/ns-a/tasks"})
	if err != nil {
		t.Fatalf("read granted ns-a resource = %v", err)
	}
	if len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, "/tasks/T1") {
		t.Fatalf("granted resource contents: %+v", res.Contents)
	}
}
