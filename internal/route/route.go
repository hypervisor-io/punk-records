// Package route picks the domain agent for a task. Deterministic first:
// declared triggers route without any LLM; an optional tie-breaker is
// consulted only when rules produce more than one top-priority candidate.
// Every decision is recorded in routing_decisions with its inputs, so
// routing is replayable and evaluable.
package route

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/hypervisor-io/punk-records/internal/obs"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/spec"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
	"github.com/hypervisor-io/punk-records/internal/topo"
)

// TieBreaker resolves rule ties. The LLM implementation arrives in P5;
// nil falls back to deterministic ordering (priority desc, name asc).
type TieBreaker interface {
	Pick(ctx context.Context, t *task.Task, candidates []string) (string, error)
}

// Decision is what Route records and returns. Propensity is the
// probability this router assigned to the chosen agent (OPE-ready:
// off-policy estimators need non-degenerate logs, P10.7).
type Decision struct {
	TaskID      string   `json:"task_id"`
	ChosenAgent string   `json:"chosen_agent"`
	Method      string   `json:"method"` // rule | llm | explore | fallback
	RuleID      string   `json:"rule_id,omitempty"`
	Candidates  []string `json:"candidates"`
	Propensity  float64  `json:"propensity"`
	Upstream    []string `json:"upstream,omitempty"` // probable-cause chain (topology)
}

type Router struct {
	db     *store.DB
	reg    *registry.Registry
	ledger *task.Ledger
	tie    TieBreaker
	now    func() time.Time

	// Fallback receives tasks no trigger matches; empty parks the task.
	Fallback string
	// TopoDB enables service-topology enrichment of routing labels
	// (owner/tier + upstream chain); nil disables (P17.2).
	TopoDB *store.DB
	// Epsilon explores uniformly among top-priority ties with this
	// probability, logging honest propensities (0 disables).
	Epsilon float64
	// Rand/RandIntn are injectable for tests; defaults are math/rand.
	Rand     func() float64
	RandIntn func(n int) int
}

func New(db *store.DB, reg *registry.Registry, ledger *task.Ledger, tie TieBreaker, now func() time.Time) *Router {
	if now == nil {
		now = time.Now
	}
	return &Router{db: db, reg: reg, ledger: ledger, tie: tie, now: now,
		Rand: rand.Float64, RandIntn: rand.Intn}
}

// SetTieBreaker installs a tie resolver after construction (serve wires
// it only when AI is enabled).
func (r *Router) SetTieBreaker(t TieBreaker) { r.tie = t }

type candidate struct {
	agent    string
	ruleID   string
	priority int
}

