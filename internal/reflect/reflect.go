// Package reflect answers a query by driving a bounded agentic loop over
// the memory plane's hierarchy: mental models first (curated synthesis),
// then observations (consolidated belief), then raw hybrid recall to
// verify anything stale or fill a gap. Citations are validated against
// what the loop actually retrieved this run — the reflect analogue of
// punk's evidence contract (never fabricate provenance).
package reflect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// ErrBudgetExhausted ends a ReflectWith run early when Opts.MaxTokens is
// set and the run's accumulated measured usage has reached it. The
// returned Answer carries the usage consumed so far, so callers
// enforcing a budget never lose spend with the error.
var ErrBudgetExhausted = errors.New("reflect: token budget exhausted")

// Stop reasons recorded on Answer.StopReason when the loop ended on a
// bound instead of a done call. "" means the model called done (or the
// iteration bound hit without any bound firing).
const (
	stopNoNewEvidence = "no-new-evidence"
	stopMaxRounds     = "max-rounds"
	stopMaxToolCalls  = "max-tool-calls"
	stopDeadline      = "deadline"
	stopTokenBudget   = "token-budget"
	stopExpandBudget  = "expansion-budget"
	stopCanceled      = "canceled"
	stopModelError    = "model-error"
)

const (
	toolListModels       = "list_models"
	toolListObservations = "list_observations"
	toolRecall           = "recall"
	toolExpand           = "expand"
	toolDone             = "done"

	defaultMaxIter    = 6
	recallLimit       = 20
	expandMaxQueries  = 4 // follow-up queries accepted per expand call
	expandMaxKeys     = 8 // keys expanded per expand call
	expandRecallLimit = 5 // facts pulled per expanded key (key + children)

	// Expansion resource bounds. The outer query/key caps do NOT bound
	// retrieval work by themselves: one high-degree key can enumerate
	// every neighbor and serialize every body. These caps bound each
	// expand call's returned evidence (facts, relation lines, exact
	// serialized output bytes, neighbor links read per key) AND the
	// run-wide totals across all expand calls, so returned evidence and
	// the next model's context stay bounded as graph degree and body
	// size grow. A fact whose serialized form does not fit the
	// remaining byte allowance is skipped whole (never truncated
	// mid-body: bodies are what citations resolve against) and the
	// truncation is reported.
	expandMaxFacts        = 24
	expandMaxNeighbors    = 16
	expandMaxRelations    = 32
	expandMaxBytes        = 32 << 10
	expandRunMaxFacts     = 64
	expandRunMaxRelations = 96
	expandRunMaxBytes     = 96 << 10
	// Reserved room for the output wrapper (note/truncation fields and
	// JSON syntax) so a call that respects its byte accounting always
	// serializes to at most expandMaxBytes.
	expandByteSlack = 512
)

// Answer is a reflect result with validated provenance.
type Answer struct {
	Text       string   `json:"text"`
	Citations  []string `json:"citations"` // fact/model IDs actually retrieved and used
	Iterations int      `json:"iterations"`
	Tokens     int      `json:"tokens"`
	// UsageUnknown counts this run's model calls whose response reported
	// no token usage (a call that errored included): unknown, which is
	// not zero. The budget bounds MEASURED usage only, so callers
	// enforcing one surface this count to make an under-measured run
	// visible instead of silently treating unreported usage as free.
	UsageUnknown int `json:"usage_unknown,omitempty"`

	// Abstained is true when the model closed the loop with
	// done{abstain:true}: the evidence gathered could not answer the
	// question, so it declined instead of guessing. An honest abstention
	// is distinguishable from a confident answer, which the membench
	// answer stage needs to score unanswerable questions.
	Abstained bool `json:"abstained,omitempty"`
	// Evidence is the sorted set of fact/model IDs actually shown to the
	// model this run (every tool result ID). Citations are a subset of
	// it; callers scoring citation existence against retrieved/known
	// facts use this as the known set rather than trusting the model.
	Evidence []string `json:"evidence,omitempty"`
	// ShownFacts carries the full evidence bodies actually shown to the
	// model this run (every tool result fact), sorted by ID: callers
	// resolve citations against the exact bodies the model saw instead
	// of re-scanning the namespace, which can silently miss them when it
	// is large. In-process artifact: not serialized, the wire result
	// stays as light as the Evidence set.
	ShownFacts []memory.Fact `json:"-"`

	// StopReason names the bound that ended the run without a done call:
	// no-new-evidence (an expansion round added nothing new), max-rounds,
	// max-tool-calls, deadline, or token-budget (with ErrBudgetExhausted).
	// "" means the model called done. A stopped run's Text/Evidence are
	// the partial result gathered so far — the explicit reason is what
	// makes a partial answer distinguishable from a complete one.
	StopReason string `json:"stop_reason,omitempty"`
	// Rounds is the per-round expansion record (only with Opts
	// .ExpandEvidence): each tool-executing round's new evidence IDs,
	// model token usage and measured latency, so an expansion run's
	// added cost is inspectable rather than folded into the totals.
	Rounds []ExpansionRound `json:"rounds,omitempty"`

	// Structured carries the answer as a JSON value when Opts.Schema was
	// given and the model's answer parsed as JSON. Text still holds the
	// raw string.
	Structured json.RawMessage `json:"structured,omitempty"`
}

