package itbench

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/llm"
)

// maxToolChars bounds a frozen tool result; the runtime truncates anyway.
const maxToolChars = 4000

// Tools presents a scenario's frozen observability snapshots as the SRE
// tool surface an agent investigates with. It implements agent.ToolCaller
// so it drops straight into a Runtime, exactly like a live MCP tool pool -
// but every answer is deterministic, read from the scenario files.
type Tools struct{ scn *Scenario }

// NewTools wraps a scenario as a frozen SRE tool provider.
func NewTools(s *Scenario) *Tools { return &Tools{scn: s} }

var toolDefs = []llm.Tool{
	{Name: "get_alerts", Description: "List the firing alerts for the incident."},
	{Name: "query_metrics", Description: "Query metric samples (Prometheus text). Optional arg: query (substring filter)."},
	{Name: "get_kubernetes_events", Description: "Recent Kubernetes cluster events."},
	{Name: "get_kubernetes_objects", Description: "Kubernetes resource states. Optional arg: kind (substring filter)."},
	{Name: "get_logs", Description: "Application/system logs (OTel). Optional arg: filter (substring)."},
	{Name: "get_traces", Description: "Distributed traces (OTel). Optional arg: filter (substring)."},
}

// filterSchema is the JSON Schema for the optional single-string arg.
func filterSchema(prop string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"` + prop +
		`":{"type":"string","description":"optional substring filter"}},"additionalProperties":false}`)
}

// Tools returns the SRE tool definitions with argument schemas.
func (t *Tools) Tools() []llm.Tool {
	out := make([]llm.Tool, len(toolDefs))
	copy(out, toolDefs)
	for i := range out {
		switch out[i].Name {
		case "query_metrics":
			out[i].Schema = filterSchema("query")
		case "get_kubernetes_objects":
			out[i].Schema = filterSchema("kind")
		case "get_logs", "get_traces":
			out[i].Schema = filterSchema("filter")
		default:
			out[i].Schema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
		}
	}
	return out
}

// Call answers a tool invocation from the frozen scenario data.
func (t *Tools) Call(_ context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "get_alerts":
		return clip(orNone(t.scn.Alerts)), nil
	case "query_metrics":
		return clip(grep(t.scn.Metrics, argString(args, "query"))), nil
	case "get_kubernetes_events":
		return clip(orNone(t.scn.K8sEvents)), nil
	case "get_kubernetes_objects":
		return clip(grep(t.scn.K8sObjects, argString(args, "kind"))), nil
	case "get_logs":
		return clip(grep(t.scn.Logs, argString(args, "filter"))), nil
	case "get_traces":
		return clip(grep(t.scn.Traces, argString(args, "filter"))), nil
	default:
		return "", &unknownToolError{name}
	}
}

type unknownToolError struct{ name string }

func (e *unknownToolError) Error() string { return "itbench: unknown tool " + e.name }

func argString(args json.RawMessage, key string) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// grep returns only the lines of data containing filter (case-insensitive);
// empty filter returns everything.
func grep(data, filter string) string {
	if filter == "" || data == "" {
		return orNone(data)
	}
	f := strings.ToLower(filter)
	var b strings.Builder
	for _, line := range strings.Split(data, "\n") {
		if strings.Contains(strings.ToLower(line), f) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return "no matching rows"
	}
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no data"
	}
	return s
}

func clip(s string) string {
	if len(s) > maxToolChars {
		return s[:maxToolChars] + "\n...[truncated]"
	}
	return s
}
