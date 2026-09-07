// Package mcpserver exposes Punk Records' capabilities as MCP tools so any
// agent (Claude Code, a copilot, another service) can submit tasks and
// use the memory plane. Tools reuse the exact service layer the REST API
// uses, so MCP and HTTP can never drift.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/reflect"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/route"
	"github.com/hypervisor-io/punk-records/internal/skillmine"
	"github.com/hypervisor-io/punk-records/internal/task"
)

type Deps struct {
	Ledger           *task.Ledger
	Router           *route.Router
	Reg              *registry.Registry
	Mem              *memory.Store
	Region           *region.Store        // nil disables region tools
	Bus              *bus.Bus             // nil disables resource subscriptions
	A2ARemotes       []A2ARemote          // outbound delegation targets; empty disables the delegate tool
	LLM              llm.Client           // nil disables the reflect tool (deterministic-first)
	Expander         memory.QueryExpander // nil disables search's expand flag (deterministic-first)
	DefaultBudget    task.Budget
	NamespaceFor     func(cwd string) string // maps a workspace path to its memory namespace; nil disables root-based resolution
	DefaultNamespace string                  // used when a call omits namespace and no root is known; empty = agent-default
	LocalFiles       bool                    // allow remember_document{path}: only for the stdio server, which runs as the user
	Toolset          string                  // "" or "full": every tool; "agent": the lean session set (see toolset.go)
	SkillNamespace   string                  // memory namespace the authored skill catalog is published into; empty falls back to DefaultNamespace
	Log              *slog.Logger            // lifecycle logging (skill index sync); nil discards
}

// SkillIndexNamespace resolves the namespace the server's skill catalog
// (authored skills from the spec registry, mined skills from the propose
// ingest) is published into and that search_skills/load_skill read by
// default: SkillNamespace, then DefaultNamespace, then "agent-default".
// The mapping is explicit and fixed per server so reload syncs and
// discovery reads always meet in the same place. search_skills and
// load_skill target this namespace directly whenever their namespace
// argument is omitted, instead of the caller's workspace-root
// namespace: the catalog lives here regardless of which repo a client
// has open, so root-based resolution would otherwise miss it.
func (d Deps) SkillIndexNamespace() string {
	if d.SkillNamespace != "" {
		return d.SkillNamespace
	}
	if d.DefaultNamespace != "" {
		return d.DefaultNamespace
	}
	return "agent-default"
}

// A2ARemote is a resolved foreign A2A agent the delegate tool can reach.
// Token is already read from its env var by the caller.
type A2ARemote struct {
	Name     string
	Endpoint string
	Token    string
}

type submitIn struct {
	ExternalRef string            `json:"external_ref,omitempty" jsonschema:"consumer reference, e.g. incident:42"`
	Source      string            `json:"source" jsonschema:"event source system"`
	Kind        string            `json:"kind,omitempty" jsonschema:"task kind, default investigate"`
	Labels      map[string]string `json:"labels,omitempty" jsonschema:"routing labels (domain, severity, ...)"`
}

type submitOut struct {
	TaskID  string `json:"task_id"`
	Created bool   `json:"created"`
	Agent   string `json:"agent,omitempty"`
	Method  string `json:"method,omitempty"`
}

type getTaskIn struct {
	ID string `json:"id" jsonschema:"task id"`
}

type getTaskOut struct {
	Task   *task.Task `json:"task"`
	Events []eventOut `json:"events"`
}

// eventOut mirrors task.Event with the payload decoded to a plain value:
// json.RawMessage would be schema-inferred as a byte array, which breaks
// MCP output validation.
type eventOut struct {
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	Actor     string `json:"actor"`
	Payload   any    `json:"payload,omitempty"`
	CreatedAt string `json:"created_at"`
}

type listAgentsOut struct {
	Agents []agentInfo `json:"agents"`
}

type agentInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Autonomy    string `json:"autonomy"`
}

type rememberIn struct {
	Namespace  string  `json:"namespace,omitempty" jsonschema:"memory namespace"`
	Key        string  `json:"key" jsonschema:"hierarchical key like /service/payments/db"`
	Body       string  `json:"body" jsonschema:"the fact"`
	Author     string  `json:"author,omitempty"`
	Importance float64 `json:"importance,omitempty" jsonschema:"author-declared weight 0..1"`
}

type rememberManyFact struct {
	Key        string         `json:"key"`
	Body       string         `json:"body"`
	Importance float64        `json:"importance,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type rememberManyIn struct {
	Namespace string             `json:"namespace,omitempty" jsonschema:"memory namespace; optional, resolved from the client's workspace root when empty"`
	Author    string             `json:"author,omitempty"`
	Facts     []rememberManyFact `json:"facts" jsonschema:"1 to 200 facts written in order; each is an independent append (latest wins per key)"`
}

type rememberManyOut struct {
	Written int      `json:"written"`
	IDs     []string `json:"ids"`
}

const rememberManyMax = 200

type documentIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"memory namespace; optional, resolved from the client's workspace root (see whoami) when empty"`
	Prefix    string `json:"prefix" jsonschema:"hierarchical key prefix, e.g. /docs/runbook"`
	Text      string `json:"text,omitempty" jsonschema:"the document body"`
	Path      string `json:"path,omitempty" jsonschema:"absolute path of a local file to ingest instead of text; only honoured by the stdio server (punk mcp)"`
	Author    string `json:"author,omitempty"`
	// Source switches on source-aware ingest (I01): stable content-
	// addressed chunk identity plus per-chunk provenance. Shape:
	// {id, uri, revision, media_type, sections: [{name, page, text}]}.
	// Deliberately a schemaless map: the agent toolset wire payload is
	// budgeted (guidance_budget_test.go) and typed fields do not fit
	// the ratchet; decodeDocumentSource enforces the exact shape and
	// names it in every error instead. The map's own wire bytes are
	// offset within the same budget by the trimmed remember_document
	// description, which no longer spells out reconcile mechanics.
	Source map[string]any `json:"source,omitempty"`
}

// documentSourceWire is the typed shape of documentIn.Source.
type documentSourceWire struct {
	ID        string                `json:"id"`
	URI       string                `json:"uri"`
	Revision  string                `json:"revision"`
	MediaType string                `json:"media_type"`
	Sections  []documentSectionWire `json:"sections"`
}

// documentSectionWire is one section entry of documentIn.Source.
type documentSectionWire struct {
	Name string `json:"name"`
	Page int    `json:"page"`
	Text string `json:"text"`
}

// decodeDocumentSource validates the free-form source object strictly:
// unknown keys or wrong value types (at any nesting level) error with
// the expected shape named, so a typo is actionable instead of silently
// dropping provenance.
func decodeDocumentSource(raw map[string]any) (documentSourceWire, error) {
	buf, err := json.Marshal(raw)
	if err != nil {
		return documentSourceWire{}, fmt.Errorf("source: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	var ds documentSourceWire
	if err := dec.Decode(&ds); err != nil {
		return documentSourceWire{}, fmt.Errorf("source: %w (want id/uri/revision/media_type and/or sections: [{name, page, text}])", err)
	}
	return ds, nil
}

type documentOut struct {
	Written   int `json:"written"`
	Unchanged int `json:"unchanged"`
	Removed   int `json:"removed"`
	Blocked   int `json:"blocked,omitempty"`
}

type recallIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Prefix    string `json:"prefix,omitempty" jsonschema:"key prefix filter"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"cap result payload in ~tokens (default 8000; -1 for no cap)"`
}

type listKeysIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Prefix    string `json:"prefix,omitempty" jsonschema:"key prefix filter"`
}

