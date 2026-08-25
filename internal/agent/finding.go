package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Finding is the structured investigation result an agent must produce.
// Evidence is mandatory: every finding cites the tool_call events that
// support it, or it is rejected (evidence or it did not happen).
type Finding struct {
	Summary    string     `json:"summary"`
	Confidence string     `json:"confidence"` // high | medium | low
	Evidence   []Evidence `json:"evidence"`
}

type Evidence struct {
	ToolCallSeq int64  `json:"tool_call_seq"`
	Note        string `json:"note,omitempty"`
}

type findingEnvelope struct {
	Finding *Finding `json:"finding"`
}

// ErrNoFinding means the content was not a finding payload at all.
var ErrNoFinding = errors.New("agent: no finding in content")

// parseFinding extracts a finding JSON object from model output,
// tolerating markdown fences and surrounding prose.
func parseFinding(content string) (*Finding, error) {
	s := strings.TrimSpace(content)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, ErrNoFinding
	}
	var env findingEnvelope
	if err := json.Unmarshal([]byte(s[start:end+1]), &env); err != nil || env.Finding == nil {
		return nil, ErrNoFinding
	}
	return env.Finding, nil
}

// validateFinding enforces the evidence contract against the tool calls
// that actually happened in this task.
func validateFinding(f *Finding, toolCallSeqs map[int64]bool) error {
	if strings.TrimSpace(f.Summary) == "" {
		return errors.New("finding rejected: summary is empty")
	}
	switch f.Confidence {
	case "high", "medium", "low":
	default:
		return fmt.Errorf("finding rejected: confidence %q (want high|medium|low)", f.Confidence)
	}
	if len(f.Evidence) == 0 {
		return errors.New("finding rejected: evidence is required; cite tool_call_seq values")
	}
	for _, e := range f.Evidence {
		if !toolCallSeqs[e.ToolCallSeq] {
			return fmt.Errorf("finding rejected: evidence cites tool_call_seq %d which does not exist", e.ToolCallSeq)
		}
	}
	return nil
}
