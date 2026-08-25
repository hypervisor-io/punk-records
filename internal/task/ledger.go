package task

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Ledger is the only writer of tasks and task_events. Status changes go
// through SetStatus (single write path, no drift between table and log).
type Ledger struct {
	db  *store.DB
	now func() time.Time

	// Notify fires after a committed status change (nil = disabled).
	// Consumers: MCP resource subscriptions, later the outbox tailer.
	Notify func(taskID, status string)
}

func (l *Ledger) notify(taskID, status string) {
	if l.Notify != nil {
		l.Notify(taskID, status)
	}
}

func NewLedger(db *store.DB, now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{db: db, now: now}
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// isUniqueViolation matches both engines' duplicate-key errors.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || // sqlite
		strings.Contains(msg, "SQLSTATE 23505") || // pgx wrapped
		strings.Contains(msg, "duplicate key value") // pg text
}

// SubmitInput is one task intake.
type SubmitInput struct {
	ExternalRef     string
	Source          string
	Kind            string
	Labels          map[string]string
	Budget          Budget // resolved defaults; agent spec may lower at claim
	ParentID        string // subagent tree
	HandoffContract string // parent edge contract; empty = results_only
	Actor           string
}

// Submit creates a task (dedup: one OPEN task per external_ref). Returns
// created=false with the existing open task when deduped.
func (l *Ledger) Submit(ctx context.Context, in SubmitInput) (*Task, bool, error) {
	if in.Kind == "" {
		in.Kind = "investigate"
	}
	if in.Actor == "" {
		in.Actor = "system"
	}
	labels := "{}"
	if len(in.Labels) > 0 {
		raw, err := json.Marshal(in.Labels)
		if err != nil {
			return nil, false, err
		}
		labels = string(raw)
	}
	contract := in.HandoffContract
	if contract == "" {
		contract = HandoffDefault
	}
	t := &Task{
		ID: newID(), ExternalRef: in.ExternalRef, Source: in.Source, Kind: in.Kind,
		Status: StatusSubmitted, HandoffContract: contract, Labels: in.Labels,
		Budget: in.Budget, ParentTaskID: in.ParentID,
		CreatedAt: l.now().UTC(), UpdatedAt: l.now().UTC(),
	}

	var extRef any
	if in.ExternalRef != "" {
		extRef = in.ExternalRef
	}
	var parent any
	if in.ParentID != "" {
		parent = in.ParentID
	}

	err := l.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, l.db.Rebind(`
			INSERT INTO tasks (id, external_ref, source, kind, status, handoff_contract,
			                   parent_task_id, labels,
			                   budget_tokens, budget_tool_calls, budget_wall_ms, budget_subagents,
			                   created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`),
			t.ID, extRef, t.Source, t.Kind, t.Status, t.HandoffContract, parent, labels,
			t.Budget.Tokens, t.Budget.ToolCalls, t.Budget.WallMS, t.Budget.Subagents,
			store.TimeToDB(t.CreatedAt), store.TimeToDB(t.UpdatedAt))
		if err != nil {
			return err
		}
		return l.appendTx(ctx, tx, t.ID, EventSubmitted, in.Actor, map[string]any{
			"source": in.Source, "external_ref": in.ExternalRef, "kind": in.Kind,
		})
	})
	if err == nil {
		return t, true, nil
	}
	if !isUniqueViolation(err) {
		return nil, false, err
	}
	// deduped: fetch the open task holding the ref
	existing, ferr := l.findOpenByRef(ctx, in.ExternalRef)
	if ferr != nil {
		return nil, false, fmt.Errorf("dedup fetch after conflict: %w", ferr)
	}
	return existing, false, nil
}

// HandoffDefault mirrors spec.HandoffResultsOnly without importing spec
// (task must stay dependency-light for every other package).
const HandoffDefault = "results_only"

func (l *Ledger) findOpenByRef(ctx context.Context, ref string) (*Task, error) {
	row := l.db.QueryRowContext(ctx, l.db.Rebind(selectTask+`
		WHERE external_ref = $1 AND status IN ('submitted','working','input_required')`), ref)
	return scanTask(row)
}