// recallOut carries the budgeted facts plus, when the budget cut the
// result, how many facts matched and what to do about it.
type recallOut struct {
	Facts     []memory.Fact `json:"facts"`
	Truncated bool          `json:"truncated,omitempty"`
	Total     int           `json:"total,omitempty"`
	Note      string        `json:"note,omitempty"`
}

// defaultMaxTokens caps a read tool's payload when the caller passes no
// max_tokens. Agent harnesses drop or spill tool results above a few
// tens of KB (OpenCode keeps nothing past 50 KB; 8000 body tokens plus
// JSON overhead stays under that), so an uncapped recall
// of a busy prefix reaches the model as an empty truncation notice. A
// caller that really wants everything passes max_tokens: -1.
const defaultMaxTokens = 8000

// effectiveMaxTokens maps the wire value to the budget TokenBudget
// expects: 0 (unset) becomes the default, negative means no cap.
func effectiveMaxTokens(maxTokens int) int {
	switch {
	case maxTokens == 0:
		return defaultMaxTokens
	case maxTokens < 0:
		return 0
	}
	return maxTokens
}

// truncationNote tells the model how to get the rest without re-reading
// what it already has.
func truncationNote(got, total int) string {
	return fmt.Sprintf("%d of %d facts returned within the token budget; narrow the prefix, raise max_tokens (-1 for no cap), or use list_keys and recall single keys", got, total)
}

// budgetRecall applies the effective budget to facts and fills the
// truncation fields when it cut anything.
func budgetRecall(facts []memory.Fact, maxTokens int) recallOut {
	kept := memory.TokenBudget(facts, effectiveMaxTokens(maxTokens))
	if len(kept) == 0 && len(facts) > 0 {
		kept = facts[:1] // one oversized document beats an empty answer
	}
	out := recallOut{Facts: kept}
	if len(kept) < len(facts) {
		out.Truncated = true
		out.Total = len(facts)
		out.Note = truncationNote(len(kept), len(facts))
	}
	return out
}

type listKeysOut struct {
	Keys []string `json:"keys"`
}

// New builds the MCP server with every tool registered. When a bus is
// wired, task resources (punk://tasks/{id}) are subscribable and
// notify on every status change (P11.3).
func New(d Deps) *mcp.Server {
	var opts *mcp.ServerOptions
	var subs *subscriptions
	if d.Bus != nil {
		subs = newSubscriptions()
		opts = &mcp.ServerOptions{
			SubscribeHandler: func(ctx context.Context, req *mcp.SubscribeRequest) error {
				// A02: namespace-scoped resources authorize at subscribe
				// time; a denial records no subscription, so nothing is
				// ever delivered. The gate captured here reauthorizes
				// every later delivery (revocation policy, see
				// reauthorizeSubscribers), and add rejects a subscribe
				// that would attach this session's subscriptions to a
				// different verified subject than the one that created
				// them (credential-changed session reuse).
				if ns, _, ok := parseMemoryURI(req.Params.URI); ok {
					if err := authorizeNS(ctx, ns, authz.OpRead); err != nil {
						return err
					}
				}
				return subs.add(req.Params.URI, req.Session, namespaceGateFrom(ctx))
			},
			UnsubscribeHandler: func(_ context.Context, req *mcp.UnsubscribeRequest) error {
				subs.remove(req.Params.URI, req.Session)
				return nil
			},
		}
	}
	if opts == nil {
		opts = &mcp.ServerOptions{}
	}
	opts.Instructions = Instructions
	s := mcp.NewServer(&mcp.Implementation{Name: "punk-records", Version: "0.1.0"}, opts)
	s.AddReceivingMiddleware(traceMiddleware)
	nsr := newNSResolver(d.NamespaceFor, d.DefaultNamespace)
	registerWhoami(s, nsr)

	if d.Bus != nil {
		s.AddResourceTemplate(&mcp.ResourceTemplate{
			Name:        "task",
			URITemplate: "punk://tasks/{id}",
			Description: "One coordination task with its event-sourced history; subscribable.",
			MIMEType:    "application/json",
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			id := strings.TrimPrefix(req.Params.URI, "punk://tasks/")
			tk, events, err := d.Ledger.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(map[string]any{"task": tk, "events": events})
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI: req.Params.URI, MIMEType: "application/json", Text: string(raw),
			}}}, nil
		})

		s.AddResourceTemplate(&mcp.ResourceTemplate{
			Name:        "memory",
			URITemplate: "punk://memory/{namespace}{+prefix}",
			Description: "Live facts under a key prefix; subscribable. Notifies on any write, tombstone or link change under the prefix.",
			MIMEType:    "application/json",
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			ns, prefix, ok := parseMemoryURI(req.Params.URI)
			if !ok {
				return nil, fmt.Errorf("bad memory resource uri %q", req.Params.URI)
			}
			// A02: the URI's namespace is a read boundary like any recall.
			if err := authorizeNS(ctx, ns, authz.OpRead); err != nil {
				return nil, err
			}
			facts, err := d.Mem.Recall(ctx, ns, prefix, 200)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(map[string]any{"namespace": ns, "prefix": prefix, "facts": facts})
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI: req.Params.URI, MIMEType: "application/json", Text: string(raw),
			}}}, nil
		})
		events, cancel := d.Bus.Subscribe()
		go func() {
			defer cancel()
			for e := range events {
				switch e.Kind {
				case "task_status":
					// the task ledger is global (matches get_task and
					// /v1/tasks): no namespace grant applies
					uri := "punk://tasks/" + e.Key
					if subs.any(uri) {
						_ = s.ResourceUpdated(context.Background(),
							&mcp.ResourceUpdatedNotificationParams{URI: uri})
					}
				case "memory":
					ns, key, found := strings.Cut(e.Key, ":")
					if !found {
						continue
					}
					for _, uri := range subs.matching(func(u string) bool {
						uns, uprefix, ok := parseMemoryURI(u)
						return ok && uns == ns && strings.HasPrefix(key, uprefix)
					}) {
						// A02: reauthorize every delivery; revoked
						// subscribers are dropped and closed first, then
						// the notification goes to the remaining
						// authorized ones (revocation policy, see
						// reauthorizeSubscribers).
						reauthorizeSubscribers(subs, uri, ns)
						_ = s.ResourceUpdated(context.Background(),
							&mcp.ResourceUpdatedNotificationParams{URI: uri})
					}
				}
			}
		}()
	}

	mcp.AddTool(s, &mcp.Tool{Name: "submit_task",
		Description: "Submit an investigation task; dedups by open external_ref and routes to a domain agent."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in submitIn) (*mcp.CallToolResult, submitOut, error) {
			if in.Source == "" {
				return nil, submitOut{}, fmt.Errorf("source is required")
			}
			tk, created, err := d.Ledger.Submit(ctx, task.SubmitInput{
				ExternalRef: in.ExternalRef, Source: in.Source, Kind: in.Kind,
				Labels: in.Labels, Budget: d.DefaultBudget, Actor: "mcp",
			})
			if err != nil {
				return nil, submitOut{}, err
			}
			out := submitOut{TaskID: tk.ID, Created: created}
			if created {
				dec, err := d.Router.Route(ctx, tk)
				if err != nil {
					return nil, submitOut{}, err
				}
				out.Agent, out.Method = dec.ChosenAgent, dec.Method
			}
			return nil, out, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_task",
		Description: "Fetch one task with its full event-sourced history."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getTaskIn) (*mcp.CallToolResult, getTaskOut, error) {
			tk, events, err := d.Ledger.Get(ctx, in.ID)
			if err != nil {
				return nil, getTaskOut{}, err
			}
			out := getTaskOut{Task: tk, Events: make([]eventOut, 0, len(events))}
			for _, e := range events {
				var payload any
				_ = json.Unmarshal(e.Payload, &payload)
				out.Events = append(out.Events, eventOut{
					Seq: e.Seq, Type: e.Type, Actor: e.Actor,
					Payload: payload, CreatedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
				})
			}
			return nil, out, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_agents",
		Description: "List the domain agents in the active spec snapshot."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listAgentsOut, error) {
			snap := d.Reg.Current()
			out := listAgentsOut{Agents: []agentInfo{}}
			if snap != nil {
				for _, a := range snap.Bundle.Agents {
					out.Agents = append(out.Agents, agentInfo{
						Name: a.Name, Version: a.Version,
						Description: a.Description, Autonomy: a.Autonomy,
					})
				}
			}
			return nil, out, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "remember",
		Description: "Store a fact in the memory plane (append-only, latest wins per key)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in rememberIn) (*mcp.CallToolResult, *memory.Fact, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, nil, err
			}
			f, err := d.Mem.Write(ctx, memory.WriteInput{
				Namespace: ns, Key: in.Key, Body: in.Body,
				Author: in.Author, Writer: in.Author, Importance: in.Importance,
			})
			if err != nil {
				return nil, nil, err
			}
			return nil, f, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "recall",
		Description: "Recall the latest live facts under a key prefix."},
		func(ctx context.Context, req *mcp.CallToolRequest, in recallIn) (*mcp.CallToolResult, recallOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, recallOut{}, err
			}
			facts, err := d.Mem.Recall(ctx, ns, in.Prefix, 0)
			if err != nil {
				return nil, recallOut{}, err
			}
			return nil, budgetRecall(facts, in.MaxTokens), nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_keys",
		Description: "List live memory keys under a prefix. Discover keys, never invent them. Cheap: keys only, no bodies, no budget; use it to enumerate a busy prefix such as /tasks before recalling single keys."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listKeysIn) (*mcp.CallToolResult, listKeysOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, listKeysOut{}, err
			}
			keys, err := d.Mem.ListKeys(ctx, ns, in.Prefix)
			if err != nil {
				return nil, listKeysOut{}, err
			}
			return nil, listKeysOut{Keys: keys}, nil
		})

	registerMemoryV2Tools(s, d, nsr)
	registerSkillTools(s, d, nsr)
	registerTaskTools(s, d, nsr)

	// Task S01 startup lifecycle: publish the spec snapshot's authored
	// skills into the skill index namespace so search_skills sees what
	// the registry already validated, without a separate ingest step.
	// Hot reload re-syncs in cmd/punk's serve loop on every snapshot
	// version advance; mined drafts sync at propose time (cmdSkills).
	syncSkillIndex(context.Background(), d)

	if d.Region != nil {
		registerRegionTools(s, d, nsr)
	}
	if len(d.A2ARemotes) > 0 {
		registerA2ATools(s, d)
	}
	if d.LLM != nil {
		registerReflectTool(s, d, nsr)
	}

	applyToolset(s, d.Toolset)
	return s
}

