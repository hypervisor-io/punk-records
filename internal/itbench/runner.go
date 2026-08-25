package itbench

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hypervisor-io/punk-records/internal/agent"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Deps are the pieces a scenario run needs: the ledger and registry to
// host the task, the model manager to power the agent, and the name of the
// (registered) agent to assign - typically an SRE specialist.
type Deps struct {
	Ledger *task.Ledger
	Reg    *registry.Registry
	LLM    *llm.Manager
	Agent  string
	Budget task.Budget
	Log    *slog.Logger
}

// Result is one scored scenario run.
type Result struct {
	Scenario  string `json:"scenario"`
	Status    string `json:"status"`
	Diagnosis string `json:"diagnosis,omitempty"`
	Score     Score  `json:"score"`
	Err       string `json:"error,omitempty"`
}

// Report aggregates a directory run into the ITBench headline number.
type Report struct {
	Total        int       `json:"total"`
	Resolved     int       `json:"resolved"`
	ResolvedRate float64   `json:"resolved_rate"`
	Results      []*Result `json:"results"`
}

// Run drives one scenario: it hosts a task, assigns the SRE agent, lets the
// agent investigate over the scenario's frozen tools, then scores whatever
// faulty entity the resulting finding names.
func Run(ctx context.Context, d Deps, scn *Scenario) (*Result, error) {
	if d.Agent == "" {
		return nil, fmt.Errorf("itbench: no agent configured to run scenarios")
	}
	_, verID, ok := d.Reg.Get(d.Agent)
	if !ok {
		return nil, fmt.Errorf("itbench: agent %q not in the current spec snapshot", d.Agent)
	}
	budget := d.Budget
	if budget.Tokens == 0 {
		budget = task.Budget{Tokens: 120000, ToolCalls: 30, WallMS: 300000, Subagents: 1}
	}
	tk, _, err := d.Ledger.Submit(ctx, task.SubmitInput{
		Source: "itbench", Kind: "investigate",
		Labels: map[string]string{"scenario": scn.ID, "domain": scn.Domain},
		Budget: budget, Actor: "itbench",
	})
	if err != nil {
		return nil, err
	}
	if err := d.Ledger.AssignAgent(ctx, tk.ID, d.Agent, verID, "itbench",
		map[string]string{"scenario": scn.ID}); err != nil {
		return nil, err
	}

	rt := agent.New(d.Ledger, d.Reg, nil, d.LLM, NewTools(scn), d.Log)
	res := &Result{Scenario: scn.ID}
	if err := rt.RunTask(ctx, tk.ID); err != nil {
		res.Err = err.Error()
	}

	final, events, err := d.Ledger.Get(ctx, tk.ID)
	if err != nil {
		return nil, err
	}
	res.Status = final.Status
	diagnosis := extractDiagnosis(events)
	res.Diagnosis = clip(diagnosis)
	res.Score = ScoreDiagnosis(diagnosis, scn.GroundTruth)
	return res, nil
}

// RunAll runs every scenario under root and aggregates the resolved rate.
func RunAll(ctx context.Context, d Deps, root string) (*Report, error) {
	scns, err := LoadDir(root)
	if err != nil {
		return nil, err
	}
	rep := &Report{Total: len(scns)}
	for _, scn := range scns {
		r, err := Run(ctx, d, scn)
		if err != nil {
			r = &Result{Scenario: scn.ID, Err: err.Error()}
		}
		if r.Score.Resolved {
			rep.Resolved++
		}
		rep.Results = append(rep.Results, r)
	}
	if rep.Total > 0 {
		rep.ResolvedRate = float64(rep.Resolved) / float64(rep.Total)
	}
	return rep, nil
}

// extractDiagnosis concatenates the raw text of every finding the agent
// produced; the faulty entity it named (if any) is somewhere in here, so
// scoring is a substring search over the full finding payload.
func extractDiagnosis(events []task.Event) string {
	var out string
	for _, e := range events {
		if e.Type == task.EventFinding {
			out += " " + string(e.Payload)
		}
	}
	return out
}
