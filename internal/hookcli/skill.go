package hookcli

import (
	"strings"
	"text/template"
)

// SkillName is the skill directory and frontmatter name every agent sees.
const SkillName = "punk-memory"

// SkillMarker is the first body line of every skill punk writes. A file
// at the target path without it was written by someone else and is left
// untouched.
const SkillMarker = "<!-- managed by punk connect; edit outside this file's marker and punk will not overwrite it -->"

// SkillOpts selects the per-agent rendering of the canonical skill.
type SkillOpts struct {
	Agent      string // claude-code | codex | opencode | cursor | copilot | antigravity | hermes | openclaw | pi
	ServerURL  string // printed in the setup section; empty omits it
	Namespace  string // baked project namespace from --project; empty means "resolved from the workspace"
	ToolPrefix string // "mcp__punk__" for Claude Code, "punk_" for OpenCode and pi, "" elsewhere
	Hermes     bool   // add version and metadata.hermes frontmatter
	Pi         bool   // pi has four HTTP-backed tools, not the MCP set
}

// ToolName renders a tool reference for the target agent.
func ToolName(prefix, tool string) string { return prefix + tool }

// skillDescriptionPi omits the tools pi's extension does not expose.
const skillDescriptionPi = "Use punk-records shared memory: resolve the namespace with punk_whoami, recall known keys, search when wording is unknown, and remember durable decisions and gotchas. Use whenever prior context, decisions, incidents, conventions or another agent's work may already be recorded."

const skillDescription = "Use punk-records shared memory: resolve the namespace, recall known keys, search or unified_search when wording is unknown, remember durable decisions and gotchas, coordinate with other agents through claims and /tasks facts, and rate hits with feedback. Use whenever prior context, decisions, incidents, conventions or another agent's work may already be recorded."

var skillTmpl = template.Must(template.New("skill").Funcs(template.FuncMap{
	"tool": func(o SkillOpts, name string) string { return "`" + ToolName(o.ToolPrefix, name) + "`" },
}).Parse(`---
name: punk-memory
description: {{.Description}}
{{- if .Opts.Hermes}}
version: 1.0.0
metadata:
  hermes:
    tags: [memory, coordination, punk]
    category: memory
{{- end}}
---
{{.Marker}}

# Punk Records memory

Punk Records is the shared memory plane for this workspace and for every agent connected to it: prior sessions, decisions, conventions, incidents, entities, relations, and the work other agents are doing right now. It is evidence about the past and about other agents, not a substitute for reading the current code.
{{if .Opts.ServerURL}}
Server: {{.Opts.ServerURL}}. {{end}}{{if .Opts.Pi}}This agent exposes four punk tools backed by the HTTP API: {{tool .Opts "whoami"}}, {{tool .Opts "recall"}}, {{tool .Opts "search"}}, {{tool .Opts "remember"}}.{{else if .Opts.ToolPrefix}}Punk tools are prefixed ` + "`{{.Opts.ToolPrefix}}`" + ` in this agent.{{else}}Punk tools appear under their plain names (recall, search, remember, and so on) in this agent.{{end}}

## Namespaces

- A namespace is one memory region. {{if .Opts.Namespace}}This project is pinned to ` + "`{{.Opts.Namespace}}`" + `.{{else}}When you omit it, the server resolves it from the workspace root you are in (` + "`agent-<repo>`" + `).{{end}}
- Call {{tool .Opts "whoami"}} once at session start. It returns the namespace, how it was resolved, and your agent identity.
- Pass an explicit namespace only to read or write a shared region another agent named (for example a coordination namespace a planner created).
- Never invent a namespace or a key. Discover keys {{if .Opts.Pi}}by recalling a prefix{{else}}with {{tool .Opts "list_keys"}} or by recalling a prefix{{end}}.

## Reading memory
{{if not .Opts.Pi}}
Pick the read tool by what you know: {{tool .Opts "recall"}} for a known key prefix, {{tool .Opts "search"}} for words or identifiers, {{tool .Opts "unified_search"}} when the wording is unknown or the question spans facts and relations.
{{end}}
{{if .Opts.Pi}}{{.RoutingPi}}{{else}}{{.Routing}}{{end}}

## Key conventions

| Prefix | Holds |
| --- | --- |
| ` + "`/decisions/<topic>`" + ` | why something was chosen; one fact per decision |
| ` + "`/conventions/<area>`" + ` | rules the team follows in this repo |
| ` + "`/incidents/<id>`" + ` | what broke, cause, fix |
| ` + "`/runbook/<name>`" + ` | how to operate something |
| ` + "`/entities/<name>`" + ` | people, services, systems (mostly auto-extracted) |
| ` + "`/code-map/<domain>`" + ` | architecture seeded from the repo; may carry a stale flag |
| ` + "`/tasks/<id>`" + `, ` + "`/tasks/<id>/status`" + ` | work items and their state (see coordination) |
| ` + "`/questions/<id>`" + `, ` + "`/answers/<id>`" + ` | blocked-work questions and their answers |
| ` + "`/agent-sessions/`" + `, ` + "`/observations/`" + `, ` + "`/mental-models/`" + ` | reserved: written by hooks and consolidation, read-only for you |

## Coordinating with other agents
{{if .Opts.Pi}}
The pi tools cover reading and writing. Coordination tools are available through the punk MCP server from other agents; the conventions below still apply to what you read and write.
{{else}}
- Register once per session: {{tool .Opts "register"}} with your agent name and role, so others can see who is in the region.
- Before editing a file or taking a task, {{tool .Opts "claim_work"}} on its key (a path or ` + "`/tasks/<id>`" + `) with a ttl_seconds; if it fails, someone holds it: pick something else. {{tool .Opts "release_work"}} when done. {{tool .Opts "list_claims"}} shows who holds what.
{{end}}
- Tasks are facts. A planner writes one fact per task at ` + "`/tasks/<id>`" + ` (title, files, depends). A worker claims ` + "`/tasks/<id>`" + `, does the work, then writes ` + "`/tasks/<id>/status`" + ` with ` + "`done: <commit> <summary>; tests: <command>`" + ` or ` + "`blocked: <reason>`" + `. Absent status means pending.
- Blocked: write the question to ` + "`/questions/<id>`" + ` and move on; check ` + "`/answers/<id>`" + ` before retrying.
- Polling: recall the ` + "`/tasks`" + ` prefix. Clients that support MCP resources can subscribe to ` + "`punk://memory/<namespace>/tasks`" + ` and get a notification on every change instead of polling. From a shell, ` + "`GET /v1/namespaces/<ns>/events?prefix=/tasks`" + ` is a server-sent event stream.
- Completion: the planner reads statuses; the last worker writes ` + "`/plan/status`" + `.
- Domain investigations (database, SRE, memory-ops) are a different system: ` + "`submit_task`" + ` and ` + "`get_task`" + ` in the full toolset route an incident to a domain agent. Do not use them for coding work.

## Writing memory

- {{tool .Opts "remember"}}: one durable fact per key under the conventions above; the latest revision per key wins and history is kept. Set importance 0.6 to 0.9 for decisions others must not miss.
{{- if not .Opts.Pi}}
- {{tool .Opts "remember_many"}} for several facts in one call.
- {{tool .Opts "remember_document"}} for long text: only changed chunks are rewritten; pass a path only on the stdio server.
- {{tool .Opts "feedback"}} with the ids of hits that helped or misled; ranking learns from it.
{{- end}}
- Bodies are prose, not JSON dumps. Say what and why in under a paragraph.
- Do not store secrets. Write-time scrubbing may redact or block them, and a blocked chunk is silently skipped.

## Etiquette

- Start of session: {{tool .Opts "whoami"}}, then {{tool .Opts "recall"}} ` + "`/decisions`" + ` and ` + "`/conventions`" + `, then ask memory before re-deriving something a previous session may have recorded.
- One or two direct calls beat a sub-agent launched just to query memory.
- Treat a compact hit as already-read evidence; recall its key only when the clipped body is insufficient.
- Verify memory against the code before acting on it, and do not repeat it back unprompted.
`))

