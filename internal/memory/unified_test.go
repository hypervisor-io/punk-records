package memory

import "testing"

// TestUnifiedSearchFusesFactsAndRelations proves UnifiedSearch surfaces
// both a "fact" hit and a "relation" hit for the same query, RRF-scored
// and sorted descending — the whole point of folding triplet
// search into one recall entry point.
func TestUnifiedSearchFusesFactsAndRelations(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"fact alpha":                 {1, 0, 0},
		"fact beta":                  {1, 0, 0},
		"alpha relates to beta":      {1, 0, 0},
		"query about alpha and beta": {1, 0, 0},
	}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "fact alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "fact beta"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLinkDescribed(ctx, "ns", "/a", "/b", "relates_to", 1.0, "alpha relates to beta"); err != nil {
		t.Fatal(err)
	}

	got, err := s.UnifiedSearch(ctx, "ns", "query about alpha and beta", 10)
	if err != nil {
		t.Fatal(err)
	}
	var haveFact, haveRelation bool
	for i, h := range got {
		if h.Kind == "fact" {
			haveFact = true
			if h.Fact == nil || h.Triplet != nil {
				t.Fatalf("hit %d: fact kind must set Fact only, got %+v", i, h)
			}
		}
		if h.Kind == "relation" {
			haveRelation = true
			if h.Triplet == nil || h.Fact != nil {
				t.Fatalf("hit %d: relation kind must set Triplet only, got %+v", i, h)
			}
		}
		if i > 0 && got[i-1].Score < h.Score {
			t.Fatalf("not sorted desc by Score: %+v then %+v", got[i-1], h)
		}
	}
	if !haveFact {
		t.Fatal("no fact hit in unified results")
	}
	if !haveRelation {
		t.Fatal("no relation hit in unified results")
	}
}

// TestUnifiedSearchNoEmbedder proves that without an embedder,
// TripletSearch contributes nothing and UnifiedSearch degrades to the
// HybridSearchScored ranking wrapped as UnifiedHit, in the same order.
func TestUnifiedSearchNoEmbedder(t *testing.T) {
	s := newTestStore(t) // no embedder
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "postgres primary failover runbook"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "postgres primary failover checklist", Importance: 1.0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "leads_to"); err != nil {
		t.Fatal(err)
	}

	want, err := s.HybridSearchScored(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UnifiedSearch(ctx, "ns", "postgres failover", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d hits, want %d", len(got), len(want))
	}
	for i, h := range got {
		if h.Kind != "fact" {
			t.Fatalf("hit %d kind = %s, want fact (no embedder -> no relations)", i, h.Kind)
		}
		if h.Fact == nil || h.Fact.Key != want[i].Key {
			t.Fatalf("hit %d key = %v, want %s (order must match HybridSearchScored)", i, h.Fact, want[i].Key)
		}
	}
}
