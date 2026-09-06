package reflect

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// The round-3 usage-accounting and shown-evidence tests (E02): a call
// whose response reports no token usage is unknown per call - never
// silently zero - and the count survives every loop outcome (done,
// budget stop, call error) so a budgeted caller can see an
// under-measured run. Answer also carries the full shown bodies so a
// caller (the membench answer stage) resolves citations against the
// exact evidence the model saw instead of re-scanning the namespace.

// TestReflectCountsUnreportedUsagePerCall pins Answer.UsageUnknown: the
// per-call count of model calls whose response reported no usage, kept
// alongside the measured tokens on every loop outcome.
func TestReflectCountsUnreportedUsagePerCall(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Done loop: the first call reports no usage, the second reports 15.
	noUsage := func(name string, args json.RawMessage) func([]llm.Turn) (*llm.Result, error) {
		return func([]llm.Turn) (*llm.Result, error) {
			return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "c", Name: name, Args: args}}}, nil
		}
	}
	script := &scriptedLLM{t: t}
	script.queue = []func([]llm.Turn) (*llm.Result, error){
		noUsage(toolListModels, json.RawMessage(`{}`)),
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolDone, Args: json.RawMessage(`{"answer":"done"}`)}})
		},
	}
	ans, err := New(s, script).Reflect(ctx, "acme", "anything")
	if err != nil {
		t.Fatal(err)
	}
	if ans.Tokens != 15 {
		t.Fatalf("tokens = %d, want 15: the measured call is preserved", ans.Tokens)
	}
	if ans.UsageUnknown != 1 {
		t.Fatalf("UsageUnknown = %d, want 1: the first call reported no usage (unknown, not zero)", ans.UsageUnknown)
	}

	// Budget-stopped loop: the count and the measured spend both survive
	// the error (call 1 unreported, call 2 lands 15 = the bound, the
	// pre-call gate stops call 3).
	script2 := &scriptedLLM{t: t}
	script2.queue = []func([]llm.Turn) (*llm.Result, error){
		noUsage(toolListModels, json.RawMessage(`{}`)),
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolRecall, Args: json.RawMessage(`{"query":"pool"}`)}})
		},
	}
	ans, err = New(s, script2).ReflectWith(ctx, "acme", "anything", Opts{MaxTokens: 15})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if ans.Tokens != 15 || ans.UsageUnknown != 1 {
		t.Fatalf("tokens = %d unknown = %d, want 15/1 retained through the budget stop", ans.Tokens, ans.UsageUnknown)
	}

	// A call that errors reported no usage either: counted unknown.
	script3 := &scriptedLLM{t: t}
	script3.queue = []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) { return nil, errors.New("endpoint down") },
	}
	ans, err = New(s, script3).Reflect(ctx, "acme", "anything")
	if err == nil {
		t.Fatal("err = nil, want the call error propagated")
	}
	if ans.Tokens != 0 || ans.UsageUnknown != 1 {
		t.Fatalf("tokens = %d unknown = %d, want 0/1: the failed call is unknown usage", ans.Tokens, ans.UsageUnknown)
	}
}

// TestReflectAnswerCarriesShownFactBodies pins Answer.ShownFacts: the
// full evidence bodies shown to the model this run, sorted by ID for
// deterministic artifacts, and kept OUT of the serialized result - the
// wire payload stays as light as the Evidence ID set.
func TestReflectAnswerCarriesShownFactBodies(t *testing.T) {
	mem := newTestStore(t)
	ctx := context.Background()
	model, err := mem.RememberModel(ctx, "ns", "fleet-posture", "fleet posture: two primaries per region", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := mem.Write(ctx, memory.WriteInput{Namespace: "ns", Key: "/raw/incident-1", Body: "pool exhausted at 14:02"})
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "1", Name: toolListModels, Args: json.RawMessage(`{}`)}})
		},
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "2", Name: toolRecall, Args: json.RawMessage(`{"query":"pool exhausted"}`)}})
		},
		func(_ []llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{"answer": "once, at 14:02", "citations": []string{fact.ID, model.ID}})
			return result([]llm.ToolCall{{ID: "3", Name: toolDone, Args: args}})
		},
	}}
	ans, err := New(mem, client).Reflect(ctx, "ns", "what happened to the pool?")
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.ShownFacts) != len(ans.Evidence) {
		t.Fatalf("ShownFacts = %d, Evidence = %d, want one body per shown ID", len(ans.ShownFacts), len(ans.Evidence))
	}
	byID := map[string]memory.Fact{}
	for i, f := range ans.ShownFacts {
		byID[f.ID] = f
		if i > 0 && ans.ShownFacts[i-1].ID > f.ID {
			t.Fatalf("ShownFacts not sorted by ID (deterministic artifact): %v", ans.ShownFacts)
		}
	}
	if got := byID[fact.ID]; got.Key != "/raw/incident-1" || got.Body != "pool exhausted at 14:02" {
		t.Fatalf("shown fact = %+v, want the recalled fact with its real body", got)
	}
	if got := byID[model.ID]; got.Key != "/mental-models/fleet-posture" || got.Body != "fleet posture: two primaries per region" {
		t.Fatalf("shown model = %+v, want the listed model with its real body", got)
	}
	// The carried bodies are an in-process artifact: the serialized
	// answer keeps the Evidence IDs and never the bodies.
	raw, err := json.Marshal(ans)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "pool exhausted at 14:02") || strings.Contains(string(raw), "shown_facts") {
		t.Fatalf("serialized answer carries the shown bodies: %s", raw)
	}
}
