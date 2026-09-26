package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/policy"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/route"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Route permission classes (task A02 inventory). Every route the server
// registers must appear in routePermissionInventory with exactly one
// class, and every inventory entry must match a registered route: an
// unclassified new route fails TestRoutePermissionInventory, which is
// the guard that keeps the boundary complete.
const (
	// classOpen: unauthenticated, no memory data (health probes, page
	// chrome, embedded assets).
	classOpen = "open"
	// classGlobal: authenticated but not namespace-scoped; task ledger,
	// proposals, spec registry, costs, A2A transport. No namespace
	// memory is read or written, so no namespace grant applies (the
	// ledger is global by design; A01 exposes it to any verified key).
	classGlobal = "global"
	// classDiagnostic: pure namespace-selection derivation. Reveals no
	// stored data and grants no access; diagnostics are not the
	// authority (the resolved namespace is authorized at the actual
	// request boundary).
	classDiagnostic = "diagnostic"
	// classNSPath: namespace comes from the {ns} path parameter;
	// enforced per request by the auth middleware (GET/HEAD need read,
	// other methods write). SSE/long-poll members additionally
	// reauthorize before every delivery/response (revocation policy).
	classNSPath = "ns-path"
	// classNSResolved: namespace is resolved at request time from query
	// overrides or cwd derivation; enforced in the handler against the
	// FINAL resolved namespace using the verified subject. Query
	// parameters select a namespace; they never grant access to it.
	classNSResolved = "ns-resolved"
	// classAggregate: cross-region enumeration surfaces; enforcement
	// mode filters per source namespace by the verified subject's read
	// grants (snapshot) or per event (stream). Namespace-less ledger
	// events stay global, matching /v1/tasks.
	classAggregate = "aggregate"
	// classMCP: the MCP streamable-HTTP endpoint; enforcement happens
	// inside the MCP protocol per tool/resource against the resolved
	// namespace (see internal/mcpserver's tool permission inventory).
	classMCP = "mcp"
)

// routePermissionInventory is the A02 endpoint permission inventory for
// the HTTP surface. Keys are "METHOD pattern" exactly as chi registers
// them.
var routePermissionInventory = map[string]string{
	"GET /healthz":                               classOpen,
	"GET /readyz":                                classOpen,
	"GET /ui":                                    classOpen,
	"GET /ui/console.css":                        classOpen,
	"GET /ui/console.js":                         classOpen,
	"GET /":                                      classOpen,
	"GET /brain":                                 classOpen,
	"GET /brain/brain.js":                        classOpen,
	"GET /brain/brain-core.js":                   classOpen,
	"GET /brain/mesh/NOTICE":                     classOpen,
	"GET /brain/mesh/brain.bin":                  classOpen,
	"GET /brain/vendor/{file}":                   classOpen,
	"GET /.well-known/agent-card.json":           classOpen,
	"GET /v1/namespaces":                         classAggregate,
	"POST /v1/namespaces/{ns}/memories":          classNSPath,
	"GET /v1/namespaces/{ns}/memories":           classNSPath,
	"DELETE /v1/namespaces/{ns}/memories":        classNSPath,
	"GET /v1/namespaces/{ns}/memories/search":    classNSPath,
	"GET /v1/namespaces/{ns}/keys":               classNSPath,
	"GET /v1/namespaces/{ns}/events":             classNSPath,
	"GET /v1/namespaces/{ns}/tasks":              classNSPath,
	"POST /v1/namespaces/{ns}/tasks/{id}/status": classNSPath,
	"GET /v1/namespaces/{ns}/profile":            classNSPath,
	"GET /v1/namespaces/{ns}/diagnose":           classNSPath,
	"GET /v1/namespaces/{ns}/pipeline":           classNSPath,
	// M3 agent messaging: namespace is the {ns} path parameter (A01
	// hook). The SSE member additionally revalidates credential and
	// grant before every delivery and on keepalive ticks.
	"POST /v1/namespaces/{ns}/members":           classNSPath,
	"GET /v1/namespaces/{ns}/members":            classNSPath,
	"DELETE /v1/namespaces/{ns}/members/{agent}": classNSPath,
	"POST /v1/namespaces/{ns}/messages":          classNSPath,
	"GET /v1/namespaces/{ns}/messages":           classNSPath,
	"GET /v1/namespaces/{ns}/messages/log":       classNSPath,
	"GET /v1/namespaces/{ns}/messages/count":     classNSPath,
	"POST /v1/namespaces/{ns}/messages/ack":      classNSPath,
	"POST /v1/namespaces/{ns}/messages/release":  classNSPath,
	"GET /v1/namespaces/{ns}/messages/events":    classNSPath,
	"GET /v1/namespaces/{ns}/messages/stream":    classNSPath,
	"POST /v1/agent/hooks":                       classNSResolved,
	"GET /v1/agent/context":                      classNSResolved,
	"GET /v1/agent/namespace":                    classDiagnostic,
	"GET /v1/brain/snapshot":                     classAggregate,
	"GET /v1/brain/events":                       classAggregate,
	"POST /v1/tasks/":                            classGlobal,
	"GET /v1/tasks/":                             classGlobal,
	"GET /v1/tasks/{id}":                         classGlobal,
	"POST /v1/tasks/{id}/cancel":                 classGlobal,
	"POST /v1/tasks/{id}/requeue":                classGlobal,
	"GET /v1/tasks/{id}/proposals":               classGlobal,
	"POST /v1/a2a":                               classGlobal,
	"POST /v1/intake/webhook":                    classGlobal,
	"PATCH /v1/proposals/{id}":                   classGlobal,
	"GET /v1/proposals":                          classGlobal,
	"GET /v1/agents":                             classGlobal,
	"GET /v1/agents/cards":                       classGlobal,
	"GET /v1/agents/{name}/card":                 classGlobal,
	"GET /v1/costs":                              classGlobal,
	// the MCP streamable-HTTP transport registers every method on /mcp
	// (POST carries JSON-RPC calls, GET the server->client SSE stream,
	// DELETE ends a session); enforcement happens per MCP message
	// inside the protocol, so all methods share the mcp class
	"POST /mcp":    classMCP,
	"GET /mcp":     classMCP,
	"PUT /mcp":     classMCP,
	"PATCH /mcp":   classMCP,
	"DELETE /mcp":  classMCP,
	"HEAD /mcp":    classMCP,
	"OPTIONS /mcp": classMCP,
	"TRACE /mcp":   classMCP,
	"CONNECT /mcp": classMCP,
}

