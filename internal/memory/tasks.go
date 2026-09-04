package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
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

// TaskRow is one task in the /tasks convention, latest revisions only,
// parsed for machines: state from the status prefix or attribute,
// dependencies from the task attributes or body. Bodies are cut to one
// line so a board of hundreds of tasks stays cheap; recall the key for
// the full text.
type TaskRow struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Status    string    `json:"status,omitempty"`
	DependsOn []string  `json:"depends_on,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	By        string    `json:"by,omitempty"`
}

// TaskStates are the recognised status prefixes, in the order the board
// reports counts. Anything else is pending.
var TaskStates = []string{"pending", "in_progress", "review", "blocked", "done"}

// ParseTaskState reads the state of a status fact: the body prefix
// ("done:", "blocked:", "review:", "in_progress:", case-insensitive) wins,
// then a string attribute "state" naming a known state, else pending.
func ParseTaskState(body string, attrs map[string]any) string {
	head := strings.ToLower(firstLine(body))
	for _, st := range TaskStates {
		if strings.HasPrefix(head, st+":") {
			return st
		}
	}
	if v, ok := attrs["state"].(string); ok {
		for _, st := range TaskStates {
			if v == st {
				return st
			}
		}
	}
	return "pending"
}

// ListTasks reads every live /tasks/<id> and /tasks/<id>/status fact in
// a namespace and returns one row per id, sorted by id. A status without
// a task fact still yields a row (empty title) so nothing is hidden.
func (s *Store) ListTasks(ctx context.Context, ns string) ([]TaskRow, error) {
	facts, err := s.Recall(ctx, ns, "/tasks/", 1000)
	if err != nil {
		return nil, err
	}
	rows := map[string]*TaskRow{}
	get := func(id string) *TaskRow {
		r, ok := rows[id]
		if !ok {
			r = &TaskRow{ID: id, State: "pending"}
			rows[id] = r
		}
		return r
	}
	for _, f := range facts {
		parts := strings.Split(strings.TrimPrefix(f.Key, "/tasks/"), "/")
		switch {
		case len(parts) == 1 && parts[0] != "":
			r := get(parts[0])
			r.Title = cut(firstLine(f.Body), 120)
			r.DependsOn = parseDepends(f.Body, f.Attributes)
			if f.CreatedAt.After(r.UpdatedAt) {
				r.UpdatedAt, r.By = f.CreatedAt, f.Writer
			}
		case len(parts) == 2 && parts[1] == "status":
			r := get(parts[0])
			r.State = ParseTaskState(f.Body, f.Attributes)
			r.Status = cut(firstLine(f.Body), 300)
			if f.CreatedAt.After(r.UpdatedAt) {
				r.UpdatedAt, r.By = f.CreatedAt, f.Writer
			}
		}
	}
	out := make([]TaskRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n])
}

// parseDepends reads dependencies from an attribute "depends_on" (array of
// strings) or from a body line starting with "depends_on:" or "depends:"
// (ids separated by commas or spaces).
func parseDepends(body string, attrs map[string]any) []string {
	if raw, ok := attrs["depends_on"].([]any); ok {
		var out []string
		for _, v := range raw {
			if id, ok := v.(string); ok && id != "" {
				out = append(out, id)
			}
		}
		return out
	}
	for _, line := range strings.Split(body, "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		var rest string
		switch {
		case strings.HasPrefix(l, "depends_on:"):
			rest = strings.TrimSpace(line)[len("depends_on:"):]
		case strings.HasPrefix(l, "depends:"):
			rest = strings.TrimSpace(line)[len("depends:"):]
		default:
			continue
		}
		var out []string
		for _, id := range strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
			if id = strings.TrimSpace(id); id != "" && id != "none" {
				out = append(out, id)
			}
		}
		return out
	}
	return nil
}

// RenderTaskStatus builds the canonical status body that ParseTaskState
// and TaskCounts read back: "done: <sha> <summary>; tests: <tests>",
// "in_progress: <phase> <summary>", "<state>: <summary>", plus a
// "deviation: ..." second line when given.
func RenderTaskStatus(state, summary, sha, tests, phase, deviation string) string {
	var b strings.Builder
	b.WriteString(state + ":")
	switch state {
	case "done":
		if sha != "" {
			b.WriteString(" " + sha)
		}
		b.WriteString(" " + summary)
		if tests != "" {
			b.WriteString("; tests: " + tests)
		}
	case "in_progress":
		if phase != "" {
			b.WriteString(" " + phase)
		}
		b.WriteString(" " + summary)
	default:
		b.WriteString(" " + summary)
	}
	if deviation != "" {
		b.WriteString("\ndeviation: " + deviation)
	}
	return b.String()
}
