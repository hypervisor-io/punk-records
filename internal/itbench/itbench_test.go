package itbench

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

const exampleDir = "../../scenarios/itbench/pool-exhaustion"

func TestLoadScenario(t *testing.T) {
	s, err := Load(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.GroundTruth.FaultyEntities; len(got) != 1 || got[0] != "checkout-service" {
		t.Fatalf("faulty_entities = %v", got)
	}
	if s.GroundTruth.FaultType != "connection_pool_exhaustion" {
		t.Fatalf("fault_type = %q", s.GroundTruth.FaultType)
	}
	for name, data := range map[string]string{
		"alerts": s.Alerts, "metrics": s.Metrics, "k8s_events": s.K8sEvents,
		"logs": s.Logs, "traces": s.Traces,
	} {
		if strings.TrimSpace(data) == "" {
			t.Fatalf("scenario %s snapshot is empty", name)
		}
	}
}

func TestLoadDir(t *testing.T) {
	scns, err := LoadDir(filepath.Dir(exampleDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(scns) == 0 {
		t.Fatal("want at least one scenario")
	}
}

func TestFrozenTools(t *testing.T) {
	s, err := Load(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	tl := NewTools(s)
	if len(tl.Tools()) != 6 {
		t.Fatalf("want 6 tools, got %d", len(tl.Tools()))
	}
	alerts, err := tl.Call(context.Background(), "get_alerts", nil)
	if err != nil || !strings.Contains(alerts, "checkout-service") {
		t.Fatalf("get_alerts = %q, %v", alerts, err)
	}
	// filtered logs
	logs, _ := tl.Call(context.Background(), "get_logs", []byte(`{"filter":"pool"}`))
	if !strings.Contains(logs, "pool") || strings.Contains(logs, "upstream cart-service returned") {
		t.Fatalf("filtered logs wrong: %q", logs)
	}
	if _, err := tl.Call(context.Background(), "nope", nil); err == nil {
		t.Fatal("want error for unknown tool")
	}
}

func TestScoreDiagnosis(t *testing.T) {
	gt := GroundTruth{FaultyEntities: []string{"checkout-service"}}
	if sc := ScoreDiagnosis("root cause: checkout-service exhausted its DB pool", gt); !sc.Resolved || sc.Recall != 1 {
		t.Fatalf("hit case: %+v", sc)
	}
	if sc := ScoreDiagnosis("cart-service is slow and web-frontend errors", gt); sc.Resolved {
		t.Fatalf("miss case should not resolve: %+v", sc)
	}
	// separator-insensitive: checkout_service matches checkout-service
	if sc := ScoreDiagnosis("the checkout_service deployment is the culprit", gt); !sc.Resolved {
		t.Fatalf("separator case should resolve: %+v", sc)
	}
}

func TestPassHatK(t *testing.T) {
	if !PassHatK([]bool{false, true, false}) {
		t.Fatal("any-pass should be true")
	}
	if PassHatK([]bool{false, false}) {
		t.Fatal("all-fail should be false")
	}
}

// TestRunNoModel exercises the full runner path deterministically: with AI
// disabled the agent cannot diagnose, so the scenario parks unresolved -
// but the wiring (submit, assign, frozen tools, score) must not error.
func TestRunNoModel(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "it.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	reg := registry.New("../../specs", db, log)
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reg.Get("sre"); !ok {
		t.Fatal("sre agent spec must load from ../../specs")
	}
	s, err := Load(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Ledger: task.NewLedger(db, nil), Reg: reg,
		LLM: llm.NewManager(false, nil, db, nil), Agent: "sre", Log: log,
	}
	res, err := Run(context.Background(), deps, s)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Score.Resolved {
		t.Fatal("no-model run must not resolve")
	}
	if res.Status == "" {
		t.Fatal("expected a terminal-ish status")
	}
}
