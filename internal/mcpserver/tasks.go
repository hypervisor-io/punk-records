package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type statusIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	ID        string `json:"id" jsonschema:"task id, the <id> in /tasks/<id>"`
	State     string `json:"state" jsonschema:"pending | in_progress | review | blocked | done"`
	Summary   string `json:"summary" jsonschema:"one line: what happened or what is blocking"`
	Sha       string `json:"sha,omitempty" jsonschema:"commit that finishes the task (done)"`
	Tests     string `json:"tests,omitempty" jsonschema:"command that proves it (done)"`
	Phase     string `json:"phase,omitempty" jsonschema:"stage word for in_progress: red, green, refactor, review"`
	Deviation string `json:"deviation,omitempty" jsonschema:"where the work departed from the plan"`
	Agent     string `json:"agent,omitempty" jsonschema:"optional; defaults to this session's identity (see whoami)"`
}

type statusOut struct {
	Key      string `json:"key"`
	State    string `json:"state"`
	Body     string `json:"body"`
	Released bool   `json:"released_claim"`
}

// renderStatus builds the canonical status body so ParseTaskState and
// TaskCounts read it back without guessing. State is already validated.
func renderStatus(state, summary, sha, tests, phase, deviation string) string {
	var b strings.Builder
	b.WriteString(state + ":")
	switch state {
	case "done":
		if sha != "" {
			b.WriteString(" " + sha)
		}
		b.WriteString(" " + summary)
		if tests != "" {
			b.WriteString("; tests: " + tests)
		}
	case "in_progress":
		if phase != "" {
			b.WriteString(" " + phase)
		}
		b.WriteString(" " + summary)
	default:
		b.WriteString(" " + summary)
	}
	if deviation != "" {
		b.WriteString("\ndeviation: " + deviation)
	}
	return b.String()
}

func validState(s string) bool {
	for _, st := range memory.TaskStates {
		if s == st {
			return true
		}
	}
	return false
}

// registerTaskTools adds the task board tools. They need only Mem; Region
// and Bus are optional and degrade (no claim release, no waiting).
func registerTaskTools(s *mcp.Server, d Deps, nsr *nsResolver) {
	mcp.AddTool(s, &mcp.Tool{Name: "set_task_status",
		Description: "Report a task's state in the /tasks convention: writes /tasks/<id>/status with a canonical body and structured attributes. done and blocked also release your claim on /tasks/<id>."},
		func(ctx context.Context, req *mcp.CallToolRequest, in statusIn) (*mcp.CallToolResult, statusOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			in.ID = strings.TrimSpace(in.ID)
			if in.ID == "" || strings.ContainsRune(in.ID, '/') {
				return nil, statusOut{}, fmt.Errorf("id is required and must not contain '/'")
			}
			if !validState(in.State) {
				return nil, statusOut{}, fmt.Errorf("state must be one of %s", strings.Join(memory.TaskStates, ", "))
			}
			if strings.TrimSpace(in.Summary) == "" {
				return nil, statusOut{}, fmt.Errorf("summary is required")
			}
			if in.Agent == "" {
				in.Agent = nsr.identity(req)
			}
			body := renderStatus(in.State, strings.TrimSpace(in.Summary), in.Sha, in.Tests, in.Phase, in.Deviation)
			attrs := map[string]any{"state": in.State, "summary": strings.TrimSpace(in.Summary), "agent": in.Agent}
			for k, v := range map[string]string{"sha": in.Sha, "tests": in.Tests, "phase": in.Phase, "deviation": in.Deviation} {
				if v != "" {
					attrs[k] = v
				}
			}
			key := "/tasks/" + in.ID + "/status"
			if _, err := d.Mem.Write(ctx, memory.WriteInput{
				Namespace: ns, Key: key, Body: body, Attributes: attrs,
				Author: in.Agent, Writer: in.Agent, Importance: 0.6,
			}); err != nil {
				return nil, statusOut{}, err
			}
			out := statusOut{Key: key, State: in.State, Body: body}
			if d.Region != nil && (in.State == "done" || in.State == "blocked") {
				if err := d.Region.ReleaseWork(ctx, ns, "/tasks/"+in.ID, in.Agent); err == nil {
					out.Released = true
				}
			}
			return nil, out, nil
		})
}