// inventoryServer builds a production-shaped server: every dependency
// wired so every route group registers, plus the four Mount calls
// cmdServe makes.
func inventoryServer(t *testing.T, enforce bool) (*Server, *Keys, *authz.Authorizer) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "inv.db"))
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
	if err := os.WriteFile(filepath.Join(specDir, "agents", "database.md"), []byte(dbAgentSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(specDir, db, slog.New(slog.DiscardHandler))
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	ledger := task.NewLedger(db, now)
	keys := NewKeys(db, now)
	az := authz.New(db, nil)
	if enforce {
		keys.SetAuthorizer(az)
	}
	s := New(testLogger(), Deps{
		Memory: memory.New(db, now), Ledger: ledger,
		Router:    route.New(db, reg, ledger, nil, now),
		Proposals: policy.NewProposals(db, now),
		Keys:      keys, Bus: bus.New(), DB: db, Reg: reg,
		Region:        region.New(db, nil),
		DefaultBudget: task.Budget{Tokens: 1000, ToolCalls: 10},
	})
	s.version = "vtest"
	s.MountUI()
	s.MountBrain()
	s.MountPipeline()
	s.MountAgentCard("vtest")
	s.MountMCP(http.NotFoundHandler())
	return s, keys, az
}

// TestRoutePermissionInventory enumerates every registered route and
// asserts each is classified, and that no inventory entry is stale.
func TestRoutePermissionInventory(t *testing.T) {
	s, _, _ := inventoryServer(t, false)
	walked := map[string]bool{}
	if err := chi.Walk(s.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		walked[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for r := range walked {
		if _, ok := routePermissionInventory[r]; !ok {
			t.Errorf("route %q is not classified in routePermissionInventory (task A02: every external surface must be inventoried)", r)
		}
	}
	for r := range routePermissionInventory {
		if !walked[r] {
			t.Errorf("inventory entry %q does not match any registered route (stale inventory)", r)
		}
	}
}

// TestRouteInventoryEnforcedClasses probes every route the inventory
// marks namespace-scoped with a valid A-only credential targeting B:
// each must answer 403 in enforcement mode. This binds the inventory
// classification to actual enforced behavior.
func TestRouteInventoryEnforcedClasses(t *testing.T) {
	s, keys, az := inventoryServer(t, true)
	ctx := context.Background()
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
	// probe bodies per route so handlers reach their authorization
	// check with well-formed input
	bodies := map[string]string{
		"POST /v1/namespaces/{ns}/memories":          `{"key":"/k","body":"v"}`,
		"POST /v1/namespaces/{ns}/tasks/{id}/status": `{"state":"done","summary":"x"}`,
		"POST /v1/agent/hooks":                       `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/work/ns-b","source":"startup"}`,
	}
	// query strings that steer resolved-namespace routes to B
	queries := map[string]string{
		"POST /v1/agent/hooks":                    "?ns=ns-b",
		"GET /v1/agent/context":                   "?ns=ns-b",
		"DELETE /v1/namespaces/{ns}/memories":     "?key=/k",
		"GET /v1/namespaces/{ns}/memories/search": "?q=x",
		"GET /v1/namespaces/{ns}/tasks":           "?state=pending",
	}
	for entry, class := range routePermissionInventory {
		if class != classNSPath && class != classNSResolved {
			continue
		}
		method, pattern, _ := strings.Cut(entry, " ")
		path := strings.Replace(pattern, "{ns}", "ns-b", 1)
		path = strings.Replace(path, "{id}", "T1", 1)
		if q, ok := queries[entry]; ok {
			path += q
		}
		var rdr io.Reader
		if b, ok := bodies[entry]; ok {
			rdr = strings.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403 (class %s must deny B access)", entry, rec.Code, class)
		}
	}
}
