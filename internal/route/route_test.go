package route

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

const dbAgent = `---
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

const netAgent = `---
name: network
version: 0.1.0
description: net specialist
triggers:
  - id: net-domain
    source: acme
    labels: {domain: network}
    priority: 10
  - id: net-critical
    source: acme
    severity: [critical]
    priority: 1
---
net prompt
`

const appAgent = `---
name: application
version: 0.1.0
description: app specialist
triggers:
  - id: app-critical
    source: acme
    severity: [critical]
    priority: 1
---
app prompt
`

func writeSpec(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, "agents", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*Router, *task.Ledger, *store.DB) {
	t.Helper()
	driver, dsn := "sqlite", filepath.Join(t.TempDir(), "route.db")
	if pg := os.Getenv("PUNK_TEST_PG_DSN"); pg != "" {
		driver, dsn = "postgres", pg
	}
	db, err := store.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if driver == "postgres" {
		for {
			n, err := db.MigrateDown(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
	}
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}

	specDir := t.TempDir()
	writeSpec(t, specDir, "database.md", dbAgent)
	writeSpec(t, specDir, "network.md", netAgent)
	writeSpec(t, specDir, "application.md", appAgent)
	reg := registry.New(specDir, db, slog.New(slog.DiscardHandler))
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}

	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	ledger := task.NewLedger(db, now)
	return New(db, reg, ledger, nil, now), ledger, db
}

func submit(t *testing.T, l *task.Ledger, labels map[string]string) *task.Task {
	t.Helper()
	tk, _, err := l.Submit(context.Background(), task.SubmitInput{
		Source: "acme", Kind: "investigate", Labels: labels,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestRouteByLabelRule(t *testing.T) {
	r, l, _ := setup(t)
	ctx := context.Background()

	tk := submit(t, l, map[string]string{"domain": "database"})
	d, err := r.Route(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	if d.ChosenAgent != "database" || d.Method != "rule" || d.RuleID != "db-domain" {
		t.Fatalf("decision = %+v", d)
	}
	got, events, err := l.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentName != "database" || got.AgentVersionID == 0 {
		t.Fatalf("assignment not persisted: %+v", got)
	}
	foundRouted := false
	for _, e := range events {
		if e.Type == task.EventRouted {
			foundRouted = true
		}
	}
	if !foundRouted {
		t.Fatal("no routed event on ledger")
	}
}

func TestRoutePriorityBeatsLowerRule(t *testing.T) {
	r, l, _ := setup(t)
	// matches network's net-domain (10) AND net-critical (1) AND
	// application's app-critical (1): priority 10 must win
	tk := submit(t, l, map[string]string{"domain": "network", "severity": "critical"})
	d, err := r.Route(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	if d.ChosenAgent != "network" || d.RuleID != "net-domain" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestRouteTieDeterministicWithoutBreaker(t *testing.T) {
	r, l, _ := setup(t)
	// severity critical matches net-critical(1) and app-critical(1):
	// tie resolves by name asc -> application
	tk := submit(t, l, map[string]string{"severity": "critical"})
	d, err := r.Route(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	if d.ChosenAgent != "application" || d.Method != "rule" {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Candidates) != 2 {
		t.Fatalf("candidates = %v", d.Candidates)
	}
}

func TestRouteNoMatchParksTask(t *testing.T) {
	r, l, _ := setup(t)
	ctx := context.Background()
	tk := submit(t, l, map[string]string{"domain": "storage"})
	d, err := r.Route(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	if d.Method != "fallback" || d.ChosenAgent != "" {
		t.Fatalf("decision = %+v", d)
	}
	got, _, err := l.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInputRequired {
		t.Fatalf("unroutable task status = %s, want input_required", got.Status)
	}
}

func TestRouteFallbackAgent(t *testing.T) {
	r, l, _ := setup(t)
	r.Fallback = "application"
	tk := submit(t, l, map[string]string{"domain": "storage"})
	d, err := r.Route(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	if d.Method != "fallback" || d.ChosenAgent != "application" {
		t.Fatalf("decision = %+v", d)
	}
}

// Replay: identical inputs must re-derive the identical choice, and every
// decision lands in routing_decisions.
func TestRouteReplayDeterminism(t *testing.T) {
	r, l, db := setup(t)
	ctx := context.Background()

	labels := map[string]string{"severity": "critical"}
	t1 := submit(t, l, labels)
	d1, err := r.Route(ctx, t1)
	if err != nil {
		t.Fatal(err)
	}
	// same inputs, fresh task (open-ref dedup would block identical refs)
	t2 := submit(t, l, labels)
	d2, err := r.Route(ctx, t2)
	if err != nil {
		t.Fatal(err)
	}
	if d1.ChosenAgent != d2.ChosenAgent || d1.Method != d2.Method {
		t.Fatalf("replay divergence: %+v vs %+v", d1, d2)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM routing_decisions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("routing_decisions rows = %d, want 2", n)
	}
}

type fakeTieClient struct{ answer string }

func (f *fakeTieClient) Chat(context.Context, []llm.Turn, []llm.Tool) (*llm.Result, error) {
	return &llm.Result{Content: f.answer}, nil
}
func (f *fakeTieClient) Model() string { return "fake" }

func TestLLMTieBreak(t *testing.T) {
	r, l, _ := setup(t)
	r.tie = &LLMTie{Client: &fakeTieClient{answer: "network"}, Reg: r.reg}
	tk := submit(t, l, map[string]string{"severity": "critical"})
	d, err := r.Route(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	if d.Method != "llm" || d.ChosenAgent != "network" {
		t.Fatalf("decision = %+v", d)
	}

	// garbage answer degrades to deterministic order
	r.tie = &LLMTie{Client: &fakeTieClient{answer: "the best agent is clearly X"}, Reg: r.reg}
	tk2 := submit(t, l, map[string]string{"severity": "critical"})
	d2, err := r.Route(context.Background(), tk2)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Method != "rule" || d2.ChosenAgent != "application" {
		t.Fatalf("fallback decision = %+v", d2)
	}
}

func TestPropensityLogging(t *testing.T) {
	r, l, db := setup(t)
	ctx := context.Background()

	// deterministic single-match: propensity 1.0
	tk := submit(t, l, map[string]string{"domain": "database"})
	d, err := r.Route(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	if d.Propensity != 1.0 {
		t.Fatalf("single-match propensity = %v", d.Propensity)
	}

	// forced exploration on a 2-way tie: epsilon=1, pick index 1
	r.Epsilon = 1.0
	r.Rand = func() float64 { return 0.0 }
	r.RandIntn = func(n int) int { return 1 }
	tk2 := submit(t, l, map[string]string{"severity": "critical"})
	d2, err := r.Route(ctx, tk2)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Method != "explore" || d2.Propensity != 0.5 || d2.ChosenAgent != "network" {
		t.Fatalf("explore decision = %+v", d2)
	}

	// greedy pick under epsilon: 1-e+e/n
	r.Epsilon = 0.1
	r.Rand = func() float64 { return 0.99 }
	tk3 := submit(t, l, map[string]string{"severity": "critical"})
	d3, err := r.Route(ctx, tk3)
	if err != nil {
		t.Fatal(err)
	}
	if d3.Method != "rule" || d3.Propensity < 0.949 || d3.Propensity > 0.951 {
		t.Fatalf("greedy decision = %+v", d3)
	}

	var p float64
	if err := db.QueryRowContext(ctx, db.Rebind(
		`SELECT propensity FROM routing_decisions WHERE task_id = $1`), tk2.ID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if p != 0.5 {
		t.Fatalf("recorded propensity = %v", p)
	}
}
