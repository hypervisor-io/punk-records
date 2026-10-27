package region

import (
	"context"
	"database/sql"
	"fmt"
)

// ArchiveReadMessages moves every acknowledged message of ns into archive
// (the /punk-consolidate archive namespace). Unread mail stays where its
// recipients will look for it. It returns the number of messages moved.
func (s *Store) ArchiveReadMessages(ctx context.Context, ns, archive string) (int64, error) {
	if err := checkNamespace(ns); err != nil {
		return 0, err
	}
	if err := checkNamespace(archive); err != nil {
		return 0, err
	}
	r, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE agent_messages SET namespace = $1 WHERE namespace = $2 AND acked_at IS NOT NULL`), archive, ns)
	if err != nil {
		return 0, fmt.Errorf("archive messages: %w", err)
	}
	return r.RowsAffected()
}

// RestoreArchivedMessages moves every message of archive back into ns,
// the inverse of ArchiveReadMessages.
func (s *Store) RestoreArchivedMessages(ctx context.Context, archive, ns string) (int64, error) {
	if err := checkNamespace(ns); err != nil {
		return 0, err
	}
	if err := checkNamespace(archive); err != nil {
		return 0, err
	}
	r, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE agent_messages SET namespace = $1 WHERE namespace = $2`), ns, archive)
	if err != nil {
		return 0, fmt.Errorf("restore messages: %w", err)
	}
	return r.RowsAffected()
}

// MessageStats summarizes a namespace's mail for the consolidation
// digest: totals, unread, the time span, and per-sender counts.
type MessageStats struct {
	Total   int            `json:"total"`
	Unread  int            `json:"unread"`
	FirstAt string         `json:"first_at,omitempty"`
	LastAt  string         `json:"last_at,omitempty"`
	Senders map[string]int `json:"senders"`
}

// NamespaceMessageStats reads MessageStats for ns.
func (s *Store) NamespaceMessageStats(ctx context.Context, ns string) (MessageStats, error) {
	st := MessageStats{Senders: map[string]int{}}
	if err := checkNamespace(ns); err != nil {
		return st, err
	}
	var first, last sql.NullString
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(`
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN acked_at IS NULL THEN 1 ELSE 0 END), 0),
		       MIN(created_at), MAX(created_at)
		FROM agent_messages WHERE namespace = $1`), ns).Scan(&st.Total, &st.Unread, &first, &last); err != nil {
		return st, fmt.Errorf("message stats: %w", err)
	}
	st.FirstAt, st.LastAt = first.String, last.String
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(
		`SELECT sender, COUNT(*) FROM agent_messages WHERE namespace = $1 GROUP BY sender`), ns)
	if err != nil {
		return st, fmt.Errorf("message senders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var who string
		var n int
		if err := rows.Scan(&who, &n); err != nil {
			return st, err
		}
		st.Senders[who] = n
	}
	return st, rows.Err()
}