type reflectIn struct {
	Namespace string          `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Query     string          `json:"query"`
	Level     string          `json:"level,omitempty" jsonschema:"reasoning depth: minimal|low|medium|high|max (default low); scales the tool-round budget"`
	Schema    json.RawMessage `json:"schema,omitempty" jsonschema:"optional JSON Schema for the answer; the structured field of the result carries the parsed value"`
}

// registerReflectTool exposes agentic hierarchical retrieval — mental
// models, then observations, then raw recall to verify — only when a
// model is configured (Deps.LLM != nil); every other memory tool works
// with ai.enabled=false.
func registerReflectTool(s *mcp.Server, d Deps, nsr *nsResolver) {
	eng := reflect.New(d.Mem, d.LLM)
	mcp.AddTool(s, &mcp.Tool{Name: "reflect",
		Description: "Answer a question by reasoning hierarchically over the memory plane (mental models, then observations, then raw recall), with citations validated against what was actually retrieved."},
		func(ctx context.Context, req *mcp.CallToolRequest, in reflectIn) (*mcp.CallToolResult, reflect.Answer, error) {
			// resolve the namespace exactly like every other memory tool
			// (its input schema always promised roots resolution) and
			// enforce read on it (A02)
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, reflect.Answer{}, err
			}
			ans, err := eng.ReflectWith(ctx, ns, in.Query, reflect.Opts{Level: in.Level, Schema: in.Schema})
			if err != nil {
				return nil, reflect.Answer{}, err
			}
			return nil, ans, nil
		})
}

type searchIn struct {
	Namespace    string   `json:"namespace"`
	Query        string   `json:"query"`
	Hybrid       bool     `json:"hybrid,omitempty" jsonschema:"fuse vector + full-text when embeddings are enabled"`
	Scored       bool     `json:"scored,omitempty" jsonschema:"with hybrid, return each hit's score and its fts/vector/recency/importance/access components"`
	Fusion       string   `json:"fusion,omitempty" jsonschema:"rrf (default) or interleave"`
	Temporal     bool     `json:"temporal,omitempty" jsonschema:"parse a time window from the query text (e.g. 'errors last month') and search within it"`
	Strategy     string   `json:"strategy,omitempty" jsonschema:"explicit retrieval route exact|semantic|historical|relationship|procedural|auto; compact hits plus routed mode/reasons/fallback meta"`
	Expand       bool     `json:"expand,omitempty" jsonschema:"with hybrid+scored, expand the query into up to 3 LLM reformulations and union results (ignored when no model configured)"`
	Limit        int      `json:"limit,omitempty"`
	MaxTokens    int      `json:"max_tokens,omitempty" jsonschema:"cap result payload in ~tokens (default 8000; -1 for no cap)"`
	Format       string   `json:"format,omitempty" jsonschema:"'' (full facts) or 'compact': key, clipped body, score, flags only; use compact unless attributes or timestamps are needed"`
	Anchors      []string `json:"anchors,omitempty" jsonschema:"exact identifiers, error strings, flags or file names; each is an extra phrase-match retrieval route fused by rank, not a filter (hybrid+scored only)"`
	RepoRevision string   `json:"repo_revision,omitempty" jsonschema:"current git revision of the workspace; code-map hits seeded from another revision are flagged stale"`
}

// routeMeta is the inspectable routing evidence of a strategy call
// (R01): which mode actually ran, what was requested, the router's
// reasons (rule names, vetoes, alias rewrites) and any degradation
// fallback. The hits ride the tool's usual compact projection, so the
// wire cost of routing metadata stays a closed-form bound. Like the
// sibling out structs it carries no jsonschema prose - the wire budget
// is ratcheted in guidance_budget_test.go.
type routeMeta struct {
	Mode          string   `json:"mode"`
	RequestedMode string   `json:"requested_mode"`
	Reasons       []string `json:"reasons,omitempty"`
	Fallback      string   `json:"fallback,omitempty"`
}

func routeMetaOf(res memory.RouteResult) *routeMeta {
	return &routeMeta{
		Mode:          string(res.Mode),
		RequestedMode: string(res.RequestedMode),
		Reasons:       res.Reasons,
		Fallback:      res.Fallback,
	}
}

// searchOut is search's response shape: plain facts, or (with Hybrid+Scored)
// facts plus the score breakdown that explains why each one ranked where it did.
// With Format=compact, Hits carries the token-lean projection instead.
// With Strategy, Hits carries the routed hits in the same compact
// projection and Route carries the routing evidence.
type searchOut struct {
	Facts     []memory.Fact       `json:"facts,omitempty"`
	Results   []memory.ScoredFact `json:"results,omitempty"`
	Hits      []memory.CompactHit `json:"hits,omitempty"`
	Route     *routeMeta          `json:"route,omitempty"`
	Truncated bool                `json:"truncated,omitempty"`
	Total     int                 `json:"total,omitempty"`
	Note      string              `json:"note,omitempty"`
}

// markTruncated fills the truncation fields when the budget kept fewer
// than total entries.
func (o searchOut) markTruncated(kept, total int) searchOut {
	if kept < total {
		o.Truncated = true
		o.Total = total
		o.Note = truncationNote(kept, total)
	}
	return o
}

type asOfIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Prefix    string `json:"prefix,omitempty"`
	AsOf      string `json:"as_of" jsonschema:"RFC3339 instant to read the region as of"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"cap result payload in ~tokens (default 8000; -1 for no cap)"`
}

type forgetIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Key       string `json:"key"`
	Author    string `json:"author,omitempty"`
}

// finishSearch applies the token budget and, for format=compact, the
// compact projection. Compaction runs before budgeting so the budget is
// spent on clipped bodies, which is the point of asking for compact.
func finishSearch(in searchIn, facts []memory.Fact, scored []memory.ScoredFact) searchOut {
	budget := effectiveMaxTokens(in.MaxTokens)
	if in.Format == "compact" {
		var hits []memory.CompactHit
		if scored != nil {
			hits = memory.CompactScored(scored, 0)
		} else {
			hits = memory.CompactFacts(facts, 0)
		}
		kept := memory.TokenBudgetCompact(hits, budget)
		if len(kept) == 0 && len(hits) > 0 {
			kept = hits[:1]
		}
		return searchOut{Hits: kept}.markTruncated(len(kept), len(hits))
	}
	if scored != nil {
		kept := memory.TokenBudgetScored(scored, budget)
		if len(kept) == 0 && len(scored) > 0 {
			kept = scored[:1]
		}
		return searchOut{Results: kept}.markTruncated(len(kept), len(scored))
	}
	kept := memory.TokenBudget(facts, budget)
	if len(kept) == 0 && len(facts) > 0 {
		kept = facts[:1]
	}
	return searchOut{Facts: kept}.markTruncated(len(kept), len(facts))
}

func registerMemoryV2Tools(s *mcp.Server, d Deps, nsr *nsResolver) {
	mcp.AddTool(s, &mcp.Tool{Name: "remember_document",
		Description: "Chunk and store a document under a key prefix; delta ingest rewrites only changed chunks."},
		func(ctx context.Context, req *mcp.CallToolRequest, in documentIn) (*mcp.CallToolResult, documentOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, documentOut{}, err
			}
			var srcDoc *memory.SourceDocument
			if len(in.Source) > 0 {
				ds, err := decodeDocumentSource(in.Source)
				if err != nil {
					return nil, documentOut{}, err
				}
				doc := memory.SourceDocument{Source: memory.DocumentSource{
					ID: ds.ID, URI: ds.URI, Revision: ds.Revision, MediaType: ds.MediaType,
				}}
				for _, sec := range ds.Sections {
					doc.Sections = append(doc.Sections, memory.DocumentSection{
						Name: sec.Name, Page: sec.Page, Text: sec.Text})
				}
				srcDoc = &doc
			}
			hasSections := srcDoc != nil && len(srcDoc.Sections) > 0
			text := in.Text
			switch {
			case hasSections && (in.Text != "" || in.Path != ""):
				return nil, documentOut{}, fmt.Errorf("pass text or path, not source.sections")
			case in.Path != "" && in.Text != "":
				return nil, documentOut{}, fmt.Errorf("pass text or path, not both")
			case in.Path != "":
				if !d.LocalFiles {
					return nil, documentOut{}, fmt.Errorf("path ingestion is disabled on this server; pass text, or use the stdio server (punk mcp)")
				}
				if !filepath.IsAbs(in.Path) {
					return nil, documentOut{}, fmt.Errorf("path must be absolute")
				}
				raw, err := os.ReadFile(in.Path)
				if err != nil {
					return nil, documentOut{}, err
				}
				text = string(raw)
			case in.Text == "" && !hasSections:
				return nil, documentOut{}, fmt.Errorf("text or path is required")
			}
			var w, u, r, b int
			if srcDoc != nil {
				if !hasSections {
					srcDoc.Sections = []memory.DocumentSection{{Text: text}}
				}
				w, u, r, b, err = d.Mem.WriteDocumentSource(ctx, ns, in.Prefix, *srcDoc, in.Author)
			} else {
				w, u, r, b, err = d.Mem.WriteDocument(ctx, ns, in.Prefix, text, in.Author)
			}
			if err != nil {
				return nil, documentOut{}, err
			}
			return nil, documentOut{Written: w, Unchanged: u, Removed: r, Blocked: b}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "remember_many",
		Description: "Store up to 200 facts in one call (same semantics as remember, one write per fact). Use instead of repeated remember calls."},
		func(ctx context.Context, req *mcp.CallToolRequest, in rememberManyIn) (*mcp.CallToolResult, rememberManyOut, error) {
			if len(in.Facts) == 0 || len(in.Facts) > rememberManyMax {
				return nil, rememberManyOut{}, fmt.Errorf("facts: want 1..%d entries, got %d", rememberManyMax, len(in.Facts))
			}
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, rememberManyOut{}, err
			}
			out := rememberManyOut{IDs: make([]string, 0, len(in.Facts))}
			for i, f := range in.Facts {
				fact, err := d.Mem.Write(ctx, memory.WriteInput{
					Namespace: ns, Key: f.Key, Body: f.Body, Attributes: f.Attributes,
					Author: in.Author, Writer: in.Author, Importance: f.Importance,
				})
				if err != nil {
					return nil, out, fmt.Errorf("facts[%d] %s: %w", i, f.Key, err)
				}
				out.Written++
				out.IDs = append(out.IDs, fact.ID)
			}
			return nil, out, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "search",
		Description: "Search a region's facts by full text, or hybrid vector+FTS when embeddings are enabled."},
		func(ctx context.Context, req *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, searchOut{}, err
			}
			// R01: an explicit strategy opts into inspectable routed
			// retrieval. Hits ride the compact projection (compaction
			// before budgeting, same as finishSearch) and the routing
			// evidence rides Route; the legacy knobs below are bypassed
			// only on this opt-in path, never by default. Namespace was
			// resolved and authorized above, so the response stays inside
			// the A02 guard.
			if in.Strategy != "" {
				res, err := d.Mem.RoutedSearch(ctx, ns, memory.RouteRequest{
					Mode:    memory.RouteMode(in.Strategy),
					Query:   in.Query,
					Limit:   in.Limit,
					Anchors: in.Anchors,
				})
				if err != nil {
					return nil, searchOut{}, err
				}
				hits := memory.CompactUnified(res.Hits, 0)
				kept := memory.TokenBudgetCompact(hits, effectiveMaxTokens(in.MaxTokens))
				if len(kept) == 0 && len(hits) > 0 {
					kept = hits[:1]
				}
				out := searchOut{Hits: kept, Route: routeMetaOf(res)}
				return nil, out.markTruncated(len(kept), len(hits)), nil
			}
			// Temporal is plain-search only: if Hybrid or Fusion=interleave
			// is also set, that path wins and Temporal is ignored (WindowedSearch
			// is FTS-only and can't do hybrid/scored/interleave).
			if in.Temporal && !in.Hybrid && in.Fusion != "interleave" {
				if from, to, cleaned, ok := memory.ParseTemporal(in.Query, time.Now()); ok {
					facts, err := d.Mem.WindowedSearch(ctx, ns, cleaned, from, to, in.Limit)
					if err != nil {
						return nil, searchOut{}, err
					}
					return nil, finishSearch(in, facts, nil), nil
				}
			}
			if in.Fusion == "interleave" {
				scored, err := d.Mem.InterleaveSearch(ctx, ns, in.Query, in.Limit)
				if err != nil {
					return nil, searchOut{}, err
				}
				memory.MarkCodeMapStale(scored, in.RepoRevision)
				return nil, finishSearch(in, nil, scored), nil
			}
			if in.Hybrid && in.Scored {
				var scored []memory.ScoredFact
				var err error
				opts := memory.HybridOpts{Limit: in.Limit, Anchors: in.Anchors}
				if in.Expand && d.Expander != nil {
					// HybridSearchExpandedWith already applies applyRerank
					// internally over the merged candidates; routing here
					// avoids a second, redundant rerank pass.
					scored, err = d.Mem.HybridSearchExpandedWith(ctx, ns, in.Query, opts, d.Expander)
				} else {
					scored, err = d.Mem.HybridSearchRerankedWith(ctx, ns, in.Query, opts)
				}
				if err != nil {
					return nil, searchOut{}, err
				}
				memory.MarkCodeMapStale(scored, in.RepoRevision)
				return nil, finishSearch(in, nil, scored), nil
			}
			var facts []memory.Fact
			if in.Hybrid {
				facts, err = d.Mem.HybridSearch(ctx, ns, in.Query, in.Limit, 0)
			} else {
				facts, err = d.Mem.Search(ctx, ns, in.Query, in.Limit)
			}
			if err != nil {
				return nil, searchOut{}, err
			}
			return nil, finishSearch(in, facts, nil), nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "recall_as_of",
		Description: "Read a region as it was at a past instant (bi-temporal); shows the facts valid then."},
		func(ctx context.Context, req *mcp.CallToolRequest, in asOfIn) (*mcp.CallToolResult, recallOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, recallOut{}, err
			}
			at, err := time.Parse(time.RFC3339, in.AsOf)
			if err != nil {
				return nil, recallOut{}, fmt.Errorf("as_of: %w", err)
			}
			facts, err := d.Mem.RecallAsOf(ctx, ns, in.Prefix, at, 0)
			if err != nil {
				return nil, recallOut{}, err
			}
			return nil, budgetRecall(facts, in.MaxTokens), nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "forget",
		Description: "Tombstone a key (closes its validity window); history is preserved."},
		func(ctx context.Context, req *mcp.CallToolRequest, in forgetIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, nil, err
			}
			if err := d.Mem.Forget(ctx, ns, in.Key, in.Author); err != nil {
				return nil, nil, err
			}
			return nil, map[string]string{"status": "tombstoned", "key": in.Key}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "link",
		Description: "Add a typed edge between two facts (from_key -> to_key), e.g. a change touches a file. An optional description (a one-sentence NL restatement of the fact the edge encodes) is embedded, making the relation itself retrievable via triplet_search."},
		func(ctx context.Context, req *mcp.CallToolRequest, in linkIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, nil, err
			}
			if in.Description != "" {
				if err := d.Mem.AddLinkDescribed(ctx, ns, in.FromKey, in.ToKey, in.LinkType, 1.0, in.Description); err != nil {
					return nil, nil, err
				}
				return nil, map[string]string{"status": "linked"}, nil
			}
			if err := d.Mem.AddLink(ctx, ns, in.FromKey, in.ToKey, in.LinkType); err != nil {
				return nil, nil, err
			}
			return nil, map[string]string{"status": "linked"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "unlink",
		Description: "Soft-delete a typed edge (from_key -> to_key): closes its validity window rather than deleting the row, so neighbors as_of an earlier instant still sees it. Errors if no live edge matches."},
		func(ctx context.Context, req *mcp.CallToolRequest, in unlinkIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, nil, err
			}
			// InvalidateLink already defaults "" to relates_to and validates
			// the type; no need to duplicate that here.
			if err := d.Mem.InvalidateLink(ctx, ns, in.FromKey, in.ToKey, in.LinkType); err != nil {
				return nil, nil, err
			}
			return nil, map[string]string{"status": "unlinked"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "triplet_search",
		Description: "Rank source->edge->target triplets by query relevance over the edge's description and both endpoint facts; makes relations first-class retrievable for multi-hop recall."},
		func(ctx context.Context, req *mcp.CallToolRequest, in tripletSearchIn) (*mcp.CallToolResult, tripletSearchOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, tripletSearchOut{}, err
			}
			triplets, err := d.Mem.TripletSearch(ctx, ns, in.Query, in.K)
			if err != nil {
				return nil, tripletSearchOut{}, err
			}
			return nil, tripletSearchOut{Triplets: triplets}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "unified_search",
		Description: "One recall entry point that fuses fact/observation/entity/mental-model hits with relation-triplet hits via reciprocal rank fusion, instead of two separate calls."},
		func(ctx context.Context, req *mcp.CallToolRequest, in unifiedSearchIn) (*mcp.CallToolResult, unifiedSearchOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, unifiedSearchOut{}, err
			}
			// R01: with an explicit strategy the caller wants the routed
			// retrieval and its evidence instead of the fused listing;
			// hits ride the same compact projection as format=compact.
			if in.Strategy != "" {
				res, err := d.Mem.RoutedSearch(ctx, ns, memory.RouteRequest{
					Mode: memory.RouteMode(in.Strategy), Query: in.Query, Limit: in.K,
				})
				if err != nil {
					return nil, unifiedSearchOut{}, err
				}
				return nil, unifiedSearchOut{Compact: memory.CompactUnified(res.Hits, 0), Route: routeMetaOf(res)}, nil
			}
			hits, err := d.Mem.UnifiedSearch(ctx, ns, in.Query, in.K)
			if err != nil {
				return nil, unifiedSearchOut{}, err
			}
			if in.Format == "compact" {
				return nil, unifiedSearchOut{Compact: memory.CompactUnified(hits, 0)}, nil
			}
			return nil, unifiedSearchOut{Hits: hits}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "neighbors",
		Description: "List facts linked to/from a key (direction: out|in). With as_of, reads the edges valid at that past instant instead of the live set; a future as_of returns the live set, same as recall_as_of."},
		func(ctx context.Context, req *mcp.CallToolRequest, in neighborsIn) (*mcp.CallToolResult, neighborsOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, neighborsOut{}, err
			}
			dir := in.Direction
			if dir == "" {
				dir = "out"
			}
			if in.AsOf != "" {
				at, err := time.Parse(time.RFC3339, in.AsOf)
				if err != nil {
					return nil, neighborsOut{}, fmt.Errorf("as_of: %w", err)
				}
				links, err := d.Mem.NeighborsAsOf(ctx, ns, in.Key, dir, at)
				if err != nil {
					return nil, neighborsOut{}, err
				}
				return nil, neighborsOut{Links: links}, nil
			}
			links, err := d.Mem.Neighbors(ctx, ns, in.Key, dir)
			if err != nil {
				return nil, neighborsOut{}, err
			}
			return nil, neighborsOut{Links: links}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "remember_model",
		Description: "Store a curated mental model — a durable, top-tier synthesis that outranks auto-consolidated observations."},
		func(ctx context.Context, req *mcp.CallToolRequest, in rememberModelIn) (*mcp.CallToolResult, *memory.Fact, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, nil, err
			}
			f, err := d.Mem.RememberModel(ctx, ns, in.Slug, in.Body, in.SourceIDs, in.Pinned)
			if err != nil {
				return nil, nil, err
			}
			return nil, f, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_models",
		Description: "List the live curated mental models in a namespace."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listModelsIn) (*mcp.CallToolResult, listModelsOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, listModelsOut{}, err
			}
			models, err := d.Mem.ListModels(ctx, ns)
			if err != nil {
				return nil, listModelsOut{}, err
			}
			return nil, listModelsOut{Models: models}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_entities",
		Description: "List the live extracted entities (people, orgs, places, concepts) in a namespace."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listEntitiesIn) (*mcp.CallToolResult, listEntitiesOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, listEntitiesOut{}, err
			}
			entities, err := d.Mem.ListEntities(ctx, ns)
			if err != nil {
				return nil, listEntitiesOut{}, err
			}
			return nil, listEntitiesOut{Entities: entities}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "feedback",
		Description: "Rate the facts used in an answer; EWMA-updates their feedback weight, which feeds future ranking."},
		func(ctx context.Context, req *mcp.CallToolRequest, in feedbackIn) (*mcp.CallToolResult, feedbackOut, error) {
			// feedback mutates ranking weights: a write on the namespace
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, feedbackOut{}, err
			}
			if err := d.Mem.RecordFeedback(ctx, ns, in.IDs, in.Rating); err != nil {
				return nil, feedbackOut{}, err
			}
			return nil, feedbackOut{Updated: len(in.IDs)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "profile",
		Description: "Deterministic namespace digest: top entities, hot keys, recent facts, counts. No LLM."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listModelsIn) (*mcp.CallToolResult, *memory.Profile, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, nil, err
			}
			p, err := d.Mem.Profile(ctx, ns)
			if err != nil {
				return nil, nil, err
			}
			return nil, p, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "diagnose",
		Description: "Namespace health counters: quarantined rows, missing embeddings, orphan links, stale observations, expired claims."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listModelsIn) (*mcp.CallToolResult, *memory.Diagnosis, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, nil, err
			}
			dg, err := d.Mem.Diagnose(ctx, ns)
			if err != nil {
				return nil, nil, err
			}
			return nil, dg, nil
		})
}

// syncSkillIndex publishes the registry's current authored skill catalog
// into the skill index namespace (Deps.SkillIndexNamespace). Sync
// failures never abort server construction: the catalog is a discovery
// aid, and cmd/punk's serve loop retries on every snapshot version
// advance; conflicts and rejections are logged, not swallowed.
func syncSkillIndex(ctx context.Context, d Deps) {
	if d.Reg == nil || d.Mem == nil {
		return
	}
	snap := d.Reg.Current()
	if snap == nil {
		return
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rep, err := skillmine.SyncBundle(ctx, d.Mem, d.SkillIndexNamespace(), snap.Bundle)
	if err != nil {
		log.Error("skill index sync failed", "err", err)
		return
	}
	if len(rep.Conflicts) > 0 {
		log.Warn("skill versions republished with changed content stay pinned; bump metadata.version to publish",
			"conflicts", rep.Conflicts)
	}
	if len(rep.Rejected) > 0 {
		log.Warn("skills rejected by index validation", "rejected", rep.Rejected)
	}
	if rep.Indexed > 0 || rep.Tombstoned > 0 {
		log.Info("skill index synced", "namespace", d.SkillIndexNamespace(),
			"indexed", rep.Indexed, "tombstoned", rep.Tombstoned)
	}
}

// skillDiscoveryPayloadBound is the pinned ceiling for one serialized
// search_skills response. The per-field validation limits bound the
// record sizes, but JSON escaping multiplies them (every '<' becomes
// six bytes), so the bound is enforced on the actual wire form below,
// not on the field lengths: hits past the bound are trimmed. 72KiB sits
// ~9% above the measured 20-hit worst case with unescaped maximal
// fields (67772 bytes; TestSkillDiscoveryPayloadWithinBound), and a
// response holding even one inlined procedure body would blow past it.
const skillDiscoveryPayloadBound = 72 * 1024

// trimSkillPayload caps the serialized search_skills response at the
// pinned bound, accounting for JSON escaping exactly as the SDK's
// encoding/json marshal applies it. Hits are ranked, so trimming drops
// only the tail; a single hit that alone exceeds the bound is dropped
// rather than shipped over the contract.
func trimSkillPayload(skills []memory.SkillMeta, bound int) []memory.SkillMeta {
	size := len(`{"skills":[]}`)
	n := 0
	for _, sk := range skills {
		raw, err := json.Marshal(sk)
		if err != nil {
			break
		}
		add := len(raw)
		if n > 0 {
			add++ // the separating comma
		}
		if size+add > bound {
			break
		}
		size += add
		n++
	}
	return skills[:n]
}

type searchSkillsIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Query     string `json:"query" jsonschema:"words or identifiers from the procedure's name, description or declared tools"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max hits (default 10, hard cap 20, then trimmed to a 72KiB serialized response); hits are metadata only, load the procedure with load_skill"`
}

