// Command punk is Punk Records: a self-hosted shared brain for AI
// agents.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hypervisor-io/punk-records/internal/a2a"
	"github.com/hypervisor-io/punk-records/internal/agent"
	"github.com/hypervisor-io/punk-records/internal/api"
	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/config"
	"github.com/hypervisor-io/punk-records/internal/cost"
	"github.com/hypervisor-io/punk-records/internal/embedlocal"
	"github.com/hypervisor-io/punk-records/internal/hookcli"
	"github.com/hypervisor-io/punk-records/internal/ingest"
	"github.com/hypervisor-io/punk-records/internal/itbench"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/mcpclient"
	"github.com/hypervisor-io/punk-records/internal/mcpserver"
	"github.com/hypervisor-io/punk-records/internal/membench"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/obs"
	"github.com/hypervisor-io/punk-records/internal/policy"
	"github.com/hypervisor-io/punk-records/internal/reflect"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/replay"
	"github.com/hypervisor-io/punk-records/internal/route"
	"github.com/hypervisor-io/punk-records/internal/skillmine"
	"github.com/hypervisor-io/punk-records/internal/spec"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
	"github.com/hypervisor-io/punk-records/internal/topo"
)

// version is stamped via -ldflags "-X main.version=..." at release time.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "punk:", err)
		os.Exit(1)
	}
}

const usage = `punk - shared brain + coordination layer for domain agents

Usage:
  punk      serve     start the coordinator (HTTP API, MCP, agent runtime)
  punk      brain     open or serve the brain view (brain [--server URL] [--open] | brain serve [serve flags])
  punk      migrate   run database migrations (up|down|status)
  punk      validate  validate agent/skill/policy specs in a directory
  punk      apikey    manage API keys (create|revoke --name <name>)
  punk      authz     manage namespace grants (grant|revoke|list --subject S [--namespace N --op read|write|admin])
  punk      mcp       serve the MCP interface on stdio
  punk      backup    snapshot the SQLite database (--out file)
  punk      embed-backfill  embed facts written before embeddings were enabled (--ns) [--force]
  punk      models    list | pull <name>: manage local static embedding models
  punk      login     store the server URL (and optional API key) in ~/.punk/credentials.json
  punk      logout    remove the stored credentials
  punk      consolidate  run a consolidation pass now, bypassing the dream triggers (--ns, empty = all)
  punk      card      manage the user's cross-project profile card (add "fact" | list | remove --key)
  punk      replay    re-run a completed task against its frozen snapshots (--task, --k, --mode)
  punk      topo      import a Backstage catalog (import --file catalog.yaml)
  punk      region    fork/merge a brain region as a git branch (branch|merge --ns --dir --branch)
  punk      a2a       delegate to a remote A2A agent (card <url> | send [--stream] <endpoint> <text>)
  punk      itbench   score the SRE loop against ITBench scenarios (run --dir <dir> --agent <name>)
  punk      membench  score recall/MRR of memory retrieval against a JSONL scenario (--file <path> [--k N] [--ns name])
                      or retrieval recall over LoCoMo gold evidence (--locomo <locomo10.json> [--k N])
                      --report <path> writes the versioned baseline+ablation report (run manifests, per-query rankings) for --file scenarios
  punk      export    write a namespace's memory history as JSONL to stdout
  punk      import    read a JSONL export from stdin into a namespace
  punk      ingest    load a document file or explicit URL into memory with source provenance (ingest --ns NS --prefix P [flags] <file>)
                      loaders: text, markdown, html, incident-json; pdf via an external adapter (--pdf-adapter, docs/CONFIG.md#document-ingest)
  punk      seed      seed memory from a code-knowledge tool (seed rinnegan [--ns NS] [--dir DIR] < map.json)
  punk      skills    propose SKILL.md drafts mined from completed task ledgers (propose --min-count N --out DIR); distill a namespace's memory into proposed CLAUDE.md additions (insights --ns NS --out DIR)
  punk      hook      run as an agent hook: forward stdin payload, inject context on SessionStart (--url URL, --from AGENT)
                      env PUNK_URL (default http://localhost:9090), PUNK_API_KEY
                      --from defaults to Claude Code passthrough; --from cursor translates Cursor's native hook payload
                      --from antigravity requires --event PostToolUse|PreInvocation|Stop (Antigravity's own hook payloads carry no event name)
                      --from copilot translates GitHub Copilot CLI's native hook payload (self-identifies its event; SessionStart injects via Copilot's own additionalContext shape)
                      --from hermes translates Hermes Agent's native shell-hook payload (self-identifies its event; first-turn pre_llm_call injects via Hermes' own {"context":...} shape)
  punk      connect   wire punk as agent memory (connect claude-code|cursor|opencode|pi|antigravity|copilot|hermes|openclaw|codex [--project] [--url URL])
  punk      skill     punk usage skill in the agent's skill directory (skill install|print|paths --agent NAME [--project] [--url URL] [--ns NS])
  punk      --version print version
`

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return fmt.Errorf("no command given")
	}
	switch args[0] {
	case "--version", "-v", "version":
		fmt.Println("punk", version)
		return nil
	case "serve":
		return cmdServe(args[1:])
	case "brain":
		return cmdBrain(args[1:])
	case "migrate":
		return cmdMigrate(args[1:])
	case "validate":
		return cmdValidate(args[1:])
	case "export":
		return cmdExport(args[1:])
	case "import":
		return cmdImport(args[1:])
	case "ingest":
		return cmdIngest(args[1:])
	case "seed":
		return cmdSeed(args[1:])
	case "apikey":
		return cmdAPIKey(args[1:])
	case "authz":
		return cmdAuthz(args[1:])
	case "mcp":
		return cmdMCP(args[1:])
	case "backup":
		return cmdBackup(args[1:])
	case "embed-backfill":
		return cmdEmbedBackfill(args[1:])
	case "models":
		return cmdModels(args[1:])
	case "login":
		return cmdLogin(args[1:])
	case "namespace":
		return cmdNamespace(args[1:])
	case "logout":
		return cmdLogout(args[1:])
	case "consolidate":
		return cmdConsolidate(args[1:])
	case "card":
		return cmdCard(args[1:])
	case "replay":
		return cmdReplay(args[1:])
	case "topo":
		return cmdTopo(args[1:])
	case "region":
		return cmdRegion(args[1:])
	case "a2a":
		return cmdA2A(args[1:])
	case "itbench":
		return cmdITBench(args[1:])
	case "membench":
		return cmdMembench(args[1:])
	case "skills":
		return cmdSkills(args[1:])
	case "hook":
		return cmdHook(args[1:])
	case "connect":
		return cmdConnect(args[1:])
	case "skill":
		return cmdSkill(args[1:])
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q, see 'punk help'", args[0])
	}
}

// obsConsolidationPrompt is the consolidation system prompt:
// UPDATE over CREATE, one observation per entity/facet, no
// derived arithmetic, every observation cites its grounding fact ids.
const obsConsolidationPrompt = `Consolidate the facts below into durable observations. Rules:
- UPDATE over CREATE: if facts restate one belief, emit ONE observation.
- Match by entity, not topic. One observation per entity/facet.
- NO arithmetic or derived computations. State only what facts support.
- Every observation MUST cite the fact ids it is grounded in.
- Label each observation's reasoning kind: "explicit" (stated outright in a fact),
  "deductive" (follows necessarily from the cited facts), "inductive" (a pattern
  across the cited facts - cite at least TWO), or "abductive" (the simplest
  explanation of the cited facts).
Respond with ONLY a JSON array: [{"slug":"kebab-case","body":"...","source_ids":["id",...],"kind":"explicit|deductive|inductive|abductive"}]`

// Dream-style consolidation triggers: a pass runs only when a namespace
// has accumulated enough fresh writes and then gone quiet, tuned to
// hook-capture volume:
const (
	consolidateCheckInterval = 10 * time.Minute // trigger evaluation cadence (cheap: one COUNT per ns)
	consolidateMinWrites     = 20               // new revisions that make a namespace consolidation-worthy
	consolidateIdleWait      = 30 * time.Minute // debounce: last write must be at least this old
	consolidateMinSpacing    = 6 * time.Hour    // floor between passes (the old fixed cadence)
	consolidateMaxSpacing    = 24 * time.Hour   // any writes at all consolidate at least daily
)

// runConsolidationPass runs one namespace's full consolidation pass -
// superseded-fold, observation rollup, and the optional AI passes
// (reconcile, contradictions, creative links) plus the IVF rebuild -
// with each step gated on its own config exactly as the old ticker did.
// It stamps the store's per-namespace consolidation record (Diagnose's
// last_consolidated_at) and, when eventBus is non-nil, publishes a
// memory event (kind "memory", key "<ns>:consolidated") so subscribers
// see consolidation the same way they see writes. Individual step
// failures are logged and never abort the remaining steps.
func runConsolidationPass(ctx context.Context, mem *memory.Store, cfg *config.Config, ns string, horizon time.Duration, force bool, observer memory.ObservationSummarizer, judge memory.MergeJudge, cJudge memory.ContradictionJudge, eventBus *bus.Bus, log *slog.Logger) {
	var compacted int64
	var observations, reconciled, contradictions, creative int
	// Compaction/observation steps run when consolidation is configured,
	// or when forced (punk consolidate: a manual run with
	// consolidate_days unset uses horizon 0, folding every superseded
	// revision - exactly what "consolidate now" means). The scheduler
	// never forces, so IVF-only mode (ConsolidateDays == 0) keeps its
	// old behavior: index rebuilds only, no compaction.
	if cfg.Memory.ConsolidateDays > 0 || force {
		if r, err := mem.Consolidate(ctx, ns, horizon, nil); err != nil {
			log.Error("consolidation failed", "ns", ns, "err", err)
		} else if r.Compacted > 0 {
			compacted = r.Compacted
			log.Info("region consolidated", "ns", ns, "compacted", r.Compacted)
		}
		if n, err := mem.ConsolidateObservations(ctx, ns, observer); err != nil {
			log.Error("observation consolidation failed", "ns", ns, "err", err)
		} else if n > 0 {
			observations = n
			log.Info("observations consolidated", "ns", ns, "written", n)
		}
		if cfg.AI.Enabled && cfg.Memory.ReconcileThreshold > 0 {
			if n, err := mem.ReconcileObservations(ctx, ns, cfg.Memory.ReconcileThreshold, judge); err != nil {
				log.Error("observation reconciliation failed", "ns", ns, "err", err)
			} else if n > 0 {
				reconciled = n
				log.Info("observations reconciled", "ns", ns, "merged", n)
			}
		}
		if cfg.Memory.Contradictions && cJudge != nil {
			if n, err := mem.DetectContradictions(ctx, ns, 0, 0, cJudge); err != nil {
				log.Error("contradiction pass failed", "ns", ns, "err", err)
			} else if n > 0 {
				contradictions = n
				log.Info("contradictions linked", "ns", ns, "pairs", n)
			}
		}
		if cfg.Memory.CreativeLinks {
			if n, err := mem.CreativePass(ctx, ns, 0, 0); err != nil {
				log.Error("creative pass failed", "ns", ns, "err", err)
			} else if n > 0 {
				creative = n
				log.Info("creative pass", "ns", ns, "links_added", n)
			}
		}
	}
	if cfg.Memory.IVFNprobe > 0 {
		if err := mem.BuildIVF(ctx, ns); err != nil {
			log.Error("ivf build failed", "ns", ns, "err", err)
		}
	}
	mem.MarkConsolidated(ns, time.Now())
	if eventBus != nil {
		eventBus.Publish(bus.Event{Kind: "memory", Key: ns + ":consolidated", Data: map[string]string{
			"namespace":      ns,
			"compacted":      strconv.FormatInt(compacted, 10),
			"observations":   strconv.Itoa(observations),
			"reconciled":     strconv.Itoa(reconciled),
			"contradictions": strconv.Itoa(contradictions),
			"creative_links": strconv.Itoa(creative),
		}})
	}
}

// obsSummarizer adapts an llm.Client to memory.ObservationSummarizer for
// the consolidation ticker. A bad/unparseable model response is logged
// and skipped (0 observations that run), never fails the ticker.
type obsSummarizer struct {
	client llm.Client
	log    *slog.Logger
}

func (o *obsSummarizer) Observe(ctx context.Context, facts []memory.Fact) ([]memory.Observation, error) {
	var b strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", f.ID, f.Key, f.Body)
	}
	res, err := o.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: obsConsolidationPrompt},
		{Role: "user", Content: b.String()},
	}, nil)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(res.Content)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	var out []memory.Observation
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &out); err != nil {
		o.log.Warn("observation consolidation: unparseable model response", "err", err)
		return nil, nil
	}
	return out, nil
}

// sessionSummaryPrompt drives the rolling session summarizer:
// recursive - prior summary plus the events since - so the narrative
// stays biased toward recent work while covering the whole session.
const sessionSummaryPrompt = `Update the running summary of one coding-agent session from its prior summary and the new events below.
Rules:
- Keep goals, decisions, current state, and unresolved threads. Drop tool noise and chatter.
- When a new event supersedes something in the prior summary, keep only the new state.
- At most 200 words. Respond with ONLY the updated summary text, no headers or preamble.`

// sessionSummarizer adapts an llm.Client to memory.SessionSummarizer.
// Failures propagate (SummarizeSessions retries next tick); an empty
// answer is handled there and never blanks an existing summary.
type sessionSummarizer struct {
	client llm.Client
	log    *slog.Logger
}

func (s *sessionSummarizer) Summarize(ctx context.Context, prior string, facts []memory.Fact) (string, error) {
	var b strings.Builder
	if prior != "" {
		b.WriteString("PRIOR SUMMARY:\n" + prior + "\n\n")
	}
	b.WriteString("NEW EVENTS (chronological):\n")
	for _, f := range facts {
		fmt.Fprintf(&b, "%s\t%s\n", f.Key, f.Body)
	}
	res, err := s.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: sessionSummaryPrompt},
		{Role: "user", Content: b.String()},
	}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Content), nil
}

// mergeJudgePrompt asks for a strict merge/keep verdict on two observations
// that already scored as near-duplicates by embedding cosine.
const mergeJudgePrompt = `Two consolidated observations scored as near-duplicates. Read both bodies below.
If they state the same belief, respond with the merged body that best states it.
If they are actually distinct beliefs, say so. Respond with ONLY JSON: {"merge":true|false,"body":"..."}`

// mergeJudge adapts an llm.Client to memory.MergeJudge for the
// reconciliation ticker. An unparseable model response keeps both
// observations (merge=false), never fails the ticker.
type mergeJudge struct {
	client llm.Client
	log    *slog.Logger
}

func (j *mergeJudge) ShouldMerge(ctx context.Context, a, b memory.Observation) (bool, string, error) {
	user := fmt.Sprintf("Observation A:\n%s\n\nObservation B:\n%s", a.Body, b.Body)
	res, err := j.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: mergeJudgePrompt},
		{Role: "user", Content: user},
	}, nil)
	if err != nil {
		return false, "", err
	}
	s := strings.TrimSpace(res.Content)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if k := strings.Index(s, "```"); k >= 0 {
			s = s[:k]
		}
	}
	var out struct {
		Merge bool   `json:"merge"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &out); err != nil {
		j.log.Warn("merge judge: unparseable model response", "err", err)
		return false, "", nil
	}
	return out.Merge, out.Body, nil
}

// contradictPrompt asks for a strict verdict on two facts that already
// scored as near-duplicates by embedding cosine.
const contradictPrompt = `Two facts from the same memory region are near-duplicates by embedding similarity.
Do they state OPPOSING claims about the same subject (one contradicts the other)?
Respond with ONLY JSON: {"contradicts":true|false}`

// contradictJudge adapts an llm.Client to memory.ContradictionJudge for
// the consolidation ticker. An unparseable model response is logged and
// treated as no contradiction, never fails the pass.
type contradictJudge struct {
	client llm.Client
	log    *slog.Logger
}

func (j *contradictJudge) Contradicts(ctx context.Context, a, b memory.Fact) (bool, error) {
	user := fmt.Sprintf("Fact A (%s):\n%s\n\nFact B (%s):\n%s", a.Key, a.Body, b.Key, b.Body)
	res, err := j.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: contradictPrompt},
		{Role: "user", Content: user},
	}, nil)
	if err != nil {
		return false, err
	}
	s := strings.TrimSpace(res.Content)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if k := strings.Index(s, "```"); k >= 0 {
			s = s[:k]
		}
	}
	var out struct {
		Contradicts bool `json:"contradicts"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &out); err != nil {
		j.log.Warn("contradiction judge: unparseable model response", "err", err)
		return false, nil
	}
	return out.Contradicts, nil
}

// entityExtractPrompt asks for a flat list of named entities, one per
// line, no commentary — parsed deterministically, never blocking a write.
const entityExtractPrompt = `List the distinct named entities (people, organizations, places, concepts) mentioned in the text below, one per line, no commentary, no numbering.`

// entityExtractor adapts an llm.Client to memory.EntityExtractor for the
// enricher. An unparseable/empty model response yields no entities, never
// fails the enrich pass.
type entityExtractor struct {
	client llm.Client
	log    *slog.Logger
}

func (e *entityExtractor) Extract(ctx context.Context, body string) ([]string, error) {
	res, err := e.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: entityExtractPrompt},
		{Role: "user", Content: body},
	}, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(res.Content, "\n") {
		if l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-")); l != "" {
			names = append(names, strings.TrimSpace(l))
		}
	}
	return names, nil
}

// entityExtractBatchPrompt asks for one entity list per numbered text,
// as a strict JSON array of arrays - one model call for a whole batch
// instead of one call per message.
const entityExtractBatchPrompt = `For EACH numbered text below, list its distinct named entities (people, organizations, places, concepts).
Respond with ONLY a JSON array containing exactly one array of entity-name strings per numbered text, in order. A text with no entities gets an empty array.`

// ExtractBatch implements memory.BatchEntityExtractor: one model call
// over several fact bodies. Any parse failure or a miscounted answer is
// returned as an error / wrong-length slice - the store falls back to
// per-fact Extract calls, so batching can only save cost, never lose
// entities.
func (e *entityExtractor) ExtractBatch(ctx context.Context, bodies []string) ([][]string, error) {
	var b strings.Builder
	for i, body := range bodies {
		fmt.Fprintf(&b, "%d. %s\n\n", i+1, body)
	}
	res, err := e.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: entityExtractBatchPrompt},
		{Role: "user", Content: b.String()},
	}, nil)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(res.Content)
	// fence-strip identical to sibling adapters (obsSummarizer et al).
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	var out [][]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &out); err != nil {
		e.log.Warn("entity batch: unparseable model response", "err", err)
		return nil, err
	}
	return out, nil
}

// entityExtractTypedPrompt asks for typed entities as strict JSON, one
// object per entity, with aliases and 1-based source text numbers.
const entityExtractTypedPrompt = `For EACH numbered text below, list its distinct named entities.
Respond with ONLY a JSON array of objects, one per entity: {"name": ..., "type": ..., "aliases": [...], "sources": [<text numbers>]}.
"type" must be one of: service, repository, database, host, incident, person, unknown. Use "unknown" when unsure.
"aliases" lists other names the text uses for the same entity. "sources" lists the numbers of the texts mentioning the entity.`

// typedEntityExtractor upgrades the plain entityExtractor to
// memory.StructuredEntityExtractor. It is wired only when
// memory.entity_types is on, so disabled mode keeps the legacy name-only
// behavior byte-identical. Parse failures surface as errors - the store
// falls back to legacy per-fact extraction.
type typedEntityExtractor struct {
	*entityExtractor
}

// ExtractStructured implements memory.StructuredEntityExtractor: one
// model call over the batch, source numbers mapped back to the exact
// fact revision IDs (EntitySource.ID, the provenance anchor the store
// validates and retains; mentions links stay key-addressed inside the
// store). A source without a revision identity is skipped, never
// cited as an empty string.
func (e *typedEntityExtractor) ExtractStructured(ctx context.Context, sources []memory.EntitySource) ([]memory.ExtractedEntity, error) {
	var b strings.Builder
	for i, src := range sources {
		fmt.Fprintf(&b, "%d. %s\n\n", i+1, src.Body)
	}
	res, err := e.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: entityExtractTypedPrompt},
		{Role: "user", Content: b.String()},
	}, nil)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(res.Content)
	// fence-strip identical to sibling adapters (obsSummarizer et al).
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	var raw []struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		Aliases []string `json:"aliases"`
		Sources []int    `json:"sources"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &raw); err != nil {
		e.log.Warn("typed entity extract: unparseable model response", "err", err)
		return nil, err
	}
	out := make([]memory.ExtractedEntity, 0, len(raw))
	for _, r := range raw {
		ent := memory.ExtractedEntity{Name: r.Name, Type: r.Type, Aliases: r.Aliases}
		seen := map[string]bool{}
		for _, n := range r.Sources {
			if n < 1 || n > len(sources) {
				continue // out-of-range source numbers are dropped
			}
			id := sources[n-1].ID
			if id == "" || seen[id] {
				continue // no revision identity to cite, or already cited
			}
			seen[id] = true
			ent.SourceFacts = append(ent.SourceFacts, id)
		}
		out = append(out, ent)
	}
	return out, nil
}