// routingBody is shared with the MCP server's initialize instructions so
// the two never drift. Plain prose, no markdown headers.
const routingBody = `- recall: you know the key prefix (for example /decisions, /code-map, /entities). Deterministic, unranked.
- search: you know words or identifiers. Set hybrid and scored for ranked fusion. Put exact identifiers, error strings, flags or file names in anchors; they are extra retrieval routes, not filters. Pass format: compact unless you need attributes or timestamps.
- unified_search: wording unknown, or the answer spans facts and relations (architecture, causality, history, "why" questions). Prefer it first; pass format: compact.
- triplet_search and neighbors: follow relations from a known key.
- recall_as_of: what was believed at a past instant.
- list_keys: discover keys. Never invent a key.
- Flags on hits: stale means newer raw facts exist since this synthesis; invalidated means a later fact superseded it (demoted, not hidden); model means a curated mental model; relation means the hit is an edge rendered as "from -> type -> to".
- A compact hit is already-read evidence. recall its key only when the clipped body is insufficient.
- Writing: remember one durable decision, fix, convention or gotcha per hierarchical key; remember_many for batches; remember_document for long text; feedback with the ids of hits that helped or misled. Do not store secrets.`

// RoutingSection returns the shared read/write routing prose.
func RoutingSection() string { return routingBody }

// routingBodyPi is the reading and writing guidance for pi, whose four
// extension tools call the HTTP API: no relation tools, no batch write,
// no feedback.
const routingBodyPi = `- recall: you know the key prefix (for example /decisions, /code-map, /entities). Deterministic, unranked.
- search: ranked hybrid search, compact hits (key, clipped body, score, flags). Put exact identifiers, error strings, flags or file names in anchors; they are extra retrieval routes, not filters.
- Flags on hits: stale means newer raw facts exist since this synthesis; invalidated means a later fact superseded it (demoted, not hidden); model means a curated mental model.
- A compact hit is already-read evidence. recall its key only when the clipped body is insufficient.
- Writing: remember one durable decision, fix, convention or gotcha per hierarchical key; keep bodies to a paragraph. Do not store secrets.`

// RenderSkill renders the canonical punk skill for one agent.
func RenderSkill(o SkillOpts) string {
	var b strings.Builder
	desc := skillDescription
	if o.Pi {
		desc = skillDescriptionPi
	}
	_ = skillTmpl.Execute(&b, map[string]any{
		"Opts":        o,
		"Description": desc,
		"Marker":      SkillMarker,
		"Routing":     routingBody,
		"RoutingPi":   routingBodyPi,
	})
	return b.String()
}