type searchSkillsOut struct {
	Skills []memory.SkillMeta `json:"skills"`
}

type loadSkillIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Name      string `json:"name" jsonschema:"skill name from a search_skills hit"`
	Version   string `json:"version,omitempty" jsonschema:"exact version from the hit; omittable only when one version is live"`
}

type loadSkillOut struct {
	Skill memory.SkillMeta `json:"skill"`
	Body  string           `json:"body"`
}

// skillToolNamespace resolves the namespace search_skills/load_skill
// read: an omitted namespace targets Deps.SkillIndexNamespace()
// directly (see its doc comment) and is authorized there, since that is
// where the catalog is published regardless of the caller's workspace
// root; an explicit namespace keeps the normal A02 resolution
// (explicit > header > roots > default).
func skillToolNamespace(ctx context.Context, d Deps, nsr *nsResolver, req *mcp.CallToolRequest, explicit string) (string, error) {
	if strings.TrimSpace(explicit) == "" {
		ns := d.SkillIndexNamespace()
		if err := authorizeNS(ctx, ns, authz.OpRead); err != nil {
			return "", err
		}
		return ns, nil
	}
	return nsr.resolveAuthed(ctx, req, explicit, authz.OpRead)
}

// registerSkillTools exposes task S01's procedural-memory tier: scoped
// discovery over skill metadata (never procedure text) plus on-demand
// loading of one exact versioned body. Both are namespace reads (A02).
func registerSkillTools(s *mcp.Server, d Deps, nsr *nsResolver) {
	mcp.AddTool(s, &mcp.Tool{Name: "search_skills",
		Description: "Discover skills (SKILL.md procedures) by metadata: name, description, tools, scope. Hits never contain the procedure; load with load_skill by name and version. Empty namespace = skill index."},
		func(ctx context.Context, req *mcp.CallToolRequest, in searchSkillsIn) (*mcp.CallToolResult, searchSkillsOut, error) {
			ns, err := skillToolNamespace(ctx, d, nsr, req, in.Namespace)
			if err != nil {
				return nil, searchSkillsOut{}, err
			}
			skills, err := d.Mem.SearchSkills(ctx, ns, in.Query, in.Limit)
			if err != nil {
				return nil, searchSkillsOut{}, err
			}
			return nil, searchSkillsOut{Skills: trimSkillPayload(skills, skillDiscoveryPayloadBound)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "load_skill",
		Description: "Load one skill's exact versioned procedure body, found via search_skills. Inactive and ungranted skills refuse to load. Empty namespace = skill index."},
		func(ctx context.Context, req *mcp.CallToolRequest, in loadSkillIn) (*mcp.CallToolResult, loadSkillOut, error) {
			ns, err := skillToolNamespace(ctx, d, nsr, req, in.Namespace)
			if err != nil {
				return nil, loadSkillOut{}, err
			}
			meta, body, err := d.Mem.LoadSkill(ctx, ns, in.Name, in.Version)
			if err != nil {
				return nil, loadSkillOut{}, err
			}
			return nil, loadSkillOut{Skill: meta, Body: body}, nil
		})
}

type feedbackIn struct {
	Namespace string   `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	IDs       []string `json:"ids" jsonschema:"fact IDs that were used in the rated answer"`
	Rating    float64  `json:"rating" jsonschema:"rating from 0 to 1, where 1 means useful and 0 means not useful"`
}

type feedbackOut struct {
	Updated int `json:"updated"`
}

type rememberModelIn struct {
	Namespace string   `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Slug      string   `json:"slug" jsonschema:"key segment: /mental-models/<slug>"`
	Body      string   `json:"body" jsonschema:"the synthesis"`
	SourceIDs []string `json:"source_ids,omitempty" jsonschema:"fact IDs this model is grounded in"`
	Pinned    bool     `json:"pinned,omitempty" jsonschema:"reserved for future refresh logic; has no effect on ranking or staleness today"`
}

type listModelsIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
}

