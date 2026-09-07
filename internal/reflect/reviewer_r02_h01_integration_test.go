package reflect

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"strings"
	"testing"
)

type reviewerCombinedClient struct {
	t                 *testing.T
	expand, summaries bool
}

func (c *reviewerCombinedClient) Model() string { return "synthetic-integration" }
func (c *reviewerCombinedClient) Chat(_ context.Context, turns []llm.Turn, offered []llm.Tool) (*llm.Result, error) {
	c.t.Helper()
	names := map[string]int{}
	for _, tool := range offered {
		names[tool.Name]++
	}
	for name, want := range map[string]bool{"expand": c.expand, "list_summaries": c.summaries} {
		if got := names[name] > 0; got != want {
			c.t.Fatalf("tool %s enabled=%v, want %v", name, got, want)
		}
	}
	for name, n := range names {
		if n != 1 {
			c.t.Fatalf("duplicate %s: %d", name, n)
		}
	}
	if names["done"] != 1 || names["recall"] != 1 || names["list_models"] != 1 || names["list_observations"] != 1 {
		c.t.Fatalf("base tools lost: %v", names)
	}
	system := turns[0].Content
	if got := strings.Contains(system, "list_summaries"); got != c.summaries {
		c.t.Fatalf("summary prompt enabled=%v, want %v", got, c.summaries)
	}
	if got := strings.Contains(system, expansionPrompt); got != c.expand {
		c.t.Fatalf("expansion prompt enabled=%v, want %v", got, c.expand)
	}
	if !c.expand && !c.summaries && system != systemPrompt {
		c.t.Fatal("default prompt changed")
	}
	return &llm.Result{PromptTokens: 1, CompletionTokens: 1, ToolCalls: []llm.ToolCall{{ID: "done", Name: "done", Args: json.RawMessage(`{"answer":"","abstain":true}`)}}}, nil
}
func TestReviewerR02H01IndependentOptIns(t *testing.T) {
	for _, expand := range []bool{false, true} {
		for _, summaries := range []bool{false, true} {
			t.Run(fmt.Sprintf("expand=%v/summaries=%v", expand, summaries), func(t *testing.T) {
				c := &reviewerCombinedClient{t: t, expand: expand, summaries: summaries}
				_, err := New(newTestStore(t), c).ReflectWith(t.Context(), "ns", "q", Opts{ExpandEvidence: expand, Summaries: summaries})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
