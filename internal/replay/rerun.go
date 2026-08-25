package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hypervisor-io/punk-records/internal/agent"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// toolAdapter exposes a SnapshotTools as an agent.ToolCaller.
type toolAdapter struct{ s *SnapshotTools }

func (a toolAdapter) Tools() []llm.Tool {
	names := a.s.ToolNames()
	out := make([]llm.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, llm.Tool{Name: n,
			Description: "replayed tool (frozen world snapshot)",
			Schema:      json.RawMessage(`{"type":"object"}`)})
	}
	return out
}
func (a toolAdapter) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return a.s.Call(ctx, name, args)
}

// Result is one replay outcome.
type Result struct {
	OriginalTask string   `json:"original_task"`
	ReplayTask   string   `json:"replay_task"`
	Status       string   `json:"status"`
	Recorded     []string `json:"recorded_trajectory"`
	Candidate    []string `json:"candidate_trajectory"`
	Pass         bool     `json:"pass"`
	Reason       string   `json:"reason"`
}

// Rerun re-executes a completed task's investigation against its frozen
// snapshots under the CURRENT spec snapshot, then scores the trajectory
// against the recorded one.
func Rerun(ctx context.Context, ledger *task.Ledger, reg *registry.Registry,
	client llm.Client, taskID string, mode TrajectoryMode, log *slog.Logger) (*Result, error) {

	orig, events, err := ledger.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	snaps, err := ledger.Snapshots(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, fmt.Errorf("replay: task %s has no snapshots (pre-P13 task?)", taskID)
	}
	if orig.AgentName == "" {
		return nil, fmt.Errorf("replay: task %s was never routed", taskID)
	}

	shadow, _, err := ledger.Submit(ctx, task.SubmitInput{
		Source: orig.Source, Kind: "replay",
		Labels: orig.Labels, Budget: orig.Budget,
		Actor: "replay",
	})
	if err != nil {
		return nil, err
	}
	_, verID, ok := reg.Get(orig.AgentName)
	if !ok {
		return nil, fmt.Errorf("replay: agent %q not in current snapshot", orig.AgentName)
	}
	if err := ledger.AssignAgent(ctx, shadow.ID, orig.AgentName, verID, "replay",
		map[string]string{"replay_of": taskID}); err != nil {
		return nil, err
	}

	rt := agent.New(ledger, reg, nil, llm.NewManager(false, nil, nil, nil),
		toolAdapter{NewSnapshotTools(snaps)}, log)
	rt.FixedClient = client
	if err := rt.RunTask(ctx, shadow.ID); err != nil {
		return nil, fmt.Errorf("replay run: %w", err)
	}

	replayed, replayEvents, err := ledger.Get(ctx, shadow.ID)
	if err != nil {
		return nil, err
	}
	res := &Result{
		OriginalTask: taskID, ReplayTask: shadow.ID, Status: replayed.Status,
		Recorded: Trajectory(events), Candidate: Trajectory(replayEvents),
	}
	res.Pass, res.Reason = MatchTrajectory(res.Recorded, res.Candidate, mode)
	if replayed.Status != task.StatusCompleted {
		res.Pass = false
		res.Reason = "replay ended " + replayed.Status + "; " + res.Reason
	}
	return res, nil
}
