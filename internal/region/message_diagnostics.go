package region

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Messaging delivery diagnostics: one latest bridge observation per
// namespace member, replaced on every report. A snapshot says what a
// delivery bridge last reported about its own wait/failure state. It is
// never proof that a model received or completed anything, and it never
// overrides liveness (region_members, open streams) or ACK state
// (agent_messages), which remain the authority.

const (
	// MessageDiagnosticStaleAfter: a snapshot whose server-assigned
	// updated_at is older than this (by the server clock) is reported as
	// stale, i.e. "last reported", not current.
	MessageDiagnosticStaleAfter = 120 * time.Second
	// MaxMessageDiagnosticCount bounds pending_ack_count and wake_count.
	MaxMessageDiagnosticCount = 10000
	// MaxMessageDiagnosticList bounds one region listing.
	MaxMessageDiagnosticList = 1000
	// maxDiagToken bounds client names and last_error reasons, in bytes.
	maxDiagToken = 64
	// maxDiagTime bounds a reported RFC3339 timestamp, in bytes.
	maxDiagTime = 64
)

var (
	diagnosticModes = map[string]bool{"idle_wake": true, "hook_continuation": true, "catch_up": true}
	// diagnosticStates are bridge observations, not delivery outcomes.
	diagnosticStates = map[string]bool{
		"ready": true, "waiting_for_idle": true, "waiting_for_next_prompt": true,
		"wake_budget_exhausted": true, "sender_filtered": true, "delivery_failed": true,
		"handoff_unconfirmed": true, "disabled": true,
	}
)

// MessageDiagnosticInput is one bridge report. The namespace comes from
// the authorized path; timestamps are RFC3339 and empty clears them.
type MessageDiagnosticInput struct {
	Namespace       string `json:"-"`
	Agent           string `json:"agent"`
	Client          string `json:"client"`
	DeliveryMode    string `json:"delivery_mode"`
	State           string `json:"state"`
	LastAttemptAt   string `json:"last_attempt_at,omitempty"`
	NextAttemptAt   string `json:"next_attempt_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	PendingAckCount int    `json:"pending_ack_count,omitempty"`
	WakeCount       int    `json:"wake_count,omitempty"`
}

// MessageDiagnostic is a stored latest snapshot plus server-computed
// staleness. Timestamps use the store's fixed-width UTC encoding.
type MessageDiagnostic struct {
	Namespace       string `json:"namespace"`
	Agent           string `json:"agent"`
	Client          string `json:"client"`
	DeliveryMode    string `json:"delivery_mode"`
	State           string `json:"state"`
	LastAttemptAt   string `json:"last_attempt_at,omitempty"`
	NextAttemptAt   string `json:"next_attempt_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	PendingAckCount int    `json:"pending_ack_count"`
	WakeCount       int    `json:"wake_count"`
	UpdatedAt       string `json:"updated_at"`
	Stale           bool   `json:"stale"`
}

// checkDiagToken accepts a short lowercase machine token: first byte
// [a-z0-9], then [a-z0-9] plus extra punctuation. It rejects free text,
// so raw errors (spaces, URLs with '/', '?', '=') cannot be stored.
func checkDiagToken(field, v, extra string, required bool) error {
	if v == "" {
		if required {
			return fmt.Errorf("%w: %s required", ErrMessageInvalid, field)
		}
		return nil
	}
	if len(v) > maxDiagToken {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrMessageInvalid, field, maxDiagToken)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (i > 0 && strings.IndexByte(extra, c) >= 0)
		if !ok {
			return fmt.Errorf("%w: %s must be a short lowercase machine reason", ErrMessageInvalid, field)
		}
	}
	return nil
}

// normalizeDiagTime parses an optional RFC3339 timestamp and re-encodes
// it in the portable storage format; empty stays empty (clears).
func normalizeDiagTime(field, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if len(v) > maxDiagTime {
		return "", fmt.Errorf("%w: %s exceeds %d bytes", ErrMessageInvalid, field, maxDiagTime)
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return "", fmt.Errorf("%w: %s must be RFC3339", ErrMessageInvalid, field)
	}
	return store.TimeToDB(t), nil
}

func checkDiagCount(field string, n int) error {
	if n < 0 || n > MaxMessageDiagnosticCount {
		return fmt.Errorf("%w: %s must be 0 to %d", ErrMessageInvalid, field, MaxMessageDiagnosticCount)
	}
	return nil
}

