package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// ErrBudgetExhausted is the typed refusal when a task hits a cap. The
// task is parked input_required, never silently truncated.
type ErrBudgetExhausted struct{ Kind string }

func (e *ErrBudgetExhausted) Error() string {
	return fmt.Sprintf("task: budget exhausted: %s", e.Kind)
}

// SpendTokens adds LLM usage to the task and enforces the token cap.
func (l *Ledger) SpendTokens(ctx context.Context, taskID string, prompt, completion int) error {
	return l.spend(ctx, taskID, "tokens", prompt+completion)
}

// SpendToolCall counts one tool invocation against the cap.
func (l *Ledger) SpendToolCall(ctx context.Context, taskID string) error {
	return l.spend(ctx, taskID, "tool_calls", 1)
}

func (l *Ledger) spend(ctx context.Context, taskID, kind string, amount int) error {
	exhausted := false
	err := l.db.WithTx(ctx, func(tx *sql.Tx) error {
		q := `SELECT status, budget_tokens, budget_tool_calls, spent_tokens, spent_tool_calls
		      FROM tasks WHERE id = $1`
		if l.db.Driver == "postgres" {
			q += ` FOR UPDATE`
		}
		var status string
		var bTok, bCalls, sTok, sCalls int
		if err := tx.QueryRowContext(ctx, l.db.Rebind(q), taskID).Scan(&status, &bTok, &bCalls, &sTok, &sCalls); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("task %s not found", taskID)
			}
			return err
		}

		var col string
		var newSpent, limit int
		switch kind {
		case "tokens":
			col, newSpent, limit = "spent_tokens", sTok+amount, bTok
		case "tool_calls":
			col, newSpent, limit = "spent_tool_calls", sCalls+amount, bCalls
		default:
			return fmt.Errorf("unknown budget kind %q", kind)
		}

		if _, err := tx.ExecContext(ctx, l.db.Rebind(
			fmt.Sprintf(`UPDATE tasks SET %s = $1, updated_at = $2 WHERE id = $3`, col)),
			newSpent, store.TimeToDB(l.now()), taskID); err != nil {
			return err
		}
		if limit > 0 && newSpent > limit {
			exhausted = true
			if err := l.appendTx(ctx, tx, taskID, EventBudgetExhausted, "system",
				map[string]any{"kind": kind, "spent": newSpent, "budget": limit}); err != nil {
				return err
			}
			// park a working task; other states keep their status but the
			// caller still gets the typed error
			if status == StatusWorking {
				if _, err := tx.ExecContext(ctx, l.db.Rebind(
					`UPDATE tasks SET status = $1, updated_at = $2 WHERE id = $3`),
					StatusInputRequired, store.TimeToDB(l.now()), taskID); err != nil {
					return err
				}
				return l.appendTx(ctx, tx, taskID, EventStatusChange, "system",
					StatusChangePayload{From: StatusWorking, To: StatusInputRequired,
						Reason: "budget exhausted: " + kind})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if exhausted {
		l.notify(taskID, StatusInputRequired)
		return &ErrBudgetExhausted{Kind: kind}
	}
	return nil
}
