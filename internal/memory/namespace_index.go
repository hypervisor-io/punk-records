package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// NamespaceSummary is one row of the namespace index: how much live
// memory a namespace holds, how many /tasks/<id> facts it carries, and
// when it was last written. It is the cheap directory the operator
// console lists before fetching any single namespace's full board.
type NamespaceSummary struct {
	Name      string    `json:"name"`
	Facts     int       `json:"facts"`
	Tasks     int       `json:"tasks"`
	LastWrite time.Time `json:"last_write,omitempty"`
}

// NamespaceSummaries lists every namespace with its live-fact and task
// counts in one grouped query, plus the namespaces that exist with no
// live facts at all (created, then fully tombstoned, or created by a
// registration that never wrote). Tasks counts the task facts
// themselves - /tasks/<id> - and not their /tasks/<id>/status children,
// so it matches the row count of taskboard.Build. A row is a key's
// latest revision when no newer revision of the key exists; that
// anti-join seeks the (namespace_id, key, created_at) index per row and
// runs about twice as fast as joining a grouped MAX(created_at) over the
// whole table. Revisions tied on created_at both survive, which the
// DISTINCT counts absorb.
func (s *Store) NamespaceSummaries(ctx context.Context) ([]NamespaceSummary, error) {
	// The full-table scan runs on the read pool so it never holds the
	// single SQLite read-write connection (see store.DB.Reader).
	rows, err := s.db.Reader().QueryContext(ctx, `
		SELECT n.name,
		       COUNT(DISTINCT m.key) AS facts,
		       COUNT(DISTINCT CASE WHEN m.key LIKE '/tasks/%' AND m.key NOT LIKE '/tasks/%/%'
		                           THEN m.key END) AS tasks,
		       MAX(m.created_at) AS last_write
		FROM memories m
		JOIN namespaces n ON n.id = m.namespace_id
		WHERE m.action <> 'tombstone'
		  AND NOT EXISTS (
		      SELECT 1 FROM memories newer
		      WHERE newer.namespace_id = m.namespace_id
		        AND newer.key = m.key
		        AND newer.created_at > m.created_at)
		GROUP BY n.name
		ORDER BY n.name`)
	if err != nil {
		return nil, fmt.Errorf("namespace summaries: %w", err)
	}
	defer rows.Close()
	out := []NamespaceSummary{}
	seen := map[string]bool{}
	for rows.Next() {
		var sum NamespaceSummary
		var last sql.NullString
		if err := rows.Scan(&sum.Name, &sum.Facts, &sum.Tasks, &last); err != nil {
			return nil, err
		}
		if last.Valid && last.String != "" {
			// A malformed created_at costs the timestamp, never the row.
			if t, err := store.TimeFromDB(last.String); err == nil {
				sum.LastWrite = t
			}
		}
		seen[sum.Name] = true
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names, err := s.Namespaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if !seen[n] {
			out = append(out, NamespaceSummary{Name: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
