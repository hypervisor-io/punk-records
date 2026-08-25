package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// WriteActivity reports how many fact revisions landed in ns strictly
// after since, and when the most recent one landed (zero time when none
// did). It counts raw revision rows - adds, updates, and tombstones alike
// - because the consolidation scheduler's dream-style triggers care
// about "did anything change", not about live-fact
// semantics. A namespace that doesn't exist reports zero activity, not an
// error: the scheduler iterates a namespace list that can race deletion.
func (s *Store) WriteActivity(ctx context.Context, ns string, since time.Time) (int, time.Time, error) {
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return 0, time.Time{}, err
	}
	var n int
	var lastRaw sql.NullString
	err = s.db.QueryRowContext(ctx, s.db.Rebind(`
		SELECT COUNT(*), MAX(created_at) FROM memories
		WHERE namespace_id = $1 AND created_at > $2`),
		nsID, store.TimeToDB(since)).Scan(&n, &lastRaw)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("write activity: %w", err)
	}
	if !lastRaw.Valid || lastRaw.String == "" {
		return n, time.Time{}, nil
	}
	last, err := store.TimeFromDB(lastRaw.String)
	if err != nil {
		// A malformed created_at shouldn't hide the count; report the
		// count with a zero last-write rather than failing the scheduler.
		return n, time.Time{}, nil
	}
	return n, last, nil
}

// Namespaces lists every memory namespace that exists - the iteration
// set for the consolidation scheduler. This deliberately reads the
// namespaces table (populated by every Write via ensureNamespace), not
// the regions table (populated only by explicit region registration):
// hook-capture namespaces like agent-<dir> get facts without ever being
// registered as regions, and iterating regions would skip them forever.
func (s *Store) Namespaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM namespaces ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkConsolidated records that ns finished a consolidation pass at t.
// In-process only (not persisted): it feeds Diagnose's observability
// fields and the scheduler's own bookkeeping, both of which tolerate a
// restart resetting it.
func (s *Store) MarkConsolidated(ns string, t time.Time) {
	s.consolMu.Lock()
	defer s.consolMu.Unlock()
	if s.lastConsolidated == nil {
		s.lastConsolidated = map[string]time.Time{}
	}
	s.lastConsolidated[ns] = t
}

// LastConsolidated reports when ns last finished a consolidation pass in
// this process, ok=false when it hasn't.
func (s *Store) LastConsolidated(ns string) (time.Time, bool) {
	s.consolMu.Lock()
	defer s.consolMu.Unlock()
	t, ok := s.lastConsolidated[ns]
	return t, ok
}