// validate checks every field and returns the normalized timestamps.
func (in MessageDiagnosticInput) validate() (last, next string, err error) {
	if err = checkInbox(in.Namespace, in.Agent); err != nil {
		return
	}
	if err = checkDiagToken("client", in.Client, "._-", true); err != nil {
		return
	}
	if !diagnosticModes[in.DeliveryMode] {
		err = fmt.Errorf("%w: delivery_mode must be idle_wake, hook_continuation or catch_up", ErrMessageInvalid)
		return
	}
	if !diagnosticStates[in.State] {
		err = fmt.Errorf("%w: unknown state", ErrMessageInvalid)
		return
	}
	if err = checkDiagToken("last_error", in.LastError, "_.:-", false); err != nil {
		return
	}
	if err = checkDiagCount("pending_ack_count", in.PendingAckCount); err != nil {
		return
	}
	if err = checkDiagCount("wake_count", in.WakeCount); err != nil {
		return
	}
	if last, err = normalizeDiagTime("last_attempt_at", in.LastAttemptAt); err != nil {
		return
	}
	next, err = normalizeDiagTime("next_attempt_at", in.NextAttemptAt)
	return
}

// RecordMessageDiagnostic replaces the latest snapshot for a registered
// member. It requires existing membership (ErrMessageNotFound otherwise),
// never creates membership, and never counts as a liveness sighting.
// updated_at is the server clock.
func (s *Store) RecordMessageDiagnostic(ctx context.Context, in MessageDiagnosticInput) error {
	last, next, err := in.validate()
	if err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		// Lock the member row first (same no-op update sends use): it
		// serializes against a concurrent RemoveMember so a snapshot is
		// never written for a member deleted mid-report, and zero rows
		// affected is the membership check.
		res, err := tx.ExecContext(ctx, s.db.Rebind(
			`UPDATE region_members SET agent = agent WHERE namespace = $1 AND agent = $2`), in.Namespace, in.Agent)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: agent %q is not registered in namespace %q", ErrMessageNotFound, in.Agent, in.Namespace)
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(`
			INSERT INTO agent_message_diagnostics
				(namespace, agent, client, delivery_mode, state, last_attempt_at, next_attempt_at, last_error, pending_ack_count, wake_count, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (namespace, agent) DO UPDATE SET
				client = excluded.client, delivery_mode = excluded.delivery_mode, state = excluded.state,
				last_attempt_at = excluded.last_attempt_at, next_attempt_at = excluded.next_attempt_at,
				last_error = excluded.last_error, pending_ack_count = excluded.pending_ack_count,
				wake_count = excluded.wake_count, updated_at = excluded.updated_at`),
			in.Namespace, in.Agent, in.Client, in.DeliveryMode, in.State, last, next, in.LastError,
			in.PendingAckCount, in.WakeCount, store.TimeToDB(s.now()))
		return err
	})
}

// MessageDiagnostics lists a namespace's latest snapshots ordered by
// agent (at most MaxMessageDiagnosticList), or only agent's when agent is
// non-empty. It is read-only: no membership, lease or message state is
// touched. An unknown agent yields an empty, non-nil slice.
func (s *Store) MessageDiagnostics(ctx context.Context, ns, agent string) ([]MessageDiagnostic, error) {
	if err := checkNamespace(ns); err != nil {
		return nil, err
	}
	if err := checkIdent("agent", agent, false); err != nil {
		return nil, err
	}
	query := `SELECT namespace, agent, client, delivery_mode, state, last_attempt_at, next_attempt_at,
		last_error, pending_ack_count, wake_count, updated_at FROM agent_message_diagnostics WHERE namespace = $1`
	args := []any{ns}
	if agent != "" {
		query += ` AND agent = $2`
		args = append(args, agent)
	}
	query += fmt.Sprintf(` ORDER BY agent LIMIT %d`, MaxMessageDiagnosticList)
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cutoff := s.now().Add(-MessageDiagnosticStaleAfter)
	out := []MessageDiagnostic{}
	for rows.Next() {
		var d MessageDiagnostic
		if err := rows.Scan(&d.Namespace, &d.Agent, &d.Client, &d.DeliveryMode, &d.State, &d.LastAttemptAt,
			&d.NextAttemptAt, &d.LastError, &d.PendingAckCount, &d.WakeCount, &d.UpdatedAt); err != nil {
			return nil, err
		}
		updated, err := store.TimeFromDB(d.UpdatedAt)
		// an unparseable server timestamp cannot be vouched for as fresh
		d.Stale = err != nil || updated.Before(cutoff)
		out = append(out, d)
	}
	return out, rows.Err()
}

// deleteMessageDiagnostic removes one member's snapshot inside the
// member-removal transaction.
func (s *Store) deleteMessageDiagnostic(ctx context.Context, tx *sql.Tx, ns, agent string) error {
	_, err := tx.ExecContext(ctx, s.db.Rebind(
		`DELETE FROM agent_message_diagnostics WHERE namespace = $1 AND agent = $2`), ns, agent)
	return err
}
