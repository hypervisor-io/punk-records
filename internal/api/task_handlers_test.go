package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/policy"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/route"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

const dbAgentSpec = `---
name: database
version: 0.1.0
description: db specialist
triggers:
  - id: db-domain
    source: acme
    labels: {domain: database}
    priority: 10
---
db prompt
`

func taskServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
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
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	ledger := task.NewLedger(db, now)
	router := route.New(db, reg, ledger, nil, now)
	return New(testLogger(), Deps{
		Ledger: ledger, Router: router, Reg: reg,
		Proposals:     policy.NewProposals(db, now),
		DefaultBudget: task.Budget{Tokens: 1000, ToolCalls: 10, WallMS: 60000, Subagents: 1},
	})
}

func TestTaskSubmitRouteFetch(t *testing.T) {
	s := taskServer(t)

	rr := do(t, s, http.MethodPost, "/v1/tasks",
		`{"external_ref":"incident:7","source":"acme","labels":{"domain":"database"}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("submit = %d: %s", rr.Code, rr.Body)
	}
	var out struct {
		Task struct {
			ID        string `json:"id"`
			AgentName string `json:"agent_name"`
		} `json:"task"`
		Created  bool `json:"created"`
		Decision struct {
			ChosenAgent string `json:"chosen_agent"`
			Method      string `json:"method"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Created || out.Decision.ChosenAgent != "database" || out.Task.AgentName != "database" {
		t.Fatalf("out = %+v", out)
	}

	// dedup: same external_ref returns the same open task, no re-route
	rr = do(t, s, http.MethodPost, "/v1/tasks",
		`{"external_ref":"incident:7","source":"acme","labels":{"domain":"database"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("dedup submit = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"created":false`) {
		t.Fatalf("dedup body: %s", rr.Body)
	}

	// fetch with events
	rr = do(t, s, http.MethodGet, "/v1/tasks/"+out.Task.ID, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"routed"`) {
		t.Fatalf("get = %d: %s", rr.Code, rr.Body)
	}

	// list by status
	rr = do(t, s, http.MethodGet, "/v1/tasks?status=submitted", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d", rr.Code)
	}
	rr = do(t, s, http.MethodGet, "/v1/tasks?status=bogus", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad status filter = %d, want 400", rr.Code)
	}

	// source required
	rr = do(t, s, http.MethodPost, "/v1/tasks", `{"labels":{}}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing source = %d, want 400", rr.Code)
	}
}

func TestProposalPatchEndpoint(t *testing.T) {
	s := taskServer(t)
	if s.props == nil {
		t.Skip("proposals not wired")
	}
	// create a task + proposal directly through the stores
	rr := do(t, s, http.MethodPost, "/v1/tasks",
		`{"external_ref":"incident:9","source":"acme","labels":{"domain":"database"}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("submit = %d", rr.Code)
	}
	var out struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	pr, err := s.props.Create(context.Background(), out.Task.ID, "mutate", "runbooks__execute_restart", `{}`)
	if err != nil {
		t.Fatal(err)
	}

	rr = do(t, s, http.MethodPatch, "/v1/proposals/"+pr.ID, `{"status":"approved","external_ref":"susanoo:execution:12"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"approved"`) {
		t.Fatalf("patch = %d: %s", rr.Code, rr.Body)
	}
	rr = do(t, s, http.MethodPatch, "/v1/proposals/ghost", `{"status":"approved"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing proposal = %d, want 404", rr.Code)
	}
	rr = do(t, s, http.MethodGet, "/v1/tasks/"+out.Task.ID+"/proposals", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), pr.ID) {
		t.Fatalf("list proposals = %d: %s", rr.Code, rr.Body)
	}
}

func TestIntakeWebhook(t *testing.T) {
	s := taskServer(t)
	rr := do(t, s, http.MethodPost, "/v1/intake/webhook",
		`{"source":"acme","external_ref":"incident:77","labels":{"domain":"database"}}`)
	if rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), `"database"`) {
		t.Fatalf("intake = %d: %s", rr.Code, rr.Body)
	}
	// retry is idempotent
	rr = do(t, s, http.MethodPost, "/v1/intake/webhook",
		`{"source":"acme","external_ref":"incident:77"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"created":false`) {
		t.Fatalf("intake retry = %d: %s", rr.Code, rr.Body)
	}
	rr = do(t, s, http.MethodPost, "/v1/intake/webhook", `{"source":"x"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing ref = %d, want 400", rr.Code)
	}
}

func TestOperatorEndpoints(t *testing.T) {
	s := taskServer(t)
	s.MountUI()

	// UI serves
	rr := do(t, s, http.MethodGet, "/ui", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "OPERATOR") {
		t.Fatalf("ui = %d", rr.Code)
	}

	// park a task, requeue it, then cancel it
	rr = do(t, s, http.MethodPost, "/v1/tasks", `{"source":"acme","labels":{"domain":"storage"}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("submit = %d", rr.Code)
	}
	var out struct {
		Task struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Task.Status != task.StatusInputRequired {
		t.Fatalf("unroutable task status = %s", out.Task.Status)
	}
	rr = do(t, s, http.MethodPost, "/v1/tasks/"+out.Task.ID+"/requeue", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("requeue = %d: %s", rr.Code, rr.Body)
	}
	rr = do(t, s, http.MethodPost, "/v1/tasks/"+out.Task.ID+"/cancel", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", rr.Code, rr.Body)
	}

	// inbox + agents feeds
	rr = do(t, s, http.MethodGet, "/v1/proposals?status=proposed", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("proposals = %d", rr.Code)
	}
	rr = do(t, s, http.MethodGet, "/v1/agents", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "database") {
		t.Fatalf("agents = %d: %s", rr.Code, rr.Body)
	}
}

func TestAgentCards(t *testing.T) {
	s := taskServer(t)
	rr := do(t, s, http.MethodGet, "/v1/agents/database/card", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"pushNotifications":true`) {
		t.Fatalf("card = %d: %s", rr.Code, rr.Body)
	}
	rr = do(t, s, http.MethodGet, "/v1/agents/cards", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "database") {
		t.Fatalf("cards = %d: %s", rr.Code, rr.Body)
	}
	rr = do(t, s, http.MethodGet, "/v1/agents/ghost/card", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("ghost card = %d, want 404", rr.Code)
	}
}
