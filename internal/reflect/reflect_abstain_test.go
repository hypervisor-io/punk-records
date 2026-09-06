package reflect

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// Tests for the E02 reflect surface: the answer-composition reuse path
// needs an explicit abstention signal (a confident non-answer on an
// unanswerable question must be distinguishable from a honest "no
// evidence") and the set of evidence IDs the model was actually shown
// (membench scores citation existence against retrieved/known facts).

// TestReflectAbstainFlag: the model can close the loop with
// done{abstain:true} instead of a fabricated answer; the engine surfaces
// it as Answer.Abstained with an empty text.
func TestReflectAbstainFlag(t *testing.T) {
	mem := newTestStore(t)
	client := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "1", Name: toolRecall, Args: json.RawMessage(`{"query":"flux"}`)}})
		},
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "2", Name: toolDone,
				Args: json.RawMessage(`{"answer":"","abstain":true}`)}})
		},
	}}
	ans, err := New(mem, client).Reflect(context.Background(), "ns", "how is the flux capacitor calibrated?")
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Abstained {
		t.Fatalf("Abstained = false, want true: the model declared no-evidence")
	}
	if ans.Text != "" {
		t.Fatalf("Text = %q, want empty on abstention", ans.Text)
	}
}

// TestReflectAnswerCarriesShownEvidence: the answer must carry the sorted
// set of fact/model IDs the loop actually showed the model this run, so a
// caller (the membench answer stage) can score citation existence against
// retrieved/known facts without trusting the model's claim.
func TestReflectAnswerCarriesShownEvidence(t *testing.T) {
	mem := newTestStore(t)
	ctx := context.Background()
	if _, err := mem.Write(ctx, memory.WriteInput{Namespace: "ns", Key: "/raw/incident-1", Body: "pool exhausted at 14:02"}); err != nil {
		t.Fatal(err)
	}
	fact, err := mem.Write(ctx, memory.WriteInput{Namespace: "ns", Key: "/raw/incident-2", Body: "pool exhausted again at 15:40"})
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "1", Name: toolRecall, Args: json.RawMessage(`{"query":"pool exhausted"}`)}})
		},
		func(_ []llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{"answer": "twice", "citations": []string{fact.ID}})
			return result([]llm.ToolCall{{ID: "2", Name: toolDone, Args: args}})
		},
	}}
	ans, err := New(mem, client).Reflect(ctx, "ns", "how often did the pool exhaust?")
	if err != nil {
		t.Fatal(err)
	}
	if ans.Abstained {
		t.Fatal("Abstained = true on an answered question")
	}
	if len(ans.Evidence) != 2 {
		t.Fatalf("Evidence = %v, want both shown fact IDs", ans.Evidence)
	}
	found := false
	for _, id := range ans.Evidence {
		if id == fact.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("Evidence %v does not contain the cited fact %s", ans.Evidence, fact.ID)
	}
	for i := 1; i < len(ans.Evidence); i++ {
		if ans.Evidence[i-1] > ans.Evidence[i] {
			t.Fatalf("Evidence %v not sorted (deterministic artifact)", ans.Evidence)
		}
	}
}