type listModelsOut struct {
	Models []memory.Fact `json:"models"`
}

type listEntitiesIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
}

type listEntitiesOut struct {
	Entities []memory.Fact `json:"entities"`
}

type linkIn struct {
	Namespace   string `json:"namespace"`
	FromKey     string `json:"from_key"`
	ToKey       string `json:"to_key"`
	LinkType    string `json:"link_type,omitempty" jsonschema:"edge type, default relates_to"`
	Description string `json:"description,omitempty" jsonschema:"one-sentence NL restatement of the fact this edge encodes; embedded for triplet_search"`
}

// unlinkIn is deliberately its own type rather than a reuse of linkIn:
// unlink has no description or weight, and reusing linkIn would leave
// those fields present but silently ignored by the handler.
type unlinkIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	FromKey   string `json:"from_key"`
	ToKey     string `json:"to_key"`
	LinkType  string `json:"link_type,omitempty" jsonschema:"edge type, default relates_to; closes only that edge type, not every edge between the pair"`
}

type tripletSearchIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Query     string `json:"query"`
	K         int    `json:"k,omitempty"`
}

type tripletSearchOut struct {
	Triplets []memory.Triplet `json:"triplets"`
}

type unifiedSearchIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Query     string `json:"query"`
	K         int    `json:"k,omitempty"`
	Format    string `json:"format,omitempty" jsonschema:"'' or 'compact': key, clipped body, score, flags; relations render as 'from -> type -> to'"`
	Strategy  string `json:"strategy,omitempty" jsonschema:"explicit retrieval route exact|semantic|historical|relationship|procedural|auto; compact hits plus routed meta instead of the fused listing"`
}

