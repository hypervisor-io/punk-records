// Package taskboard joins the /tasks facts of a namespace with its live
// work claims and members into one machine-readable board: what is done,
// who holds what, which task is ready next. It is the read model behind
// the list_tasks and await_tasks tools and GET /v1/namespaces/{ns}/tasks.
package taskboard

import (
	"context"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
)

// Task is one board row: the parsed fact plus the claim on it and whether
// it can be started now.
type Task struct {
	memory.TaskRow
	Holder         string `json:"holder,omitempty"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
	Ready          bool   `json:"ready"`
}

// Counts is the per-state tally; Total counts rows.
type Counts struct {
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Review     int `json:"review"`
	Blocked    int `json:"blocked"`
	Done       int `json:"done"`
}

// Board is the whole namespace view. Next is the first ready id in id
// order, empty when nothing can start.
type Board struct {
	Namespace string          `json:"namespace"`
	Now       string          `json:"now"`
	Next      string          `json:"next,omitempty"`
	Counts    Counts          `json:"counts"`
	Tasks     []Task          `json:"tasks"`
	Members   []region.Member `json:"members"`
}

// Build reads the board. reg may be nil: then no claims or members are
// joined and readiness ignores holders.
func Build(ctx context.Context, mem *memory.Store, reg *region.Store, ns string) (Board, error) {
	b := Board{Namespace: ns, Now: time.Now().UTC().Format(time.RFC3339), Tasks: []Task{}, Members: []region.Member{}}
	rows, err := mem.ListTasks(ctx, ns)
	if err != nil {
		return b, err
	}
	holders := map[string]region.Claim{}
	if reg != nil {
		claims, err := reg.ListClaims(ctx, ns)
		if err != nil {
			return b, err
		}
		for _, c := range claims {
			holders[c.Key] = c
		}
		members, err := reg.Members(ctx, ns)
		if err != nil {
			return b, err
		}
		b.Members = members
	}
	done := map[string]bool{}
	for _, r := range rows {
		if r.State == "done" {
			done[r.ID] = true
		}
	}
	for _, r := range rows {
		t := Task{TaskRow: r}
		if c, ok := holders["/tasks/"+r.ID]; ok {
			t.Holder, t.LeaseExpiresAt = c.Holder, c.ExpiresAt
		}
		t.Ready = r.State == "pending" && t.Holder == ""
		for _, dep := range r.DependsOn {
			if !done[dep] {
				t.Ready = false
			}
		}
		if t.Ready && b.Next == "" {
			b.Next = r.ID
		}
		b.Counts.Total++
		switch r.State {
		case "in_progress":
			b.Counts.InProgress++
		case "review":
			b.Counts.Review++
		case "blocked":
			b.Counts.Blocked++
		case "done":
			b.Counts.Done++
		default:
			b.Counts.Pending++
		}
		b.Tasks = append(b.Tasks, t)
	}
	return b, nil
}