// expandPrompt asks for diverse reformulations as a strict JSON array.
const expandPrompt = `Generate up to 3 diverse reformulations of the search query below for a memory retrieval system. Different wording, same intent. Respond with ONLY a JSON array of strings.`

// queryExpander adapts an llm.Client to memory.QueryExpander for search's
// expand flag. An unparseable model response is logged and treated as no
// reformulations, never fails the search.
type queryExpander struct {
	client llm.Client
	log    *slog.Logger
}

func (e *queryExpander) Expand(ctx context.Context, query string) ([]string, error) {
	res, err := e.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: expandPrompt},
		{Role: "user", Content: query},
	}, nil)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(res.Content)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if k := strings.Index(s, "```"); k >= 0 {
			s = s[:k]
		}
	}
	var refs []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &refs); err != nil {
		e.log.Warn("query expander: unparseable model response", "err", err)
		return nil, nil
	}
	return refs, nil
}

// newKeys builds the API key manager for the HTTP boundary and wires
// namespace authorization when configured (authz.enforcement: deny).
// With enforcement off the authorizer stays nil and the server keeps
// the legacy trusted behavior: any verified key reaches every
// namespace.
func newKeys(cfg *config.Config, db *store.DB, log *slog.Logger) *api.Keys {
	keys := api.NewKeys(db, nil)
	keys.Log = log
	if cfg.Authz.Enabled() {
		az := authz.New(db, nil)
		az.Log = log
		keys.SetAuthorizer(az)
		log.Info("namespace authorization enforced", "mode", cfg.Authz.Enforcement)
	}
	return keys
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := obs.NewLogger(cfg.Log.Format, cfg.Log.Level)

	shutdownTracing, err := obs.SetupTracing(context.Background(), cfg.OTel.Endpoint, "punk-records")
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flushCtx)
	}()

	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	st, err := db.MigrateStatus(context.Background())
	if err != nil {
		return err
	}
	for _, m := range st {
		if !m.Applied {
			return fmt.Errorf("migration %04d_%s pending: run 'punk migrate up'", m.Version, m.Name)
		}
	}

	mem := memory.New(db, nil)
	mem.SetDefense(cfg.Memory.Defense)
	mem.SetQuantizeVectors(cfg.Memory.QuantizeVectors)
	mem.SetIVFConfig(cfg.Memory.IVFNprobe, cfg.Memory.IVFMinFacts)
	for ns, mode := range cfg.Memory.DefensePolicies {
		mem.SetDefensePolicy(ns, mode)
	}
	regionStore := region.New(db, nil)
	emb, err := newEmbedder(context.Background(), cfg, log)
	if err != nil {
		return err
	}
	if emb != nil {
		mem.SetEmbedder(emb)
		mem.SetEmbedMaxTokens(cfg.AI.Embeddings.MaxInputTokens)
	}
	if cfg.Memory.RerankerURL != "" {
		mem.SetReranker(&memory.HTTPReranker{URL: cfg.Memory.RerankerURL})
		log.Info("cross-encoder reranking enabled", "url", cfg.Memory.RerankerURL)
	}

	reg := registry.New(cfg.Specs.Dir, db, log)
	if err := reg.Load(context.Background()); err != nil {
		return fmt.Errorf("initial spec load: %w", err)
	}
	if snap := reg.Current(); snap != nil {
		declared := map[string][]string{}
		for name, a := range snap.Bundle.Agents {
			if len(a.Regions) > 0 {
				declared[name] = a.Regions
			}
		}
		if err := regionStore.SyncFromSpecs(context.Background(), declared); err != nil {
			return fmt.Errorf("region sync: %w", err)
		}
	}

	eventBus := bus.New()
	ledger := task.NewLedger(db, nil)
	ledger.Notify = func(taskID, status string) {
		eventBus.Publish(bus.Event{Kind: "task_status", Key: taskID,
			Data: map[string]string{"status": status}})
	}
	router := route.New(db, reg, ledger, nil, nil)
	router.Epsilon = cfg.Route.Epsilon
	router.Fallback = cfg.Route.Fallback
	router.TopoDB = db

	profiles := make(map[string]llm.Profile, len(cfg.AI.Profiles))
	for name, p := range cfg.AI.Profiles {
		profiles[name] = llm.Profile{BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv, Model: p.Model}
	}
	llmMgr := llm.NewManager(cfg.AI.Enabled, profiles, db, nil)
	prices, err := llm.LoadPrices(cfg.Budgets.PriceTablePath)
	if err != nil {
		return fmt.Errorf("price table: %w", err)
	}
	if prices.Stale(time.Now(), 60*24*time.Hour) {
		log.Warn("price table is stale; costs will drift", "as_of", prices.AsOf)
	}
	llmMgr.Prices = prices

	pool := mcpclient.NewPool(log)
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 30*time.Second)
	pool.Connect(connectCtx, cfg.MCP.Servers)
	cancelConnect()

	props := policy.NewProposals(db, nil)
	runtime := agent.New(ledger, reg, mem, llmMgr, pool, log)
	runtime.Props = props
	runtime.DailySpend = func(ctx context.Context, agentName string) (int64, error) {
		return cost.DailySpendByAgent(ctx, db, agentName, time.Now())
	}

	// LLM tie-breaker rides the default profile; failures degrade to
	// deterministic order inside the router
	if cfg.AI.Enabled {
		if tieClient, err := llmMgr.Client("default"); err == nil {
			router.SetTieBreaker(&route.LLMTie{Client: tieClient, Reg: reg})
		} else {
			log.Warn("llm tie-breaker disabled", "err", err)
		}
	}

	// Observation consolidation rides the default profile too; nil stays
	// a no-op (deterministic-first) unless AI is enabled and reachable.
	var observer memory.ObservationSummarizer
	var judge memory.MergeJudge
	var cJudge memory.ContradictionJudge
	var sessSum memory.SessionSummarizer
	var reflectClient llm.Client
	var expander memory.QueryExpander
	if cfg.AI.Enabled {
		if obsClient, err := llmMgr.Client("default"); err == nil {
			observer = &obsSummarizer{client: obsClient, log: log}
			judge = &mergeJudge{client: obsClient, log: log}
			cJudge = &contradictJudge{client: obsClient, log: log}
			sessSum = &sessionSummarizer{client: obsClient, log: log}
			reflectClient = obsClient
			expander = &queryExpander{client: obsClient, log: log}
			if cfg.Memory.Entities {
				ex := &entityExtractor{client: obsClient, log: log}
				if cfg.Memory.EntityTypes {
					mem.SetEntityExtractor(&typedEntityExtractor{entityExtractor: ex})
				} else {
					mem.SetEntityExtractor(ex)
				}
			}
		} else {
			log.Warn("llm observer disabled", "err", err)
		}
	}

	mcpDeps := mcpserver.Deps{
		Ledger: ledger, Router: router, Reg: reg, Mem: mem, Region: regionStore, Bus: eventBus,
		A2ARemotes: a2aRemotes(cfg), LLM: reflectClient, Expander: expander,
		NamespaceFor:     api.AgentNamespace,
		DefaultNamespace: os.Getenv("PUNK_NAMESPACE"),
		DefaultBudget: task.Budget{
			Tokens:    cfg.Budgets.Tokens,
			ToolCalls: cfg.Budgets.ToolCalls,
			WallMS:    cfg.Budgets.WallMS,
			Subagents: cfg.Budgets.Subagents,
		},
	}
	mcpSrv := mcpserver.New(mcpDeps)
	agentDeps := mcpDeps
	agentDeps.Toolset = "agent"
	agentSrv := mcpserver.New(agentDeps)

	srv := api.New(log, api.Deps{
		Memory:            mem,
		Ledger:            ledger,
		Router:            router,
		Proposals:         props,
		Keys:              newKeys(cfg, db, log),
		Bus:               eventBus,
		DB:                db,
		Reg:               reg,
		Region:            regionStore,
		Expander:          expander,
		TurnContextTokens: cfg.Memory.TurnContextTokens,
		Inject:            cfg.Memory.Inject,
		DefaultBudget: task.Budget{
			Tokens:    cfg.Budgets.Tokens,
			ToolCalls: cfg.Budgets.ToolCalls,
			WallMS:    cfg.Budgets.WallMS,
			Subagents: cfg.Budgets.Subagents,
		},
	})
	srv.Ready = db.Ping
	srv.MountUI()
	srv.MountBrain()
	srv.MountPipeline()
	srv.MountAgentCard(version)
	srv.MountMCP(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if r.URL.Query().Get("toolset") == "agent" {
			return agentSrv
		}
		return mcpSrv
	}, nil))
	httpSrv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.HTTP.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// hot-reload: file watcher + SIGHUP both funnel into the registry
	reloadCh := make(chan struct{}, 1)
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			select {
			case reloadCh <- struct{}{}:
			default:
			}
		}
	}()
	go func() {
		if err := reg.Watch(ctx, reloadCh); err != nil && ctx.Err() == nil {
			log.Error("spec watcher stopped", "err", err)
		}
	}()

	// skill index reload lifecycle: every spec snapshot the registry
	// activates (watcher, SIGHUP) is republished into the skill discovery
	// namespace; skills whose source vanished are swept. The startup sync
	// already happened inside mcpserver.New; this loop covers reloads and
	// retries a startup sync that failed.
	go syncSkillsOnSpecChange(ctx, log, mem, reg, mcpDeps.SkillIndexNamespace(), 2*time.Second)

	// dispatcher: routed-but-unclaimed tasks get an investigation loop.
	// Sequential in v1: one investigation at a time keeps costs legible.
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				tasks, err := ledger.List(ctx, task.StatusSubmitted, 10)
				if err != nil {
					log.Error("dispatcher list failed", "err", err)
					continue
				}
				for _, tk := range tasks {
					if tk.AgentName == "" || tk.ParentTaskID != "" {
						continue // unrouted or subagent-managed
					}
					if err := runtime.RunTask(ctx, tk.ID); err != nil {
						log.Error("task run failed", "task", tk.ID, "err", err)
					}
				}
			}
		}
	}()

	// burn-rate projector (P14.3)
	projector := &cost.Projector{DB: db, Bus: eventBus, Log: log,
		DailyBudgetUS: int64(cfg.Budgets.GlobalDailyUSD * 1_000_000), Now: time.Now}
	go func() {
		tick := time.NewTicker(10 * time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := projector.Tick(ctx); err != nil {
					log.Error("cost projector failed", "err", err)
				}
			}
		}
	}()

	// consolidation (Vegapunk daily-sync) + IVF build, dream-scheduled.
	// Instead of unconditionally
	// sweeping every region on a slow fixed tick, a fast tick evaluates
	// per-namespace triggers: enough new writes since the last pass, a
	// minimum spacing between passes, and a short idle debounce so a
	// namespace mid-burst isn't consolidated between two writes (a new
	// write effectively resets the pending pass, exactly like a dream
	// cancelled by fresh messages). A namespace with zero new writes is
	// skipped entirely - superseded-fold, observations, and every LLM
	// pass with it - so quiet regions cost one COUNT per check. Any
	// activity at all still consolidates within consolidateMaxSpacing.
	// `punk consolidate` runs the same pass by hand, no gating.
	if cfg.Memory.ConsolidateDays > 0 || cfg.Memory.IVFNprobe > 0 {
		horizon := time.Duration(cfg.Memory.ConsolidateDays) * 24 * time.Hour
		go func() {
			lastRun := map[string]time.Time{}    // per-ns last pass (this process)
			ivfBootstrapped := map[string]bool{} // per-ns first BuildIVF done
			tick := time.NewTicker(consolidateCheckInterval)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					// memory namespaces, not regions: hook capture
					// creates namespaces that are never region-
					// registered, and they need consolidation most.
					names, err := mem.Namespaces(ctx)
					if err != nil {
						log.Error("consolidation list failed", "err", err)
						continue
					}
					now := time.Now()
					for _, ns := range names {
						// IVF bootstrap: the index is in-memory, so after a
						// restart it must be built once per namespace even
						// with no new writes, or approximate search would
						// stay on brute force until the first pass.
						if cfg.Memory.IVFNprobe > 0 && !ivfBootstrapped[ns] {
							if err := mem.BuildIVF(ctx, ns); err != nil {
								log.Error("ivf build failed", "ns", ns, "err", err)
							} else {
								ivfBootstrapped[ns] = true
							}
						}
						since := lastRun[ns]
						writes, lastWrite, err := mem.WriteActivity(ctx, ns, since)
						if err != nil {
							log.Error("consolidation activity check failed", "ns", ns, "err", err)
							continue
						}
						if writes == 0 {
							continue
						}
						if now.Sub(lastWrite) < consolidateIdleWait {
							continue // mid-burst: debounce until the region goes quiet
						}
						due := (writes >= consolidateMinWrites && now.Sub(since) >= consolidateMinSpacing) ||
							now.Sub(since) >= consolidateMaxSpacing
						if !due {
							continue
						}
						runConsolidationPass(ctx, mem, cfg, ns, horizon, false, observer, judge, cJudge, eventBus, log)
						lastRun[ns] = now
					}
				}
			}
		}()
	}

	// rolling session summaries: every few minutes, any
	// captured session whose new events weigh >= session_summary_tokens
	// gets its /agent-sessions/<sid>/summary updated recursively (prior
	// summary + new events). Opt-in: needs ai.enabled and the threshold.
	if cfg.AI.Enabled && cfg.Memory.SessionSummaryTokens > 0 && sessSum != nil {
		go func() {
			// summaries expire with the capture they compress (the hook
			// layer's 30-day TTL); consolidation is the durable tier.
			const summaryTTL = 30 * 24 * time.Hour
			tick := time.NewTicker(5 * time.Minute)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					names, err := mem.Namespaces(ctx)
					if err != nil {
						log.Error("session summary list failed", "err", err)
						continue
					}
					for _, ns := range names {
						if n, err := mem.SummarizeSessions(ctx, ns, cfg.Memory.SessionSummaryTokens, sessSum, summaryTTL); err != nil {
							log.Error("session summaries failed", "ns", ns, "err", err)
						} else if n > 0 {
							log.Info("session summaries updated", "ns", ns, "written", n)
						}
					}
				}
			}
		}()
	}

	// outbox tailer: durable memory events -> bus -> SSE + MCP subs
	go mem.RunOutboxTailer(ctx, eventBus, time.Second, log)
	go mem.RunEnricher(ctx, eventBus, log)
	srv.StartA2APushDelivery(ctx) // A2A webhooks on terminal task states

	// lease reaper: requeue tasks whose agent died mid-investigation
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if n, err := ledger.ReapExpired(ctx); err != nil {
					log.Error("lease reap failed", "err", err)
				} else if n > 0 {
					log.Info("requeued expired tasks", "count", n)
				}
			}
		}
	}()

	if cfg.Proposals.ExpireAfterHours > 0 {
		expireAfter := time.Duration(cfg.Proposals.ExpireAfterHours) * time.Hour
		go func() {
			tick := time.NewTicker(time.Hour)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if n, err := props.ExpireStale(ctx, expireAfter); err != nil {
						log.Error("proposal expiry failed", "err", err)
					} else if n > 0 {
						log.Info("expired stale proposals", "count", n)
					}
				}
			}
		}()
	}

	if cfg.Memory.RetentionDays > 0 {
		retention := time.Duration(cfg.Memory.RetentionDays) * 24 * time.Hour
		go func() {
			tick := time.NewTicker(time.Hour)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					n, err := mem.SweepRetention(ctx, retention)
					if err != nil {
						log.Error("retention sweep failed", "err", err)
					} else if n > 0 {
						log.Info("retention sweep", "rows", n)
					}
				}
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// cmdBrain prints or opens the brain view of a running server, or with
// "serve" runs the server itself and prints the URL first so one command
// gets a demo on screen.
func cmdBrain(args []string) error {
	if len(args) > 0 && args[0] == "serve" {
		// Print the URL serve will actually bind: the same config file
		// plus PUNK_HTTP_ADDR overrides that cmdServe reads.
		pf := flag.NewFlagSet("brain serve", flag.ContinueOnError)
		cfgPath := pf.String("config", "config.yaml", "path to config file")
		_ = pf.Parse(args[1:])
		addr := ":9090"
		if cfg, err := config.Load(*cfgPath); err == nil {
			addr = cfg.HTTP.Addr
		}
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		fmt.Fprintf(os.Stderr, "punk: brain view at http://%s/\n", addr)
		return cmdServe(args[1:])
	}
	fs := flag.NewFlagSet("brain", flag.ContinueOnError)
	server := fs.String("server", "http://127.0.0.1:9090", "punk server base URL")
	open := fs.Bool("open", false, "open the brain view in the default browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := strings.TrimRight(*server, "/") + "/"
	fmt.Println(url)
	if !*open {
		return nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w (visit %s)", err, url)
	}
	return nil
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	action := "status"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	switch action {
	case "up":
		n, err := db.MigrateUp(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("applied %d migration(s)\n", n)
		return nil
	case "down":
		n, err := db.MigrateDown(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("reverted %d migration(s)\n", n)
		return nil
	case "status":
		st, err := db.MigrateStatus(ctx)
		if err != nil {
			return err
		}
		for _, m := range st {
			state := "pending"
			if m.Applied {
				state = "applied " + m.AppliedAt
			}
			fmt.Printf("%04d %-20s %s\n", m.Version, m.Name, state)
		}
		return nil
	default:
		return fmt.Errorf("migrate: unknown action %q (want up|down|status)", action)
	}
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := "./specs"
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	b, errs := spec.LoadDir(dir)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d spec error(s) in %s", len(errs), dir)
	}
	fmt.Printf("ok: %d agent(s), %d skill(s), %d policy(ies) in %s\n",
		len(b.Agents), len(b.Skills), len(b.Policies), dir)
	return nil
}

func openMemory(cfgPath string) (*memory.Store, func(), error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return nil, nil, err
	}
	mem := memory.New(db, nil)
	mem.SetDefense(cfg.Memory.Defense)
	for ns, mode := range cfg.Memory.DefensePolicies {
		mem.SetDefensePolicy(ns, mode)
	}
	return mem, func() { _ = db.Close() }, nil
}

// newEmbedder builds the configured embedder, or nil when embeddings are
// off. "local" loads a pinned static model from the model cache,
// downloading it on first use; "ollama" (or empty) keeps today's
// behaviour and stays off when model is empty.
func newEmbedder(ctx context.Context, cfg *config.Config, log *slog.Logger) (memory.Embedder, error) {
	e := cfg.AI.Embeddings
	switch e.Provider {
	case "local":
		name := e.Model
		if name == "" {
			name = embedlocal.DefaultModel
		}
		cache := e.ModelCache
		if cache == "" {
			cache = embedlocal.DefaultCacheDir()
		}
		dir, err := embedlocal.Ensure(ctx, cache, name, "", os.Stderr)
		if err != nil {
			return nil, err
		}
		st, err := embedlocal.Load(dir, e.MaxInputTokens)
		if err != nil {
			return nil, err
		}
		if log != nil {
			log.Info("embeddings enabled", "provider", "local", "model", name, "dims", st.Dims())
		}
		return st, nil
	case "", "ollama":
		if e.Model == "" {
			return nil, nil
		}
		if log != nil {
			log.Info("embeddings enabled", "provider", "ollama", "model", e.Model)
		}
		return &memory.OllamaEmbedder{BaseURL: e.BaseURL, Model: e.Model, D: e.Dims}, nil
	default:
		return nil, fmt.Errorf("ai.embeddings.provider %q not supported", e.Provider)
	}
}

func cmdModels(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: punk models list | pull <name> [--cache DIR]")
	}
	switch args[0] {
	case "list":
		names := make([]string, 0, len(embedlocal.Catalog))
		for n := range embedlocal.Catalog {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			m := embedlocal.Catalog[n]
			fmt.Printf("%s\t%s@%s\tdims=%d\n", n, m.Repo, m.Revision[:7], m.Dims)
		}
		return nil
	case "pull":
		// Flags may appear before or after the model name, so parse
		// manually instead of flag.FlagSet (which stops at the first
		// positional argument).
		var cache string
		var rest []string
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--cache" && i+1 < len(args):
				cache = args[i+1]
				i++
			case strings.HasPrefix(args[i], "--cache="):
				cache = strings.TrimPrefix(args[i], "--cache=")
			default:
				rest = append(rest, args[i])
			}
		}
		if cache == "" {
			cache = embedlocal.DefaultCacheDir()
		}
		if len(rest) != 1 {
			return errors.New("usage: punk models pull <name> [--cache DIR]")
		}
		dir, err := embedlocal.Ensure(context.Background(), cache, rest[0], "", os.Stderr)
		if err != nil {
			return err
		}
		fmt.Printf("ready: %s\n", dir)
		return nil
	default:
		return fmt.Errorf("unknown models subcommand %q", args[0])
	}
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	ns := fs.String("ns", "", "namespace to export (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ns == "" {
		return errors.New("export: --ns is required")
	}
	mem, closeDB, err := openMemory(*cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	return mem.ExportJSONL(context.Background(), *ns, os.Stdout)
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	ns := fs.String("ns", "", "namespace to import into (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ns == "" {
		return errors.New("import: --ns is required")
	}
	mem, closeDB, err := openMemory(*cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	imported, skipped, blocked, err := mem.ImportJSONL(context.Background(), *ns, os.Stdin)
	if err != nil {
		return err
	}
	fmt.Printf("imported %d, skipped %d, blocked %d\n", imported, skipped, blocked)
	return nil
}

// cmdIngest loads one document file (or an explicit --url fetch) into
// memory through the internal/ingest loaders and I01's source-aware
// write path: unchanged chunks are never rewritten, every chunk carries
// source provenance, and the namespace's write-time defense applies.
// PDF extraction needs an external adapter (--pdf-adapter or
// PUNK_INGEST_PDF_ADAPTER); without one a PDF ingest fails with the
// configuration instructions and writes nothing.
func cmdIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	ns := fs.String("ns", "", "namespace to ingest into (required)")
	prefix := fs.String("prefix", "", "key prefix the document's chunks live under (required)")
	urlFlag := fs.String("url", "", "fetch the document from this URL instead of a file (explicit remote fetch; private/loopback/link-local targets are refused)")
	format := fs.String("format", "", "force a loader: text|markdown|html|incident-json|pdf (default: detect from name, content type or bytes)")
	sourceID := fs.String("source-id", "", "stable source identity owning the chunks (default: the file/URL itself)")
	revision := fs.String("revision", "", "source revision label (git SHA, etag)")
	author := fs.String("author", "punk-ingest", "writer recorded on the chunks")
	maxBytes := fs.Int64("max-bytes", ingest.DefaultMaxBytes, "size cap on input and adapter output")
	timeout := fs.Duration("timeout", ingest.DefaultTimeout, "time cap on loading, fetching and external adapters")
	pdfAdapter := fs.String("pdf-adapter", os.Getenv("PUNK_INGEST_PDF_ADAPTER"), "external PDF extractor command (adapter contract: docs/CONFIG.md#document-ingest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if *ns == "" {
		return errors.New("ingest: --ns is required")
	}
	if *prefix == "" {
		return errors.New("ingest: --prefix is required")
	}
	switch {
	case *urlFlag != "" && len(pos) > 0:
		return errors.New("ingest: pass a file path or --url, not both")
	case *urlFlag == "" && len(pos) == 0:
		return errors.New("usage: punk ingest --ns NS --prefix P [flags] <file>  (or --url URL)")
	case len(pos) > 1:
		return fmt.Errorf("ingest: one file at a time, got %d paths", len(pos))
	}
	spec := ingest.Spec{
		URL: *urlFlag, Format: *format, SourceID: *sourceID,
		Revision: *revision, MaxBytes: *maxBytes, Timeout: *timeout,
	}
	if len(pos) == 1 {
		spec.Path = pos[0]
	}
	if *pdfAdapter != "" {
		spec.PDFAdapter = strings.Fields(*pdfAdapter)
	}
	mem, closeDB, err := openMemory(*cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	w, u, r, b, err := ingest.Ingest(context.Background(), mem, *ns, *prefix, *author, spec)
	if err != nil {
		return err
	}
	fmt.Printf("ingested %s: %d written, %d unchanged, %d removed, %d blocked\n", *prefix, w, u, r, b)
	return nil
}

// cmdSeed backfills memory from a tool that reads an existing codebase.
// Hook capture is forward-looking by construction: it learns only what
// happens after `punk connect` runs, so a repository with years of history
// starts empty. Seeding is how that gap gets closed.
//
// One source is supported today: `rinnegan map --json`
// (github.com/hypervisor-io/rinnegan), a deterministic AST-derived
// architecture map. The document is read from STDIN rather than by
// shelling out to the rinnegan binary, which keeps punk free of any
// dependency on it - punk never has to know where it is installed, which
// version is present, or whether the repository is indexed yet:
//
//	rinnegan map --json | punk seed rinnegan
//
// --ns defaults to the same agent-<dir> namespace hook capture and context
// injection already use for the current directory, so a seeded fact is
// visible to the coding agent immediately. Point --dir at the repository
// when running from somewhere else.
func cmdSeed(args []string) error {
	source := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		source, args = args[0], args[1:]
	}
	if source == "" {
		return errors.New("usage: punk seed rinnegan [--ns NS] [--dir DIR] < map.json")
	}
	if source != "rinnegan" {
		return fmt.Errorf("unknown seed source %q, only \"rinnegan\" is supported", source)
	}

	fs := flag.NewFlagSet("seed rinnegan", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	ns := fs.String("ns", "", "namespace to seed (default: the agent namespace for --dir)")
	dir := fs.String("dir", ".", "repository the map describes; picks the default namespace")
	revision := fs.String("revision", "", "repository revision the map describes (default: git rev-parse HEAD in --dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	namespace := *ns
	if namespace == "" {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", *dir, err)
		}
		// The SAME derivation handleAgentHook and handleAgentContext use,
		// called rather than reimplemented: a seed that landed in a
		// different namespace than the one the agent reads would be
		// invisible, and silently so.
		namespace = api.AgentNamespace(abs)
	}

	mem, closeDB, err := openMemory(*cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	rev := *revision
	if rev == "" {
		out, err := exec.Command("git", "-C", *dir, "rev-parse", "HEAD").Output()
		if err == nil {
			rev = strings.TrimSpace(string(out))
		}
	}

	stats, err := mem.SeedCodeMapWith(context.Background(), namespace, os.Stdin, memory.SeedCodeMapOpts{Revision: rev})
	if err != nil {
		return err
	}
	summary := ""
	if len(rev) >= 7 {
		summary = " at " + rev[:7]
	} else if rev != "" {
		summary = " at " + rev
	}
	fmt.Printf("seeded %s%s: %d written, %d unchanged, %d removed\n",
		namespace, summary, stats.Written, stats.Unchanged, stats.Removed)
	return nil
}

// apiKeySubject defaults an API key's subject to its name when --subject
// is omitted (or blank): under authz.enforcement=deny an empty subject
// can never be granted anything (authz.validateSubject rejects empty),
// so a key created without --subject would otherwise be permanently
// ungrantable. An explicit, non-blank subject always wins.
func apiKeySubject(name, subject string) string {
	if strings.TrimSpace(subject) == "" {
		return name
	}
	return subject
}

func cmdAPIKey(args []string) error {
	// action comes first (Go's flag parsing stops at the first non-flag):
	// punk apikey create --name ci
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("apikey", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	name := fs.String("name", "", "key name (required)")
	subject := fs.String("subject", "", "external identity principal (Okta/Entra)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("apikey: --name is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	keys := api.NewKeys(db, nil)
	switch action {
	case "create":
		effectiveSubject := apiKeySubject(*name, *subject)
		token, err := keys.Create(context.Background(), *name, effectiveSubject)
		if err != nil {
			return err
		}
		fmt.Println(token)
		fmt.Fprintln(os.Stderr, "store this token now; it is not shown again")
		if strings.TrimSpace(*subject) == "" {
			fmt.Fprintf(os.Stderr, "subject defaults to name: %s\n", effectiveSubject)
		}
		return nil
	case "revoke":
		return keys.Revoke(context.Background(), *name)
	default:
		return fmt.Errorf("apikey: want create or revoke, got %q", action)
	}
}

// cmdAuthz is the local provisioning path for namespace grants (task
// A01): same trust level as `punk apikey create` - direct DB access, no
// HTTP, no unauthenticated endpoint. It is also the recovery path when
// authz.enforcement=deny has locked every subject out: grant locally,
// then authenticate with the matching key.
func cmdAuthz(args []string) error {
	// action may come first (Go's flag parsing stops at the first
	// non-flag): punk authz grant --subject alice ...
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("authz", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	subject := fs.String("subject", "", "verified API-key subject the grant applies to")
	namespace := fs.String("namespace", "", "namespace the grant applies to (exact match, no wildcards)")
	op := fs.String("op", "", "read | write | admin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if action == "" && fs.NArg() > 0 {
		action = fs.Arg(0)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	az := authz.New(db, nil)
	ctx := context.Background()

	switch action {
	case "grant", "revoke":
		if *subject == "" || *namespace == "" || *op == "" {
			return fmt.Errorf("authz %s: --subject, --namespace and --op are required", action)
		}
		if action == "grant" {
			if err := az.Grant(ctx, *subject, *namespace, authz.Op(*op)); err != nil {
				return err
			}
			fmt.Printf("granted %s %s on %s\n", *subject, *op, *namespace)
			return nil
		}
		if err := az.Revoke(ctx, *subject, *namespace, authz.Op(*op)); err != nil {
			return err
		}
		fmt.Printf("revoked %s %s on %s\n", *subject, *op, *namespace)
		return nil
	case "list":
		if *subject == "" {
			return errors.New("authz list: --subject is required")
		}
		grants, err := az.Grants(ctx, *subject)
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			fmt.Printf("no active grants for %s\n", *subject)
			return nil
		}
		for _, g := range grants {
			fmt.Printf("%s %s %s %s\n", g.Subject, g.Namespace, g.Op, g.CreatedAt.Format(time.RFC3339))
		}
		return nil
	default:
		return fmt.Errorf("authz: unknown action %q (want grant|revoke|list)", action)
	}
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	toolset := fs.String("toolset", envOr("PUNK_MCP_TOOLSET", "full"), "agent | full")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// stdout is the protocol; logs go to stderr via slog default
	log := obs.NewLogger(cfg.Log.Format, cfg.Log.Level)
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	reg := registry.New(cfg.Specs.Dir, db, log)
	if err := reg.Load(context.Background()); err != nil {
		return err
	}
	eventBus := bus.New()
	ledger := task.NewLedger(db, nil)
	ledger.Notify = func(taskID, status string) {
		eventBus.Publish(bus.Event{Kind: "task_status", Key: taskID,
			Data: map[string]string{"status": status}})
	}
	router := route.New(db, reg, ledger, nil, nil)
	router.Epsilon = cfg.Route.Epsilon
	router.Fallback = cfg.Route.Fallback
	router.TopoDB = db
	mem := memory.New(db, nil)
	mem.SetDefense(cfg.Memory.Defense)
	for ns, mode := range cfg.Memory.DefensePolicies {
		mem.SetDefensePolicy(ns, mode)
	}
	srv := mcpserver.New(mcpserver.Deps{
		Ledger: ledger, Router: router, Reg: reg, Mem: mem,
		A2ARemotes:       a2aRemotes(cfg),
		NamespaceFor:     api.AgentNamespace,
		DefaultNamespace: os.Getenv("PUNK_NAMESPACE"),
		LocalFiles:       true,
		Toolset:          *toolset,
		DefaultBudget: task.Budget{
			Tokens:    cfg.Budgets.Tokens,
			ToolCalls: cfg.Budgets.ToolCalls,
			WallMS:    cfg.Budgets.WallMS,
			Subagents: cfg.Budgets.Subagents,
		},
	})
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	out := fs.String("out", "", "destination snapshot file (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("backup: --out is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := db.BackupSQLite(context.Background(), *out); err != nil {
		return err
	}
	fmt.Printf("snapshot written to %s (specs live in git; that is the whole state)\n", *out)
	return nil
}

// cardSlugRe collapses everything that isn't alphanumeric to a hyphen
// when deriving a /profile/ key slug from a card fact's text.
var cardSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// cmdCard manages the user's cross-project profile card: stable facts
// and standing instructions that ground every session. Facts live under /profile/ in the global user-profile
// namespace (api.ProfileNamespace) and are injected into every
// project's session-start block via the "profile" inject component, up
// to 40 entries. Subcommands:
//
//	punk card add "Prefers table-driven tests" [--key prefers-tests]
//	punk card list
//	punk card remove --key prefers-tests
//
// add derives the key slug from the text when --key is absent, so
// re-adding a reworded fact under the same --key supersedes it instead
// of stacking near-duplicates.
func cmdCard(args []string) error {
	if len(args) == 0 {
		return errors.New("card: want add \"fact\" | list | remove --key <slug>")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("card "+sub, flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	key := fs.String("key", "", "profile key slug (add: derived from text when absent; remove: required)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	mem := memory.New(db, nil)
	mem.SetDefense(cfg.Memory.Defense)
	for n, mode := range cfg.Memory.DefensePolicies {
		mem.SetDefensePolicy(n, mode)
	}
	ctx := context.Background()

	switch sub {
	case "add":
		body := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if body == "" {
			return errors.New("card add: fact text required")
		}
		// Go's flag package stops flag parsing at the first positional, so
		// a flag typed AFTER the fact text would silently become part of
		// the fact. Catch that instead of storing it.
		for _, a := range fs.Args() {
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("card add: flag %q must come before the fact text (punk card add --key slug \"fact\")", a)
			}
		}
		slug := *key
		if slug == "" {
			slug = strings.Trim(cardSlugRe.ReplaceAllString(strings.ToLower(body), "-"), "-")
			if len(slug) > 48 {
				slug = strings.Trim(slug[:48], "-")
			}
		}
		if slug == "" {
			return errors.New("card add: could not derive a key slug; pass --key")
		}
		f, err := mem.Write(ctx, memory.WriteInput{
			Namespace: api.ProfileNamespace, Key: "/profile/" + slug, Body: body,
			Author: "user", Writer: "card-cli", Importance: 0.8,
		})
		if err != nil {
			return fmt.Errorf("card add: %w", err)
		}
		fmt.Printf("added %s\n", f.Key)
		return nil
	case "list":
		facts, err := mem.Recall(ctx, api.ProfileNamespace, "/profile/", 100)
		if err != nil {
			return fmt.Errorf("card list: %w", err)
		}
		if len(facts) == 0 {
			fmt.Println("profile card is empty; add with: punk card add \"...\"")
			return nil
		}
		for _, f := range facts {
			fmt.Printf("%-40s %s\n", f.Key, f.Body)
		}
		return nil
	case "remove":
		if *key == "" {
			return errors.New("card remove: --key is required")
		}
		if err := mem.Forget(ctx, api.ProfileNamespace, "/profile/"+strings.TrimPrefix(*key, "/profile/"), "user"); err != nil {
			return fmt.Errorf("card remove: %w", err)
		}
		fmt.Printf("removed /profile/%s\n", strings.TrimPrefix(*key, "/profile/"))
		return nil
	default:
		return fmt.Errorf("card: unknown subcommand %q (want add|list|remove)", sub)
	}
}

// cmdConsolidate is `punk consolidate`: run a consolidation pass right
// now, bypassing
// the serve loop's dream triggers entirely. It opens the store directly
// - same pattern as embed-backfill - builds the same optional AI pieces
// serve would (observer/judge/contradiction judge, when ai.enabled and
// the default profile resolves), and runs runConsolidationPass with
// force=true for one namespace (--ns) or every namespace. With
// consolidate_days unset the horizon is 0: every superseded revision
// folds, which is what a manual "consolidate now" means.
func cmdConsolidate(args []string) error {
	fs := flag.NewFlagSet("consolidate", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	ns := fs.String("ns", "", "namespace (empty = all namespaces)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	log := obs.NewLogger(cfg.Log.Format, cfg.Log.Level)
	mem := memory.New(db, nil)
	mem.SetDefense(cfg.Memory.Defense)
	mem.SetQuantizeVectors(cfg.Memory.QuantizeVectors)
	for n, mode := range cfg.Memory.DefensePolicies {
		mem.SetDefensePolicy(n, mode)
	}
	if cfg.AI.Embeddings.Model != "" {
		mem.SetEmbedder(&memory.OllamaEmbedder{
			BaseURL: cfg.AI.Embeddings.BaseURL,
			Model:   cfg.AI.Embeddings.Model,
			D:       cfg.AI.Embeddings.Dims,
		})
	}
	mem.SetIVFConfig(cfg.Memory.IVFNprobe, cfg.Memory.IVFMinFacts)

	var observer memory.ObservationSummarizer
	var judge memory.MergeJudge
	var cJudge memory.ContradictionJudge
	if cfg.AI.Enabled {
		profiles := map[string]llm.Profile{}
		for name, p := range cfg.AI.Profiles {
			profiles[name] = llm.Profile{BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv, Model: p.Model}
		}
		llmMgr := llm.NewManager(cfg.AI.Enabled, profiles, db, nil)
		if obsClient, err := llmMgr.Client("default"); err == nil {
			observer = &obsSummarizer{client: obsClient, log: log}
			judge = &mergeJudge{client: obsClient, log: log}
			cJudge = &contradictJudge{client: obsClient, log: log}
		} else {
			log.Warn("llm observer disabled", "err", err)
		}
	}

	ctx := context.Background()
	names := []string{*ns}
	if *ns == "" {
		names, err = mem.Namespaces(ctx)
		if err != nil {
			return fmt.Errorf("consolidate: list namespaces: %w", err)
		}
	}
	horizon := time.Duration(cfg.Memory.ConsolidateDays) * 24 * time.Hour
	for _, n := range names {
		runConsolidationPass(ctx, mem, cfg, n, horizon, true, observer, judge, cJudge, nil, log)
		fmt.Printf("consolidated %s\n", n)
	}
	return nil
}

func cmdEmbedBackfill(args []string) error {
	fs := flag.NewFlagSet("embed-backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	ns := fs.String("ns", "", "namespace (required)")
	force := fs.Bool("force", false, "re-embed every live fact, not only those missing a vector")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ns == "" {
		return errors.New("embed-backfill: --ns is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	mem := memory.New(db, nil)
	mem.SetDefense(cfg.Memory.Defense)
	mem.SetQuantizeVectors(cfg.Memory.QuantizeVectors)
	for ns, mode := range cfg.Memory.DefensePolicies {
		mem.SetDefensePolicy(ns, mode)
	}
	emb, err := newEmbedder(context.Background(), cfg, nil)
	if err != nil {
		return err
	}
	if emb == nil {
		return errors.New("embed-backfill: embeddings not configured")
	}
	mem.SetEmbedder(emb)
	mem.SetEmbedMaxTokens(cfg.AI.Embeddings.MaxInputTokens)
	var n int
	if *force {
		n, err = mem.ReembedAll(context.Background(), *ns, 64)
	} else {
		n, err = mem.BackfillEmbeddings(context.Background(), *ns, 64)
	}
	if err != nil {
		return err
	}
	fmt.Printf("embedded %d fact(s)\n", n)
	return nil
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	taskID := fs.String("task", "", "completed task id (required)")
	mode := fs.String("mode", "strict", "trajectory match: strict|unordered|subset")
	k := fs.Int("k", 1, "number of replays (pass^k)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		return errors.New("replay: --task is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !cfg.AI.Enabled {
		return errors.New("replay: ai.enabled=true required (replay drives a model against the frozen world)")
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	log := obs.NewLogger(cfg.Log.Format, cfg.Log.Level)
	reg := registry.New(cfg.Specs.Dir, db, log)
	if err := reg.Load(context.Background()); err != nil {
		return err
	}
	ledger := task.NewLedger(db, nil)
	profiles := make(map[string]llm.Profile, len(cfg.AI.Profiles))
	for name, p := range cfg.AI.Profiles {
		profiles[name] = llm.Profile{BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv, Model: p.Model}
	}
	client, err := llm.NewManager(true, profiles, db, nil).Client("default")
	if err != nil {
		return err
	}
	results := make([]bool, 0, *k)
	for i := 0; i < *k; i++ {
		res, err := replay.Rerun(context.Background(), ledger, reg, client,
			*taskID, replay.TrajectoryMode(*mode), log)
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(res, "", "  ")
		fmt.Printf("run %d/%d:\n%s\n", i+1, *k, raw)
		results = append(results, res.Pass)
	}
	fmt.Printf("pass^%d: %v\n", *k, replay.PassHatK(results))
	return nil
}

// skillDraftPrompt: strict JSON so parsing stays deterministic.
const skillDraftPrompt = `You are drafting an agentskills.io SKILL.md from repeated agent investigations.
Given the tool trajectory and finding summaries below, write a reusable procedure.
Respond with ONLY JSON: {"name":"short title","description":"one sentence","body":"markdown procedure with numbered steps"}`

// insightsDraftPrompt asks for proposed CLAUDE.md additions distilled
// from a namespace's memory.
// Every rule must cite the fact IDs behind it - the evidence contract,
// applied to instructions.
const insightsDraftPrompt = `You distill an AI coding agent's project memory into PROPOSED additions to that project's CLAUDE.md (the instructions file the agent reads every session).
From the JSON facts below (id, key, body), extract only durable, actionable guidance: style rules, workflow rules, architectural constraints, gotchas.
Rules:
- Only what the facts support. No inventions, no generic advice.
- Group under "## Style", "## Workflow", "## Architecture", "## Gotchas" (omit empty groups).
- One bullet per rule, imperative mood, ending with (evidence: id, id).
- Skip anything transient (one-off bugs, in-flight work).
Respond with ONLY the markdown body, no preamble.`

// insightsDrafter adapts an llm.Client to skillmine.InsightDrafter.
type insightsDrafter struct{ client llm.Client }

func (d insightsDrafter) DraftInsights(ctx context.Context, ns string, facts []memory.Fact) (string, error) {
	type slim struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Body string `json:"body"`
	}
	slims := make([]slim, len(facts))
	for i, f := range facts {
		slims[i] = slim{ID: f.ID, Key: f.Key, Body: f.Body}
	}
	payload, err := json.Marshal(map[string]any{"namespace": ns, "facts": slims})
	if err != nil {
		return "", err
	}
	res, err := d.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: insightsDraftPrompt},
		{Role: "user", Content: string(payload)},
	}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Content), nil
}

// llmDrafter adapts an llm.Client to skillmine.Drafter for "punk skills
// propose". An unparseable model response fails the draft (unlike the
// consolidation/merge/contradiction adapters above, which swallow a bad
// response and keep going): those run unattended on a ticker where
// skipping one round is harmless, but propose is an interactive one-shot
// command - silently dropping a draft the operator is waiting on would be
// worse than surfacing the error.
type llmDrafter struct{ client llm.Client }

func (d llmDrafter) Draft(ctx context.Context, g skillmine.Group) (string, string, string, error) {
	payload, err := json.Marshal(map[string]any{"agent": g.Agent, "trajectory": g.Trajectory, "summaries": g.Summaries})
	if err != nil {
		return "", "", "", err
	}
	res, err := d.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: skillDraftPrompt},
		{Role: "user", Content: string(payload)},
	}, nil)
	if err != nil {
		return "", "", "", err
	}
	s := strings.TrimSpace(res.Content)
	// fence-strip identical to sibling adapters (obsSummarizer, mergeJudge,
	// contradictJudge above).
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if k := strings.Index(s, "```"); k >= 0 {
			s = s[:k]
		}
	}
	var out struct {
		Name, Description, Body string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &out); err != nil {
		return "", "", "", fmt.Errorf("skill draft: unparseable model response: %w", err)
	}
	return out.Name, out.Description, out.Body, nil
}

// cmdSkills implements "punk skills propose": mine completed investigate
// syncSkillsOnSpecChange keeps the skill discovery index aligned with
// the registry's active spec snapshot: every version advance (file
// watcher, SIGHUP) republishes authored skills into ns and sweeps the
// ones whose source vanished. Startup indexing happens in
// mcpserver.New; because last starts at 0 the first tick re-syncs even
// then, which is an idempotent no-op on success and a retry when the
// startup sync failed. Blocks until ctx is done.
func syncSkillsOnSpecChange(ctx context.Context, log *slog.Logger, mem *memory.Store, reg *registry.Registry, ns string, interval time.Duration) {
	var last int64
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		snap := reg.Current()
		if snap == nil || snap.Version == last {
			continue
		}
		rep, err := skillmine.SyncBundle(ctx, mem, ns, snap.Bundle)
		if err != nil {
			log.Error("skill index sync failed, retrying on next tick", "err", err)
			continue
		}
		if len(rep.Conflicts) > 0 {
			log.Warn("skill versions republished with changed content stay pinned; bump metadata.version to publish",
				"conflicts", rep.Conflicts)
		}
		if len(rep.Rejected) > 0 {
			log.Warn("skills rejected by index validation", "rejected", rep.Rejected)
		}
		last = snap.Version
		log.Info("skill index synced", "namespace", ns, "snapshot", snap.Version,
			"indexed", rep.Indexed, "tombstoned", rep.Tombstoned)
	}
}

// tasks for recurring tool trajectories (deterministic) and draft one
// SKILL.md per recurring group (LLM prose) into --out for a human to
// review before moving the good ones into specs/skills/.
//
// punk skills propose [--config F] [--min-count 3] [--out ./proposed-skills] [--agent NAME] [--ns NS]
func cmdSkills(args []string) error {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("skills", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	minCount := fs.Int("min-count", 3, "minimum recurring tasks to propose a skill")
	out := fs.String("out", "", "output directory for drafts (never specs/; default ./proposed-skills or ./proposed-insights)")
	agentName := fs.String("agent", "", "restrict mining to one agent")
	ns := fs.String("ns", "", "namespace to distill (insights) / to publish mined draft metadata into (propose); default PUNK_NAMESPACE or agent-default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if action != "propose" && action != "insights" {
		return fmt.Errorf("skills: want propose or insights, got %q", action)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !cfg.AI.Enabled {
		return errors.New("skills " + action + ": ai.enabled=true required (drafting needs a model; mining alone has nothing to write)")
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	profiles := make(map[string]llm.Profile, len(cfg.AI.Profiles))
	for name, p := range cfg.AI.Profiles {
		profiles[name] = llm.Profile{BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv, Model: p.Model}
	}
	client, err := llm.NewManager(true, profiles, db, nil).Client("default")
	if err != nil {
		return err
	}

	// skills insights: distill one
	// namespace's memory into proposed CLAUDE.md additions - a proposal
	// file for a human to merge, never an edit to a real CLAUDE.md.
	if action == "insights" {
		if *ns == "" {
			return errors.New("skills insights: --ns is required (e.g. --ns agent-myproj)")
		}
		if *out == "" {
			*out = "./proposed-insights"
		}
		mem := memory.New(db, nil)
		path, err := skillmine.WriteInsights(context.Background(), mem, *ns, insightsDrafter{client}, *out)
		if err != nil {
			return err
		}
		if path == "" {
			fmt.Println("nothing to distill in", *ns)
			return nil
		}
		fmt.Println(path)
		fmt.Println("review and merge the rules you agree with into your CLAUDE.md - punk never edits it itself")
		return nil
	}

	if *out == "" {
		*out = "./proposed-skills"
	}
	ledger := task.NewLedger(db, nil)
	groups, err := skillmine.Mine(context.Background(), ledger, *agentName, *minCount)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		fmt.Println("no recurring trajectories at this threshold; nothing to propose")
		return nil
	}
	paths, err := skillmine.WriteDrafts(context.Background(), groups, llmDrafter{client}, *out)
	if err != nil {
		for _, p := range paths {
			fmt.Println(p)
		}
		return fmt.Errorf("%d draft(s) written to %s before failure: %w", len(paths), *out, err)
	}
	for _, p := range paths {
		fmt.Println(p)
	}

	// Mined-ingest lifecycle: publish the drafts' metadata into the
	// memory plane so search_skills can discover them (procedures stay
	// on-demand via load_skill). Drafts without a frontmatter version
	// get a content-derived revision, so a rerun that regenerated a
	// draft publishes its new generation and sweeps the old one.
	skillNS := *ns
	if skillNS == "" {
		skillNS = os.Getenv("PUNK_NAMESPACE")
	}
	if skillNS == "" {
		skillNS = "agent-default"
	}
	mem := memory.New(db, nil)
	rep, err := skillmine.SyncDrafts(context.Background(), mem, skillNS, *out)
	if err != nil {
		return fmt.Errorf("publish draft metadata into namespace %s: %w", skillNS, err)
	}
	if rep.Indexed > 0 || rep.Tombstoned > 0 {
		fmt.Printf("skill index[%s]: %d draft version(s) published, %d swept\n", skillNS, rep.Indexed, rep.Tombstoned)
	}
	for _, c := range rep.Conflicts {
		fmt.Printf("skill index[%s]: %s kept its pinned published version (bump metadata.version in the draft to publish the edit)\n", skillNS, c)
	}
	fmt.Printf("%d draft(s) written to %s - review and move the good ones into specs/skills/ (rerunning overwrites drafts with the same slug)\n", len(paths), *out)
	return nil
}

// a2aRemotes resolves configured outbound A2A targets, reading each token
// from its env var at wiring time (secrets never live in config).
func a2aRemotes(cfg *config.Config) []mcpserver.A2ARemote {
	out := make([]mcpserver.A2ARemote, 0, len(cfg.A2A.Remotes))
	for _, r := range cfg.A2A.Remotes {
		tok := ""
		if r.TokenEnv != "" {
			tok = os.Getenv(r.TokenEnv)
		}
		out = append(out, mcpserver.A2ARemote{Name: r.Name, Endpoint: r.URL, Token: tok})
	}
	return out
}

// cmdA2A is the outbound A2A client CLI:
//
//	punk a2a card <base-or-card-url>
//	punk a2a send [--stream] [--token-env NAME] [--task ID] <endpoint> <text...>
func cmdA2A(args []string) error {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("a2a", flag.ContinueOnError)
	tokenEnv := fs.String("token-env", "PUNK_A2A_TOKEN", "env var holding the bearer token")
	stream := fs.Bool("stream", false, "stream events instead of blocking (send)")
	taskID := fs.String("task", "", "continue an existing remote task by id (send)")
	history := fs.Int("history", 0, "history length to request (send)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	token := os.Getenv(*tokenEnv)
	ctx := context.Background()

	switch action {
	case "card":
		if len(rest) < 1 {
			return errors.New("a2a card: need a base or card URL")
		}
		card, err := a2a.FetchCard(ctx, nil, rest[0], token)
		if err != nil {
			return err
		}
		return emitJSON(card)
	case "send":
		if len(rest) < 2 {
			return errors.New("a2a send: need <endpoint> <text>")
		}
		cl := a2a.NewClient(rest[0], token)
		msg := a2a.TextMessage(strings.Join(rest[1:], " "))
		if *taskID != "" {
			msg.TaskID = *taskID
		}
		if *stream {
			return cl.StreamMessage(ctx, msg, nil, func(kind string, raw json.RawMessage) error {
				fmt.Printf("%s\t%s\n", kind, string(raw))
				return nil
			})
		}
		t, err := cl.SendMessage(ctx, msg, &a2a.SendMessageConfig{Blocking: true, HistoryLength: *history})
		if err != nil {
			return err
		}
		return emitJSON(t)
	default:
		return fmt.Errorf("a2a: want card|send, got %q", action)
	}
}

func emitJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// cmdITBench runs Punk Records against a directory of ITBench-style SRE
// scenarios and reports the resolved rate (faulty-entity identification).
//
//	punk itbench run --dir scenarios/itbench --agent sre [--config config.yaml] [--k N]
func cmdITBench(args []string) error {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("itbench", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	dir := fs.String("dir", "scenarios/itbench", "directory of scenario folders")
	agentName := fs.String("agent", "sre", "registered agent to run each scenario")
	k := fs.Int("k", 1, "attempts per scenario (pass^k: resolved if any attempt resolves)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if action != "run" {
		return fmt.Errorf("itbench: want run, got %q", action)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !cfg.AI.Enabled {
		fmt.Fprintln(os.Stderr, "warning: ai.enabled=false; scenarios cannot be diagnosed and will score unresolved")
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	log := obs.NewLogger(cfg.Log.Format, cfg.Log.Level)
	reg := registry.New(cfg.Specs.Dir, db, log)
	if err := reg.Load(context.Background()); err != nil {
		return err
	}
	ledger := task.NewLedger(db, nil)
	profiles := make(map[string]llm.Profile, len(cfg.AI.Profiles))
	for name, p := range cfg.AI.Profiles {
		profiles[name] = llm.Profile{BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv, Model: p.Model}
	}
	llmMgr := llm.NewManager(cfg.AI.Enabled, profiles, db, nil)
	deps := itbench.Deps{Ledger: ledger, Reg: reg, LLM: llmMgr, Agent: *agentName, Log: log}

	scns, err := itbench.LoadDir(*dir)
	if err != nil {
		return err
	}
	resolved := 0
	for _, scn := range scns {
		attempts := make([]bool, 0, *k)
		var last *itbench.Result
		for i := 0; i < *k; i++ {
			r, err := itbench.Run(context.Background(), deps, scn)
			if err != nil {
				return err
			}
			attempts = append(attempts, r.Score.Resolved)
			last = r
		}
		pass := itbench.PassHatK(attempts)
		if pass {
			resolved++
		}
		mark := "MISS"
		if pass {
			mark = "PASS"
		}
		fmt.Printf("%-4s %-28s want=%v got=%v\n", mark, scn.ID,
			scn.GroundTruth.FaultyEntities, last.Score.Matched)
	}
	rate := 0.0
	if len(scns) > 0 {
		rate = float64(resolved) / float64(len(scns))
	}
	fmt.Printf("\nITBench SRE: resolved %d/%d (%.1f%%) at pass^%d\n",
		resolved, len(scns), rate*100, *k)
	return nil
}

func cmdTopo(args []string) error {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("topo", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	file := fs.String("file", "", "Backstage catalog YAML (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if action != "import" {
		return fmt.Errorf("topo: want import, got %q", action)
	}
	if *file == "" {
		return errors.New("topo import: --file is required")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	svcs, edges, err := topo.ImportBackstage(context.Background(), db, raw)
	if err != nil {
		return err
	}
	fmt.Printf("imported %d service(s), %d declared edge(s)\n", svcs, edges)
	return nil
}

func cmdRegion(args []string) error {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("region", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file")
	ns := fs.String("ns", "", "region namespace (required)")
	dir := fs.String("dir", "", "git worktree dir (required)")
	branch := fs.String("branch", "", "branch name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ns == "" || *dir == "" {
		return errors.New("region: --ns and --dir are required")
	}
	mem, closeDB, err := openMemory(*cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	ctx := context.Background()
	switch action {
	case "branch":
		if err := mem.BranchRegion(ctx, *ns, *dir, *branch); err != nil {
			return err
		}
		fmt.Printf("region %s exported to %s (branch %s)\n", *ns, *dir, *branch)
		return nil
	case "merge":
		imported, skipped, blocked, err := mem.MergeBranch(ctx, *ns, *dir, *branch)
		if err != nil {
			return err
		}
		fmt.Printf("merged: %d new, %d deduped, %d blocked\n", imported, skipped, blocked)
		return nil
	default:
		return fmt.Errorf("region: want branch or merge, got %q", action)
	}
}

// cmdMembench runs the deterministic recall/MRR regression harness
// against a fresh in-memory store, so results never depend on
// pre-existing data or config.
//
//	punk membench --file scenarios/membench/sample.jsonl [--k N] [--ns name]
//	punk membench --file ... --rerank --reranker-url http://host:8080/rerank
//	punk membench --file scenarios/membench/baseline.jsonl --report report.json [--k N] [--seed N] [--commit SHA]
//	punk membench --file scenarios/membench/answers.jsonl --report report.json --answers [--judge-url URL --judge-model M --answer-budget N]
//	punk membench --locomo locomo10.json [--k N] [--config config.yaml]
//
// --report writes the versioned machine-readable report (membench.Report:
// run manifests with corpus hash/seed/commit/model IDs/settings/mode,
// per-query rankings and latency, aggregate hit_at_k/evidence recall/MRR)
// for the baseline and its ablations in one command. The report's
// source_revision records the code-state provenance resolved at report
// time (git HEAD of the tree the command runs in, "+dirty.<hash>" when
// uncommitted changes are present, else explicit "unknown"), separate
// from the --commit build label. Offline by design: --file mode wires no
// embedder, and model-based ablations are recorded explicitly unavailable
// unless a reranker URL is given; no paid model calls happen by default.
// A reranker that fails degrades the rerank ablation to recorded baseline
// fallback, never to a silent "successful" rerank.
//
// --answers attaches the optional answer stage (membench.RunAnswers) to
// the report: per-case answers, retrieved/cited IDs, abstention decisions
// and token usage, with citation existence scored separately from claim
// support and unanswerable examples kept distinct from retrieval misses.
// The defaults are fully offline (extractive composer + gold judge); a
// model-backed judge or reflect composer runs ONLY with explicit
// endpoint/model/--answer-budget configuration. Like the rest of --file
// mode, the stage runs against a throwaway in-memory store in the
// membench namespace - never the live memory namespace.
func cmdMembench(args []string) error {
	fs := flag.NewFlagSet("membench", flag.ContinueOnError)
	file := fs.String("file", "", "JSONL scenario file")
	locomo := fs.String("locomo", "", "LoCoMo dataset JSON (locomo10.json); mutually exclusive with --file")
	k := fs.Int("k", 5, "top-k results considered per query")
	ns := fs.String("ns", "membench", "namespace to ingest facts into")
	rerank := fs.Bool("rerank", false, "score via HybridSearchReranked instead of HybridSearch")
	rerankerURL := fs.String("reranker-url", "", "cross-encoder endpoint for --rerank (TEI /rerank shape)")
	cfgPath := fs.String("config", "config.yaml", "path to config file (wires the embedder if configured, for a hybrid rather than FTS-only number)")
	report := fs.String("report", "", "write the versioned baseline+ablation report (manifests, per-query rankings, aggregate metrics) to this path; --file scenarios only")
	seed := fs.Int64("seed", 1, "seed recorded in report manifests (retrieval is deterministic; reserved for future randomized ablations)")
	commit := fs.String("commit", version, "build/commit label recorded in report manifests (code-state provenance is resolved separately into the report's source_revision)")
	answers := fs.Bool("answers", false, "attach the answer stage to --report: compose and grade answers, citation existence/support and abstention per query (offline defaults; --file scenarios only)")
	composerName := fs.String("composer", "extractive", "answer composer for --answers: extractive (deterministic, offline) | reflect (model-backed agentic loop; requires --composer-url, --composer-model and --answer-budget)")
	composerURL := fs.String("composer-url", "", "OpenAI-compatible endpoint for --composer reflect")
	composerModel := fs.String("composer-model", "", "model ID for --composer reflect")
	judgeURL := fs.String("judge-url", "", "OpenAI-compatible endpoint for the semantic judge (requires --judge-model and --answer-budget); empty = deterministic gold grading")
	judgeModel := fs.String("judge-model", "", "model ID for the judge")
	judgeKeyEnv := fs.String("judge-key-env", "", "env var HOLDING the API key for the judge/composer endpoint (optional; never the key itself)")
	answerBudget := fs.Int("answer-budget", 0, "hard token budget across composer+judge calls; required when any model-backed answer component is configured")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" && *locomo == "" {
		return errors.New("membench: --file or --locomo is required")
	}
	if *file != "" && *locomo != "" {
		return errors.New("membench: --file and --locomo are mutually exclusive")
	}
	if *report != "" && *locomo != "" {
		return errors.New("membench: --report supports --file scenarios only")
	}
	if *report != "" && *rerank {
		return errors.New("membench: --report runs its own rerank ablation when --reranker-url is set; --rerank applies to plain scoring only")
	}
	answerFlagsSet := *answers || *composerName != "extractive" || *composerURL != "" || *composerModel != "" || *judgeURL != "" || *judgeModel != "" || *answerBudget != 0
	if answerFlagsSet && !*answers {
		return errors.New("membench: answer-stage flags require --answers")
	}
	if *answers {
		if *report == "" {
			return errors.New("membench: --answers attaches to the versioned report; --report is required")
		}
		if *locomo != "" {
			return errors.New("membench: --answers supports --file scenarios only")
		}
		if *composerName != "extractive" && *composerName != "reflect" {
			return fmt.Errorf("membench: --composer %q: want extractive or reflect", *composerName)
		}
		if *composerName == "reflect" && (*composerURL == "" || *composerModel == "") {
			return errors.New("membench: --composer reflect requires explicit --composer-url and --composer-model")
		}
		if (*judgeURL != "") != (*judgeModel != "") {
			return errors.New("membench: --judge-url and --judge-model are required together")
		}
		if (*composerName == "reflect" || *judgeModel != "") && *answerBudget <= 0 {
			return errors.New("membench: a model-backed answer stage requires an explicit --answer-budget (tokens)")
		}
	}
	db, err := store.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		return err
	}
	// ponytail: no SetDefense here — membench writes only its own bundled
	// scenario fixtures into a throwaway in-memory db, never user input.
	mem := memory.New(db, nil)
	if *rerankerURL != "" {
		mem.SetReranker(&memory.HTTPReranker{URL: *rerankerURL})
	}

	if *locomo != "" {
		// config/embedder wiring is --locomo-only: --file stays FTS-only
		// and config-independent, as it always has been.
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		if cfg.AI.Embeddings.Model != "" {
			mem.SetEmbedder(&memory.OllamaEmbedder{
				BaseURL: cfg.AI.Embeddings.BaseURL,
				Model:   cfg.AI.Embeddings.Model,
				D:       cfg.AI.Embeddings.Dims,
			})
		}
		fmt.Fprintln(os.Stderr, "NOTE: retrieval recall over gold evidence dia_ids, NOT an answer-accuracy LoCoMo score.")
		res, err := membench.RunLoCoMo(ctx, mem, *locomo, *k)
		if err != nil {
			return err
		}
		out, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	recs, err := membench.Load(*file)
	if err != nil {
		return err
	}
	if *report != "" {
		rerankerID := "none"
		if *rerankerURL != "" {
			rerankerID = *rerankerURL
		}
		suiteOpts := membench.SuiteOptions{
			K:          *k,
			Seed:       *seed,
			Commit:     *commit,
			EmbedderID: "none", // --file mode wires no embedder: offline FTS-only, no paid model calls
			RerankerID: rerankerID,
			Fixture:    filepath.Base(*file),
		}
		var rep membench.Report
		if *answers {
			arecs, err := membench.LoadAnswers(*file)
			if err != nil {
				return err
			}
			ao := &membench.AnswerOptions{K: *k, MaxTokens: *answerBudget}
			// Model-backed components are wired only from the explicit
			// endpoint/model/budget flags; the client records nothing to
			// any database (nil store) and the whole stage runs against
			// the throwaway in-memory membench store above.
			explicitClient := func(url, model string) (llm.Client, error) {
				m := llm.NewManager(true, map[string]llm.Profile{
					"default": {BaseURL: url, APIKeyEnv: *judgeKeyEnv, Model: model},
				}, nil, nil)
				return m.Client("default")
			}
			if *composerName == "reflect" {
				client, err := explicitClient(*composerURL, *composerModel)
				if err != nil {
					return fmt.Errorf("membench: composer client: %w", err)
				}
				ao.Composer = membench.NewReflectComposer(reflect.New(mem, client), *ns, *composerURL, *composerModel)
			}
			if *judgeModel != "" {
				client, err := explicitClient(*judgeURL, *judgeModel)
				if err != nil {
					return fmt.Errorf("membench: judge client: %w", err)
				}
				ao.Judge = membench.NewLLMJudge(client, *judgeURL, *answerBudget)
			}
			suiteOpts.Answers = ao
			rep, err = membench.SuiteWithAnswers(ctx, mem, *ns, arecs, suiteOpts)
			if err != nil {
				return err
			}
		} else {
			rep, err = membench.Suite(ctx, mem, *ns, recs, suiteOpts)
			if err != nil {
				return err
			}
		}
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*report, append(out, '\n'), 0o644); err != nil {
			return err
		}
		sum := rep.Runs[0].Summary
		fmt.Printf("report written: %s (%d runs; baseline hit_at_k=%.4f evidence_recall_at_k=%.4f mrr=%.4f)\n",
			*report, len(rep.Runs), sum.HitAtK, sum.EvidenceRecallAtK, sum.MRR)
		for _, r := range rep.Runs {
			if r.Available && r.Reason != "" {
				fmt.Printf("NOTE: %s run degraded: %s\n", r.Name, r.Reason)
			}
		}
		if rep.Answers != nil {
			a := rep.Answers
			fmt.Printf("answers: %d cases (%d answered, %d abstained, %d failed; %d retrieval misses, %d confident-on-unanswerable); exact_match=%s judge_correct=%s citation_existence=%s support=%s; tokens=%d judge_tokens=%d budget=%d\n",
				a.Summary.Queries, a.Summary.Answered, a.Summary.Abstained, a.Summary.Failed,
				a.Summary.RetrievalMisses, a.Summary.ConfidentUnanswerable,
				answerRate(a.Summary.ExactMatchRate), answerRate(a.Summary.JudgeCorrectRate),
				answerRate(a.Summary.CitationExistenceRate), answerRate(a.Summary.SupportRate),
				a.Summary.Tokens, a.Summary.JudgeTokens, a.BudgetTokens)
		}
		return nil
	}
	res, err := membench.Run(ctx, mem, *ns, recs, *k, *rerank)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// answerRate formats an optional answer-stage rate for the CLI summary:
// "n/a" when its denominator was zero, never a vacuous number.
func answerRate(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.4f", *v)
}

// cmdHook runs "punk hook": a hook process reads one hook payload on
// stdin, forwards it to the punk-records server, and (Claude Code only,
// --from empty or "claude") on SessionStart prints the additionalContext
// injection JSON to stdout. --from names the agent that produced stdin's
// payload (e.g. "cursor"); non-Claude agents get their native payload
// translated by hookcli.Normalize before forwarding and never receive
// stdout injection - see hookcli.RunFrom. The server URL comes from
// --url, then PUNK_URL, defaulting to http://localhost:9090; the API key
// comes from PUNK_API_KEY (empty is valid - an unauthenticated server).
// See hookcli.RunFrom for the fail-open contract: this always exits 0,
// since a dead memory server or unrecognized --from must never break the
// user's coding session.
func cmdHook(args []string) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "punk-records base URL (default $PUNK_URL or http://localhost:9090)")
	from := fs.String("from", "", "source agent the stdin payload is native to (default empty = Claude Code passthrough; e.g. \"cursor\")")
	event := fs.String("event", "", "hook event name, required for agents whose native payload doesn't self-identify it (currently only antigravity: PostToolUse, PreInvocation, or Stop - see hookcli.ConnectAntigravity)")
	nsFlag := fs.String("ns", "", "namespace override (written by punk connect --project)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *nsFlag != "" {
		hookcli.SetNamespaceOverride(*nsFlag)
	}
	baseURL, apiKey := hookcli.ResolveServer(*urlFlag)
	// Antigravity CLI's hook payloads carry no field identifying which
	// event fired (see hookcli.RunFromAntigravity's doc comment), so
	// ConnectAntigravity wires a distinct "punk hook --from antigravity
	// --event <Name>" command per event and this dispatch routes to a
	// dedicated entry point that already knows the event, rather than
	// through RunFrom's shared registry dispatch (which only ever reads
	// the event out of the payload itself).
	if strings.EqualFold(*from, "antigravity") {
		return hookcli.RunFromAntigravity(*event, os.Stdin, baseURL, apiKey, os.Stdout, os.Stderr)
	}
	// GitHub Copilot CLI, like Cursor, self-identifies its event from the
	// payload's own hook_event_name field, but needs its own entry point
	// (not RunFrom's generic dispatch) for SessionStart context injection,
	// which uses Copilot's own flat {"additionalContext":...} stdout
	// contract rather than Claude Code's nested one - see
	// hookcli.RunFromCopilot's doc comment for why.
	if strings.EqualFold(*from, "copilot") {
		return hookcli.RunFromCopilot(os.Stdin, baseURL, apiKey, os.Stdout, os.Stderr)
	}
	// Hermes Agent also self-identifies its event from the payload's own
	// hook_event_name field, but needs its own entry point for the same
	// reason Copilot does: its context injection uses Hermes' own flat
	// {"context":...} stdout contract on pre_llm_call, gated to the
	// session's first turn - see hookcli.RunFromHermes' doc comment.
	if strings.EqualFold(*from, "hermes") {
		return hookcli.RunFromHermes(os.Stdin, baseURL, apiKey, os.Stdout, os.Stderr)
	}
	return hookcli.RunFrom(*from, os.Stdin, baseURL, apiKey, os.Stdout, os.Stderr)
}

// cmdConnect wires punk into an agent's hook/extension config. Four
// targets are supported: "claude-code" merges punk's hook entries into a
// Claude Code settings.json (project-local with --project, otherwise the
// user's global ~/.claude/settings.json), preserving any existing user
// entries - see hookcli.ConnectClaudeCode. "cursor" merges punk's hook
// entries into a Cursor hooks.json (see hookcli.ConnectCursor);
// --project switches that file between the user's global
// ~/.cursor/hooks.json and the current directory's ./.cursor/hooks.json,
// mirroring claude-code's convention. "opencode" writes a self-contained
// JS plugin file (see hookcli.ConnectOpenCode and cmdConnectOpenCode's
// own doc comment). "pi" writes a self-contained TypeScript extension
// file for the pi coding agent (see hookcli.ConnectPi and cmdConnectPi's
// own doc comment) - both opencode and pi have no punk-binary-invoking
// hook config to merge; the written file IS the whole integration.
// "antigravity" merges punk's hook entries into an Antigravity CLI
// hooks.json (see hookcli.ConnectAntigravity and cmdConnectAntigravity's
// own doc comment) - a punk-binary-invoking hooks.json merge like
// claude-code/cursor, not a self-contained file like opencode/pi, but
// with a different merge granularity (see ConnectAntigravity's own doc
// comment) and no UserPromptSubmit capture at all (Antigravity's hook
// payloads never carry prompt text - see hookcli's translateAntigravity).
// "copilot" merges punk's hook entries into a GitHub Copilot CLI hooks
// file (see hookcli.ConnectCopilot and cmdConnectCopilot's own doc
// comment) - a punk-binary-invoking merge like claude-code/cursor/
// antigravity, into punk's OWN dedicated file rather than a single shared
// config (Copilot combines every *.json file under its hooks directory on
// its own), with SessionStart context injection like claude-code/
// antigravity (see hookcli.RunFromCopilot), unlike cursor's rules-file
// workaround.
//
// The punk-owned .cursor/rules/punk-memory.mdc rules file (see
// hookcli.WriteCursorRules) is PROJECT-ONLY, unlike hooks.json: Cursor has
// no native support for a user-level ~/.cursor/rules directory - per
// Cursor's docs and forum, rules live either in the Settings -> Rules UI
// or a project's own .cursor/rules/, so a file written to a global
// ~/.cursor/rules/ would silently never be read. Without --project,
// cmdConnectCursor never writes that file; it prints the rules text
// instead, for the user to paste into Cursor Settings -> Rules, or to
// rerun the command with --project inside a repo.
func cmdConnect(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: punk connect claude-code|cursor|opencode|pi|antigravity|copilot|hermes|openclaw|codex [--project] [--url URL]")
	}
	target := args[0]
	switch target {
	case "claude-code":
		return cmdConnectClaudeCode(args[1:])
	case "cursor":
		return cmdConnectCursor(args[1:])
	case "opencode":
		return cmdConnectOpenCode(args[1:])
	case "pi":
		return cmdConnectPi(args[1:])
	case "antigravity":
		return cmdConnectAntigravity(args[1:])
	case "copilot":
		return cmdConnectCopilot(args[1:])
	case "hermes":
		return cmdConnectHermes(args[1:])
	case "openclaw":
		return cmdConnectOpenClaw(args[1:])
	case "codex":
		return cmdConnectCodex(args[1:])
	case "verify":
		return cmdConnectVerify(args[1:])
	default:
		return fmt.Errorf("unknown connect target %q, only \"claude-code\", \"cursor\", \"opencode\", \"pi\", \"antigravity\", \"copilot\", \"hermes\", \"openclaw\", and \"codex\" are supported", target)
	}
}

// cmdConnectVerify proves an MCP session to the server works: connect,
// tools/list, whoami. Standalone or invoked by the connect commands'
// --verify flag. Standalone it cannot inspect any client config, so the
// two sides' pins are independent inputs (--ns for the MCP entry's
// X-Punk-Namespace, --hook-ns for the hook command's --ns) and the
// result is labeled as comparing supplied pins, never as proof about
// what is installed.
func cmdConnectVerify(args []string) error {
	fs := flag.NewFlagSet("connect verify", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	nsFlag := fs.String("ns", "", "namespace pin the MCP entry carries (X-Punk-Namespace), as written by punk connect --project")
	hookNSFlag := fs.String("hook-ns", "", "namespace pin the hook command carries (--ns), as written by punk connect --project")
	if err := fs.Parse(args); err != nil {
		return err
	}
	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)
	sides := verifySides{
		hook: verifySide{ns: *hookNSFlag, known: *hookNSFlag != "", assumed: true, from: "--hook-ns"},
		mcp:  verifySide{ns: *nsFlag, known: true, assumed: true, from: "--ns"},
	}
	if *hookNSFlag == "" {
		sides.hook.from = "no --hook-ns given, and standalone verify cannot inspect the installed hook config"
	}
	return printVerify(context.Background(), serverURL, apiKey, sides)
}

// verifySide is one side of the connection verify compares: the
// namespace pin it carries ("" = unpinned, the namespace derives from
// the hook payload's cwd or the client's advertised roots) and where
// that value came from. known is false when the side's installed config
// could not be inspected (nothing installed, an unreadable file, or a
// client verify cannot inspect) - verify never invents a pin for an
// unknown side. assumed marks a value supplied on the command line
// rather than one read from the installed config, so the result is
// labeled an assumption, not proof. from names the origin: a config
// file, a flag, or - when known is false - the reason the side is
// unknown.
type verifySide struct {
	ns      string
	known   bool
	assumed bool
	from    string
}

// verifySides pairs the hook (capture) side with the MCP (retrieval)
// side printVerify compares.
type verifySides struct{ hook, mcp verifySide }

// pinOrUnpinned renders a side's pin for verify output.
func pinOrUnpinned(ns string) string {
	if ns == "" {
		return "unpinned"
	}
	return ns
}

// connectVerifySides builds the verify sides for a connect command that
// installed the connection itself: each side carries the pin this
// invocation wrote, with the file it was written to as the origin. A
// side skipped by a --no-* flag is left unknown - whatever a previous
// invocation installed was not inspected, so verify must not invent its
// pin.
func connectVerifySides(hookNS, hooksFrom string, noHooks bool, mcpNS, mcpFrom string, noMCP bool) verifySides {
	var s verifySides
	if noHooks {
		s.hook = verifySide{from: "--no-hooks left the hook config untouched and it was not inspected"}
	} else {
		s.hook = verifySide{ns: hookNS, known: true, from: hooksFrom}
	}
	if noMCP {
		s.mcp = verifySide{from: "--no-mcp left the MCP entry untouched and it was not inspected"}
	} else {
		s.mcp = verifySide{ns: mcpNS, known: true, from: mcpFrom}
	}
	return s
}

// codexVerifyScopes enumerates the installed Codex configuration verify
// compares: every applicable hooks.json scope and every config.toml
// scope, highest layer first. The project scope is ALWAYS considered,
// even for a global invocation - native Codex appends hook registrations
// from all applicable config layers (upstream codex-rs discovery loops
// the layers and appends hook events), so a global invocation verified
// inside a repository is still captured by that repository's project
// hooks.json alongside the global one, and a verify that inspected only
// the scope this invocation wrote would claim alignment for captures
// that actually land in another namespace. Scope paths aliasing the same
// file are deduped inside the enumerators.
func codexVerifyScopes(codexHome, punkPath string) ([]hookcli.CodexHookScope, []hookcli.CodexMCPScope) {
	hookPaths := []string{filepath.Join(".codex", "hooks.json"), filepath.Join(codexHome, "hooks.json")}
	mcpPaths := []string{filepath.Join(".codex", "config.toml"), filepath.Join(codexHome, "config.toml")}
	return hookcli.EnumerateCodexHookScopes(punkPath, hookPaths...),
		hookcli.EnumerateCodexMCPScopes(mcpPaths...)
}

// codexAuthProblem compares the EFFECTIVE MCP entry's credential
// identity against the credential this verify probed with. Only names
// are ever reported; an env var's value is compared but never printed,
// and an Authorization header travels as presence only - its value is
// never handled. An identity that cannot be established safely makes
// the alignment unverified instead of proving it through a different
// connection: the entry authenticating from an Authorization header
// (static or env-resolved) whose credential verify cannot compare -
// including one a lower config layer's header fragment contributes to
// the merged entry - , the entry reading its bearer token from a var
// this environment does not carry (inherited from a lower layer just
// the same; Codex REJECTS a configured bearer_token_env_var that is
// missing or empty - upstream codex-rs codex-mcp/src/rmcp_client.rs
// resolve_bearer_token - so a missing var is never "consistent
// unauthenticated", regardless of the probe key), or the two sides
// holding different credentials.
func codexAuthProblem(entry *hookcli.CodexMCPEffective, apiKey string) string {
	if entry.HeaderAuth {
		return fmt.Sprintf("the effective MCP entry authenticates through an Authorization header (static or env-resolved) contributed by %s, whose credential this verify cannot compare safely - the installed connection's identity cannot be established", entry.HeaderAuthFrom)
	}
	if entry.BearerEnv == "" {
		if apiKey != "" {
			return "the effective MCP entry configures no bearer token in any config layer, but this verify probed with an API key - the installed connection would authenticate differently"
		}
		return ""
	}
	v := os.Getenv(entry.BearerEnv)
	switch {
	case v == "":
		return fmt.Sprintf("the effective MCP entry authenticates from $%s (set in %s), which is missing or empty in this environment - Codex rejects a configured bearer variable that is not set, so the installed connection's credential cannot be established", entry.BearerEnv, entry.BearerEnvFrom)
	case apiKey != "" && v == apiKey:
		return "" // the same credential on both sides (compared, never printed)
	case apiKey == "":
		return fmt.Sprintf("the effective MCP entry authenticates from $%s (set in %s), which is set in this environment, but this verify probed without an API key - the installed connection would authenticate differently", entry.BearerEnv, entry.BearerEnvFrom)
	default:
		return fmt.Sprintf("the effective MCP entry authenticates from $%s (set in %s), which holds a different credential than the one this verify probed with - the installed connection's identity cannot be established", entry.BearerEnv, entry.BearerEnvFrom)
	}
}

// printCodexVerify proves an MCP session to the server works and - only
// when every effective side of the installed connection is known AND
// points at this server - that the namespaces the hooks capture into
// and MCP sessions resolve are the same. Every hook scope contributes
// capture destinations (Codex appends hook registrations from all
// applicable config layers, so a global registration fires alongside a
// project one and both capture), and every config.toml scope
// contributes MCP entry FIELDS: Codex recursively merges MCP table
// fields across config layers rather than replacing whole entries
// (upstream config/src/merge.rs:58-132, state.rs:343), so the entry
// compared is the field-wise merge of every contributing scope - an
// enabled = false, http_headers_helper, bearer_token_env_var or
// Authorization-header fragment inherited from a lower layer is part
// of the effective connection, and a top-entry-only view would miss it
// and claim a false alignment. The endpoint plus credential identity
// read back from the installed files are compared against the
// endpoint and credential this verify actually probed: a pin that
// agrees but was installed for another server proves nothing about
// this one, so an endpoint or credential provenance that mismatches or
// cannot be established safely reports the alignment as unverified
// instead. Secrets are never printed - env var names yes, values no. A
// namespace mismatch is reported, not failed: the session works, the
// memories just land where the agent cannot see them.
func printCodexVerify(ctx context.Context, serverURL, apiKey string, hookScopes []hookcli.CodexHookScope, mcpScopes []hookcli.CodexMCPScope) error {
	cwd, _ := os.Getwd()
	probeEndpoint := serverURL + "/mcp?toolset=agent"

	var captures []hookcli.CaptureDestination
	var problems []string
	seenProblem := map[string]bool{}
	addProblem := func(s string) {
		if !seenProblem[s] {
			seenProblem[s] = true
			problems = append(problems, s)
		}
	}

	hookInstalled := false
	var hookFiles []string
	for _, sc := range hookScopes {
		hookFiles = append(hookFiles, sc.File)
		if sc.Err != nil {
			addProblem(fmt.Sprintf("the hook side is unknown (could not inspect %s: %v)", sc.File, sc.Err))
			continue
		}
		if !sc.Installed {
			continue
		}
		hookInstalled = true
		for _, r := range sc.Registrations {
			switch {
			case r.Endpoint == "":
				addProblem(fmt.Sprintf("the punk hook command registered for %s in %s carries no --url, so the server its captures reach cannot be established", r.Event, sc.File))
			case strings.TrimRight(r.Endpoint, "/") != strings.TrimRight(serverURL, "/"):
				addProblem(fmt.Sprintf("the punk hook command registered for %s in %s forwards captures to %s, but this verify probed %s - alignment across different servers is not proven", r.Event, sc.File, r.Endpoint, serverURL))
			case r.Pin != "":
				captures = append(captures, hookcli.CaptureDestination{Namespace: r.Pin, Source: "pin", Origin: sc.File})
			default:
				captures = append(captures, hookcli.CaptureDestination{Source: "path", Origin: sc.File})
			}
		}
	}
	if !hookInstalled && len(problems) == 0 {
		addProblem("the hook side is unknown (no punk-managed hook command found in " + strings.Join(hookFiles, " or ") + ")")
	}

	// The MCP entry retrieval rides is the EFFECTIVE one: the field-wise
	// merge of every contributing config layer (project over global, per
	// upstream Codex merge semantics), never just the top installed
	// entry - fields the top entry lacks inherit from lower layers, and
	// a header fragment in a layer without its own [mcp_servers.punk]
	// table still contributes. A scope that cannot be inspected,
	// fragment fields with no layer defining the table, a cross-layer
	// case-variant pin conflict or no punk config at all make the
	// effective entry unknown.
	merged := hookcli.MergeCodexMCPScopes(mcpScopes)
	if merged.Unknown != "" {
		addProblem("the MCP side is unknown (" + merged.Unknown + ")")
	}
	mcpNS := ""
	if merged.Unknown == "" {
		mcpNS = merged.Pin
		if merged.Disabled {
			addProblem(fmt.Sprintf("the effective MCP entry carries enabled = false from %s; Codex does not open disabled entries, so the installed connection is not a working one and no alignment can be proven through it", merged.DisabledFrom))
		}
		if merged.HeadersHelper {
			addProblem(fmt.Sprintf("the effective MCP entry carries http_headers_helper from %s, whose dynamically resolved headers this verify never executes - the entry's effective namespace pin and credentials cannot be established", merged.HeadersHelperFrom))
		}
		switch {
		case merged.Endpoint == "":
			addProblem(fmt.Sprintf("the effective MCP entry merged from %s carries no url, so the connection Codex actually opens cannot be established", strings.Join(merged.Contributing, " and ")))
		case merged.Endpoint != probeEndpoint:
			addProblem(fmt.Sprintf("the effective MCP entry points at %s (from %s), but this verify probed %s - alignment proven through a different connection is not alignment of the installed one", merged.Endpoint, merged.EndpointFrom, probeEndpoint))
		}
		if reason := codexAuthProblem(&merged, apiKey); reason != "" {
			addProblem(reason)
		}
	}

	al, err := hookcli.CheckNamespaceAlignmentCaptures(ctx, probeEndpoint, apiKey, cwd, captures, mcpNS)
	if err != nil {
		return err
	}
	rep := al.Roots
	instructions := "no"
	if rep.Instructions {
		instructions = "yes"
	}
	fmt.Printf("punk: verified: %d tools, namespace %s (%s), instructions %s\n", len(rep.Tools), rep.Namespace, rep.Source, instructions)
	if al.Mismatch() {
		for _, w := range al.Warnings() {
			fmt.Println("punk: warning - " + w)
		}
	}
	if len(problems) > 0 {
		fmt.Printf("punk: namespace alignment not verified: %s; verify compares only the pins, endpoints and credential identities it inspected or was handed, it never assumes one\n", strings.Join(problems, "; "))
		return nil
	}
	if al.Mismatch() {
		return nil
	}
	fmt.Printf("punk: namespace alignment ok: hooks and MCP sessions both resolve %s\n", al.Capture)
	return nil
}

// printVerify proves an MCP session to the server works and - only when
// both sides of the connection are known - that the namespace the hooks
// capture into for the current directory is the one MCP sessions
// resolve. A side read from the installed client config counts as
// inspected; a side supplied on the command line is labeled an
// assumption; an unknown side makes verify report the alignment as
// unverified rather than inventing a pin. A namespace mismatch is
// reported, not failed: the session works, the memories just land where
// the agent cannot see them.
func printVerify(ctx context.Context, serverURL, apiKey string, sides verifySides) error {
	cwd, _ := os.Getwd()
	hookNS, mcpNS := "", ""
	if sides.hook.known {
		hookNS = sides.hook.ns
	}
	if sides.mcp.known {
		mcpNS = sides.mcp.ns
	}
	al, err := hookcli.CheckNamespaceAlignment(ctx, serverURL+"/mcp?toolset=agent", apiKey, cwd, hookNS, mcpNS)
	if err != nil {
		return err
	}
	rep := al.Roots
	instructions := "no"
	if rep.Instructions {
		instructions = "yes"
	}
	fmt.Printf("punk: verified: %d tools, namespace %s (%s), instructions %s\n", len(rep.Tools), rep.Namespace, rep.Source, instructions)
	var unknown []string
	if !sides.hook.known {
		unknown = append(unknown, "the hook side is unknown ("+sides.hook.from+")")
	}
	if !sides.mcp.known {
		unknown = append(unknown, "the MCP side is unknown ("+sides.mcp.from+")")
	}
	if len(unknown) > 0 {
		fmt.Printf("punk: namespace alignment not verified: %s; verify compares only pins it inspected or was handed, it never assumes one\n", strings.Join(unknown, " and "))
		return nil
	}
	if al.Mismatch() {
		for _, w := range al.Warnings() {
			fmt.Println("punk: warning - " + w)
		}
		return nil
	}
	if sides.hook.assumed || sides.mcp.assumed {
		fmt.Printf("punk: namespace alignment ok for the supplied pins (hook %s via %s, MCP %s via %s); the installed client config was not inspected, so this proves the supplied pins agree, not what is installed\n",
			pinOrUnpinned(sides.hook.ns), sides.hook.from, pinOrUnpinned(sides.mcp.ns), sides.mcp.from)
		return nil
	}
	fmt.Printf("punk: namespace alignment ok: hooks and MCP sessions both resolve %s\n", al.Capture)
	return nil
}

func cmdConnectClaudeCode(args []string) error {
	fs := flag.NewFlagSet("connect claude-code", flag.ContinueOnError)
	project := fs.Bool("project", false, "write ./.claude/settings.json instead of the global ~/.claude/settings.json")
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	noMCP := fs.Bool("no-mcp", false, "only wire hooks; do not register the MCP server or its permission rule")
	force := fs.Bool("force", false, "replace an mcpServers.punk entry punk did not write")
	verify := fs.Bool("verify", false, "after writing config, open an MCP session to the server and call whoami")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	apiKeyEnv := fs.String("api-key-env", "", "write Authorization as Bearer ${NAME} instead of the literal key")
	agentName := fs.String("agent", defaultAgentName(), "identity written into the MCP entry (X-Punk-Agent)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var settingsPath string
	var home string
	if *project {
		settingsPath = filepath.Join(".claude", "settings.json")
	} else {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		settingsPath = filepath.Join(home, ".claude", "settings.json")
	}

	punkPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve punk executable path: %w", err)
	}
	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)

	var projNS string
	if *project {
		ns, src := hookcli.ProjectNamespace(".")
		projNS = ns
		fmt.Printf("punk: namespace %s (from %s)\n", ns, src)
	}

	_, statErr := os.Stat(settingsPath)
	existedBefore := statErr == nil

	var changed bool
	if *project {
		var hookErr error
		changed, hookErr = hookcli.ConnectClaudeCodeNS(settingsPath, punkPath, serverURL, projNS)
		if hookErr != nil {
			return fmt.Errorf("connect claude-code: %w", hookErr)
		}
	} else {
		var hookErr error
		changed, hookErr = hookcli.ConnectClaudeCode(settingsPath, punkPath, serverURL)
		if hookErr != nil {
			return fmt.Errorf("connect claude-code: %w", hookErr)
		}
	}
	if err != nil {
		return fmt.Errorf("connect claude-code: %w", err)
	}
	if changed {
		fmt.Printf("punk: wired Claude Code hooks into %s\n", settingsPath)
		if existedBefore {
			fmt.Println("punk: note - the file was re-formatted (keys sorted, 2-space indent) while merging")
		}
	} else {
		fmt.Printf("punk: %s already has punk's Claude Code hooks up to date\n", settingsPath)
	}
	mcpPath := filepath.Join(".mcp.json")
	if !*project {
		mcpPath = filepath.Join(home, ".claude.json")
	}
	if !*noMCP {
		mcpChanged, err := hookcli.ConnectClaudeCodeMCP(mcpPath,
			hookcli.MCPEntryOpts{ServerURL: serverURL, APIKey: apiKey, APIKeyEnv: *apiKeyEnv, Namespace: projNS, Agent: *agentName}, *force)
		if err != nil {
			return fmt.Errorf("connect claude-code mcp: %w", err)
		}
		if *apiKeyEnv == "" && apiKey != "" {
			fmt.Printf("punk: note - the API key is stored in %s; keep that file private\n", mcpPath)
		}
		permChanged, err := hookcli.EnsureClaudePermission(settingsPath, hookcli.ClaudeMCPRule)
		if err != nil {
			return fmt.Errorf("connect claude-code permission: %w", err)
		}
		fmt.Printf("punk: MCP server entry in %s (%s)\n", mcpPath, changedWord(mcpChanged))
		fmt.Printf("punk: permission %s in %s (%s)\n", hookcli.ClaudeMCPRule, settingsPath, changedWord(permChanged))
		fmt.Println("punk: restart Claude Code or start a new session to pick up the MCP server")
	}
	if *verify {
		if err := printVerify(context.Background(), serverURL, apiKey, connectVerifySides(projNS, settingsPath, false, projNS, mcpPath, *noMCP)); err != nil {
			return err
		}
	}
	if !*noSkill {
		installSkillFor("claude-code", *project, serverURL, projNS)
	}
	fmt.Printf("punk: make sure 'punk serve' is reachable at %s\n", serverURL)
	return nil
}

// defaultAgentName is user@host, the identity claims default to when
// the operator does not name the machine's agent explicitly.
func defaultAgentName() string {
	u, host := "user", "host"
	if cu, err := user.Current(); err == nil && cu.Username != "" {
		u = cu.Username
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		host = h
	}
	return u + "@" + host
}

// installSkillFor writes the punk-memory and punk-plan skills where agent
// loads skills from. A hand-written file at either path is reported and
// left alone; a skill problem never fails the connect that hooks and MCP
// already succeeded at.
func installSkillFor(agent string, project bool, serverURL, ns string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("punk: warning - skill not installed: %v\n", err)
		return
	}
	targets, err := hookcli.AllSkillTargets(agent, project, home, os.Getenv)
	if err != nil {
		fmt.Printf("punk: warning - skill not installed: %v\n", err)
		return
	}
	// Fill the effective options for EVERY target before anything compares
	// or writes: Render(tg) is the byte baseline, and a reconcile pass that
	// ran with blank options would never recognize an installed shared copy
	// rendered with the real ServerURL/Namespace as equivalent.
	for i := range targets {
		targets[i].Opts.ServerURL, targets[i].Opts.Namespace = serverURL, ns
	}
	// Codex discovers both CODEX_HOME/skills and the shared
	// ~/.agents/skills tree, so a global codex install must first be
	// reconciled against whatever the shared root already holds - see
	// hookcli.ReconcileCodexSkillTargets.
	if agent == "codex" && !project {
		var notes []string
		targets, notes = hookcli.ReconcileCodexSkillTargets(targets, home, os.Getenv)
		for _, n := range notes {
			fmt.Printf("punk: note - %s\n", n)
		}
	}
	for _, tg := range targets {
		changed, err := hookcli.WriteSkill(tg.Path, hookcli.Render(tg))
		if err != nil {
			fmt.Printf("punk: warning - %v\n", err)
			continue
		}
		fmt.Printf("punk: skill %s in %s (%s)\n", tg.Name, tg.Path, changedWord(changed))
	}
}

// changedWord renders a change flag for connect summaries.
func changedWord(b bool) string {
	if b {
		return "written"
	}
	return "already up to date"
}

// cmdConnectCursor wires punk into Cursor: a hooks.json merge (six mapped
// events, see hookcli.cursorHookEvents), global or project-local via
// --project like cmdConnectClaudeCode. The rules file
// (.cursor/rules/punk-memory.mdc, see hookcli.WriteCursorRules) is
// PROJECT-ONLY (see cmdConnect's doc comment for why Cursor has no global
// equivalent) - it is only written when --project is set; without
// --project its content is printed instead, for the user to paste into
// Cursor Settings -> Rules.
func cmdConnectCursor(args []string) error {
	fs := flag.NewFlagSet("connect cursor", flag.ContinueOnError)
	project := fs.Bool("project", false, "write ./.cursor/hooks.json (instead of the global ~/.cursor/hooks.json) and ./.cursor/rules/punk-memory.mdc; the rules file is project-only and is only written with this flag")
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	noMCP := fs.Bool("no-mcp", false, "only wire hooks; do not register the MCP server")
	force := fs.Bool("force", false, "replace an mcpServers.punk entry punk did not write")
	verify := fs.Bool("verify", false, "after writing config, open an MCP session to the server and call whoami")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	apiKeyEnv := fs.String("api-key-env", "", "write Authorization as Bearer ${NAME} instead of the literal key")
	agentName := fs.String("agent", defaultAgentName(), "identity written into the MCP entry (X-Punk-Agent)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var cursorDir string
	if *project {
		cursorDir = ".cursor"
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		cursorDir = filepath.Join(home, ".cursor")
	}
	hooksPath := filepath.Join(cursorDir, "hooks.json")

	punkPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve punk executable path: %w", err)
	}
	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)

	var projNS string
	if *project {
		ns, src := hookcli.ProjectNamespace(".")
		projNS = ns
		fmt.Printf("punk: namespace %s (from %s)\n", ns, src)
	}

	_, statErr := os.Stat(hooksPath)
	hooksExistedBefore := statErr == nil

	var hooksChanged bool
	{
		var hookErr error
		if *project {
			hooksChanged, hookErr = hookcli.ConnectCursorNS(hooksPath, punkPath, serverURL, projNS)
		} else {
			hooksChanged, hookErr = hookcli.ConnectCursor(hooksPath, punkPath, serverURL)
		}
		if hookErr != nil {
			return fmt.Errorf("connect cursor: %w", hookErr)
		}
	}
	if hooksChanged {
		fmt.Printf("punk: wired Cursor hooks into %s\n", hooksPath)
		if hooksExistedBefore {
			fmt.Println("punk: note - the file was re-formatted (keys sorted, 2-space indent) while merging")
		}
	} else {
		fmt.Printf("punk: %s already has punk's Cursor hooks up to date\n", hooksPath)
	}

	if *project {
		rulesPath := filepath.Join(cursorDir, "rules", "punk-memory.mdc")
		rulesChanged, err := hookcli.WriteCursorRules(rulesPath, serverURL)
		if err != nil {
			return fmt.Errorf("connect cursor: %w", err)
		}
		if rulesChanged {
			fmt.Printf("punk: wrote Cursor memory rules to %s\n", rulesPath)
		} else {
			fmt.Printf("punk: %s already up to date\n", rulesPath)
		}
	} else {
		// Cursor has no native ~/.cursor/rules support for user-level
		// rules (see cmdConnect's doc comment) - print the same content
		// WriteCursorRules would have written, for the user to paste into
		// Cursor Settings -> Rules, or to rerun with --project inside a
		// repo to have it written automatically.
		fmt.Println("punk: Cursor has no global rules directory (~/.cursor/rules is not read); paste the following into Cursor Settings -> Rules, or rerun with --project inside a repo to write .cursor/rules/punk-memory.mdc automatically:")
		fmt.Println()
		fmt.Print(hookcli.CursorRulesText(serverURL))
		fmt.Println()
	}

	fmt.Printf("punk: make sure 'punk serve' is reachable at %s\n", serverURL)
	mcpPath := filepath.Join(cursorDir, "mcp.json")
	if !*noMCP {
		mcpChanged, err := hookcli.ConnectCursorMCP(mcpPath,
			hookcli.MCPEntryOpts{ServerURL: serverURL, APIKey: apiKey, APIKeyEnv: *apiKeyEnv, Namespace: projNS, Agent: *agentName}, *force)
		if err != nil {
			return fmt.Errorf("connect cursor mcp: %w", err)
		}
		if *apiKeyEnv == "" && apiKey != "" {
			fmt.Printf("punk: note - the API key is stored in %s; keep that file private\n", mcpPath)
		}
		fmt.Printf("punk: MCP server entry in %s (%s)\n", mcpPath, changedWord(mcpChanged))
		fmt.Println("punk: restart Cursor or start a new session to pick up the MCP server")
		if *verify {
			if err := printVerify(context.Background(), serverURL, apiKey, connectVerifySides(projNS, hooksPath, false, projNS, mcpPath, *noMCP)); err != nil {
				return err
			}
		}
		if !*noSkill {
			installSkillFor("cursor", *project, serverURL, projNS)
		}
		return nil
	}
	if *verify {
		if err := printVerify(context.Background(), serverURL, apiKey, connectVerifySides(projNS, hooksPath, false, projNS, mcpPath, *noMCP)); err != nil {
			return err
		}
	}
	if !*noSkill {
		installSkillFor("cursor", *project, serverURL, projNS)
	}
	fmt.Print(cursorMCPRegistrationNote(serverURL))
	return nil
}

// cursorMCPRegistrationNote reminds the operator that hooks/rules alone
// don't give a Cursor agent the recall/remember MCP tools the rules file
// tells it to call - the punk-records MCP server also needs registering
// in Cursor's own mcp.json, separate from the hooks.json this command
// writes. The snippet's shape (mcpServers.punk-records.url) matches the
// Cursor tab already documented on the project site's /connect section.
// The trailing line about a "headers" entry covers the day punk serve
// starts requiring API keys: Cursor's mcp.json accepts a per-server
// "headers" map alongside "url" for exactly this (an Authorization Bearer
// header), same shape as any other remote MCP client - it is not a
// punk-specific extension.
func cursorMCPRegistrationNote(serverURL string) string {
	return fmt.Sprintf(`punk: for recall/remember to work, also register punk-records as an MCP server in Cursor (~/.cursor/mcp.json):
{
  "mcpServers": {
    "punk-records": { "url": %q }
  }
}
punk: once an API key exists (punk apikey create --name ...), add a "headers": { "Authorization": "Bearer <key>" } entry alongside "url" above - Cursor's mcp.json has no separate API-key field, so the Bearer header is the only way to authenticate.
`, serverURL+"/mcp")
}

// cmdConnectOpenCode wires punk into OpenCode by installing a JS plugin
// file that talks to the punk-records server directly over HTTP at
// runtime (see hookcli.ConnectOpenCode) - unlike claude-code/cursor,
// OpenCode has no punk-binary-invoking hook config to merge and no
// separate MCP-registration step: the plugin file IS the whole
// integration, self-contained, reading its own env at runtime.
//
// Plugin directories - project-local .opencode/plugins/ or global
// ~/.config/opencode/plugins/, switched by --project exactly like
// cmdConnectClaudeCode/cmdConnectCursor's --project - come from
// https://opencode.ai/docs/plugins/ ("Use a plugin > From local files")
// and https://opencode.ai/docs/config/, which documents the plural
// "plugins/" as the current directory name (a singular "plugin/" is also
// accepted, for backwards compatibility only, per that same page) - punk
// always writes the plural, current form.
// openCodePaths returns the plugin file and the config file punk connect
// opencode writes: project-local under ./.opencode and ./opencode.json,
// else under $XDG_CONFIG_HOME/opencode (default ~/.config/opencode).
// OpenCode reads opencode.json from its config directory (verified with
// `opencode mcp list`), not from ~/.config itself - v1.3.1 wrote the
// global MCP entry one directory too high.
func openCodePaths(project bool, xdgConfigHome, home string) (pluginPath, configPath string) {
	if project {
		return filepath.Join(".opencode", "plugins", "punk-memory.js"), "opencode.json"
	}
	base := xdgConfigHome
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "opencode")
	return filepath.Join(dir, "plugins", "punk-memory.js"), filepath.Join(dir, "opencode.json")
}

func cmdConnectOpenCode(args []string) error {
	fs := flag.NewFlagSet("connect opencode", flag.ContinueOnError)
	project := fs.Bool("project", false, "write ./.opencode/plugins/punk-memory.js and ./opencode.json instead of the global ~/.config/opencode equivalents")
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	noMCP := fs.Bool("no-mcp", false, "only install the plugin; do not register the MCP server")
	force := fs.Bool("force", false, "replace an mcp.punk entry punk did not write")
	verify := fs.Bool("verify", false, "after writing config, open an MCP session to the server and call whoami")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	apiKeyEnv := fs.String("api-key-env", "", "write Authorization as Bearer ${NAME} instead of the literal key")
	agentName := fs.String("agent", defaultAgentName(), "identity written into the MCP entry (X-Punk-Agent)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	pluginPath, configPath := openCodePaths(*project, os.Getenv("XDG_CONFIG_HOME"), home)

	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)

	changed, err := hookcli.ConnectOpenCode(pluginPath, serverURL)
	if err != nil {
		return fmt.Errorf("connect opencode: %w", err)
	}
	if changed {
		fmt.Printf("punk: wrote OpenCode plugin to %s\n", pluginPath)
	} else {
		fmt.Printf("punk: %s already up to date\n", pluginPath)
	}
	if !*noMCP {
		mcpPath := configPath
		mcpChanged, err := hookcli.ConnectOpenCodeMCP(mcpPath,
			hookcli.MCPEntryOpts{ServerURL: serverURL, APIKey: apiKey, APIKeyEnv: *apiKeyEnv, Agent: *agentName}, *force)
		if err != nil {
			return fmt.Errorf("connect opencode mcp: %w", err)
		}
		if *apiKeyEnv == "" && apiKey != "" {
			fmt.Printf("punk: note - the API key is stored in %s; keep that file private\n", mcpPath)
		}
		fmt.Printf("punk: MCP server entry in %s (%s)\n", mcpPath, changedWord(mcpChanged))
		fmt.Println("punk: restart OpenCode or reload plugins to pick up the MCP server")
	}
	if *verify {
		if err := printVerify(context.Background(), serverURL, apiKey, connectVerifySides("", pluginPath, false, "", configPath, *noMCP)); err != nil {
			return err
		}
	}
	fmt.Printf("punk: note - the plugin reads PUNK_URL and PUNK_API_KEY from its own process environment at runtime (falling back to %s when PUNK_URL is unset); restart OpenCode or reload plugins to pick up this file\n", serverURL)
	if !*noSkill {
		installSkillFor("opencode", *project, serverURL, "")
	}
	fmt.Printf("punk: make sure 'punk serve' is reachable at %s\n", serverURL)
	return nil
}

// cmdConnectPi wires punk into the pi coding agent
// (github.com/earendil-works/pi, pi.dev) by installing a TypeScript
// extension file that talks to the punk-records server directly over HTTP
// at runtime (see hookcli.ConnectPi) - like OpenCode, there is no punk
// binary in the runtime path here: the extension file IS the whole
// integration, self-contained, reading its own env at runtime.
//
// Extension locations - project-local .pi/extensions/ or global
// ~/.pi/agent/extensions/, switched by --project exactly like
// cmdConnectClaudeCode/cmdConnectCursor/cmdConnectOpenCode's --project -
// come from https://raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/extensions.md
// ("Put extensions in ~/.pi/agent/extensions/ (global) or
// .pi/extensions/ (project-local) for auto-discovery."). Extensions
// auto-discover "*.ts" files directly in either directory (or
// "*/index.ts" in a subdirectory), so a plain punk-memory.ts file is
// enough - no index.ts subdirectory needed.
func cmdConnectPi(args []string) error {
	fs := flag.NewFlagSet("connect pi", flag.ContinueOnError)
	project := fs.Bool("project", false, "write ./.pi/extensions/punk-memory.ts instead of the global ~/.pi/agent/extensions/punk-memory.ts; bakes the remote-derived namespace into the extension")
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	verify := fs.Bool("verify", false, "after writing the extension, call the server's /v1/agent/namespace with this machine's credentials (pi tools use the HTTP API, not MCP)")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var extensionPath string
	if *project {
		extensionPath = filepath.Join(".pi", "extensions", "punk-memory.ts")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		extensionPath = filepath.Join(home, ".pi", "agent", "extensions", "punk-memory.ts")
	}

	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)

	var piOpts hookcli.PiOpts
	var piNS string
	if *project {
		ns, nsrc := hookcli.ProjectNamespace(".")
		piOpts = hookcli.PiOpts{Namespace: ns}
		piNS = ns
		fmt.Printf("punk: namespace %s (from %s)\n", ns, nsrc)
	}

	changed, err := hookcli.ConnectPi(extensionPath, serverURL, piOpts)
	if err != nil {
		return fmt.Errorf("connect pi: %w", err)
	}
	if changed {
		fmt.Printf("punk: wrote pi extension to %s\n", extensionPath)
	} else {
		fmt.Printf("punk: %s already up to date\n", extensionPath)
	}
	if *project {
		fmt.Println("punk: note - pi only auto-discovers project-local .pi/extensions/ once the project itself is trusted (pi's own project_trust prompt/flow); an untrusted project will not load this file")
	}
	if *verify {
		cwd, _ := os.Getwd()
		ns, err := hookcli.VerifyHTTP(context.Background(), serverURL, apiKey, cwd)
		if err != nil {
			return err
		}
		fmt.Printf("punk: verified: HTTP API reachable, namespace %s\n", ns)
	}
	fmt.Printf("punk: note - the extension reads PUNK_URL and PUNK_API_KEY from its own process environment at runtime (falling back to %s when PUNK_URL is unset); restart pi or run /reload to pick up this file\n", serverURL)
	if !*noSkill {
		installSkillFor("pi", *project, serverURL, piNS)
	}
	fmt.Printf("punk: make sure 'punk serve' is reachable at %s\n", serverURL)
	return nil
}

// cmdConnectAntigravity wires punk into Antigravity CLI (antigravity.google,
// Google's successor to Gemini CLI - Gemini CLI was discontinued June 2026
// with its hook functionality carried over) by merging a "punk" hook-name
// entry into a hooks.json (see hookcli.ConnectAntigravity), global or
// project-local via --project like cmdConnectClaudeCode/cmdConnectCursor.
//
// Config file locations - project-local .agents/hooks.json or global
// ~/.gemini/config/hooks.json - and the full hook event/payload contract
// come from antigravity.google/docs/hooks (fetched directly, section by
// section: Configuration, Schema and File Format, Supported Events,
// Common Input Fields, and each of PreToolUse/PostToolUse/PreInvocation/
// PostInvocation/Stop's own Input/Output Fields tables and examples). This
// is a fully documented current contract (decision ladder rung A: docs
// give the config file and payload shape directly), not a Gemini-CLI-
// legacy-only inference.
//
// Three of Antigravity's five documented events are wired: PostToolUse
// (observational capture), PreInvocation (doubles as session-start capture
// AND once-per-conversation context injection, gated to invocationNum==0
// - see translateAntigravity in normalize.go), and Stop (terminal capture,
// with its own required-decision reply - see RunFromAntigravity in
// hookcli.go). PreToolUse (permission gating - out of scope for a
// passive capture feature, see antigravityGroupEvents' doc comment in
// connect_antigravity.go) and PostInvocation (identical input schema to
// PreInvocation, no capturable signal of its own - see
// antigravityFlatEvents' doc comment) are deliberately never wired.
//
// Antigravity's hook payloads carry no prompt text or assistant response
// text anywhere in their documented schema (see translateAntigravity's own
// doc comment for the field-by-field check) - there is no UserPromptSubmit
// capture for Antigravity at all, unlike every other agent this command
// supports. This command still connects the three events that ARE
// capturable rather than declining ladder rung A wholesale over that one
// gap.
func cmdConnectAntigravity(args []string) error {
	fs := flag.NewFlagSet("connect antigravity", flag.ContinueOnError)
	project := fs.Bool("project", false, "write ./.agents/hooks.json and ./.agents/mcp_config.json instead of the global ~/.gemini/config equivalents")
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	noMCP := fs.Bool("no-mcp", false, "only wire hooks; do not register the MCP server")
	force := fs.Bool("force", false, "replace an mcpServers.punk entry punk did not write")
	apiKeyEnv := fs.String("api-key-env", "", "write Authorization as Bearer ${NAME} instead of the literal key")
	agentName := fs.String("agent", defaultAgentName(), "identity written into the MCP entry (X-Punk-Agent)")
	verify := fs.Bool("verify", false, "after writing config, open an MCP session to the server and call whoami")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var hooksPath string
	if *project {
		hooksPath = filepath.Join(".agents", "hooks.json")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		hooksPath = filepath.Join(home, ".gemini", "config", "hooks.json")
	}

	punkPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve punk executable path: %w", err)
	}
	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)

	var projNS string
	if *project {
		ns, nsrc := hookcli.ProjectNamespace(".")
		projNS = ns
		fmt.Printf("punk: namespace %s (from %s)\n", ns, nsrc)
	}

	_, statErr := os.Stat(hooksPath)
	existedBefore := statErr == nil

	changed, err := hookcli.ConnectAntigravity(hooksPath, punkPath, serverURL)
	if err != nil {
		return fmt.Errorf("connect antigravity: %w", err)
	}
	if changed {
		fmt.Printf("punk: wired Antigravity CLI hooks into %s\n", hooksPath)
		if existedBefore {
			fmt.Println("punk: note - the file was re-formatted (keys sorted, 2-space indent) while merging")
		}
	} else {
		fmt.Printf("punk: %s already has punk's Antigravity hooks up to date\n", hooksPath)
	}
	fmt.Println("punk: note - PreToolUse (permission gating) and PostInvocation are deliberately not wired; punk only observes PostToolUse, PreInvocation (session start + once-per-conversation context injection), and Stop")
	fmt.Println("punk: note - Antigravity's own hook payloads carry no prompt text, so there is no UserPromptSubmit capture for Antigravity")
	mcpPath := ""
	if *project {
		mcpPath = filepath.Join(".agents", "mcp_config.json")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		mcpPath = filepath.Join(home, ".gemini", "config", "mcp_config.json")
	}
	if !*noMCP {
		mcpChanged, err := hookcli.ConnectAntigravityMCP(mcpPath,
			hookcli.MCPEntryOpts{ServerURL: serverURL, APIKey: apiKey, APIKeyEnv: *apiKeyEnv, Namespace: projNS, Agent: *agentName}, *force)
		if err != nil {
			return fmt.Errorf("connect antigravity mcp: %w", err)
		}
		if *apiKeyEnv == "" && apiKey != "" {
			fmt.Printf("punk: note - the API key is stored in %s; keep that file private\n", mcpPath)
		}
		fmt.Printf("punk: MCP server entry in %s (%s)\n", mcpPath, changedWord(mcpChanged))
	}
	if *verify {
		// ConnectAntigravity has no namespace-pin variant: the hooks are
		// always unpinned, so only the MCP side carries projNS.
		if err := printVerify(context.Background(), serverURL, apiKey, connectVerifySides("", hooksPath, false, projNS, mcpPath, *noMCP)); err != nil {
			return err
		}
	}
	if !*noSkill {
		installSkillFor("antigravity", *project, serverURL, projNS)
	}
	fmt.Printf("punk: make sure 'punk serve' is reachable at %s\n", serverURL)
	return nil
}

// cmdConnectCopilot wires punk into GitHub Copilot CLI by writing punk's
// own hooks file (see hookcli.ConnectCopilot), global or project-local via
// --project like cmdConnectClaudeCode/cmdConnectCursor/
// cmdConnectAntigravity.
//
// Config file locations and the full hook event/payload contract come from
// docs.github.com/en/copilot/reference/hooks-reference and
// docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/
// use-hooks (both fetched directly). Repository-level hook files live at
// .github/hooks/*.json in the repository root; use-hooks says these are
// committed to the repo and take effect for anyone with it checked out
// once merged - it does not say anything about "current working
// directory". User-level hook files live in ~/.copilot/hooks/ on
// macOS/Linux (or $COPILOT_HOME/hooks/ when COPILOT_HOME is set - honored
// below) or %USERPROFILE%\.copilot\hooks\ on Windows. Every *.json file
// under a hooks directory is loaded and combined, so punk writes its own
// "punk.json" rather than merging into a single shared file the way
// claude-code/cursor/antigravity do (see hookcli.ConnectCopilot's own doc
// comment). This is a fully documented current contract (decision ladder
// rung A), not a preview/inference.
//
// Caveat: --project below resolves hooksPath as a path relative to the
// process's current working directory (.github/hooks/punk.json), the
// same cwd assumption cmdConnectClaudeCode/cmdConnectCursor make for
// their own --project paths. Run this from somewhere other than the repo
// root and it writes a .github/hooks/punk.json under the wrong directory
// - one Copilot CLI, reading from the actual repository root, will never
// load.
//
// Five of Copilot's documented events are wired (see
// hookcli.copilotHookEvents): SessionStart, UserPromptSubmit, PostToolUse,
// Stop, and SessionEnd - registered under their PascalCase spelling, which
// per the docs selects the snake_case payload shape ("VS Code compatible
// format") translateCopilot decodes. PreToolUse/permissionRequest
// (permission gating - out of scope for a passive capture feature, same
// reasoning as Antigravity's PreToolUse omission) and preCompact/
// notification/subagentStart/subagentStop/postToolUseFailure (no Claude
// Code hook equivalent for this package's existing four-event capture set)
// are deliberately never wired.
//
// Unlike Cursor, Copilot's own sessionStart hook supports optional
// additionalContext injection directly (docs' Hook events table:
// "sessionStart ... Optional - can inject additionalContext into the
// session"), so - like claude-code and antigravity, and unlike cursor -
// there is no separate rules-file workaround here; hookcli.RunFromCopilot
// (wired via cmdHook's --from copilot) handles injection natively.
func cmdConnectCopilot(args []string) error {
	fs := flag.NewFlagSet("connect copilot", flag.ContinueOnError)
	project := fs.Bool("project", false, "write ./.github/hooks/punk.json instead of the global ~/.copilot/hooks/punk.json (or $COPILOT_HOME/hooks/punk.json when COPILOT_HOME is set)")
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	noMCP := fs.Bool("no-mcp", false, "only wire hooks; do not register the MCP server")
	force := fs.Bool("force", false, "replace an mcpServers.punk entry punk did not write")
	apiKeyEnv := fs.String("api-key-env", "", "write Authorization as Bearer ${NAME} instead of the literal key")
	agentName := fs.String("agent", defaultAgentName(), "identity written into the MCP entry (X-Punk-Agent)")
	verify := fs.Bool("verify", false, "after writing config, open an MCP session to the server and call whoami")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var hooksPath string
	if *project {
		hooksPath = filepath.Join(".github", "hooks", "punk.json")
	} else if copilotHome := os.Getenv("COPILOT_HOME"); copilotHome != "" {
		hooksPath = filepath.Join(copilotHome, "hooks", "punk.json")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		hooksPath = filepath.Join(home, ".copilot", "hooks", "punk.json")
	}

	punkPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve punk executable path: %w", err)
	}
	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)

	_, statErr := os.Stat(hooksPath)
	existedBefore := statErr == nil

	changed, err := hookcli.ConnectCopilot(hooksPath, punkPath, serverURL)
	if err != nil {
		return fmt.Errorf("connect copilot: %w", err)
	}
	if changed {
		fmt.Printf("punk: wired GitHub Copilot CLI hooks into %s\n", hooksPath)
		if existedBefore {
			fmt.Println("punk: note - the file was re-formatted (keys sorted, 2-space indent) while merging")
		}
	} else {
		fmt.Printf("punk: %s already has punk's Copilot hooks up to date\n", hooksPath)
	}
	fmt.Println("punk: note - restart Copilot CLI (hook configuration is loaded when the CLI starts) to pick up this file")
	mcpPath := ""
	if copilotHome := os.Getenv("COPILOT_HOME"); copilotHome != "" {
		mcpPath = filepath.Join(copilotHome, "mcp-config.json")
	} else if *project {
		mcpPath = filepath.Join(".copilot", "mcp-config.json")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		mcpPath = filepath.Join(home, ".copilot", "mcp-config.json")
	}
	if !*noMCP {
		mcpChanged, err := hookcli.ConnectCopilotMCP(mcpPath,
			hookcli.MCPEntryOpts{ServerURL: serverURL, APIKey: apiKey, APIKeyEnv: *apiKeyEnv, Agent: *agentName}, *force)
		if err != nil {
			return fmt.Errorf("connect copilot mcp: %w", err)
		}
		if *apiKeyEnv == "" && apiKey != "" {
			fmt.Printf("punk: note - the API key is stored in %s; keep that file private\n", mcpPath)
		}
		fmt.Printf("punk: MCP server entry in %s (%s)\n", mcpPath, changedWord(mcpChanged))
	}
	if *verify {
		if err := printVerify(context.Background(), serverURL, apiKey, connectVerifySides("", hooksPath, false, "", mcpPath, *noMCP)); err != nil {
			return err
		}
	}
	if !*noSkill {
		installSkillFor("copilot", *project, serverURL, "")
	}
	fmt.Printf("punk: make sure 'punk serve' is reachable at %s\n", serverURL)
	return nil
}

// cmdConnectHermes wires punk into Hermes Agent (NousResearch) by merging
// punk hook entries into its config.yaml (see hookcli.ConnectHermes).
//
// Config location and the whole hook contract come from
// hermes-agent.nousresearch.com/docs/user-guide/features/hooks (fetched
// directly). Hermes documents four separate hook systems; only SHELL hooks
// spawn a subprocess per event with the payload on stdin, which is the only
// kind a punk binary can be wired into, and they are configured in
// ~/.hermes/config.yaml under a top-level "hooks" mapping keyed by event
// name. There is no project-local equivalent documented, so unlike
// claude-code/cursor/antigravity/copilot this command has no --project
// flag; --config points it at a non-default file instead.
//
// Four events are wired (see hookcli.hermesHookEvents): on_session_start,
// pre_llm_call, post_tool_call and post_llm_call. on_session_end and
// pre_tool_call are deliberately not wired - see hookcli's translateHermes
// doc comment for both reasons.
//
// Hermes' payload is the closest of any supported agent to Claude Code's
// own (hook_event_name, session_id, cwd, tool_name and tool_input are all
// top-level fields with the same meaning), and pre_llm_call accepts a
// {"context": "..."} reply on stdout that Hermes appends to the user
// message - so, like claude-code/antigravity/copilot and unlike cursor,
// context injection is native here with no rules-file workaround.
func cmdConnectHermes(args []string) error {
	fs := flag.NewFlagSet("connect hermes", flag.ContinueOnError)
	configPath := fs.String("config", "", "Hermes config.yaml to merge into (default ~/.hermes/config.yaml)")
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	noMCP := fs.Bool("no-mcp", false, "only wire hooks; do not register the MCP server")
	force := fs.Bool("force", false, "replace an mcp_servers.punk entry punk did not write")
	apiKeyEnv := fs.String("api-key-env", "", "write Authorization as Bearer ${NAME} instead of the literal key")
	agentName := fs.String("agent", defaultAgentName(), "identity written into the MCP entry (X-Punk-Agent)")
	verify := fs.Bool("verify", false, "after writing config, open an MCP session to the server and call whoami")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *configPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		path = filepath.Join(home, ".hermes", "config.yaml")
	}

	punkPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve punk executable path: %w", err)
	}
	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)

	_, statErr := os.Stat(path)
	existedBefore := statErr == nil

	changed, err := hookcli.ConnectHermes(path, punkPath, serverURL)
	if err != nil {
		return fmt.Errorf("connect hermes: %w", err)
	}
	if changed {
		fmt.Printf("punk: wired Hermes Agent shell hooks into %s\n", path)
		if existedBefore {
			fmt.Println("punk: note - the file was re-encoded (2-space indent); comments and key order are preserved, other whitespace is not")
		}
	} else {
		fmt.Printf("punk: %s already has punk's Hermes hooks up to date\n", path)
	}
	// hooks_auto_accept is Hermes' own consent switch and punk deliberately
	// never sets it: flipping a user's consent default from a connect
	// command is not punk's call to make.
	fmt.Println("punk: note - Hermes prompts for consent the first time a shell hook runs, unless hooks_auto_accept is true in the same config")
	fmt.Println("punk: note - restart Hermes (hooks are registered at CLI and gateway startup) to pick up these entries")
	if !*noMCP {
		mcpChanged, err := hookcli.ConnectHermesMCP(path,
			hookcli.MCPEntryOpts{ServerURL: serverURL, APIKey: apiKey, APIKeyEnv: *apiKeyEnv, Agent: *agentName}, *force)
		if err != nil {
			return fmt.Errorf("connect hermes mcp: %w", err)
		}
		if *apiKeyEnv == "" && apiKey != "" {
			fmt.Printf("punk: note - the API key is stored in %s; keep that file private\n", path)
		}
		fmt.Printf("punk: MCP server entry in %s (%s)\n", path, changedWord(mcpChanged))
	}
	if *verify {
		if err := printVerify(context.Background(), serverURL, apiKey, connectVerifySides("", path, false, "", path, *noMCP)); err != nil {
			return err
		}
	}
	if !*noSkill {
		installSkillFor("hermes", false, serverURL, "")
	}
	fmt.Printf("punk: make sure 'punk serve' is reachable at %s\n", serverURL)
	return nil
}

