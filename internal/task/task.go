// Package task is the event-sourced ledger: A2A-shaped lifecycle, append-
// only events with per-task sequence numbers, budgets, leases. Every
// status change is an event; the ledger IS the audit log.
package task

import (
	"encoding/json"
	"fmt"
	"time"
)

// Statuses (A2A-shaped).
const (
	StatusSubmitted     = "submitted"
	StatusWorking       = "working"
	StatusInputRequired = "input_required"
	StatusCompleted     = "completed"
	StatusFailed        = "failed"
	StatusCanceled      = "canceled"
)

// Event types. Every constant gets a payload struct and a round-trip test.
const (
	EventSubmitted       = "submitted"
	EventRouted          = "routed"
	EventStatusChange    = "status_change"
	EventIteration       = "iteration"
	EventToolCall        = "tool_call"
	EventFinding         = "finding"
	EventProposal        = "proposal"
	EventBudgetExhausted = "budget_exhausted"
	EventHandoff         = "handoff"
	EventError           = "error"
	EventLabel           = "label"   // harvested ground truth (P13.3)
	EventMessage         = "message" // A2A inbound/outbound message (task history)
)

// transitions is the whole state machine. working -> submitted is the
// lease-reaper requeue path.
var transitions = map[string]map[string]bool{
	StatusSubmitted:     {StatusWorking: true, StatusCanceled: true},
	StatusWorking:       {StatusCompleted: true, StatusFailed: true, StatusInputRequired: true, StatusCanceled: true, StatusSubmitted: true},
	StatusInputRequired: {StatusWorking: true, StatusCanceled: true},
	StatusCompleted:     {},
	StatusFailed:        {},
	StatusCanceled:      {},
}

// ErrBadTransition is the typed refusal for illegal status moves.
type ErrBadTransition struct{ From, To string }

func (e *ErrBadTransition) Error() string {
	return fmt.Sprintf("task: illegal transition %s -> %s", e.From, e.To)
}

// CheckTransition validates a status move against the state machine.
func CheckTransition(from, to string) error {
	allowed, ok := transitions[from]
	if !ok {
		return fmt.Errorf("task: unknown status %q", from)
	}
	if _, ok := transitions[to]; !ok {
		return fmt.Errorf("task: unknown status %q", to)
	}
	if !allowed[to] {
		return &ErrBadTransition{From: from, To: to}
	}
	return nil
}

// Budget caps one task's spend. Zero fields mean "no cap set here";
// resolution against config defaults happens at submit.
type Budget struct {
	Tokens    int   `json:"tokens"`
	ToolCalls int   `json:"tool_calls"`
	WallMS    int64 `json:"wall_ms"`
	Subagents int   `json:"subagents"`
}

// Task is one row of the ledger's current-state view.
type Task struct {
	ID              string            `json:"id"`
	ExternalRef     string            `json:"external_ref,omitempty"`
	Source          string            `json:"source"`
	Kind            string            `json:"kind"`
	Status          string            `json:"status"`
	AgentName       string            `json:"agent_name,omitempty"`
	AgentVersionID  int64             `json:"agent_version_id,omitempty"`
	ParentTaskID    string            `json:"parent_task_id,omitempty"`
	HandoffContract string            `json:"handoff_contract"`
	Labels          map[string]string `json:"labels,omitempty"`
	Budget          Budget            `json:"budget"`
	SpentTokens     int               `json:"spent_tokens"`
	SpentToolCalls  int               `json:"spent_tool_calls"`
	StartedAt       *time.Time        `json:"started_at,omitempty"`
	LeaseExpiresAt  *time.Time        `json:"lease_expires_at,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// Event is one append-only ledger entry.
type Event struct {
	ID        int64           `json:"id"`
	TaskID    string          `json:"task_id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Actor     string          `json:"actor"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// StatusChangePayload is the payload of every status_change event.
type StatusChangePayload struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}
