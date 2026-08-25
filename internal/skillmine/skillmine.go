// Package skillmine mines completed investigation ledgers for recurring
// tool trajectories and drafts SKILL.md procedures from them
// (a procedural-memory tier grounded in punk's event-sourced ledger). Mining is deterministic;
// the LLM only writes prose (a later stage drafts SKILL.md text from a
// Group; this package only groups).
package skillmine

import (
	"context"
	"encoding/json"

	"github.com/hypervisor-io/punk-records/internal/replay"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Group is a set of completed investigate tasks run by the same agent that
// followed the same (unordered) tool trajectory, along with what each run
// found. Trajectory is the first member's ordered call list - a
// representative, not a canonical form (member order need not match it,
// only its multiset).
type Group struct {
	Agent      string
	Trajectory []string // representative (first member's)
	TaskIDs    []string
	Summaries  []string // finding summaries of members
}

// Mine groups completed "investigate" tasks by agent + unordered tool-
// trajectory match. Greedy single pass: each task joins the first existing
// group (same agent, same trajectory multiset) it matches; otherwise it
// seeds a new group. Groups smaller than minCount are dropped; minCount
// <= 0 defaults to 3.
//
// Task order (and therefore group membership order) follows
// task.Ledger.List: newest-first (ORDER BY created_at DESC), not
// submission order. Group.TaskIDs and Group.Summaries are in that same
// newest-first order.
//
// List clamps its limit to 500 max, but a limit outside (0, 500] silently
// resets to a default of 100 rather than to 500 - so Mine passes 500
// explicitly to scan the largest window List allows. Ledgers with more
// than 500 completed tasks are only scanned up to that most-recent window;
// older completed tasks are invisible to mining.
func Mine(ctx context.Context, ledger *task.Ledger, agentFilter string, minCount int) ([]Group, error) {
	if minCount <= 0 {
		minCount = 3
	}
	tasks, err := ledger.List(ctx, task.StatusCompleted, 500)
	if err != nil {
		return nil, err
	}
	var groups []Group
	for _, tk := range tasks {
		if tk.Kind != "investigate" || tk.AgentName == "" {
			continue
		}
		if agentFilter != "" && tk.AgentName != agentFilter {
			continue
		}
		_, events, err := ledger.Get(ctx, tk.ID)
		if err != nil {
			return nil, err
		}
		traj := replay.Trajectory(events)
		if len(traj) == 0 {
			continue
		}
		summary := findingSummary(events)
		placed := false
		for i := range groups {
			if groups[i].Agent != tk.AgentName {
				continue
			}
			if ok, _ := replay.MatchTrajectory(groups[i].Trajectory, traj, replay.Unordered); ok {
				groups[i].TaskIDs = append(groups[i].TaskIDs, tk.ID)
				groups[i].Summaries = append(groups[i].Summaries, summary)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, Group{Agent: tk.AgentName, Trajectory: traj,
				TaskIDs: []string{tk.ID}, Summaries: []string{summary}})
		}
	}
	// Fresh output slice rather than groups[:0]: filtering in place would
	// be safe here too (the write index never runs ahead of the read
	// index), but a fresh slice makes that safety obvious at every call
	// site instead of requiring the reader to re-derive it.
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		if len(g.TaskIDs) >= minCount {
			out = append(out, g)
		}
	}
	return out, nil
}

// findingSummary returns the last finding event's summary, if any.
func findingSummary(events []task.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != task.EventFinding {
			continue
		}
		var env struct {
			Finding struct {
				Summary string `json:"summary"`
			} `json:"finding"`
		}
		if json.Unmarshal(events[i].Payload, &env) == nil {
			return env.Finding.Summary
		}
	}
	return ""
}