// ExpansionRound is one tool-executing round of an expansion-enabled
// run: what the round asked the store to retrieve, which evidence IDs it
// newly contributed (deduplicated against every prior round), and what
// the round's model call cost. Relations shown to the model are context
// only and never appear here — they have no fact IDs.
type ExpansionRound struct {
	Round         int      `json:"round"`
	Queries       []string `json:"queries,omitempty"`        // follow-up queries executed (relationship retrieval)
	Keys          []string `json:"keys,omitempty"`           // keys expanded (neighborhood retrieval)
	EvidenceAdded []string `json:"evidence_added,omitempty"` // new IDs after cross-round dedup
	Tokens        int      `json:"tokens"`                   // this round's measured model usage
	LatencyMS     int64    `json:"latency_ms"`               // this round's model call latency
	// Truncated records that the round's expansion hit a resource bound
	// and returned a bounded subset of what it reached (TruncatedBy names
	// the bound: facts | relations | bytes | neighbors). Only the facts
	// actually in the returned payload are shown and citable.
	Truncated   bool   `json:"truncated,omitempty"`
	TruncatedBy string `json:"truncated_by,omitempty"`
}

// Levels map a caller-facing reasoning level
// (minimal|low|medium|high|max) to the loop's iteration bound.
// The default ("" or "low") is the pre-existing bound.
var levels = map[string]int{
	"minimal": 2,
	"low":     defaultMaxIter,
	"medium":  10,
	"high":    16,
	"max":     24,
}

// Opts tunes one Reflect run.
type Opts struct {
	// Level is minimal|low|medium|high|max; empty means low. An unknown
	// level degrades to low rather than erroring - the loop still runs.
	Level string
	// Schema, when non-empty, is a JSON Schema the answer should conform
	// to: it is embedded as the done tool's answer schema, so a
	// tool-call-capable model is constrained to produce it, and the
	// answer is parsed into Answer.Structured when it is valid JSON.
	// Conformance beyond being valid JSON is the model's job - punk does
	// not ship a schema validator.
	Schema json.RawMessage
	// MaxTokens, when >0, bounds this run's measured model usage: the
	// accumulated usage is checked before EVERY model call and the loop
	// stops with ErrBudgetExhausted once the bound is reached. A call
	// already in flight may overshoot the bound by its own usage - the
	// gate is a pre-call check, not a preemption. 0 (the default) is
	// unbounded, the pre-existing behavior.
	MaxTokens int

	// ExpandEvidence opts in to bounded relationship/evidence expansion
	// (default off): the toolset gains an expand tool the model calls
	// with follow-up queries and/or known fact keys, and each call runs
	// relationship retrieval (R01's RoutedSearch relationship mode) plus
	// neighborhood expansion over the memory plane's links. Adapted from
	// Cognee's GraphCompletionContextExtensionRetriever (pinned at commit
	// 78ff576559a7f75f65884c5bd90b22cdc790016e): rounds of retrieval
	// expansion that merge new triplets and converge when a round adds
	// nothing new. Here the model only ever PROPOSES queries and keys —
	// its suggestions are executed as retrievals and never become facts
	// themselves. Every expanded fact lands in the evidence set, so done
	// citations validate against the expansion too, and a round whose
	// expansion added no new evidence IDs stops the run (see Answer
	// .StopReason no-new-evidence). Opt-in pending E02 evidence under
	// equal budgets; there is no default-on path and no recursion beyond
	// the caps below.
	ExpandEvidence bool

	// MaxRounds bounds expand tool executions per run (only meaningful
	// with ExpandEvidence). 0 or negative inherits the level's iteration
	// bound, which caps expansion anyway — never unbounded.
	MaxRounds int

	// MaxToolCalls, when >0, bounds TOTAL tool executions across the run
	// (every tool, expansion or not): once reached, no further tool runs
	// and the loop returns the partial answer with StopReason
	// max-tool-calls. The model can still spend a later call on done.
	// 0 (the default) is unbounded, the pre-existing behavior.
	MaxToolCalls int

	// Deadline, when >0, propagates cancellation to model and retrieval
	// calls (a timeout context is derived for the whole run) and is
	// checked between operations, including inside expansion retrieval
	// loops. Calls must honor context cancellation to stop promptly; a
	// call that ignores its context can still only run until its own
	// natural completion - the gate is cooperative, not preemption. An
	// in-flight cancellation returns the partial answer (evidence,
	// rounds and usage gathered so far preserved) with StopReason
	// deadline and the context error. 0 leaves the caller's context
	// unchanged.
	Deadline time.Duration
}

