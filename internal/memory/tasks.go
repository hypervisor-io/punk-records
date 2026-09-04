package memory

import (
	"context"
	"fmt"
	"strings"
)

// TaskCounts tallies the /tasks convention in one namespace: one fact per
// task at /tasks/<id>, and a status fact at /tasks/<id>/status whose body
// starts with "done:" or "blocked:". Pending is whatever has a task fact
// and no terminal status. It is a read-only digest for dashboards; the
// planner protocol itself lives in the punk-memory skill.
type TaskCounts struct {
	Total   int `json:"total"`
	Done    int `json:"done"`
	Blocked int `json:"blocked"`
	Pending int `json:"pending"`
}

// TaskCounts reads the latest non-tombstone revision of every key under
// /tasks/ (same visibility rule as ListKeys) and classifies each key by
// shape: "/tasks/<id>" counts toward Total, "/tasks/<id>/status" toward
// Done or Blocked by body prefix. A status without a task fact still
// counts as done or blocked, so Pending is floored at zero.
func (s *Store) TaskCounts(ctx context.Context, ns string) (TaskCounts, error) {
	var out TaskCounts
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT m.key, substr(m.body, 1, 8) FROM memories m
		JOIN (
		    SELECT key, MAX(created_at) AS mc FROM memories
		    WHERE namespace_id = $1 AND key LIKE $2 ESCAPE '\'
		    GROUP BY key
		) latest ON m.key = latest.key AND m.created_at = latest.mc
		WHERE m.namespace_id = $1 AND m.action <> 'tombstone'`),
		nsID, likePrefix("/tasks/"))
	if err != nil {
		return out, fmt.Errorf("task counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, head string
		if err := rows.Scan(&key, &head); err != nil {
			return out, err
		}
		rest := strings.TrimPrefix(key, "/tasks/")
		parts := strings.Split(rest, "/")
		switch {
		case len(parts) == 1 && parts[0] != "":
			out.Total++
		case len(parts) == 2 && parts[1] == "status":
			switch {
			case strings.HasPrefix(head, "done:"):
				out.Done++
			case strings.HasPrefix(head, "blocked:"):
				out.Blocked++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Pending = out.Total - out.Done - out.Blocked
	if out.Pending < 0 {
		out.Pending = 0
	}
	return out, nil
}