// cmdConnectOpenClaw wires punk into OpenClaw by writing punk's own plugin
// (see hookcli.WriteOpenClawPlugin) and enabling it in OpenClaw's
// config.json (see hookcli.ConnectOpenClaw).
//
// Paths and the plugin/hook contract come from docs.openclaw.ai/plugins/
// hooks and docs.openclaw.ai/tools/plugin (both fetched directly). Plugins
// live in a directory under the plugins root holding package.json plus the
// entry file its "openclaw.pluginEntry" names; config.json holds the
// plugins.entries map that enables them. OpenClaw's docs describe both a
// global and a workspace plugin root but state that workspace-origin
// plugins are disabled by default, so this writes to the global root only
// (--home relocates the whole ~/.openclaw tree for a non-default install).
//
// Unlike claude-code/cursor/antigravity/copilot/hermes, no punk binary is
// invoked at runtime: OpenClaw plugins are in-process JavaScript, so the
// written plugin talks to the punk server over HTTP itself, exactly like
// the opencode and pi integrations.
func cmdConnectOpenClaw(args []string) error {
	fs := flag.NewFlagSet("connect openclaw", flag.ContinueOnError)
	homeFlag := fs.String("home", "", "OpenClaw home directory (default ~/.openclaw)")
	urlFlag := fs.String("url", "", "punk-records server URL (default $PUNK_URL, the credentials file from 'punk login', or http://localhost:9090)")
	noMCP := fs.Bool("no-mcp", false, "only wire the plugin; do not register the MCP server")
	force := fs.Bool("force", false, "replace an mcp.servers.punk entry punk did not write")
	apiKeyEnv := fs.String("api-key-env", "", "write Authorization as Bearer ${NAME} instead of the literal key")
	agentName := fs.String("agent", defaultAgentName(), "identity written into the MCP entry (X-Punk-Agent)")
	verify := fs.Bool("verify", false, "after writing config, open an MCP session to the server and call whoami")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root := *homeFlag
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		root = filepath.Join(home, ".openclaw")
	}
	pluginDir := filepath.Join(root, "plugins", hookcli.OpenClawPluginID)
	configPath := filepath.Join(root, "config.json")
	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)

	pluginChanged, err := hookcli.WriteOpenClawPlugin(pluginDir, serverURL)
	if err != nil {
		return fmt.Errorf("connect openclaw: %w", err)
	}
	if pluginChanged {
		fmt.Printf("punk: wrote OpenClaw plugin to %s\n", pluginDir)
	} else {
		fmt.Printf("punk: %s already has punk's OpenClaw plugin up to date\n", pluginDir)
	}

	configChanged, err := hookcli.ConnectOpenClaw(configPath)
	if err != nil {
		return fmt.Errorf("connect openclaw: %w", err)
	}
	if configChanged {
		fmt.Printf("punk: enabled plugin %q in %s\n", hookcli.OpenClawPluginID, configPath)
		// Both flags are what make the plugin's one non-observational hook
		// work at all (OpenClaw gates conversation reads and prompt
		// mutation separately), so each is named rather than folded into a
		// vague "configured" line.
		fmt.Println("punk: set plugins.entries." + hookcli.OpenClawPluginID + ".hooks.allowConversationAccess = true (lets the plugin read the prompt it captures)")
		fmt.Println("punk: set plugins.entries." + hookcli.OpenClawPluginID + ".hooks.allowPromptInjection = true (lets it prepend recalled memory)")
	} else {
		fmt.Printf("punk: %s already enables punk's plugin\n", configPath)
	}
	fmt.Println("punk: note - restart the OpenClaw gateway (plugins load at startup) to pick this up")
	if !*noMCP {
		mcpChanged, err := hookcli.ConnectOpenClawMCP(configPath,
			hookcli.MCPEntryOpts{ServerURL: serverURL, APIKey: apiKey, APIKeyEnv: *apiKeyEnv, Agent: *agentName}, *force)
		if err != nil {
			return fmt.Errorf("connect openclaw mcp: %w", err)
		}
		if *apiKeyEnv == "" && apiKey != "" {
			fmt.Printf("punk: note - the API key is stored in %s; keep that file private\n", configPath)
		}
		fmt.Printf("punk: MCP server entry in %s (%s)\n", configPath, changedWord(mcpChanged))
	}
	if *verify {
		if err := printVerify(context.Background(), serverURL, apiKey, connectVerifySides("", pluginDir, false, "", configPath, *noMCP)); err != nil {
			return err
		}
	}
	if !*noSkill {
		installSkillFor("openclaw", false, serverURL, "")
	}
	fmt.Printf("punk: make sure 'punk serve' is reachable at %s\n", serverURL)
	return nil
}