// Engine runs the bounded agentic loop.
type Engine struct {
	store   *memory.Store
	client  llm.Client
	maxIter int
	now     func() time.Time // injectable for deterministic deadline tests
}

// New builds an Engine with the default iteration bound.
func New(store *memory.Store, client llm.Client) *Engine {
	return &Engine{store: store, client: client, maxIter: defaultMaxIter, now: time.Now}
}

func (e *Engine) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// roundLatency measures one round's model-call latency on the engine
// clock; zero when no round was timed (expansion off).
func (e *Engine) roundLatency(pre time.Time) time.Duration {
	if pre.IsZero() {
		return 0
	}
	return e.clock().Sub(pre)
}

const systemPrompt = `You answer questions by reflecting over this deployment's memory plane, checked hierarchically:
1. list_models — curated mental models, the highest-confidence synthesis. Check these first.
2. list_observations — consolidated observations. Check these second.
3. recall — raw hybrid search over all facts. Drop to this only to verify something flagged stale, or to fill a gap the higher layers didn't cover.
Cite ONLY fact or model IDs you were actually shown in a tool result. Never invent an ID.
If the evidence you gathered cannot answer the question, call done with abstain=true and an empty answer rather than guessing.
When you have gathered enough evidence, call done with your answer and the IDs that support it.`

// expansionPrompt extends the system prompt only when Opts.ExpandEvidence
// is on: it describes the expand tool and states the two invariants the
// tool enforces - relation lines are context only (no citable IDs), and
// a no-new-evidence round ends the run.
const expansionPrompt = `
When the higher layers leave a gap, call expand with follow-up queries and/or known fact keys ("/..."): each runs relationship-weighted retrieval over described links and neighborhood expansion, returning the linked facts. At most 4 queries and 8 keys per call. Relation lines in the result are context only — they carry no citable IDs; cite fact IDs from the results. If an expand call returns nothing new, further expansion is pointless: the run ends there.`