// matchTrigger: every set field must match. Empty rules were rejected at
// spec validation.
func matchTrigger(t *task.Task, tr spec.Trigger) bool {
	if tr.Source != "" {
		if p, ok := strings.CutSuffix(tr.Source, "*"); ok {
			if !strings.HasPrefix(t.Source, p) {
				return false
			}
		} else if t.Source != tr.Source {
			return false
		}
	}
	if tr.Kind != "" && t.Kind != tr.Kind {
		return false
	}
	for k, v := range tr.Labels {
		if t.Labels[k] != v {
			return false
		}
	}
	if len(tr.Severity) > 0 {
		sev := t.Labels["severity"]
		found := false
		for _, s := range tr.Severity {
			if s == sev {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Route decides, records the decision, and assigns the agent on the
// ledger. Zero candidates with no fallback parks the task input_required.
func (r *Router) Route(ctx context.Context, t *task.Task) (*Decision, error) {
	ctx, span := obs.Tracer().Start(ctx, "task.route")
	defer span.End()
	span.SetAttributes(attribute.String("punk.task.id", t.ID))

	snap := r.reg.Current()
	if snap == nil {
		return nil, fmt.Errorf("route: no spec snapshot loaded")
	}

	var upstream []string
	if r.TopoDB != nil {
		enriched, chain, err := topo.Enrich(ctx, r.TopoDB, t.Labels)
		if err == nil {
			t.Labels, upstream = enriched, chain
		}
	}

	var cands []candidate
	for name, a := range snap.Bundle.Agents {
		if a.Disabled {
			continue // cost kill switch (P14.4)
		}
		for _, tr := range a.Triggers {
			if matchTrigger(t, tr) {
				cands = append(cands, candidate{agent: name, ruleID: tr.ID, priority: tr.Priority})
				break // one match per agent is enough
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].priority != cands[j].priority {
			return cands[i].priority > cands[j].priority
		}
		return cands[i].agent < cands[j].agent
	})

	d := &Decision{TaskID: t.ID, Candidates: make([]string, 0, len(cands)), Propensity: 1.0, Upstream: upstream}
	for _, c := range cands {
		d.Candidates = append(d.Candidates, c.agent)
	}

	switch {
	case len(cands) == 0:
		d.Method = "fallback"
		d.ChosenAgent = r.Fallback
	case len(cands) == 1 || cands[0].priority > cands[1].priority:
		d.Method = "rule"
		d.ChosenAgent = cands[0].agent
		d.RuleID = cands[0].ruleID
	default:
		// top-priority tie
		top := []string{}
		for _, c := range cands {
			if c.priority == cands[0].priority {
				top = append(top, c.agent)
			}
		}
		if r.tie != nil {
			pick, err := r.tie.Pick(ctx, t, top)
			if err == nil && pick != "" {
				d.Method = "llm"
				d.ChosenAgent = pick
				break
			}
			// tie-breaker failure degrades to deterministic order
		}
		// epsilon-greedy exploration among the tie set with honest
		// propensities: chosen greedy = 1-e+e/n, explored arm = e/n
		n := len(top)
		idx := 0
		if r.Epsilon > 0 && r.Rand() < r.Epsilon {
			idx = r.RandIntn(n)
		}
		if idx == 0 {
			d.Propensity = 1 - r.Epsilon + r.Epsilon/float64(n)
			d.Method = "rule"
			d.RuleID = cands[0].ruleID
		} else {
			d.Propensity = r.Epsilon / float64(n)
			d.Method = "explore"
		}
		d.ChosenAgent = top[idx]
	}

	span.SetAttributes(
		attribute.String("punk.route.method", d.Method),
		attribute.String("punk.route.agent", d.ChosenAgent))
	if err := r.record(ctx, t, d); err != nil {
		return nil, err
	}

	if d.ChosenAgent == "" {
		// no rule, no fallback: park for a human
		if err := r.ledger.SetStatus(ctx, t.ID, task.StatusWorking, "router", "routing"); err != nil {
			return nil, err
		}
		if err := r.ledger.SetStatus(ctx, t.ID, task.StatusInputRequired, "router", "no matching agent and no fallback"); err != nil {
			return nil, err
		}
		return d, nil
	}

	_, verID, ok := r.reg.Get(d.ChosenAgent)
	if !ok {
		return nil, fmt.Errorf("route: chose %q but it is not in the snapshot", d.ChosenAgent)
	}
	if err := r.ledger.AssignAgent(ctx, t.ID, d.ChosenAgent, verID, "router", d); err != nil {
		return nil, err
	}
	return d, nil
}

func (r *Router) record(ctx context.Context, t *task.Task, d *Decision) error {
	inputs, err := json.Marshal(map[string]any{
		"source": t.Source, "kind": t.Kind, "labels": t.Labels,
		"upstream": d.Upstream,
	})
	if err != nil {
		return err
	}
	cands, err := json.Marshal(d.Candidates)
	if err != nil {
		return err
	}
	var ruleID any
	if d.RuleID != "" {
		ruleID = d.RuleID
	}
	_, err = r.db.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO routing_decisions (task_id, inputs, candidates, chosen_agent, method, rule_id, propensity, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`),
		t.ID, string(inputs), string(cands), d.ChosenAgent, d.Method, ruleID, d.Propensity, store.TimeToDB(r.now()))
	if err != nil {
		return fmt.Errorf("record routing decision: %w", err)
	}
	return nil
}
