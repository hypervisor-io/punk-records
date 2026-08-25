package memory

import (
	"testing"
	"time"
)

// TestVectorSearchExcludesExpiredFacts proves liveVectorRows applies the
// same expiration_date predicate as every other live read path (Recall,
// liveByKeys, unified search): a fact past its per-fact expiration_date
// must not surface via VectorSearch or HybridSearchScored, even though it
// still has a live (non-tombstoned) revision. Without this predicate,
// expired facts - including 30d-TTL agent captures holding prompts and
// tool payloads - keep surfacing through the vector and hybrid arms.
func TestVectorSearchExcludesExpiredFacts(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	fe := &fakeEmbedder{m: map[string][]float32{
		"expired secret token":   {1, 0, 0},
		"still valid credential": {1, 0, 0},
		"secret":                 {1, 0, 0},
	}}
	s.SetEmbedder(fe)

	exp := clk.t.Add(time.Hour)
	if _, err := s.Write(ctx, WriteInput{
		Namespace: "ns", Key: "/expired", Body: "expired secret token",
		Author: "w", Writer: "w", ExpiresAt: &exp,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "ns", "/keep", "still valid credential", nil, "w"); err != nil {
		t.Fatal(err)
	}

	clk.t = clk.t.Add(2 * time.Hour) // past /expired's expiration_date

	out, err := s.VectorSearch(ctx, "ns", "secret", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range out {
		if f.Key == "/expired" {
			t.Fatalf("VectorSearch returned expired fact: %+v", out)
		}
	}
	if len(out) != 1 || out[0].Key != "/keep" {
		t.Fatalf("VectorSearch = %+v, want only /keep", out)
	}

	scored, err := s.HybridSearchScored(ctx, "ns", "secret", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sf := range scored {
		if sf.Key == "/expired" {
			t.Fatalf("HybridSearchScored returned expired fact: %+v", scored)
		}
	}
	if len(scored) != 1 || scored[0].Key != "/keep" {
		t.Fatalf("HybridSearchScored = %+v, want only /keep", scored)
	}
}
