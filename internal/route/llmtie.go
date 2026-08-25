package route

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// LLMTie breaks routing ties with one cheap model call (P10.6). Any
// failure or unrecognized answer degrades to the deterministic order;
// routing never depends on a model being reachable.
type LLMTie struct {
	Client llm.Client
	Reg    *registry.Registry
}

func (l *LLMTie) Pick(ctx context.Context, t *task.Task, candidates []string) (string, error) {
	if l.Client == nil {
		return "", fmt.Errorf("llm tie: no client")
	}
	type cand struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	cs := make([]cand, 0, len(candidates))
	snap := l.Reg.Current()
	for _, name := range candidates {
		c := cand{Name: name}
		if snap != nil {
			if a, ok := snap.Bundle.Agents[name]; ok {
				c.Description = a.Description
			}
		}
		cs = append(cs, c)
	}
	payload, err := json.Marshal(map[string]any{
		"task":       map[string]any{"source": t.Source, "kind": t.Kind, "labels": t.Labels},
		"candidates": cs,
	})
	if err != nil {
		return "", err
	}
	res, err := l.Client.Chat(llm.WithTaskID(ctx, t.ID), []llm.Turn{
		{Role: "system", Content: "You route operations tasks to the best-fitting domain agent. Reply with EXACTLY one candidate name, nothing else."},
		{Role: "user", Content: string(payload)},
	}, nil)
	if err != nil {
		return "", err
	}
	answer := strings.ToLower(strings.TrimSpace(res.Content))
	for _, name := range candidates {
		if answer == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("llm tie: unrecognized answer %q", answer)
}
