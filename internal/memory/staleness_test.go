package memory

import (
	"context"
	"testing"
	"time"
)

type countingSummarizer struct{ calls int }

func (c *countingSummarizer) Summarize(_ context.Context, facts []Fact) (string, error) {
	c.calls++
	return "summary", nil
}

func TestConsolidateSkipsWhenNothingNew(t *testing.T) {
	s := newTestStore(t) // must use an injectable clock; if newTestStore
	// fixes time, follow memory_test.go's pattern for advancing it
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "fact one"}); err != nil {
		t.Fatal(err)
	}
	sum := &countingSummarizer{}
	if _, err := s.Consolidate(ctx, "ns", 24*time.Hour, sum); err != nil {
		t.Fatal(err)
	}
	if sum.calls != 1 {
		t.Fatalf("first consolidate calls = %d, want 1", sum.calls)
	}
	// nothing new since: second run must not summarize
	res, err := s.Consolidate(ctx, "ns", 24*time.Hour, sum)
	if err != nil {
		t.Fatal(err)
	}
	if sum.calls != 1 || res.Summarized != 0 {
		t.Fatalf("stale run: calls=%d summarized=%d, want 1, 0", sum.calls, res.Summarized)
	}
	// new fact => summarize again (advance the injected clock first so
	// the new fact's created_at is strictly after the summary's)
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "fact two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consolidate(ctx, "ns", 24*time.Hour, sum); err != nil {
		t.Fatal(err)
	}
	if sum.calls != 2 {
		t.Fatalf("after new fact calls = %d, want 2", sum.calls)
	}
}
