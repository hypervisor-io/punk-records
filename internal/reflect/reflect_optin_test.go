package reflect

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
)

type reviewerH01ToolClient struct{ summaryTool bool }

func (c *reviewerH01ToolClient) Model() string { return "reviewer" }

func (c *reviewerH01ToolClient) Chat(_ context.Context, _ []llm.Turn, tools []llm.Tool) (*llm.Result, error) {
	for _, tool := range tools {
		if tool.Name == "list_summaries" {
			c.summaryTool = true
		}
	}
	return &llm.Result{ToolCalls: []llm.ToolCall{
		{ID: "done", Name: "done", Args: json.RawMessage(`{"answer":""}`)},
	}}, nil
}

// TestReviewerH01DefaultDoesNotEnableSummaries pins that the summary
// retrieval tool is OFF by default: a plain ReflectWith(Opts{}) exposes no
// list_summaries tool, so the copied system prompt and toolset are
// unchanged until an explicit opt-in requests the hierarchy.
func TestReviewerH01DefaultDoesNotEnableSummaries(t *testing.T) {
	c := &reviewerH01ToolClient{}
	if _, err := New(newTestStore(t), c).ReflectWith(t.Context(), "ns", "q", Opts{}); err != nil {
		t.Fatal(err)
	}
	if c.summaryTool {
		t.Fatal("summary retrieval enabled in default toolset without opt-in")
	}
	// The opt-in must offer it, and the default system prompt must not
	// mention summaries while the opt-in prompt does.
	c.summaryTool = false
	if _, err := New(newTestStore(t), c).ReflectWith(t.Context(), "ns", "q", Opts{Summaries: true}); err != nil {
		t.Fatal(err)
	}
	if !c.summaryTool {
		t.Fatal("Opts{Summaries:true} did not enable the list_summaries tool")
	}
}