func tools(schema json.RawMessage, expand bool) []llm.Tool {
	doneSchema := json.RawMessage(`{"type":"object","properties":{
				"answer":{"type":"string"},
				"abstain":{"type":"boolean"},
				"citations":{"type":"array","items":{"type":"string"}}},"required":["answer"]}`)
	if len(schema) > 0 {
		// Embed the caller's schema as the answer's shape. Invalid caller
		// JSON would corrupt the tool schema, so it is only embedded when
		// it parses; otherwise the default string answer stands.
		if json.Valid(schema) {
			doneSchema = json.RawMessage(`{"type":"object","properties":{
				"answer":` + string(schema) + `,
				"abstain":{"type":"boolean"},
				"citations":{"type":"array","items":{"type":"string"}}},"required":["answer"]}`)
		}
	}
	toolset := []llm.Tool{
		{Name: toolListModels,
			Description: "List the curated mental models in this namespace. Check these first.",
			Schema:      json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: toolListObservations,
			Description: "List consolidated observations (/observations/*). Check these second.",
			Schema:      json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: toolRecall,
			Description: "Hybrid search over raw facts. Use to verify something stale or fill a gap.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"query":{"type":"string"}},"required":["query"]}`)},
	}
	if expand {
		toolset = append(toolset, llm.Tool{Name: toolExpand,
			Description: "Bounded evidence expansion: follow-up queries and/or known fact keys, answered with relationship-linked facts.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"queries":{"type":"array","items":{"type":"string"}},
				"keys":{"type":"array","items":{"type":"string"}}}}`)})
	}
	toolset = append(toolset, llm.Tool{Name: toolDone,
		Description: "Finish and return the answer. Cite only IDs you were actually shown.",
		Schema:      doneSchema})
	return toolset
}

// Reflect answers a query by gathering evidence hierarchically, then
// composing an answer whose citations are validated against what was
// actually retrieved. Bounded at maxIter tool rounds: if the model never
// calls done, a best-effort answer is returned rather than looping
// forever or hard-erroring.
func (e *Engine) Reflect(ctx context.Context, ns, query string) (Answer, error) {
	return e.ReflectWith(ctx, ns, query, Opts{})
}

// ReflectWith is Reflect with per-run options: a reasoning level that
// scales the iteration bound, an optional answer schema, an optional
// measured-usage budget checked before every model call, and the opt-in
// bounded evidence expansion (see Opts).
func (e *Engine) ReflectWith(ctx context.Context, ns, query string, opts Opts) (Answer, error) {
	if opts.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Deadline)
		defer cancel()
	}
	maxIter := e.maxIter
	if n, ok := levels[opts.Level]; ok {
		maxIter = n
	}
	system := systemPrompt
	if opts.ExpandEvidence {
		system += expansionPrompt
	}
	turns := []llm.Turn{
		{Role: "system", Content: system},
		{Role: "user", Content: query},
	}
	toolset := tools(opts.Schema, opts.ExpandEvidence)
	structured := len(opts.Schema) > 0
	seen := map[string]memory.Fact{}
	totalTokens := 0
	usageUnknown := 0
	var lastText string
	exp := newExpansion(opts, maxIter)
	var deadline time.Time
	if opts.Deadline > 0 {
		deadline = e.clock().Add(opts.Deadline)
	}
	executed := 0

	for i := 0; i < maxIter; i++ {
		if err := ctx.Err(); err != nil {
			return partialAnswer(lastText, seen, i, totalTokens, usageUnknown, errorStopReason(err), exp), err
		}
		if !deadline.IsZero() && e.clock().After(deadline) {
			return partialAnswer(lastText, seen, i, totalTokens, usageUnknown, stopDeadline, exp), nil
		}
		if opts.MaxTokens > 0 && totalTokens >= opts.MaxTokens {
			return partialAnswer(lastText, seen, i, totalTokens, usageUnknown, stopTokenBudget, exp), ErrBudgetExhausted
		}
		var pre time.Time
		if exp != nil {
			pre = e.clock()
		}
		res, err := e.client.Chat(ctx, turns, toolset)
		if err != nil {
			// The failed call reported no usage: counted unknown, like a
			// response without usage - unknown is never zero. Everything
			// gathered in earlier rounds (evidence, bodies, round records,
			// measured usage) is preserved on the partial answer: spent
			// work is never dropped with the error.
			usageUnknown++
			return partialAnswer(lastText, seen, i, totalTokens, usageUnknown, errorStopReason(err), exp), err
		}
		totalTokens += res.PromptTokens + res.CompletionTokens
		if res.PromptTokens+res.CompletionTokens == 0 {
			usageUnknown++
		}
		if err := ctx.Err(); err != nil {
			return partialAnswer(lastText, seen, i+1, totalTokens, usageUnknown, errorStopReason(err), exp), err
		}
		if res.Content != "" {
			lastText = res.Content
		}
		turns = append(turns, llm.Turn{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})

		if len(res.ToolCalls) == 0 {
			// model produced only text; nudge it to act within the bound
			turns = append(turns, llm.Turn{Role: "user", Content: "Continue: call a tool, or call done with your answer."})
			continue
		}

		if exp != nil {
			exp.beginRound(seen)
		}
		for _, tc := range res.ToolCalls {
			if err := ctx.Err(); err != nil {
				exp.finalizeRound(i+1, res.PromptTokens+res.CompletionTokens, e.roundLatency(pre).Milliseconds(), seen)
				return partialAnswer(lastText, seen, i+1, totalTokens, usageUnknown, errorStopReason(err), exp), err
			}
			if tc.Name == toolDone {
				// A done call can share a response with other tool calls
				// that already executed: finalize their round record so
				// the answer's ledger carries that work too.
				exp.finalizeRound(i+1, res.PromptTokens+res.CompletionTokens, e.roundLatency(pre).Milliseconds(), seen)
				text, structuredAns, citations, abstained := parseDone(tc.Args, seen, structured)
				return Answer{Text: text, Structured: structuredAns, Citations: citations, Abstained: abstained, Evidence: sortedIDs(seen), ShownFacts: sortedFacts(seen), Iterations: i + 1, Tokens: totalTokens, UsageUnknown: usageUnknown, Rounds: expRounds(exp)}, nil
			}
			if opts.MaxToolCalls > 0 && executed >= opts.MaxToolCalls {
				exp.finalizeRound(i+1, res.PromptTokens+res.CompletionTokens, e.roundLatency(pre).Milliseconds(), seen)
				return partialAnswer(lastText, seen, i+1, totalTokens, usageUnknown, stopMaxToolCalls, exp), nil
			}
			if exp != nil && tc.Name == toolExpand && exp.calls >= exp.maxRounds {
				exp.finalizeRound(i+1, res.PromptTokens+res.CompletionTokens, e.roundLatency(pre).Milliseconds(), seen)
				return partialAnswer(lastText, seen, i+1, totalTokens, usageUnknown, stopMaxRounds, exp), nil
			}
			if exp != nil && tc.Name == toolExpand && !exp.runBudgetLeft() {
				// A previous call IN THIS BATCH exhausted the run-wide
				// expansion budget: the remaining expand calls are
				// skipped without executing (they record nothing - no
				// queries, keys, or evidence), the round finalizes with
				// exactly the work that ran, and the model is not called
				// again.
				exp.finalizeRound(i+1, res.PromptTokens+res.CompletionTokens, e.roundLatency(pre).Milliseconds(), seen)
				return partialAnswer(lastText, seen, i+1, totalTokens, usageUnknown, stopExpandBudget, exp), nil
			}
			executed++
			out, callErr := e.execTool(ctx, ns, tc, seen, exp)
			if callErr != nil {
				out = "error: " + callErr.Error()
			}
			turns = append(turns, llm.Turn{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
		if exp != nil {
			roundTokens := res.PromptTokens + res.CompletionTokens
			var latency time.Duration
			if !pre.IsZero() {
				latency = e.clock().Sub(pre)
			}
			r, converged := exp.endRound(i+1, roundTokens, latency.Milliseconds(), seen)
			exp.rounds = append(exp.rounds, r)
			if exp.budgetExhausted {
				// The run-wide expansion budget bound this round's
				// retrieval: further expansion can only return a bounded
				// nothing, so the run stops with the partial answer.
				return partialAnswer(lastText, seen, i+1, totalTokens, usageUnknown, stopExpandBudget, exp), nil
			}
			if converged {
				// The expansion found nothing the earlier rounds had not
				// already retrieved: further rounds can only repeat it,
				// so the run stops before spending another model call.
				return partialAnswer(lastText, seen, i+1, totalTokens, usageUnknown, stopNoNewEvidence, exp), nil
			}
		}
	}
	return Answer{Text: lastText, Evidence: sortedIDs(seen), ShownFacts: sortedFacts(seen), Iterations: maxIter, Tokens: totalTokens, UsageUnknown: usageUnknown, Rounds: expRounds(exp)}, nil
}

// expansion carries the opt-in expansion run's bookkeeping: the round
// cap, how many expand calls have executed, the run-wide resource
// totals, and the pending round's state. nil when Opts.ExpandEvidence
// is off - every touch is guarded, so the default path behaves exactly
// as before.
type expansion struct {
	maxRounds int
	calls     int
	rounds    []ExpansionRound
	snapshot  map[string]bool // evidence IDs when the pending round began
	queries   []string        // pending round's executed queries
	keys      []string        // pending round's executed keys
	hadExpand bool            // pending round included an expand call
	expandErr bool            // pending round's expand call errored
	truncated string          // pending round's resource bound, "" when untruncated
	open      bool            // a round is between beginRound and its finalize

	// Run-wide returned-evidence totals across all expand calls: the
	// determinstic bounds on how much evidence (and next-model context)
	// one run's expansion can produce, regardless of graph degree.
	factsShown     int
	relationsShown int
	bytesShown     int
	// budgetExhausted is set when a call truncated BECAUSE a run-wide
	// bound (not just the per-call bound) stopped it.
	budgetExhausted bool
}

// runBudgetLeft reports whether the run-wide expansion budgets still
// have room for another expand call. Checked BEFORE each expand
// execution - including every call inside one model response's tool
// batch - so a high-volume batch stops retrieving as soon as the run
// totals are spent instead of after the batch ends.
func (exp *expansion) runBudgetLeft() bool {
	return !exp.budgetExhausted &&
		exp.factsShown < expandRunMaxFacts &&
		exp.relationsShown < expandRunMaxRelations &&
		exp.bytesShown < expandRunMaxBytes
}

func newExpansion(opts Opts, maxIter int) *expansion {
	if !opts.ExpandEvidence {
		return nil
	}
	maxRounds := maxIter
	if opts.MaxRounds > 0 {
		maxRounds = opts.MaxRounds
	}
	return &expansion{maxRounds: maxRounds}
}

func expRounds(exp *expansion) []ExpansionRound {
	if exp == nil {
		return nil
	}
	return exp.rounds
}

// beginRound snapshots the evidence set so endRound can diff the round's
// new IDs (the cross-round dedup that drives the no-new-evidence rule).
func (exp *expansion) beginRound(seen map[string]memory.Fact) {
	exp.snapshot = make(map[string]bool, len(seen))
	for id := range seen {
		exp.snapshot[id] = true
	}
	exp.queries, exp.keys = nil, nil
	exp.hadExpand, exp.expandErr, exp.truncated = false, false, ""
	exp.open = true
}

// finalizeRound records a round that ended on an early exit - the tool
// cap, the round cap, cancellation, or a done call sharing the response
// with already-executed tools. Completed tool work keeps its ledger
// entry (evidence, truncation, queries/keys, measured usage and
// latency); skipped calls claim nothing. Idempotent: a round is
// finalized at most once.
func (exp *expansion) finalizeRound(n, tokens int, latencyMS int64, seen map[string]memory.Fact) {
	if exp == nil || !exp.open {
		return
	}
	exp.open = false
	exp.rounds = append(exp.rounds, exp.buildRound(n, tokens, latencyMS, seen))
}

// endRound finalizes the normally-completed round exactly once.
func (exp *expansion) endRound(n, tokens int, latencyMS int64, seen map[string]memory.Fact) (ExpansionRound, bool) {
	if !exp.open {
		return ExpansionRound{}, false
	}
	exp.open = false
	r := exp.buildRound(n, tokens, latencyMS, seen)
	converged := exp.hadExpand && !exp.expandErr && len(r.EvidenceAdded) == 0
	return r, converged
}

// buildRound assembles the pending round's record.
func (exp *expansion) buildRound(n, tokens int, latencyMS int64, seen map[string]memory.Fact) ExpansionRound {
	r := ExpansionRound{Round: n, Queries: exp.queries, Keys: exp.keys, Tokens: tokens, LatencyMS: latencyMS}
	for id := range seen {
		if !exp.snapshot[id] {
			r.EvidenceAdded = append(r.EvidenceAdded, id)
		}
	}
	sort.Strings(r.EvidenceAdded)
	r.Truncated = exp.truncated != ""
	r.TruncatedBy = exp.truncated
	return r
}

// partialAnswer is a bounded stop's result: everything gathered so far,
// with the reason that stopped the run explicit on the answer.
func partialAnswer(text string, seen map[string]memory.Fact, iter, tokens, unknown int, stop string, exp *expansion) Answer {
	return Answer{
		Text: text, Evidence: sortedIDs(seen), ShownFacts: sortedFacts(seen),
		Iterations: iter, Tokens: tokens, UsageUnknown: unknown,
		StopReason: stop, Rounds: expRounds(exp),
	}
}

// errorStopReason classifies a run-aborting error into the stop reason
// recorded on the partial answer: the run's own deadline, a caller
// cancellation, or any other model/provider failure.
func errorStopReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return stopDeadline
	}
	if errors.Is(err, context.Canceled) {
		return stopCanceled
	}
	return stopModelError
}

// sortedIDs renders the seen-ID set as a sorted slice: the Evidence set
// is an artifact, so its serialization is deterministic.
func sortedIDs(seen map[string]memory.Fact) []string {
	if len(seen) == 0 {
		return nil
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// sortedFacts renders the seen evidence as an ID-sorted slice: the same
// determinism as sortedIDs, for callers that resolve citations against
// the full shown bodies rather than only the ID set.
func sortedFacts(seen map[string]memory.Fact) []memory.Fact {
	if len(seen) == 0 {
		return nil
	}
	out := make([]memory.Fact, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// parseDone extracts the model's answer and validates its citations
// against every ID actually seen this run — a fabricated ID never
// survives. Bad args degrade to an empty answer rather than a hard error;
// the loop is already ending, there is no round left to recover in.
// The answer may be a JSON string (the default schema) or any JSON value
// (a caller schema, see Opts.Schema): a string answer fills text; a
// non-string answer fills both text (compact JSON) and structured. A
// string answer that itself parses as JSON also fills structured when
// the caller asked for a schema, tolerating models that stringify.
// abstain is the model's explicit no-evidence signal (see Answer
// .Abstained): honored as given, with no attempt to second-guess it.
func parseDone(raw json.RawMessage, seen map[string]memory.Fact, structured bool) (string, json.RawMessage, []string, bool) {
	var args struct {
		Answer    json.RawMessage `json:"answer"`
		Abstain   bool            `json:"abstain"`
		Citations []string        `json:"citations"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", nil, nil, false
	}
	valid := make([]string, 0, len(args.Citations))
	for _, c := range args.Citations {
		if _, ok := seen[c]; ok {
			valid = append(valid, c)
		}
	}
	var s string
	if err := json.Unmarshal(args.Answer, &s); err == nil {
		var structuredAns json.RawMessage
		if structured && json.Valid([]byte(s)) {
			structuredAns = json.RawMessage(s)
		}
		return s, structuredAns, valid, args.Abstain
	}
	if len(args.Answer) == 0 {
		return "", nil, valid, args.Abstain
	}
	return string(args.Answer), args.Answer, valid, args.Abstain
}

// execTool runs one tool call against the store. Every fact/model ID
// returned is recorded into seen so done's citations can be validated.
// Bad args or store errors come back as an error the caller turns into a
// tool-result the model can recover from, never a hard failure. exp is
// the expansion bookkeeping (nil unless Opts.ExpandEvidence); the expand
// tool records its executed queries/keys and errors into it.
func (e *Engine) execTool(ctx context.Context, ns string, tc llm.ToolCall, seen map[string]memory.Fact, exp *expansion) (string, error) {
	switch tc.Name {
	case toolListModels:
		facts, err := e.store.ListModels(ctx, ns)
		if err != nil {
			return "", err
		}
		return marshalFacts(facts, seen)
	case toolListObservations:
		facts, err := e.store.Recall(ctx, ns, "/observations", recallLimit)
		if err != nil {
			return "", err
		}
		return marshalFacts(facts, seen)
	case toolRecall:
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(tc.Args, &args); err != nil {
			return "", fmt.Errorf("bad recall args: %w", err)
		}
		scored, err := e.store.HybridSearchScored(ctx, ns, args.Query, recallLimit, 0)
		if err != nil {
			return "", err
		}
		facts := make([]memory.Fact, len(scored))
		for i, sf := range scored {
			facts[i] = sf.Fact
		}
		return marshalFacts(facts, seen)
	case toolExpand:
		if exp == nil {
			return "", fmt.Errorf("tool %q requires opt-in evidence expansion (Opts.ExpandEvidence)", tc.Name)
		}
		return e.execExpand(ctx, ns, tc, seen, exp)
	default:
		return "", fmt.Errorf("unknown tool %q", tc.Name)
	}
}

// execExpand executes one bounded evidence-expansion call: the model's
// proposed queries run through R01's relationship-mode routed retrieval
// (described links, lexical fallback without an embedder) and its
// proposed keys through neighborhood expansion (live links both ways,
// endpoints resolved as facts). The proposals are retrieval INPUTS only
// — nothing the model generated is ever written to the store — and the
// output's relation lines are context without citable IDs, so a citation
// naming one is dropped by validation like any other fabricated ID.
//
// Resource bounds are enforced at every boundary: per-call caps on
// queries, keys, facts, relation lines, neighbor links read per key and
// exact serialized output bytes, plus run-wide totals across calls; the
// caller context is checked inside every loop, so a deadline or
// cancellation stops the retrieval work, not just the model calls.
// Returned facts are staged and registered into seen ONLY when the
// complete payload is successfully produced: a later failed retrieval
// must not make earlier, unshown facts citable.
func (e *Engine) execExpand(ctx context.Context, ns string, tc llm.ToolCall, seen map[string]memory.Fact, exp *expansion) (string, error) {
	exp.hadExpand = true
	exp.calls++
	var args struct {
		Queries []string `json:"queries"`
		Keys    []string `json:"keys"`
	}
	if err := json.Unmarshal(tc.Args, &args); err != nil {
		exp.expandErr = true
		return "", fmt.Errorf("bad expand args: %w", err)
	}
	if len(args.Queries) > expandMaxQueries {
		args.Queries = args.Queries[:expandMaxQueries]
	}
	if len(args.Keys) > expandMaxKeys {
		args.Keys = args.Keys[:expandMaxKeys]
	}

	// This call's allowances: the per-call caps, narrowed to the
	// remaining run-wide budgets. runLimited records that a run-wide
	// bound (not just the per-call bound) constrained the call.
	factsLeft := expandMaxFacts
	relationsLeft := expandMaxRelations
	bytesLeft := expandMaxBytes - expandByteSlack
	runLimited := false
	if r := expandRunMaxFacts - exp.factsShown; r < factsLeft {
		factsLeft, runLimited = r, true
	}
	if r := expandRunMaxRelations - exp.relationsShown; r < relationsLeft {
		relationsLeft, runLimited = r, true
	}
	if r := expandRunMaxBytes - exp.bytesShown - expandByteSlack; r < bytesLeft {
		bytesLeft, runLimited = r, true
	}
	truncatedBy := ""
	trunc := func(by string) {
		if truncatedBy == "" {
			truncatedBy = by
			// Write through to the round metadata immediately: the
			// round record must name the bound even when the call later
			// errors or a second expand call runs in the same round.
			if exp.truncated == "" {
				exp.truncated = by
			}
		}
	}

	have := map[string]bool{}
	var collected []memory.Fact // staged: registered into seen only on success
	var rawFacts []json.RawMessage
	var relations []json.RawMessage
	add := func(f memory.Fact) {
		if f.ID == "" || have[f.ID] {
			return
		}
		if factsLeft <= 0 {
			trunc("facts")
			return
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return
		}
		if len(raw)+1 > bytesLeft {
			// Oversized single bodies are skipped whole, never cut
			// mid-body: shown bodies are what citations resolve against.
			trunc("bytes")
			return
		}
		rawFacts = append(rawFacts, raw)
		collected = append(collected, f)
		have[f.ID] = true
		factsLeft--
		bytesLeft -= len(raw) + 1
	}
	relate := func(line string) {
		if relationsLeft <= 0 {
			trunc("relations")
			return
		}
		raw, err := json.Marshal(line)
		if err != nil {
			return
		}
		if len(raw)+1 > bytesLeft {
			trunc("bytes")
			return
		}
		relations = append(relations, raw)
		relationsLeft--
		bytesLeft -= len(raw) + 1
	}
	pull := func(key string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		fs, err := e.store.Recall(ctx, ns, key, expandRecallLimit)
		if err != nil {
			return err
		}
		for _, f := range fs {
			add(f)
		}
		return nil
	}
	for _, q := range args.Queries {
		if err := ctx.Err(); err != nil {
			exp.expandErr = true
			return "", err
		}
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		exp.queries = append(exp.queries, q)
		res, err := e.store.RoutedSearch(ctx, ns, memory.RouteRequest{Mode: memory.RouteRelationship, Query: q, Limit: recallLimit})
		if err != nil {
			exp.expandErr = true
			return "", fmt.Errorf("expand query %q: %w", q, err)
		}
		for _, h := range res.Hits {
			switch {
			case h.Fact != nil:
				add(h.Fact.Fact)
			case h.Triplet != nil:
				t := h.Triplet
				relate(t.From.Key + " -" + t.LinkType + "-> " + t.To.Key + ": " + t.Description)
				add(t.From)
				add(t.To)
			}
		}
	}
	for _, key := range args.Keys {
		if err := ctx.Err(); err != nil {
			exp.expandErr = true
			return "", err
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		exp.keys = append(exp.keys, key)
		if err := pull(key); err != nil {
			exp.expandErr = true
			return "", fmt.Errorf("expand key %q: %w", key, err)
		}
		for _, dir := range [...]string{"out", "in"} {
			if err := ctx.Err(); err != nil {
				exp.expandErr = true
				return "", err
			}
			// Bounded neighbor read at the memory layer: the query
			// carries a LIMIT (at most expandMaxNeighbors+1 rows are
			// materialized), so a high-degree key cannot force the
			// database to enumerate its whole neighborhood. HasMore
			// makes the truncation observable, not inferred from slice
			// length.
			page, err := e.store.NeighborsBounded(ctx, ns, key, memory.NeighborQuery{Direction: dir, Limit: expandMaxNeighbors})
			if err != nil {
				exp.expandErr = true
				return "", fmt.Errorf("expand neighbors of %q: %w", key, err)
			}
			for _, l := range page.Links {
				relate(l.FromKey + " -" + l.LinkType + "-> " + l.ToKey + ": " + l.Description)
				other := l.ToKey
				if dir == "in" {
					other = l.FromKey
				}
				if err := pull(other); err != nil {
					exp.expandErr = true
					return "", fmt.Errorf("expand neighbor %q: %w", other, err)
				}
			}
			if page.HasMore {
				trunc("neighbors")
			}
		}
	}
	out, err := json.Marshal(expandOutput{
		Note:        expandOutputNote,
		Truncated:   truncatedBy != "",
		TruncatedBy: truncatedBy,
		Relations:   relations,
		Facts:       rawFacts,
	})
	if err != nil {
		exp.expandErr = true
		return "", err
	}
	// Only a successfully returned payload authorizes citations. A later
	// failed retrieval must not make earlier, unshown facts citable:
	// exactly the facts in the returned payload enter the evidence set.
	for _, f := range collected {
		seen[f.ID] = f
	}
	exp.factsShown += len(collected)
	exp.relationsShown += len(relations)
	exp.bytesShown += len(out)
	if truncatedBy != "" && runLimited {
		exp.budgetExhausted = true
	}
	return string(out), nil
}

// expandOutput is the expand tool result. Facts are pre-serialized JSON
// (embedded verbatim) so the byte accounting during collection matches
// the final payload exactly; relations are context-only strings.
type expandOutput struct {
	Note        string            `json:"note"`
	Truncated   bool              `json:"truncated,omitempty"`
	TruncatedBy string            `json:"truncated_by,omitempty"`
	Relations   []json.RawMessage `json:"relations,omitempty"`
	Facts       []json.RawMessage `json:"facts,omitempty"`
}

const expandOutputNote = "relations are context only and carry no citable IDs; cite fact IDs from the results"

func marshalFacts(facts []memory.Fact, seen map[string]memory.Fact) (string, error) {
	for _, f := range facts {
		seen[f.ID] = f
	}
	out, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
