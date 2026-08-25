package cost

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

func seed(t *testing.T) (*store.DB, time.Time) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "cost.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	ledger := task.NewLedger(db, func() time.Time { return now })
	tk, _, err := ledger.Submit(ctx, task.SubmitInput{Source: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, db.Rebind(`
		UPDATE tasks SET status='completed', agent_name='database' WHERE id=$1`), tk.ID); err != nil {
		t.Fatal(err)
	}
	ins := func(agent, at string, cost int64, taskID any) {
		if _, err := db.ExecContext(ctx, db.Rebind(`
			INSERT INTO llm_calls (task_id, profile, model, prompt_tokens, completion_tokens, latency_ms, created_at, cost_microusd, agent_name)
			VALUES ($1,'default','m',10,5,100,$2,$3,$4)`), taskID, at, cost, agent); err != nil {
			t.Fatal(err)
		}
	}
	ins("database", store.TimeToDB(now.Add(-2*time.Hour)), 2_000_000, tk.ID) // today, $2
	ins("database", store.TimeToDB(now.Add(-40*time.Hour)), 5_000_000, nil)  // this month, not today
	ins("network", store.TimeToDB(now.Add(-1*time.Hour)), 1_000_000, nil)    // today, $1
	return db, now
}

func TestDailySpendAndSummary(t *testing.T) {
	db, now := seed(t)
	ctx := context.Background()

	spend, err := DailySpendByAgent(ctx, db, "database", now)
	if err != nil || spend != 2_000_000 {
		t.Fatalf("daily = %d err=%v", spend, err)
	}
	o, err := Summarize(ctx, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if o.DayTotalMicroUSD != 3_000_000 || o.MonthTotalMicroUSD != 8_000_000 {
		t.Fatalf("totals = %+v", o)
	}
	var dbRow *AgentSpend
	for i := range o.Agents {
		if o.Agents[i].Agent == "database" {
			dbRow = &o.Agents[i]
		}
	}
	if dbRow == nil || dbRow.CompletedTasks != 1 || dbRow.AvgPerTask != 2_000_000 {
		t.Fatalf("burn table row = %+v", dbRow)
	}
}

func TestProjectorLevels(t *testing.T) {
	db, now := seed(t) // $3 spent by 12:00 -> projected $6/day
	b := bus.New()
	events, cancel := b.Subscribe()
	defer cancel()
	p := &Projector{DB: db, Bus: b, Log: slog.New(slog.DiscardHandler),
		DailyBudgetUS: 7_000_000, Now: func() time.Time { return now }}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-events:
		if e.Kind != "cost_alert" || e.Key != "80" {
			t.Fatalf("alert = %+v, want level 80", e)
		}
	default:
		t.Fatal("no alert at 85% projection")
	}
	// same level does not re-alert
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-events:
		t.Fatalf("duplicate alert: %+v", e)
	default:
	}
}