// nextSeq must run inside the tx; postgres serializes appends per task by
// locking the task row (sqlite's single writer already serializes).
func (l *Ledger) nextSeq(ctx context.Context, tx *sql.Tx, taskID string) (int64, error) {
	if l.db.Driver == "postgres" {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("task %s not found", taskID)
			}
			return 0, err
		}
	}
	var seq int64
	err := tx.QueryRowContext(ctx, l.db.Rebind(
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM task_events WHERE task_id = $1`), taskID).Scan(&seq)
	return seq, err
}

func (l *Ledger) appendTx(ctx context.Context, tx *sql.Tx, taskID, typ, actor string, payload any) error {
	_, err := l.appendTxSeq(ctx, tx, taskID, typ, actor, payload)
	return err
}

func (l *Ledger) appendTxSeq(ctx context.Context, tx *sql.Tx, taskID, typ, actor string, payload any) (int64, error) {
	seq, err := l.nextSeq(ctx, tx, taskID)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, l.db.Rebind(`
		INSERT INTO task_events (task_id, seq, type, actor, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`),
		taskID, seq, typ, actor, string(raw), store.TimeToDB(l.now()))
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// Append writes one event and returns its per-task sequence number so
// callers (the runtime) can hand models citable references.
func (l *Ledger) Append(ctx context.Context, taskID, typ, actor string, payload any) (int64, error) {
	var seq int64
	err := l.db.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		seq, e = l.appendTxSeq(ctx, tx, taskID, typ, actor, payload)
		return e
	})
	return seq, err
}

// SetStatus is the single path for status changes: validates the
// transition, updates the row, appends the status_change event, same tx.
func (l *Ledger) SetStatus(ctx context.Context, taskID, to, actor, reason string) error {
	err := l.db.WithTx(ctx, func(tx *sql.Tx) error {
		var from string
		q := `SELECT status FROM tasks WHERE id = $1`
		if l.db.Driver == "postgres" {
			q += ` FOR UPDATE`
		}
		if err := tx.QueryRowContext(ctx, l.db.Rebind(q), taskID).Scan(&from); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("task %s not found", taskID)
			}
			return err
		}
		if err := CheckTransition(from, to); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, l.db.Rebind(
			`UPDATE tasks SET status = $1, updated_at = $2 WHERE id = $3`),
			to, store.TimeToDB(l.now()), taskID); err != nil {
			return err
		}
		return l.appendTx(ctx, tx, taskID, EventStatusChange, actor,
			StatusChangePayload{From: from, To: to, Reason: reason})
	})
	if err == nil {
		l.notify(taskID, to)
	}
	return err
}

// Claim moves submitted -> working for an agent, pinning the spec version
// and taking a lease.
func (l *Ledger) Claim(ctx context.Context, taskID, agent string, versionID int64, lease time.Duration) error {
	err := l.db.WithTx(ctx, func(tx *sql.Tx) error {
		var from string
		q := `SELECT status FROM tasks WHERE id = $1`
		if l.db.Driver == "postgres" {
			q += ` FOR UPDATE`
		}
		if err := tx.QueryRowContext(ctx, l.db.Rebind(q), taskID).Scan(&from); err != nil {
			return err
		}
		if err := CheckTransition(from, StatusWorking); err != nil {
			return err
		}
		now := l.now().UTC()
		if _, err := tx.ExecContext(ctx, l.db.Rebind(`
			UPDATE tasks SET status = $1, agent_name = $2, agent_version_id = $3,
			       started_at = $4, lease_expires_at = $5, updated_at = $4
			WHERE id = $6`),
			StatusWorking, agent, versionID,
			store.TimeToDB(now), store.TimeToDB(now.Add(lease)), taskID); err != nil {
			return err
		}
		return l.appendTx(ctx, tx, taskID, EventStatusChange, "agent:"+agent,
			StatusChangePayload{From: from, To: StatusWorking, Reason: "claimed"})
	})
	if err == nil {
		l.notify(taskID, StatusWorking)
	}
	return err
}

// ExtendLease refreshes a working task's lease; the runtime calls this
// every iteration so live investigations are never reaped (P10.2).
func (l *Ledger) ExtendLease(ctx context.Context, taskID string, lease time.Duration) error {
	res, err := l.db.ExecContext(ctx, l.db.Rebind(`
		UPDATE tasks SET lease_expires_at = $1, updated_at = $2
		WHERE id = $3 AND status = 'working'`),
		store.TimeToDB(l.now().Add(lease)), store.TimeToDB(l.now()), taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("extend lease: task %s not working", taskID)
	}
	return nil
}

// Requeue returns a parked (input_required) task to the dispatcher:
// input_required -> working -> submitted, audited (P15 inbox resume).
func (l *Ledger) Requeue(ctx context.Context, taskID, actor, reason string) error {
	if err := l.SetStatus(ctx, taskID, StatusWorking, actor, "requeue: "+reason); err != nil {
		return err
	}
	return l.SetStatus(ctx, taskID, StatusSubmitted, actor, "requeue: "+reason)
}

// ReapExpired requeues working tasks whose lease has lapsed. History is
// preserved; the runtime re-claims with full event context.
func (l *Ledger) ReapExpired(ctx context.Context) (int, error) {
	rows, err := l.db.QueryContext(ctx, l.db.Rebind(`
		SELECT id FROM tasks WHERE status = 'working' AND lease_expires_at < $1`),
		store.TimeToDB(l.now()))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := l.db.WithTx(ctx, func(tx *sql.Tx) error {
			if err := l.appendTx(ctx, tx, id, EventError, "system",
				map[string]string{"error": "lease expired, requeued"}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, l.db.Rebind(`
				UPDATE tasks SET status = 'submitted', lease_expires_at = NULL, updated_at = $1
				WHERE id = $2 AND status = 'working'`),
				store.TimeToDB(l.now()), id); err != nil {
				return err
			}
			return l.appendTx(ctx, tx, id, EventStatusChange, "system",
				StatusChangePayload{From: StatusWorking, To: StatusSubmitted, Reason: "lease expired"})
		})
		if err != nil {
			return n, err
		}
		l.notify(id, StatusSubmitted)
		n++
	}
	return n, nil
}

// AssignAgent records the router's choice without claiming.
func (l *Ledger) AssignAgent(ctx context.Context, taskID, agent string, versionID int64, actor string, decision any) error {
	return l.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, l.db.Rebind(
			`UPDATE tasks SET agent_name = $1, agent_version_id = $2, updated_at = $3 WHERE id = $4`),
			agent, versionID, store.TimeToDB(l.now()), taskID); err != nil {
			return err
		}
		return l.appendTx(ctx, tx, taskID, EventRouted, actor, decision)
	})
}

const selectTask = `
SELECT id, external_ref, source, kind, status, agent_name, agent_version_id,
       parent_task_id, handoff_contract, labels,
       budget_tokens, budget_tool_calls, budget_wall_ms, budget_subagents,
       spent_tokens, spent_tool_calls, started_at, lease_expires_at,
       created_at, updated_at
FROM tasks`

type rowScanner interface{ Scan(dest ...any) error }

func scanTask(row rowScanner) (*Task, error) {
	var t Task
	var extRef, parent, started, lease sql.NullString
	var agentVerID sql.NullInt64
	var labels, createdAt, updatedAt string
	err := row.Scan(&t.ID, &extRef, &t.Source, &t.Kind, &t.Status, &t.AgentName, &agentVerID,
		&parent, &t.HandoffContract, &labels,
		&t.Budget.Tokens, &t.Budget.ToolCalls, &t.Budget.WallMS, &t.Budget.Subagents,
		&t.SpentTokens, &t.SpentToolCalls, &started, &lease, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("task not found")
	}
	if err != nil {
		return nil, err
	}
	t.ExternalRef = extRef.String
	t.AgentVersionID = agentVerID.Int64
	t.ParentTaskID = parent.String
	if labels != "" && labels != "{}" {
		if err := json.Unmarshal([]byte(labels), &t.Labels); err != nil {
			return nil, fmt.Errorf("task %s: bad labels json: %w", t.ID, err)
		}
	}
	parseT := func(s sql.NullString, dst **time.Time) error {
		if !s.Valid || s.String == "" {
			return nil
		}
		v, err := store.TimeFromDB(s.String)
		if err != nil {
			return err
		}
		*dst = &v
		return nil
	}
	if err := parseT(started, &t.StartedAt); err != nil {
		return nil, err
	}
	if err := parseT(lease, &t.LeaseExpiresAt); err != nil {
		return nil, err
	}
	if t.CreatedAt, err = store.TimeFromDB(createdAt); err != nil {
		return nil, err
	}
	if t.UpdatedAt, err = store.TimeFromDB(updatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// Get returns a task with its full event history, oldest first.
func (l *Ledger) Get(ctx context.Context, id string) (*Task, []Event, error) {
	t, err := scanTask(l.db.QueryRowContext(ctx, l.db.Rebind(selectTask+` WHERE id = $1`), id))
	if err != nil {
		return nil, nil, err
	}
	rows, err := l.db.QueryContext(ctx, l.db.Rebind(`
		SELECT id, task_id, seq, type, actor, payload, created_at
		FROM task_events WHERE task_id = $1 ORDER BY seq`), id)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []Event
	for rows.Next() {
		var e Event
		var payload, createdAt string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Seq, &e.Type, &e.Actor, &payload, &createdAt); err != nil {
			return nil, nil, err
		}
		e.Payload = json.RawMessage(payload)
		if e.CreatedAt, err = store.TimeFromDB(createdAt); err != nil {
			return nil, nil, err
		}
		events = append(events, e)
	}
	return t, events, rows.Err()
}

// Snapshot stores one tool call's FULL result for offline replay.
func (l *Ledger) Snapshot(ctx context.Context, taskID string, seq int64, tool, args, result, status string) error {
	_, err := l.db.ExecContext(ctx, l.db.Rebind(`
		INSERT INTO task_snapshots (task_id, seq, tool, args, result, status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`),
		taskID, seq, tool, args, result, status, store.TimeToDB(l.now()))
	return err
}

// SnapshotRow is one frozen tool interaction.
type SnapshotRow struct {
	Seq    int64  `json:"seq"`
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Result string `json:"result"`
	Status string `json:"status"`
}

// Snapshots returns a task's frozen world, oldest first.
func (l *Ledger) Snapshots(ctx context.Context, taskID string) ([]SnapshotRow, error) {
	rows, err := l.db.QueryContext(ctx, l.db.Rebind(`
		SELECT seq, tool, args, result, status FROM task_snapshots
		WHERE task_id = $1 ORDER BY seq`), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SnapshotRow{}
	for rows.Next() {
		var r SnapshotRow
		if err := rows.Scan(&r.Seq, &r.Tool, &r.Args, &r.Result, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FindLatestByRef returns the newest task (any status) for a consumer
// reference: label harvesting attaches ground truth after tasks close.
func (l *Ledger) FindLatestByRef(ctx context.Context, ref string) (*Task, error) {
	row := l.db.QueryRowContext(ctx, l.db.Rebind(selectTask+`
		WHERE external_ref = $1 ORDER BY created_at DESC LIMIT 1`), ref)
	return scanTask(row)
}

// CountChildren counts a task's direct subagent tasks.
func (l *Ledger) CountChildren(ctx context.Context, parentID string) (int, error) {
	var n int
	err := l.db.QueryRowContext(ctx, l.db.Rebind(
		`SELECT count(*) FROM tasks WHERE parent_task_id = $1`), parentID).Scan(&n)
	return n, err
}

// List returns tasks newest first, optionally filtered by status.
func (l *Ledger) List(ctx context.Context, status string, limit int) ([]Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q, args := selectTask+` ORDER BY created_at DESC LIMIT $1`, []any{limit}
	if status != "" {
		if _, ok := transitions[status]; !ok {
			return nil, fmt.Errorf("unknown status %q", status)
		}
		q, args = selectTask+` WHERE status = $1 ORDER BY created_at DESC LIMIT $2`, []any{status, limit}
	}
	rows, err := l.db.QueryContext(ctx, l.db.Rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
