package memory

import (
	"context"
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

type recordingEmbedder struct {
	inputs []string
	dims   int
}

func (r *recordingEmbedder) Dims() int { return r.dims }
func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.inputs = append(r.inputs, texts...)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, r.dims)
		out[i][0] = 1
	}
	return out, nil
}

func TestWriteEmbedsKeyedInput(t *testing.T) {
	s := newTestStore(t)
	rec := &recordingEmbedder{dims: 2}
	s.SetEmbedder(rec)
	if _, err := s.Write(context.Background(), WriteInput{Namespace: "ns", Key: "/svc/db", Body: "primary is pg14"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.inputs) != 1 || rec.inputs[0] != "key: /svc/db\nprimary is pg14" {
		t.Fatalf("embed inputs = %q", rec.inputs)
	}
}

func TestBackfillAndReembedUseKeyedInput(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingEmbedder{dims: 2}
	s.SetEmbedder(rec)
	n, err := s.BackfillEmbeddings(ctx, "ns", 64)
	if err != nil || n != 1 {
		t.Fatalf("backfill = %d %v", n, err)
	}
	if rec.inputs[0] != "key: /a\none" {
		t.Fatalf("backfill input = %q", rec.inputs[0])
	}
	rec.inputs = nil
	n, err = s.ReembedAll(ctx, "ns", 64)
	if err != nil || n != 1 {
		t.Fatalf("reembed = %d %v", n, err)
	}
	if len(rec.inputs) != 1 || rec.inputs[0] != "key: /a\none" {
		t.Fatalf("reembed inputs = %q", rec.inputs)
	}
	if n, _ := s.BackfillEmbeddings(ctx, "ns", 64); n != 0 {
		t.Fatalf("nothing should be missing after reembed, got %d", n)
	}
}
