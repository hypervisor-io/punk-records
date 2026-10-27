package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// TaskStateCounts tallies the /tasks convention by board state. Every
// task fact lands in exactly one bucket.
type TaskStateCounts struct {
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Review     int `json:"review"`
	Blocked    int `json:"blocked"`
	Done       int `json:"done"`
}

// Total is the number of tasks tallied.
func (c TaskStateCounts) Total() int {
	return c.Pending + c.InProgress + c.Review + c.Blocked + c.Done
}

func (c *TaskStateCounts) add(state string) {
	switch state {
	case "in_progress":
		c.InProgress++
	case "review":
		c.Review++
	case "blocked":
		c.Blocked++
	case "done":
		c.Done++
	default:
		c.Pending++
	}
}

// NamespaceTaskStates counts tasks by state for every namespace in two
// passes over one grouped query: it reads the latest live revision of
// every /tasks/<id> and /tasks/<id>/status key, then resolves each task's
// state with ParseTaskState (a task with no status is pending). A status
// whose task fact is gone is not counted, so Total per namespace matches
// NamespaceSummary.Tasks. Namespaces without tasks are absent.
func (s *Store) NamespaceTaskStates(ctx context.Context) (map[string]TaskStateCounts, error) {
	rows, err := s.db.Reader().QueryContext(ctx, s.db.Rebind(`
		SELECT n.name, m.key,
		       CASE WHEN m.key LIKE '%/status' THEN substr(m.body, 1, 200) ELSE '' END,
		       CASE WHEN m.key LIKE '%/status' THEN m.attributes ELSE '{}' END
		FROM memories m
		JOIN namespaces n ON n.id = m.namespace_id
		JOIN (
		    SELECT namespace_id, key, MAX(created_at) AS mc
		    FROM memories
		    WHERE key LIKE $1 ESCAPE '\'
		    GROUP BY namespace_id, key
		) latest ON latest.namespace_id = m.namespace_id
		        AND latest.key = m.key
		        AND latest.mc = m.created_at
		WHERE m.action <> 'tombstone'
		  AND (m.expiration_date IS NULL OR m.expiration_date > $2)
		  AND (m.key NOT LIKE '/tasks/%/%' OR m.key LIKE '/tasks/%/status')`),
		likePrefix("/tasks/"), store.TimeToDB(s.now()))
	if err != nil {
		return nil, fmt.Errorf("namespace task states: %w", err)
	}
	defer rows.Close()
	type nsTasks struct {
		ids    map[string]bool
		states map[string]string
	}
	byNS := map[string]*nsTasks{}
	for rows.Next() {
		var ns, key, body, attrsJSON string
		if err := rows.Scan(&ns, &key, &body, &attrsJSON); err != nil {
			return nil, err
		}
		parts := strings.Split(strings.TrimPrefix(key, "/tasks/"), "/")
		if parts[0] == "" || len(parts) > 2 {
			continue
		}
		t := byNS[ns]
		if t == nil {
			t = &nsTasks{ids: map[string]bool{}, states: map[string]string{}}
			byNS[ns] = t
		}
		if len(parts) == 1 {
			t.ids[parts[0]] = true
			continue
		}
		var attrs map[string]any
		// A malformed attributes blob costs the attribute fallback only.
		_ = json.Unmarshal([]byte(attrsJSON), &attrs)
		t.states[parts[0]] = ParseTaskState(body, attrs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]TaskStateCounts, len(byNS))
	for ns, t := range byNS {
		if len(t.ids) == 0 {
			continue
		}
		var c TaskStateCounts
		for id := range t.ids {
			c.add(t.states[id]) // "" (no status) counts as pending
		}
		out[ns] = c
	}
	return out, nil
}

// RecentFact is one row of a namespace's recent-activity feed: a live
// fact's newest revision with a clipped body.
type RecentFact struct {
	Key        string  `json:"key"`
	Body       string  `json:"body"`
	Author     string  `json:"author"`
	Action     string  `json:"action"`
	CreatedAt  string  `json:"created_at"`
	Importance float64 `json:"importance,omitempty"`
}

// RecentBodyChars clips RecentFacts bodies; recall the key for the rest.
const RecentBodyChars = 600

// sessionKeyPrefix holds per-session scratch facts; they dominate feeds
// of busy namespaces, so RecentFacts leaves them out unless asked.
const sessionKeyPrefix = "/agent-sessions/"

// RecentFacts returns the newest revision of each live key in a
// namespace, most recently written first. Tombstoned and expired keys
// are skipped. Keys under /agent-sessions/ are excluded unless
// includeSessions. limit defaults to 50 and is capped at 200; offset
// skips that many rows of the same order, for the console's "load more"
// (negative counts as zero).
func (s *Store) RecentFacts(ctx context.Context, ns string, limit, offset int, includeSessions bool) ([]RecentFact, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	out := []RecentFact{}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT m.key, substr(m.body, 1, $5), m.author, m.action, m.created_at, COALESCE(m.importance, 0)
		FROM memories m
		WHERE m.namespace_id = $1 AND m.action <> 'tombstone'
		  AND (m.expiration_date IS NULL OR m.expiration_date > $2)
		  AND ($3 = 1 OR m.key NOT LIKE $4 ESCAPE '\')
		  AND m.created_at = (
		      SELECT MAX(m2.created_at) FROM memories m2
		      WHERE m2.namespace_id = m.namespace_id AND m2.key = m.key)
		ORDER BY m.created_at DESC, m.key
		LIMIT $6 OFFSET $7`),
		nsID, store.TimeToDB(s.now()), boolInt(includeSessions), likePrefix(sessionKeyPrefix),
		RecentBodyChars, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("recent facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f RecentFact
		if err := rows.Scan(&f.Key, &f.Body, &f.Author, &f.Action, &f.CreatedAt, &f.Importance); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
