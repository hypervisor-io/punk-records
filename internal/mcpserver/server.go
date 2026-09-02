// Package mcpserver exposes Punk Records' capabilities as MCP tools so any
// agent (Claude Code, a copilot, another service) can submit tasks and
// use the memory plane. Tools reuse the exact service layer the REST API
// uses, so MCP and HTTP can never drift.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/reflect"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/route"
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

type documentIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"memory namespace; optional, resolved from the client's workspace root (see whoami) when empty"`
	Prefix    string `json:"prefix" jsonschema:"hierarchical key prefix, e.g. /docs/runbook"`
	Text      string `json:"text" jsonschema:"the document body"`
	Author    string `json:"author,omitempty"`
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
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"cap result payload in ~tokens"`
}

type recallOut struct {
	Facts []memory.Fact `json:"facts"`
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
		subs = &subscriptions{uris: map[string]bool{}}
		opts = &mcp.ServerOptions{
			SubscribeHandler: func(_ context.Context, req *mcp.SubscribeRequest) error {
				subs.set(req.Params.URI, true)
				return nil
			},
			UnsubscribeHandler: func(_ context.Context, req *mcp.UnsubscribeRequest) error {
				subs.set(req.Params.URI, false)
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
		events, cancel := d.Bus.Subscribe()
		go func() {
			defer cancel()
			for e := range events {
				if e.Kind != "task_status" {
					continue
				}
				uri := "punk://tasks/" + e.Key
				if subs.get(uri) {
					_ = s.ResourceUpdated(context.Background(),
						&mcp.ResourceUpdatedNotificationParams{URI: uri})
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
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
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
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			facts, err := d.Mem.Recall(ctx, ns, in.Prefix, 0)
			if err != nil {
				return nil, recallOut{}, err
			}
			return nil, recallOut{Facts: memory.TokenBudget(facts, in.MaxTokens)}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_keys",
		Description: "List live memory keys under a prefix. Discover keys, never invent them."},
		func(ctx context.Context, req *mcp.CallToolRequest, in recallIn) (*mcp.CallToolResult, listKeysOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			keys, err := d.Mem.ListKeys(ctx, ns, in.Prefix)
			if err != nil {
				return nil, listKeysOut{}, err
			}
			return nil, listKeysOut{Keys: keys}, nil
		})

	registerMemoryV2Tools(s, d, nsr)

	if d.Region != nil {
		registerRegionTools(s, d, nsr)
	}
	if len(d.A2ARemotes) > 0 {
		registerA2ATools(s, d)
	}
	if d.LLM != nil {
		registerReflectTool(s, d)
	}

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
func registerReflectTool(s *mcp.Server, d Deps) {
	eng := reflect.New(d.Mem, d.LLM)
	mcp.AddTool(s, &mcp.Tool{Name: "reflect",
		Description: "Answer a question by reasoning hierarchically over the memory plane (mental models, then observations, then raw recall), with citations validated against what was actually retrieved."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in reflectIn) (*mcp.CallToolResult, reflect.Answer, error) {
			ans, err := eng.ReflectWith(ctx, in.Namespace, in.Query, reflect.Opts{Level: in.Level, Schema: in.Schema})
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
	Expand       bool     `json:"expand,omitempty" jsonschema:"with hybrid+scored, expand the query into up to 3 LLM reformulations and union results (ignored when no model configured)"`
	Limit        int      `json:"limit,omitempty"`
	MaxTokens    int      `json:"max_tokens,omitempty" jsonschema:"cap result payload in ~tokens"`
	Format       string   `json:"format,omitempty" jsonschema:"'' (full facts) or 'compact': key, clipped body, score, flags only; use compact unless attributes or timestamps are needed"`
	Anchors      []string `json:"anchors,omitempty" jsonschema:"exact identifiers, error strings, flags or file names; each is an extra phrase-match retrieval route fused by rank, not a filter (hybrid+scored only)"`
	RepoRevision string   `json:"repo_revision,omitempty" jsonschema:"current git revision of the workspace; code-map hits seeded from another revision are flagged stale"`
}

// searchOut is search's response shape: plain facts, or (with Hybrid+Scored)
// facts plus the score breakdown that explains why each one ranked where it did.
// With Format=compact, Hits carries the token-lean projection instead.
type searchOut struct {
	Facts   []memory.Fact       `json:"facts,omitempty"`
	Results []memory.ScoredFact `json:"results,omitempty"`
	Hits    []memory.CompactHit `json:"hits,omitempty"`
}

type asOfIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, resolved from the client's workspace root (see whoami) when empty"`
	Prefix    string `json:"prefix,omitempty"`
	AsOf      string `json:"as_of" jsonschema:"RFC3339 instant to read the region as of"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"cap result payload in ~tokens"`
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
	if in.Format == "compact" {
		var hits []memory.CompactHit
		if scored != nil {
			hits = memory.CompactScored(scored, 0)
		} else {
			hits = memory.CompactFacts(facts, 0)
		}
		return searchOut{Hits: memory.TokenBudgetCompact(hits, in.MaxTokens)}
	}
	if scored != nil {
		return searchOut{Results: memory.TokenBudgetScored(scored, in.MaxTokens)}
	}
	return searchOut{Facts: memory.TokenBudget(facts, in.MaxTokens)}
}

func registerMemoryV2Tools(s *mcp.Server, d Deps, nsr *nsResolver) {
	mcp.AddTool(s, &mcp.Tool{Name: "remember_document",
		Description: "Chunk and store a document under a key prefix; rewrites only changed chunks and tombstones chunks past the new end (delta ingest)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in documentIn) (*mcp.CallToolResult, documentOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			w, u, r, b, err := d.Mem.WriteDocument(ctx, ns, in.Prefix, in.Text, in.Author)
			if err != nil {
				return nil, documentOut{}, err
			}
			return nil, documentOut{Written: w, Unchanged: u, Removed: r, Blocked: b}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "search",
		Description: "Search a region's facts by full text, or hybrid vector+FTS when embeddings are enabled."},
		func(ctx context.Context, req *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
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
			var err error
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
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			at, err := time.Parse(time.RFC3339, in.AsOf)
			if err != nil {
				return nil, recallOut{}, fmt.Errorf("as_of: %w", err)
			}
			facts, err := d.Mem.RecallAsOf(ctx, ns, in.Prefix, at, 0)
			if err != nil {
				return nil, recallOut{}, err
			}
			return nil, recallOut{Facts: memory.TokenBudget(facts, in.MaxTokens)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "forget",
		Description: "Tombstone a key (closes its validity window); history is preserved."},
		func(ctx context.Context, req *mcp.CallToolRequest, in forgetIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			if err := d.Mem.Forget(ctx, ns, in.Key, in.Author); err != nil {
				return nil, nil, err
			}
			return nil, map[string]string{"status": "tombstoned", "key": in.Key}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "link",
		Description: "Add a typed edge between two facts (from_key -> to_key), e.g. a change touches a file. An optional description (a one-sentence NL restatement of the fact the edge encodes) is embedded, making the relation itself retrievable via triplet_search."},
		func(ctx context.Context, req *mcp.CallToolRequest, in linkIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
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
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
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
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			triplets, err := d.Mem.TripletSearch(ctx, ns, in.Query, in.K)
			if err != nil {
				return nil, tripletSearchOut{}, err
			}
			return nil, tripletSearchOut{Triplets: triplets}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "unified_search",
		Description: "One recall entry point that fuses fact/observation/entity/mental-model hits with relation-triplet hits via reciprocal rank fusion, instead of two separate calls."},
		func(ctx context.Context, req *mcp.CallToolRequest, in unifiedSearchIn) (*mcp.CallToolResult, unifiedSearchOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
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
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
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
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			f, err := d.Mem.RememberModel(ctx, ns, in.Slug, in.Body, in.SourceIDs, in.Pinned)
			if err != nil {
				return nil, nil, err
			}
			return nil, f, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_models",
		Description: "List the live curated mental models in a namespace."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listModelsIn) (*mcp.CallToolResult, listModelsOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			models, err := d.Mem.ListModels(ctx, ns)
			if err != nil {
				return nil, listModelsOut{}, err
			}
			return nil, listModelsOut{Models: models}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_entities",
		Description: "List the live extracted entities (people, orgs, places, concepts) in a namespace."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listEntitiesIn) (*mcp.CallToolResult, listEntitiesOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			entities, err := d.Mem.ListEntities(ctx, ns)
			if err != nil {
				return nil, listEntitiesOut{}, err
			}
			return nil, listEntitiesOut{Entities: entities}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "feedback",
		Description: "Rate the facts used in an answer; EWMA-updates their feedback weight, which feeds future ranking."},
		func(ctx context.Context, req *mcp.CallToolRequest, in feedbackIn) (*mcp.CallToolResult, feedbackOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			if err := d.Mem.RecordFeedback(ctx, ns, in.IDs, in.Rating); err != nil {
				return nil, feedbackOut{}, err
			}
			return nil, feedbackOut{Updated: len(in.IDs)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "profile",
		Description: "Deterministic namespace digest: top entities, hot keys, recent facts, counts. No LLM."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listModelsIn) (*mcp.CallToolResult, *memory.Profile, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			p, err := d.Mem.Profile(ctx, ns)
			if err != nil {
				return nil, nil, err
			}
			return nil, p, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "diagnose",
		Description: "Namespace health counters: quarantined rows, missing embeddings, orphan links, stale observations, expired claims."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listModelsIn) (*mcp.CallToolResult, *memory.Diagnosis, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			dg, err := d.Mem.Diagnose(ctx, ns)
			if err != nil {
				return nil, nil, err
			}
			return nil, dg, nil
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
}

type unifiedSearchOut struct {
	Hits    []memory.UnifiedHit `json:"hits,omitempty"`
	Compact []memory.CompactHit `json:"compact,omitempty"`
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
	Agent     string `json:"agent" jsonschema:"agent/consumer name"`
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
	Holder     string `json:"holder" jsonschema:"claiming agent"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

type claimsOut struct {
	Claims []region.Claim `json:"claims"`
}

func registerRegionTools(s *mcp.Server, d Deps, nsr *nsResolver) {
	mcp.AddTool(s, &mcp.Tool{Name: "claim_work",
		Description: "Claim a sub-key in a region so no other satellite works it (conflict-free work partitioning). Fails if a live claim already holds it."},
		func(ctx context.Context, req *mcp.CallToolRequest, in claimIn) (*mcp.CallToolResult, *region.Claim, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			ttl := time.Duration(in.TTLSeconds) * time.Second
			if ttl <= 0 {
				ttl = 5 * time.Minute
			}
			c, err := d.Region.ClaimWork(ctx, ns, in.Key, in.Holder, ttl)
			if err != nil {
				return nil, nil, err
			}
			if d.Bus != nil {
				d.Bus.Publish(bus.Event{Kind: "claim", Key: ns + ":" + in.Key,
					Data: map[string]string{"holder": in.Holder, "action": "claimed"}})
			}
			return nil, c, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "release_work",
		Description: "Release a work claim so other satellites can take the sub-key."},
		func(ctx context.Context, req *mcp.CallToolRequest, in claimIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			if err := d.Region.ReleaseWork(ctx, ns, in.Key, in.Holder); err != nil {
				return nil, nil, err
			}
			if d.Bus != nil {
				d.Bus.Publish(bus.Event{Kind: "claim", Key: ns + ":" + in.Key,
					Data: map[string]string{"holder": in.Holder, "action": "released"}})
			}
			return nil, map[string]string{"status": "released"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_claims",
		Description: "List the live work claims in a region."},
		func(ctx context.Context, req *mcp.CallToolRequest, in membersIn) (*mcp.CallToolResult, claimsOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			c, err := d.Region.ListClaims(ctx, ns)
			if err != nil {
				return nil, claimsOut{}, err
			}
			return nil, claimsOut{Claims: c}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "register",
		Description: "Register an agent/consumer as a satellite of a brain region (namespace). Satellites coordinate through the region's shared memory."},
		func(ctx context.Context, req *mcp.CallToolRequest, in registerIn) (*mcp.CallToolResult, map[string]string, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
			if err := d.Region.Register(ctx, ns, in.Agent, in.Role); err != nil {
				return nil, nil, err
			}
			return nil, map[string]string{"status": "registered", "namespace": ns, "agent": in.Agent}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_region_members",
		Description: "List the satellites registered to a brain region."},
		func(ctx context.Context, req *mcp.CallToolRequest, in membersIn) (*mcp.CallToolResult, membersOut, error) {
			ns, _ := nsr.resolve(ctx, req, in.Namespace)
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
			return nil, membersOut{Members: m}, nil
		})
}

// subscriptions tracks which resource URIs any client asked to watch.
type subscriptions struct {
	mu   sync.RWMutex
	uris map[string]bool
}

func (s *subscriptions) set(uri string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.uris[uri] = true
	} else {
		delete(s.uris, uri)
	}
}

func (s *subscriptions) get(uri string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.uris[uri]
}