// cmdConnectCodex wires punk into the Codex CLI: capture hooks into
// hooks.json (a Claude-shaped merge; Codex's payloads share Claude
// Code's field names so hookcli.Run is a passthrough) and the punk MCP
// entry plus the hooks feature flag into config.toml (a marker-fenced
// text block; punk has no TOML dependency). Both default to $CODEX_HOME
// (~/.codex); --project writes ./.codex/ instead and derives the
// namespace from the git remote, baked into both the hook command and
// the MCP entry's X-Punk-Namespace header.
func cmdConnectCodex(args []string) error {
	fs := flag.NewFlagSet("connect codex", flag.ContinueOnError)
	project := fs.Bool("project", false, "write ./.codex/hooks.json and ./.codex/config.toml instead of the global $CODEX_HOME (~/.codex) files; also derives the namespace from the git remote")
	urlFlag := fs.String("url", "", "punk-records server URL (default: PUNK_URL, then ~/.punk/credentials.json, then http://localhost:9090)")
	noMCP := fs.Bool("no-mcp", false, "only wire hooks; do not write the MCP entry or the hooks feature flag")
	noHooks := fs.Bool("no-hooks", false, "only write the MCP entry; do not wire capture hooks")
	force := fs.Bool("force", false, "replace an [mcp_servers.punk] table punk did not write")
	apiKeyEnv := fs.String("api-key-env", "", "name of the env var Codex reads the bearer token from (default PUNK_API_KEY when a key is configured)")
	agent := fs.String("agent", defaultAgentName(), "identity written into the MCP entry (X-Punk-Agent)")
	verify := fs.Bool("verify", false, "after writing config, open an MCP session to the server and call whoami")
	noSkill := fs.Bool("no-skill", false, "do not install the punk-memory skill")
	if err := fs.Parse(args); err != nil {
		return err
	}
	serverURL, apiKey := hookcli.ResolveServer(*urlFlag)
	punkPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve punk executable path: %w", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	dir := codexHome
	ns := ""
	if *project {
		dir = ".codex"
		var src string
		ns, src = hookcli.ProjectNamespace(".")
		fmt.Printf("punk: namespace %s (from %s)\n", ns, src)
	}
	hooksPath := filepath.Join(dir, "hooks.json")
	configPath := filepath.Join(dir, "config.toml")

	if !*noHooks {
		changed, err := hookcli.ConnectCodexHooks(hooksPath, punkPath, serverURL, ns)
		if err != nil {
			return fmt.Errorf("connect codex hooks: %w", err)
		}
		fmt.Printf("punk: Codex hooks in %s (%s)\n", hooksPath, changedWord(changed))
		if *project {
			// Codex merges global and project hook scopes; a punk group
			// identical to the global one would fire every event twice.
			notes, _, err := hookcli.DedupeCodexHookScopes(filepath.Join(codexHome, "hooks.json"), hooksPath, punkPath)
			if err != nil {
				return fmt.Errorf("reconcile codex hook scopes: %w", err)
			}
			for _, n := range notes {
				fmt.Printf("punk: note - %s\n", n)
			}
		}
	}
	if !*noMCP {
		opts := hookcli.MCPEntryOpts{ServerURL: serverURL, APIKey: apiKey, APIKeyEnv: *apiKeyEnv, Namespace: ns, Agent: *agent}
		changed, err := hookcli.ConnectCodexConfig(configPath, opts, !*noHooks, *force)
		if err != nil {
			return fmt.Errorf("connect codex config: %w", err)
		}
		fmt.Printf("punk: MCP entry%s in %s (%s)\n", map[bool]string{true: " and hooks feature flag", false: ""}[!*noHooks], configPath, changedWord(changed))
		if aliases, err := hookcli.DetectCodexMCPAliases(configPath, serverURL); err == nil && len(aliases) > 1 {
			fmt.Printf("punk: warning - %s registers more than one MCP alias for the same punk server (%s); codex will connect twice - remove the extra [mcp_servers.*] table by hand\n", configPath, strings.Join(aliases, ", "))
		}
		if apiKey != "" || *apiKeyEnv != "" {
			envName := *apiKeyEnv
			if envName == "" {
				envName = "PUNK_API_KEY"
			}
			fmt.Printf("punk: note - Codex reads the bearer token from $%s; export it in the shell that starts codex\n", envName)
		}
	}
	if !*noHooks {
		fmt.Println("punk: note - Codex asks once to trust the punk hook command; accept it, or hooks stay disabled")
	}
	if *verify {
		hookScopes, mcpScopes := codexVerifyScopes(codexHome, punkPath)
		if err := printCodexVerify(context.Background(), serverURL, apiKey, hookScopes, mcpScopes); err != nil {
			return err
		}
	}
	if !*noSkill {
		installSkillFor("codex", *project, serverURL, ns)
	}
	fmt.Println("punk: restart codex to pick up the changes")
	return nil
}

