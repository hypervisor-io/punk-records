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
	"fmt"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

const (
	toolListModels       = "list_models"
	toolListObservations = "list_observations"
	toolRecall           = "recall"
	toolDone             = "done"

	defaultMaxIter = 6
	recallLimit    = 20
)

// Answer is a reflect result with validated provenance.
type Answer struct {
	Text       string   `json:"text"`
	Citations  []string `json:"citations"` // fact/model IDs actually retrieved and used
	Iterations int      `json:"iterations"`
	Tokens     int      `json:"tokens"`

	// Structured carries the answer as a JSON value when Opts.Schema was
	// given and the model's answer parsed as JSON. Text still holds the
	// raw string.
	Structured json.RawMessage `json:"structured,omitempty"`
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
}

// Engine runs the bounded agentic loop.
type Engine struct {
	store   *memory.Store
	client  llm.Client
	maxIter int
}

// New builds an Engine with the default iteration bound.
func New(store *memory.Store, client llm.Client) *Engine {
	return &Engine{store: store, client: client, maxIter: defaultMaxIter}
}

const systemPrompt = `You answer questions by reflecting over this deployment's memory plane, checked hierarchically:
1. list_models — curated mental models, the highest-confidence synthesis. Check these first.
2. list_observations — consolidated observations. Check these second.
3. recall — raw hybrid search over all facts. Drop to this only to verify something flagged stale, or to fill a gap the higher layers didn't cover.
Cite ONLY fact or model IDs you were actually shown in a tool result. Never invent an ID.
When you have gathered enough evidence, call done with your answer and the IDs that support it.`

func tools(schema json.RawMessage) []llm.Tool {
	doneSchema := json.RawMessage(`{"type":"object","properties":{
				"answer":{"type":"string"},
				"citations":{"type":"array","items":{"type":"string"}}},"required":["answer"]}`)
	if len(schema) > 0 {
		// Embed the caller's schema as the answer's shape. Invalid caller
		// JSON would corrupt the tool schema, so it is only embedded when
		// it parses; otherwise the default string answer stands.
		if json.Valid(schema) {
			doneSchema = json.RawMessage(`{"type":"object","properties":{
				"answer":` + string(schema) + `,
				"citations":{"type":"array","items":{"type":"string"}}},"required":["answer"]}`)
		}
	}
	return []llm.Tool{
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
		{Name: toolDone,
			Description: "Finish and return the answer. Cite only IDs you were actually shown.",
			Schema:      doneSchema},
	}
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
// scales the iteration bound, and an optional answer schema (see Opts).
func (e *Engine) ReflectWith(ctx context.Context, ns, query string, opts Opts) (Answer, error) {
	maxIter := e.maxIter
	if n, ok := levels[opts.Level]; ok {
		maxIter = n
	}
	turns := []llm.Turn{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: query},
	}
	toolset := tools(opts.Schema)
	structured := len(opts.Schema) > 0
	seen := map[string]bool{}
	totalTokens := 0
	var lastText string

	for i := 0; i < maxIter; i++ {
		res, err := e.client.Chat(ctx, turns, toolset)
		if err != nil {
			return Answer{Iterations: i, Tokens: totalTokens}, err
		}
		totalTokens += res.PromptTokens + res.CompletionTokens
		if res.Content != "" {
			lastText = res.Content
		}
		turns = append(turns, llm.Turn{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})

		if len(res.ToolCalls) == 0 {
			// model produced only text; nudge it to act within the bound
			turns = append(turns, llm.Turn{Role: "user", Content: "Continue: call a tool, or call done with your answer."})
			continue
		}

		for _, tc := range res.ToolCalls {
			if tc.Name == toolDone {
				text, structuredAns, citations := parseDone(tc.Args, seen, structured)
				return Answer{Text: text, Structured: structuredAns, Citations: citations, Iterations: i + 1, Tokens: totalTokens}, nil
			}
			out, callErr := e.execTool(ctx, ns, tc, seen)
			if callErr != nil {
				out = "error: " + callErr.Error()
			}
			turns = append(turns, llm.Turn{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}
	return Answer{Text: lastText, Iterations: maxIter, Tokens: totalTokens}, nil
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
func parseDone(raw json.RawMessage, seen map[string]bool, structured bool) (string, json.RawMessage, []string) {
	var args struct {
		Answer    json.RawMessage `json:"answer"`
		Citations []string        `json:"citations"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", nil, nil
	}
	valid := make([]string, 0, len(args.Citations))
	for _, c := range args.Citations {
		if seen[c] {
			valid = append(valid, c)
		}
	}
	var s string
	if err := json.Unmarshal(args.Answer, &s); err == nil {
		var structuredAns json.RawMessage
		if structured && json.Valid([]byte(s)) {
			structuredAns = json.RawMessage(s)
		}
		return s, structuredAns, valid
	}
	if len(args.Answer) == 0 {
		return "", nil, valid
	}
	return string(args.Answer), args.Answer, valid
}

// execTool runs one tool call against the store. Every fact/model ID
// returned is recorded into seen so done's citations can be validated.
// Bad args or store errors come back as an error the caller turns into a
// tool-result the model can recover from, never a hard failure.
func (e *Engine) execTool(ctx context.Context, ns string, tc llm.ToolCall, seen map[string]bool) (string, error) {
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
	default:
		return "", fmt.Errorf("unknown tool %q", tc.Name)
	}
}

func marshalFacts(facts []memory.Fact, seen map[string]bool) (string, error) {
	for _, f := range facts {
		seen[f.ID] = true
	}
	out, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