type unifiedSearchOut struct {
	Hits    []memory.UnifiedHit `json:"hits,omitempty"`
	Compact []memory.CompactHit `json:"compact,omitempty"`
	Route   *routeMeta          `json:"route,omitempty"`
}

type neighborsIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Key       string `json:"key"`
	Direction string `json:"direction,omitempty" jsonschema:"out (default) or in"`
	AsOf      string `json:"as_of,omitempty" jsonschema:"RFC3339 instant; when set, reads edges valid then instead of the live set"`
}

type neighborsOut struct {
	Links []memory.Link `json:"links"`
}

type registerIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"brain region to join; optional, resolved from the client's workspace root (see whoami) when empty"`
	Agent     string `json:"agent,omitempty" jsonschema:"optional; defaults to this session's identity (see whoami)"`
	Role      string `json:"role,omitempty" jsonschema:"this satellite's role in the region"`
}

type membersIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
}

type regionsForIn struct {
	Agent string `json:"agent"`
}

type membersOut struct {
	Members []region.Member `json:"members"`
}

type claimIn struct {
	Namespace  string `json:"namespace"`
	Key        string `json:"key" jsonschema:"sub-key to claim, e.g. a file path"`
	Holder     string `json:"holder,omitempty" jsonschema:"optional; defaults to this session's identity (see whoami)"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

