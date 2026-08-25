// Package replay is the golden-ledger eval harness (P13): re-run
// investigations against a frozen world snapshot, compare trajectories
// deterministically, and report pass^k. Judges, when used, score ONLY
// the tool-call record - never the narrative (judge-gaming evidence,
// GAPS2 C2#2).
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hypervisor-io/punk-records/internal/task"
)

// SnapshotTools serves tool calls from a recorded task's snapshots: the
// offline frozen world (live replay rejected on Datadog's evidence).
// Exact (tool,args) matches win; a second call with the same shape
// consumes the next matching row, preserving order.
type SnapshotTools struct {
	rows []task.SnapshotRow
	used []bool
}

func NewSnapshotTools(rows []task.SnapshotRow) *SnapshotTools {
	return &SnapshotTools{rows: rows, used: make([]bool, len(rows))}
}

// Tools is not used for advertising during replay; the agent spec's
// allowlist filters the live pool. Replay advertises the recorded tools.
func (s *SnapshotTools) ToolNames() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range s.rows {
		if !seen[r.Tool] {
			seen[r.Tool] = true
			out = append(out, r.Tool)
		}
	}
	sort.Strings(out)
	return out
}

// Call returns the frozen result: exact (tool, args) first, then the
// oldest unused row for the tool (models rephrase arguments; the world
// is still the world).
func (s *SnapshotTools) Call(_ context.Context, name string, args json.RawMessage) (string, error) {
	argStr := string(args)
	for i, r := range s.rows {
		if !s.used[i] && r.Tool == name && r.Args == argStr {
			s.used[i] = true
			return s.frozen(r)
		}
	}
	for i, r := range s.rows {
		if !s.used[i] && r.Tool == name {
			s.used[i] = true
			return s.frozen(r)
		}
	}
	return "", fmt.Errorf("replay: no snapshot for tool %s (world ends here)", name)
}

func (s *SnapshotTools) frozen(r task.SnapshotRow) (string, error) {
	if r.Status != "ok" {
		return "", fmt.Errorf("%s", r.Result)
	}
	return r.Result, nil
}

// TrajectoryMode selects how strictly two tool trajectories must agree.
type TrajectoryMode string

const (
	Strict    TrajectoryMode = "strict"    // same tools, same order
	Unordered TrajectoryMode = "unordered" // same multiset of tools
	Subset    TrajectoryMode = "subset"    // candidate within recorded set
)

// Trajectory extracts the ordered tool names from a ledger.
func Trajectory(events []task.Event) []string {
	out := []string{}
	for _, e := range events {
		if e.Type != task.EventToolCall {
			continue
		}
		var p struct {
			Tool string `json:"tool"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Tool != "" {
			out = append(out, p.Tool)
		}
	}
	return out
}

// MatchTrajectory scores candidate against recorded under mode.
// Returns pass plus a reason for the eval report.
func MatchTrajectory(recorded, candidate []string, mode TrajectoryMode) (bool, string) {
	switch mode {
	case Strict:
		if len(recorded) != len(candidate) {
			return false, fmt.Sprintf("length %d != %d", len(candidate), len(recorded))
		}
		for i := range recorded {
			if recorded[i] != candidate[i] {
				return false, fmt.Sprintf("position %d: %s != %s", i, candidate[i], recorded[i])
			}
		}
		return true, "exact"
	case Unordered:
		if len(recorded) != len(candidate) {
			return false, fmt.Sprintf("length %d != %d", len(candidate), len(recorded))
		}
		counts := map[string]int{}
		for _, t := range recorded {
			counts[t]++
		}
		for _, t := range candidate {
			counts[t]--
			if counts[t] < 0 {
				return false, "extra tool " + t
			}
		}
		return true, "same multiset"
	case Subset:
		allowed := map[string]bool{}
		for _, t := range recorded {
			allowed[t] = true
		}
		for _, t := range candidate {
			if !allowed[t] {
				return false, "tool outside recorded set: " + t
			}
		}
		return true, "within recorded set"
	}
	return false, "unknown mode"
}

// Labels extracts harvested ground-truth labels from a ledger.
func Labels(events []task.Event) []map[string]string {
	out := []map[string]string{}
	for _, e := range events {
		if e.Type != task.EventLabel {
			continue
		}
		var m map[string]string
		if json.Unmarshal(e.Payload, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// PassHatK reports pass^k: the fraction is all-or-nothing - every run
// must pass (reliability, not luck; Grafana o11y-bench precedent).
func PassHatK(results []bool) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if !r {
			return false
		}
	}
	return true
}
