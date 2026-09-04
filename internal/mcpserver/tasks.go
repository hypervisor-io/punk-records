package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/taskboard"
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
			body := memory.RenderTaskStatus(in.State, strings.TrimSpace(in.Summary), in.Sha, in.Tests, in.Phase, in.Deviation)
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

	registerBoardTools(s, d, nsr)
}

type listTasksIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	State     string `json:"state,omitempty" jsonschema:"only rows in this state: pending | in_progress | review | blocked | done"`
}

type awaitIn struct {
	Namespace      string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait for a change before returning anyway (default 60, max 300); keep it under your client's tool timeout"`
}

type awaitOut struct {
	taskboard.Board
	Changed bool     `json:"changed"`
	Changes []string `json:"changes"`
}

const (
	awaitDefault = 60 * time.Second
	awaitMax     = 300 * time.Second
)

func filterBoard(b taskboard.Board, state string) taskboard.Board {
	if state == "" {
		return b
	}
	kept := b.Tasks[:0:0]
	for _, t := range b.Tasks {
		if t.State == state {
			kept = append(kept, t)
		}
	}
	b.Tasks = kept
	return b
}

func registerBoardTools(s *mcp.Server, d Deps, nsr *nsResolver) {
	mcp.AddTool(s, &mcp.Tool{Name: "list_tasks",
		Description: "The task board of a namespace: every /tasks/<id> with its parsed state, one-line status, dependencies, holder and lease, whether it is ready to start, plus next (the first ready id), counts and members. Cheap: one line per task; recall /tasks/<id> for the full text."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listTasksIn) (*mcp.CallToolResult, taskboard.Board, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			if in.State != "" && !validState(in.State) {
				return nil, taskboard.Board{}, fmt.Errorf("state must be one of %s", strings.Join(memory.TaskStates, ", "))
			}
			b, err := taskboard.Build(ctx, d.Mem, d.Region, ns)
			if err != nil {
				return nil, taskboard.Board{}, err
			}
			return nil, filterBoard(b, in.State), nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "await_tasks",
		Description: "Block until something under /tasks changes in the namespace (a task, status or claim), or the timeout passes, then return the task board with changed and the keys that fired. Use it instead of a polling loop."},
		func(ctx context.Context, req *mcp.CallToolRequest, in awaitIn) (*mcp.CallToolResult, awaitOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			timeout := time.Duration(in.TimeoutSeconds) * time.Second
			if timeout <= 0 {
				timeout = awaitDefault
			}
			if timeout > awaitMax {
				timeout = awaitMax
			}
			keys := taskboard.WaitForChange(ctx, d.Bus, ns, timeout)
			b, err := taskboard.Build(ctx, d.Mem, d.Region, ns)
			if err != nil {
				return nil, awaitOut{}, err
			}
			if keys == nil {
				keys = []string{}
			}
			return nil, awaitOut{Board: b, Changed: len(keys) > 0, Changes: keys}, nil
		})
}
