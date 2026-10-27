package region

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// NamespaceActivity is one namespace's coordination activity: who is
// registered, who is alive, and how much mail and claim traffic it has.
// Zero times mean "never".
type NamespaceActivity struct {
	Members     int
	Live        int // members holding an open inbox stream right now
	Recent      int // live members plus those seen within the recent window (Active semantics)
	Unread      int // unacked messages
	Messages24h int // messages created within the last 24 hours
	Claims      int // live (unreleased, unexpired) work claims

	LastMessageAt time.Time
	LastSeenAt    time.Time
}

// Activity summarizes every namespace's coordination state in one query
// per table (members, messages, claims) instead of one per namespace, for
// the cross-namespace overview. recent is the last_seen_at window that
// makes a member "recent" (see Active). Namespaces with no members,
// messages or claims are absent from the result.
func (s *Store) Activity(ctx context.Context, recent time.Duration) (map[string]*NamespaceActivity, error) {
	out := map[string]*NamespaceActivity{}
	get := func(ns string) *NamespaceActivity {
		a := out[ns]
		if a == nil {
			a = &NamespaceActivity{}
			out[ns] = a
		}
		return a
	}
	now := s.now()
	recentCut := store.TimeToDB(now.Add(-recent))

	rows, err := s.db.QueryContext(ctx, `SELECT namespace, agent, COALESCE(last_seen_at, '') FROM region_members`)
	if err != nil {
		return nil, fmt.Errorf("activity members: %w", err)
	}
	for rows.Next() {
		var ns, agent, seen string
		if err := rows.Scan(&ns, &agent, &seen); err != nil {
			rows.Close()
			return nil, err
		}
		a := get(ns)
		a.Members++
		listening := s.Listening(ns, agent)
		if listening {
			a.Live++
		}
		if listening || (seen != "" && seen >= recentCut) {
			a.Recent++
		}
		// A malformed last_seen_at costs the timestamp, never the row.
		if t, err := store.TimeFromDB(seen); err == nil && t.After(a.LastSeenAt) {
			a.LastSeenAt = t
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT namespace,
		       SUM(CASE WHEN acked_at IS NULL THEN 1 ELSE 0 END),
		       SUM(CASE WHEN created_at >= $1 THEN 1 ELSE 0 END),
		       MAX(created_at)
		FROM agent_messages GROUP BY namespace`),
		store.TimeToDB(now.Add(-24*time.Hour)))
	if err != nil {
		return nil, fmt.Errorf("activity messages: %w", err)
	}
	for rows.Next() {
		var ns string
		var unread, day int
		var last sql.NullString
		if err := rows.Scan(&ns, &unread, &day, &last); err != nil {
			rows.Close()
			return nil, err
		}
		a := get(ns)
		a.Unread, a.Messages24h = unread, day
		if last.Valid {
			if t, err := store.TimeFromDB(last.String); err == nil {
				a.LastMessageAt = t
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT namespace, COUNT(*) FROM work_claims
		WHERE released_at IS NULL AND expires_at > $1
		GROUP BY namespace`), store.TimeToDB(now))
	if err != nil {
		return nil, fmt.Errorf("activity claims: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ns string
		var n int
		if err := rows.Scan(&ns, &n); err != nil {
			return nil, err
		}
		get(ns).Claims = n
	}
	return out, rows.Err()
}