type claimsOut struct {
	Claims []region.Claim `json:"claims"`
}

func registerRegionTools(s *mcp.Server, d Deps, nsr *nsResolver) {
	mcp.AddTool(s, &mcp.Tool{Name: "claim_work",
		Description: "Claim a sub-key in a region so no other satellite works it (conflict-free work partitioning). Fails if a live claim already holds it."},
		func(ctx context.Context, req *mcp.CallToolRequest, in claimIn) (*mcp.CallToolResult, *region.Claim, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, nil, err
			}
			if in.Holder == "" {
				in.Holder = nsr.identity(req)
			}
			ttl := time.Duration(in.TTLSeconds) * time.Second
			if ttl <= 0 {
				ttl = 5 * time.Minute
			}
			c, err := d.Region.ClaimWork(ctx, ns, in.Key, in.Holder, ttl)
			if err != nil {
				return nil, nil, err
			}
			touch(ctx, d, nsr, req, ns, in.Holder)
			if d.Bus != nil {
				d.Bus.Publish(bus.Event{Kind: "claim", Key: ns + ":" + in.Key,
					Data: map[string]string{"holder": in.Holder, "action": "claimed"}})
			}
			return nil, c, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "release_work",
		Description: "Release a work claim so other satellites can take the sub-key."},
		func(ctx context.Context, req *mcp.CallToolRequest, in claimIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, nil, err
			}
			if in.Holder == "" {
				in.Holder = nsr.identity(req)
			}
			if err := d.Region.ReleaseWork(ctx, ns, in.Key, in.Holder); err != nil {
				return nil, nil, err
			}
			touch(ctx, d, nsr, req, ns, in.Holder)
			if d.Bus != nil {
				d.Bus.Publish(bus.Event{Kind: "claim", Key: ns + ":" + in.Key,
					Data: map[string]string{"holder": in.Holder, "action": "released"}})
			}
			return nil, map[string]string{"status": "released"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_claims",
		Description: "List the live work claims in a region."},
		func(ctx context.Context, req *mcp.CallToolRequest, in membersIn) (*mcp.CallToolResult, claimsOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, claimsOut{}, err
			}
			c, err := d.Region.ListClaims(ctx, ns)
			if err != nil {
				return nil, claimsOut{}, err
			}
			return nil, claimsOut{Claims: c}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "register",
		Description: "Register an agent/consumer as a satellite of a brain region (namespace). Satellites coordinate through the region's shared memory."},
		func(ctx context.Context, req *mcp.CallToolRequest, in registerIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, nil, err
			}
			if in.Agent == "" {
				in.Agent = nsr.identity(req)
			}
			if err := d.Region.Register(ctx, ns, in.Agent, in.Role); err != nil {
				return nil, nil, err
			}
			return nil, map[string]string{"status": "registered", "namespace": ns, "agent": in.Agent}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_region_members",
		Description: "List the satellites registered to a brain region."},
		func(ctx context.Context, req *mcp.CallToolRequest, in membersIn) (*mcp.CallToolResult, membersOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpRead)
			if err != nil {
				return nil, membersOut{}, err
			}
			m, err := d.Region.Members(ctx, ns)
			if err != nil {
				return nil, membersOut{}, err
			}
			return nil, membersOut{Members: m}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_agent_regions",
		Description: "List the brain regions an agent is registered to."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in regionsForIn) (*mcp.CallToolResult, membersOut, error) {
			m, err := d.Region.Regions(ctx, in.Agent)
			if err != nil {
				return nil, membersOut{}, err
			}
			// A02: cross-region enumeration. The agent argument is
			// client-chosen, so under enforcement each source region is
			// checked separately against the verified subject's read
			// grants and unreadable regions are omitted. Trusted
			// transports (no gate) see the full list.
			if g := namespaceGateFrom(ctx); g != nil {
				kept := m[:0]
				for _, mem := range m {
					if g.Allow(ctx, mem.Namespace, authz.OpRead) {
						kept = append(kept, mem)
					}
				}
				m = kept
			}
			return nil, membersOut{Members: m}, nil
		})
}