// cmdSkill installs, prints or locates the canonical punk-memory and
// punk-plan skills for one agent (see hookcli.AllSkillTargets for the
// per-agent locations and hookcli.Render for the text). install is
// marker-gated: a file at the target that punk did not write is refused.
// --name narrows the action to one skill.
func cmdSkill(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: punk skill install|print|paths --agent NAME [--name punk-memory|punk-plan] [--project] [--url URL] [--ns NS]")
	}
	action := args[0]
	fs := flag.NewFlagSet("skill "+action, flag.ContinueOnError)
	agent := fs.String("agent", "", "target agent: claude-code|codex|opencode|cursor|copilot|antigravity|hermes|openclaw|pi")
	project := fs.Bool("project", false, "project-local location instead of the global one")
	urlFlag := fs.String("url", "", "server URL mentioned in the skill (default: resolved like connect)")
	nsFlag := fs.String("ns", "", "pin the skill to a namespace (default: resolved from the workspace)")
	nameFlag := fs.String("name", "", "only this skill: punk-memory or punk-plan (default: both)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *agent == "" {
		return errors.New("skill: --agent is required")
	}
	if *nameFlag != "" && *nameFlag != hookcli.SkillName && *nameFlag != hookcli.PlanSkillName {
		return fmt.Errorf("skill: --name must be %s or %s", hookcli.SkillName, hookcli.PlanSkillName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	targets, err := hookcli.AllSkillTargets(*agent, *project, home, os.Getenv)
	if err != nil {
		return err
	}
	serverURL, _ := hookcli.ResolveServer(*urlFlag)
	for _, tg := range targets {
		if *nameFlag != "" && tg.Name != *nameFlag {
			continue
		}
		tg.Opts.ServerURL, tg.Opts.Namespace = serverURL, *nsFlag
		content := hookcli.Render(tg)
		switch action {
		case "print":
			fmt.Print(content)
		case "paths":
			fmt.Println(tg.Path)
		case "install":
			changed, err := hookcli.WriteSkill(tg.Path, content)
			if err != nil {
				return err
			}
			fmt.Printf("punk: skill %s in %s (%s)\n", tg.Name, tg.Path, changedWord(changed))
		default:
			return fmt.Errorf("skill: want install, print or paths, got %q", action)
		}
	}
	return nil
}

// envOr returns the environment variable's value, or def when unset/empty.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "punk-records server URL (required)")
	key := fs.String("api-key", "", "bearer token from 'punk apikey create' on the server")
	keyStdin := fs.Bool("api-key-stdin", false, "read the token from stdin (first line)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *urlFlag == "" {
		return errors.New("login: --url is required")
	}
	token := *key
	if *keyStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		token = strings.TrimSpace(line)
	}
	path := hookcli.CredentialsPath()
	if err := hookcli.SaveCredentials(path, hookcli.Credentials{URL: *urlFlag, APIKey: token}); err != nil {
		return err
	}
	if token == "" {
		fmt.Printf("punk: saved %s (no API key; fine for an unauthenticated server)\n", path)
	} else {
		fmt.Printf("punk: saved %s\n", path)
	}
	return nil
}

// cmdNamespace prints the namespace a checkout maps to and how it was
// derived (remote = stable across machines, path = local fallback).
func cmdNamespace(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	ns, source := hookcli.ProjectNamespace(dir)
	fmt.Printf("%s\t%s\n", ns, source)
	return nil
}

func cmdLogout(_ []string) error {
	path := hookcli.CredentialsPath()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("punk: removed %s\n", path)
	return nil
}
