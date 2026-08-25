package policy

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Proposal is one action awaiting an external approver. Punk Records never
// executes; the consumer reports the outcome back over the API.
type Proposal struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	ActionClass string    `json:"action_class"`
	Target      string    `json:"target"` // tool or runbook identifier
	Params      string    `json:"params"` // JSON
	Status      string    `json:"status"` // proposed|submitted|approved|rejected|expired
	ExternalRef string    `json:"external_ref,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

var proposalStatuses = map[string]bool{
	"proposed": true, "submitted": true, "approved": true, "rejected": true, "expired": true,
}

// Proposals persists proposal state.
type Proposals struct {
	db  *store.DB
	now func() time.Time
}

func NewProposals(db *store.DB, now func() time.Time) *Proposals {
	if now == nil {
		now = time.Now
	}
	return &Proposals{db: db, now: now}
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Create records a new proposal in status proposed.
func (p *Proposals) Create(ctx context.Context, taskID, actionClass, target, params string) (*Proposal, error) {
	if params == "" {
		params = "{}"
	}
	pr := &Proposal{
		ID: newID(), TaskID: taskID, ActionClass: actionClass, Target: target,
		Params: params, Status: "proposed",
		CreatedAt: p.now().UTC(), UpdatedAt: p.now().UTC(),
	}
	_, err := p.db.ExecContext(ctx, p.db.Rebind(`
		INSERT INTO proposals (id, task_id, action_class, target, params, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`),
		pr.ID, pr.TaskID, pr.ActionClass, pr.Target, pr.Params, pr.Status,
		store.TimeToDB(pr.CreatedAt), store.TimeToDB(pr.UpdatedAt))
	if err != nil {
		return nil, fmt.Errorf("create proposal: %w", err)
	}
	return pr, nil
}

// UpdateStatus moves a proposal along its lifecycle (consumer callback).
func (p *Proposals) UpdateStatus(ctx context.Context, id, status, externalRef string) (*Proposal, error) {
	if !proposalStatuses[status] {
		return nil, fmt.Errorf("proposal: unknown status %q", status)
	}
	var ext any
	if externalRef != "" {
		ext = externalRef
	}
	res, err := p.db.ExecContext(ctx, p.db.Rebind(`
		UPDATE proposals SET status = $1,
		       external_ref = COALESCE($2, external_ref),
		       updated_at = $3
		WHERE id = $4`),
		status, ext, store.TimeToDB(p.now()), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("proposal %s not found", id)
	}
	return p.Get(ctx, id)
}

// Get fetches one proposal.
func (p *Proposals) Get(ctx context.Context, id string) (*Proposal, error) {
	row := p.db.QueryRowContext(ctx, p.db.Rebind(`
		SELECT id, task_id, action_class, target, params, status,
		       COALESCE(external_ref,''), created_at, updated_at
		FROM proposals WHERE id = $1`), id)
	return scanProposal(row)
}

// ListByStatus returns proposals in a lifecycle state, newest first
// (the approval inbox's feed).
func (p *Proposals) ListByStatus(ctx context.Context, status string, limit int) ([]Proposal, error) {
	if !proposalStatuses[status] {
		return nil, fmt.Errorf("proposal: unknown status %q", status)
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := p.db.QueryContext(ctx, p.db.Rebind(`
		SELECT id, task_id, action_class, target, params, status,
		       COALESCE(external_ref,''), created_at, updated_at
		FROM proposals WHERE status = $1 ORDER BY created_at DESC LIMIT $2`), status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		pr, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *pr)
	}
	return out, rows.Err()
}

// ListByTask returns a task's proposals, oldest first.
func (p *Proposals) ListByTask(ctx context.Context, taskID string) ([]Proposal, error) {
	rows, err := p.db.QueryContext(ctx, p.db.Rebind(`
		SELECT id, task_id, action_class, target, params, status,
		       COALESCE(external_ref,''), created_at, updated_at
		FROM proposals WHERE task_id = $1 ORDER BY created_at`), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		pr, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *pr)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanProposal(row rowScanner) (*Proposal, error) {
	var pr Proposal
	var created, updated string
	err := row.Scan(&pr.ID, &pr.TaskID, &pr.ActionClass, &pr.Target, &pr.Params,
		&pr.Status, &pr.ExternalRef, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("proposal not found")
	}
	if err != nil {
		return nil, err
	}
	if pr.CreatedAt, err = store.TimeFromDB(created); err != nil {
		return nil, err
	}
	if pr.UpdatedAt, err = store.TimeFromDB(updated); err != nil {
		return nil, err
	}
	return &pr, nil
}

// ExpireStale moves proposals stuck in proposed/submitted past maxAge to
// expired (P10.5). Approval decisions must be made while context is
// fresh; ancient proposals are noise and risk.
func (p *Proposals) ExpireStale(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := store.TimeToDB(p.now().Add(-maxAge))
	res, err := p.db.ExecContext(ctx, p.db.Rebind(`
		UPDATE proposals SET status = 'expired', updated_at = $1
		WHERE status IN ('proposed','submitted') AND created_at < $2`),
		store.TimeToDB(p.now()), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