// subscriptions tracks resource subscriptions per session, together
// with the authorization gate captured at subscribe time (nil for
// trusted transports like stdio) and the verified subject the session's
// subscriptions belong to. Deliveries reauthorize through the gate on
// every event, so a revoked grant OR a revoked credential stops
// subsequent notifications (task A02 revocation policy, see
// reauthorizeSubscribers).
//
// The registry never discards tracked state while the MCP SDK can still
// deliver to a session: the SDK keeps its own per-session subscription
// set (mcp.Server.resourceSubscriptions) and keeps notifying every
// session that ever subscribed, so forgetting a session here - e.g. via
// a wholesale bounded reset - would silently exempt it from every later
// reauthorization while deliveries continue. Instead of resetting, the
// registry bounds the number of distinct sessions: when a NEW session
// would exceed maxCachedSessions, the oldest tracked sessions are
// evicted by closing them, and a closed session loses its SDK delivery
// subscriptions - an evicted session cannot receive what the registry
// can no longer gate. Closing an already-closed (churned-out) session
// is a no-op, so ordinary churn is pruned under the same bound.
type subscriptions struct {
	mu       sync.RWMutex
	uris     map[string]map[*mcp.ServerSession]NamespaceGate
	sessions map[*mcp.ServerSession]*subSession
	order    []*mcp.ServerSession // first-subscribe order, oldest first
}

// subSession is the registry state of one session: the verified subject
// its subscriptions belong to and the URIs it subscribes to. The
// subject binds at the session's first gated subscribe; later subscribes
// under a different verified subject are rejected, so a
// credential-changed reuse of the session cannot attach to (or rebind)
// another subject's subscriptions. Trusted (gate-less) transports never
// bind and stay fully trusted, matching the gate model.
type subSession struct {
	bound   bool
	subject string
	uris    map[string]bool
}

func newSubscriptions() *subscriptions {
	return &subscriptions{
		uris:     map[string]map[*mcp.ServerSession]NamespaceGate{},
		sessions: map[*mcp.ServerSession]*subSession{},
	}
}

// add records one subscription. It rejects a subscribe that would bind
// the session's subscriptions to a different verified subject than the
// one that created them, and evicts (closes) the oldest sessions when a
// new session would exceed the cap. Eviction closes the SDK session
// BEFORE dropping its registry entries - the opposite order would open
// a window where the SDK could still deliver to a session this registry
// already forgot - and closing runs outside the lock, so a victim is
// still fully gated while its close is in flight.
func (s *subscriptions) add(uri string, ss *mcp.ServerSession, g NamespaceGate) error {
	s.mu.Lock()
	var evicted []struct {
		ss   *mcp.ServerSession
		uris map[string]bool
	}
	st := s.sessions[ss]
	if st == nil {
		for len(s.sessions) >= maxCachedSessions {
			victim := s.order[0]
			s.order = s.order[1:]
			vst := s.sessions[victim]
			delete(s.sessions, victim)
			evicted = append(evicted, struct {
				ss   *mcp.ServerSession
				uris map[string]bool
			}{victim, vst.uris})
		}
		st = &subSession{uris: map[string]bool{}}
		s.sessions[ss] = st
		s.order = append(s.order, ss)
	}
	if g != nil {
		subject := gateSubject(g)
		if st.bound && st.subject != subject {
			s.mu.Unlock()
			return fmt.Errorf("mcp session subscriptions are bound to a different verified subject")
		}
		st.bound = true
		st.subject = subject
	}
	if s.uris[uri] == nil {
		s.uris[uri] = map[*mcp.ServerSession]NamespaceGate{}
	}
	s.uris[uri][ss] = g
	st.uris[uri] = true
	s.mu.Unlock()
	for _, ev := range evicted {
		_ = ev.ss.Close() // the SDK delivery subscriptions die here...
		s.mu.Lock()
		// ...so dropping the entries is safe now. A session that
		// re-subscribed mid-eviction is tracked again with fresh state;
		// leave that alone.
		if _, live := s.sessions[ev.ss]; !live {
			for u := range ev.uris {
				if m, ok := s.uris[u]; ok {
					delete(m, ev.ss)
					if len(m) == 0 {
						delete(s.uris, u)
					}
				}
			}
		}
		s.mu.Unlock()
	}
	return nil
}

func (s *subscriptions) remove(uri string, ss *mcp.ServerSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.uris[uri]; ok {
		delete(m, ss)
		if len(m) == 0 {
			delete(s.uris, uri)
		}
	}
	// The session state (subject binding) outlives individual
	// unsubscribes: a session that unsubscribed everything is still the
	// same verified subject's session.
	if st, ok := s.sessions[ss]; ok {
		delete(st.uris, uri)
	}
}

// any reports whether any session subscribes to uri.
func (s *subscriptions) any(uri string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.uris[uri]) > 0
}

// matching returns the subscribed URIs accepted by keep.
func (s *subscriptions) matching(keep func(uri string) bool) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for uri := range s.uris {
		if keep(uri) {
			out = append(out, uri)
		}
	}
	return out
}

// snapshot copies uri's subscriber set (session -> gate) for iteration
// outside the lock.
func (s *subscriptions) snapshot(uri string) map[*mcp.ServerSession]NamespaceGate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[*mcp.ServerSession]NamespaceGate, len(s.uris[uri]))
	for ss, g := range s.uris[uri] {
		out[ss] = g
	}
	return out
}

// reauthorizeSubscribers is the task A02 revocation policy for one
// resource notification: every subscribed session's gate is rechecked
// against the event's namespace BEFORE delivery, and the gate itself
// revalidates that the subscribing credential is still valid (a key
// revoked since subscribe fails here). A session that lost its grant or
// its credential is closed - closing removes the session's SDK delivery
// subscriptions, so it receives neither this nor any later notification
// - and only then dropped from the registry, so no window exists where
// the SDK could still deliver to a session whose gate was discarded.
// Delivery then proceeds for the remaining subscribers: one revocation
// silences only the revoked session, never the authorized ones. Trusted
// sessions (no gate: stdio, enforcement off) always pass.
func reauthorizeSubscribers(subs *subscriptions, uri, ns string) {
	for ss, g := range subs.snapshot(uri) {
		if g == nil || g.Allow(context.Background(), ns, authz.OpRead) {
			continue
		}
		_ = ss.Close()       // SDK delivery subscriptions die first...
		subs.remove(uri, ss) // ...so this drop cannot lose a live gate
	}
}

// parseMemoryURI splits punk://memory/<ns><prefix> into namespace and
// key prefix ("/" + rest). punk://memory/ns/tasks -> ("ns", "/tasks").
func parseMemoryURI(uri string) (ns, prefix string, ok bool) {
	rest, found := strings.CutPrefix(uri, "punk://memory/")
	if !found || rest == "" {
		return "", "", false
	}
	ns, tail, _ := strings.Cut(rest, "/")
	if ns == "" {
		return "", "", false
	}
	return ns, "/" + tail, true
}
