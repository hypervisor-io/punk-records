package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/a2a"
)

// delegateTimeout bounds a blocking delegation before the tool returns the
// remote task in whatever (possibly non-terminal) state it has reached.
const delegateTimeout = 60 * time.Second

type delegateIn struct {
	Remote string `json:"remote" jsonschema:"name of a configured remote A2A agent"`
	Text   string `json:"text" jsonschema:"the task/message to delegate to the remote agent"`
	TaskID string `json:"task_id,omitempty" jsonschema:"continue an existing remote task by id"`
}

type delegatedArtifact struct {
	Name string `json:"name"`
	Data string `json:"data,omitempty"`
}

type delegateOut struct {
	Remote    string              `json:"remote"`
	TaskID    string              `json:"task_id"`
	State     string              `json:"state"`
	Artifacts []delegatedArtifact `json:"artifacts,omitempty"`
}

// registerA2ATools exposes `delegate`: the brain hands a task to a foreign
// A2A agent and returns its result, so a Punk agent can compose work across
// the wider agent mesh. Remotes and their tokens are resolved at wiring.
func registerA2ATools(s *mcp.Server, d Deps) {
	remotes := make(map[string]*a2a.Client, len(d.A2ARemotes))
	names := make([]string, 0, len(d.A2ARemotes))
	for _, r := range d.A2ARemotes {
		remotes[r.Name] = a2a.NewClient(r.Endpoint, r.Token)
		names = append(names, r.Name)
	}

	mcp.AddTool(s, &mcp.Tool{Name: "delegate",
		Description: "Delegate a task to a remote A2A agent (" + strings.Join(names, ", ") + ") and return its task state and artifacts."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in delegateIn) (*mcp.CallToolResult, delegateOut, error) {
			cl, ok := remotes[in.Remote]
			if !ok {
				return nil, delegateOut{}, fmt.Errorf("unknown remote %q (configured: %s)", in.Remote, strings.Join(names, ", "))
			}
			if strings.TrimSpace(in.Text) == "" {
				return nil, delegateOut{}, fmt.Errorf("text is required")
			}
			msg := a2a.TextMessage(in.Text)
			if in.TaskID != "" {
				msg.TaskID = in.TaskID
			}
			cctx, cancel := context.WithTimeout(ctx, delegateTimeout)
			defer cancel()
			t, err := cl.SendMessage(cctx, msg, &a2a.SendMessageConfig{Blocking: true})
			if err != nil {
				return nil, delegateOut{}, err
			}
			out := delegateOut{Remote: in.Remote, TaskID: t.ID, State: string(t.Status.State)}
			for _, a := range t.Artifacts {
				out.Artifacts = append(out.Artifacts, delegatedArtifact{Name: a.Name, Data: partsText(a.Parts)})
			}
			return nil, out, nil
		})
}

// partsText renders artifact parts to a compact string for the tool result.
func partsText(parts []a2a.Part) string {
	var b strings.Builder
	for _, p := range parts {
		switch p.Kind {
		case a2a.KindTextPart:
			b.WriteString(p.Text)
		case a2a.KindDataPart:
			b.Write(p.Data)
		}
	}
	return b.String()
}
