package a2a

import (
	"encoding/json"
	"time"

	"github.com/hypervisor-io/punk-records/internal/task"
)

// ContextLabel is the task-label key under which we stash the A2A contextId.
const ContextLabel = "a2a.context"

// StateFromLedger maps a Punk Records task status to an A2A TaskState.
// Five states are identical on the wire; only input_required differs
// (underscore vs the A2A kebab-case).
func StateFromLedger(status string) TaskState {
	switch status {
	case task.StatusSubmitted:
		return StateSubmitted
	case task.StatusWorking:
		return StateWorking
	case task.StatusInputRequired:
		return StateInputRequired
	case task.StatusCompleted:
		return StateCompleted
	case task.StatusFailed:
		return StateFailed
	case task.StatusCanceled:
		return StateCanceled
	default:
		return StateUnknown
	}
}

// TaskFromLedger builds the A2A Task view from a ledger task and its events.
// History is drawn from message events; artifacts from finding events.
// historyLength <= 0 means "all".
func TaskFromLedger(t *task.Task, events []task.Event, historyLength int) Task {
	out := Task{
		Kind:      KindTask,
		ID:        t.ID,
		ContextID: t.Labels[ContextLabel],
		Status: TaskStatus{
			State:     StateFromLedger(t.Status),
			Timestamp: t.UpdatedAt.UTC().Format(time.RFC3339),
		},
		Metadata: map[string]any{
			"source":          t.Source,
			"kind":            t.Kind,
			"handoffContract": t.HandoffContract,
		},
	}
	if t.AgentName != "" {
		out.Metadata["agent"] = t.AgentName
	}

	for _, e := range events {
		switch e.Type {
		case task.EventMessage:
			var m Message
			if err := json.Unmarshal(e.Payload, &m); err == nil && m.MessageID != "" {
				out.History = append(out.History, m)
			}
		case task.EventFinding:
			if a, ok := artifactFromFinding(e); ok {
				out.Artifacts = append(out.Artifacts, a)
			}
		}
	}

	if historyLength > 0 && len(out.History) > historyLength {
		out.History = out.History[len(out.History)-historyLength:]
	}
	return out
}

// artifactFromFinding turns a finding event into an A2A artifact. The whole
// finding object rides as a data part; a human-readable name is lifted from
// its summary/title when present.
func artifactFromFinding(e task.Event) (Artifact, bool) {
	// finding events wrap the payload as {"finding": {...}}
	var env struct {
		Finding json.RawMessage `json:"finding"`
	}
	body := e.Payload
	if err := json.Unmarshal(e.Payload, &env); err == nil && len(env.Finding) > 0 {
		body = env.Finding
	}
	if len(body) == 0 {
		return Artifact{}, false
	}
	var meta struct {
		Summary string `json:"summary"`
		Title   string `json:"title"`
	}
	_ = json.Unmarshal(body, &meta)
	name := meta.Title
	if name == "" {
		name = meta.Summary
	}
	if name == "" {
		name = "finding"
	}
	return Artifact{
		ArtifactID: artifactID(e),
		Name:       name,
		Parts:      []Part{{Kind: KindDataPart, Data: body}},
		Metadata:   map[string]any{"seq": e.Seq, "actor": e.Actor},
	}, true
}

func artifactID(e task.Event) string {
	return "finding-" + itoa(e.Seq)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// EncodeMessage serializes an A2A message for storage as a ledger event
// payload. Kind is normalized so round-tripped history is spec-shaped.
func EncodeMessage(m Message) ([]byte, error) {
	m.Kind = KindMessage
	return json.Marshal(m)
}
