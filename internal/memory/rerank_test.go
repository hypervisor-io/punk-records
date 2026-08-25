package memory

import (
	"context"
	"errors"
	"testing"
)

// fakeReranker scores bodies by exact string lookup so tests can assert
// specific inversions without any network dependency.
type fakeReranker struct {
	scores map[string]float64
	err    error
}

func (f *fakeReranker) Rerank(ctx context.Context, query string, bodies []string) ([]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]float64, len(bodies))
	for i, b := range bodies {
		out[i] = f.scores[b]
	}
	return out, nil
}

const bodyA = "postgres primary failover runbook"
const bodyB = "postgres primary failover checklist"

// seedAB writes two facts where fusion (fts + importance boost) ranks A
// ahead of B, so reranker tests can invert that order deterministically.
func seedAB(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: bodyA, Importance: 1.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: bodyB}); err != nil {
		t.Fatal(err)
	}
}

func TestHybridSearchRerankedReorders(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedAB(t, s, ctx)

	base, err := s.HybridSearchScored(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 2 || base[0].Key != "/a" {
		t.Fatalf("fusion baseline = %+v, want /a first (importance boost)", base)
	}

	s.SetReranker(&fakeReranker{scores: map[string]float64{
		bodyA: 0.1, // A: low cross-encoder score
		bodyB: 0.9, // B: high cross-encoder score, inverts fusion order
	}})

	res, err := s.HybridSearchReranked(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	if res[0].Key != "/b" || res[1].Key != "/a" {
		t.Fatalf("order = [%s, %s], want [/b, /a] (rerank inverted fusion order)", res[0].Key, res[1].Key)
	}
	if res[0].Components["rerank"] != 0.9 || res[1].Components["rerank"] != 0.1 {
		t.Fatalf("rerank components = %v / %v", res[0].Components, res[1].Components)
	}
	if res[0].Components["fts"] <= 0 || res[1].Components["fts"] <= 0 {
		t.Fatalf("fts component dropped after rerank: %v / %v", res[0].Components, res[1].Components)
	}
}

func TestHybridSearchRerankedNilPassthrough(t *testing.T) {
	s := newTestStore(t) // no reranker set
	ctx := t.Context()
	seedAB(t, s, ctx)

	scored, err := s.HybridSearchScored(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	reranked, err := s.HybridSearchReranked(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reranked) != len(scored) {
		t.Fatalf("len = %d, want %d", len(reranked), len(scored))
	}
	for i := range scored {
		if reranked[i].Key != scored[i].Key {
			t.Fatalf("order[%d] = %s, want %s (nil reranker must not reorder)", i, reranked[i].Key, scored[i].Key)
		}
		if _, ok := reranked[i].Components["rerank"]; ok {
			t.Fatalf("result[%d] has rerank component with no reranker configured", i)
		}
	}
}

func TestHybridSearchRerankedFallback(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedAB(t, s, ctx)

	base, err := s.HybridSearchScored(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	s.SetReranker(&fakeReranker{err: errors.New("cross-encoder unreachable")})

	res, err := s.HybridSearchReranked(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatalf("reranker error must not surface: %v", err)
	}
	if len(res) != len(base) {
		t.Fatalf("len = %d, want %d", len(res), len(base))
	}
	for i := range base {
		if res[i].Key != base[i].Key {
			t.Fatalf("fallback order[%d] = %s, want %s (fusion order)", i, res[i].Key, base[i].Key)
		}
		if _, ok := res[i].Components["rerank"]; ok {
			t.Fatalf("result[%d] has rerank component despite reranker error", i)
		}
	}
}
