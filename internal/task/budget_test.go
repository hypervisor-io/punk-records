package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

func claimedTask(t *testing.T, l *Ledger, clk *fakeClock, budget Budget) *Task {
	t.Helper()
	ctx := context.Background()
	var verID int64
	if err := l.db.QueryRowContext(ctx, l.db.Rebind(`
		INSERT INTO agent_versions (name, version, content_hash, loaded_at, active)
		VALUES ('database', '0.1.0', 'h', $1, TRUE) RETURNING id`),
		store.TimeToDB(clk.Now())).Scan(&verID); err != nil {
		t.Fatal(err)
	}
	tk, _, err := l.Submit(ctx, SubmitInput{Source: "test", Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Claim(ctx, tk.ID, "database", verID, time.Hour); err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestBudgetTokensParksTask(t *testing.T) {
	l, _, clk := newTest(t)
	ctx := context.Background()
	tk := claimedTask(t, l, clk, Budget{Tokens: 100, ToolCalls: 10})

	if err := l.SpendTokens(ctx, tk.ID, 40, 20); err != nil {
		t.Fatalf("under budget errored: %v", err)
	}
	err := l.SpendTokens(ctx, tk.ID, 30, 20) // 110 > 100
	var ex *ErrBudgetExhausted
	if !errors.As(err, &ex) || ex.Kind != "tokens" {
		t.Fatalf("err = %v, want ErrBudgetExhausted tokens", err)
	}

	got, events, err := l.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInputRequired {
		t.Fatalf("status = %s, want input_required", got.Status)
	}
	if got.SpentTokens != 110 {
		t.Fatalf("spent_tokens = %d, want 110 (spend recorded even when over)", got.SpentTokens)
	}
	found := false
	for _, e := range events {
		if e.Type == EventBudgetExhausted {
			found = true
		}
	}
	if !found {
		t.Fatal("no budget_exhausted event")
	}
}

func TestBudgetToolCalls(t *testing.T) {
	l, _, clk := newTest(t)
	ctx := context.Background()
	tk := claimedTask(t, l, clk, Budget{Tokens: 1000, ToolCalls: 2})

	if err := l.SpendToolCall(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if err := l.SpendToolCall(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	err := l.SpendToolCall(ctx, tk.ID)
	var ex *ErrBudgetExhausted
	if !errors.As(err, &ex) || ex.Kind != "tool_calls" {
		t.Fatalf("err = %v, want ErrBudgetExhausted tool_calls", err)
	}
}

func TestBudgetZeroMeansUncapped(t *testing.T) {
	l, _, clk := newTest(t)
	ctx := context.Background()
	tk := claimedTask(t, l, clk, Budget{})
	if err := l.SpendTokens(ctx, tk.ID, 1_000_000, 0); err != nil {
		t.Fatalf("uncapped spend errored: %v", err)
	}
}
