package reflect

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// TestReflectWithTokenBudgetGatesEveryCall: Opts.MaxTokens is checked
// before EVERY model call, not only before the first - the loop stops
// with ErrBudgetExhausted instead of starting the next call, and the
// usage consumed so far is retained on the returned Answer so a budget
// caller never loses spend with the error.
func TestReflectWithTokenBudgetGatesEveryCall(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, memory.WriteInput{Namespace: "acme", Key: "/raw/incident-42", Body: "pool size hit max at 14:02 during deploy"}); err != nil {
		t.Fatal(err)
	}
	script := &scriptedLLM{t: t}
	script.queue = []func([]llm.Turn) (*llm.Result, error){
		// round 1: recall (lands 15 tokens, reaching the 15-token bound)
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolRecall, Args: json.RawMessage(`{"query":"pool"}`)}})
		},
		// no round 2: the budget gate must stop the loop first
	}
	e := New(s, script)
	ans, err := e.ReflectWith(ctx, "acme", "what happened to the pool?", Opts{MaxTokens: 15})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if script.calls != 1 {
		t.Fatalf("model calls = %d, want 1: the pre-call gate stopped the second call", script.calls)
	}
	if ans.Tokens != 15 {
		t.Fatalf("tokens = %d, want 15: usage consumed before the stop is retained", ans.Tokens)
	}
}

// TestReflectWithZeroBudgetKeepsUnboundedBehavior: the default
// Opts.MaxTokens (0) changes nothing - the loop runs to done with no
// gate.
func TestReflectWithZeroBudgetKeepsUnboundedBehavior(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	script := &scriptedLLM{t: t}
	script.queue = []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{"answer": "done here", "citations": []string{}})
			return result([]llm.ToolCall{{ID: "c1", Name: toolDone, Args: args}})
		},
	}
	e := New(s, script)
	ans, err := e.ReflectWith(ctx, "acme", "anything", Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Text != "done here" || script.calls != 1 {
		t.Fatalf("answer = %+v calls = %d, want the ungated default loop", ans, script.calls)
	}
}
