package memory

import (
	"context"
	"testing"
)

// reviewerP01TypedExtractor is the reviewer's G01-compatibility fixture:
// a StructuredEntityExtractor citing the exact source revision ID, the
// shape the pipeline's entity stage must route through the typed apply
// path (canonical /entities/<type>/<slug> keys, revision provenance).
type reviewerP01TypedExtractor struct{}

func (reviewerP01TypedExtractor) Extract(context.Context, string) ([]string, error) {
	return []string{"Mercury"}, nil
}

func (reviewerP01TypedExtractor) ExtractStructured(_ context.Context, src []EntitySource) ([]ExtractedEntity, error) {
	return []ExtractedEntity{{Name: "Mercury", Type: "service", SourceFacts: []string{src[0].ID}}}, nil
}

// TestReviewerP01PipelinePreservesTypedExtraction pins that the durable
// entity stage never bypasses G01 typed extraction: a structured
// extractor flushed through flushEntityStage must produce the typed
// mentions edge, not the legacy untyped /entities/<slug> path.
func TestReviewerP01PipelinePreservesTypedExtraction(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service"}); err != nil {
		t.Fatal(err)
	}
	s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())
	links, err := s.Neighbors(ctx, "ns", "/f", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/service/mercury" {
			return
		}
	}
	t.Fatalf("pipeline bypassed G01 typed extraction: links=%v", links)
}
