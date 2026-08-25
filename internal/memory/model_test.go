package memory

import "testing"

func TestRememberAndListModels(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f, err := s.RememberModel(ctx, "ns", "db-topology", "postgres is primary, redis is cache", []string{"src1", "src2"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if f.Key != "/mental-models/db-topology" {
		t.Fatalf("key = %q, want /mental-models/db-topology", f.Key)
	}

	got, err := s.ListModels(ctx, "ns")
	if err != nil || len(got) != 1 {
		t.Fatalf("list models: %v %v", got, err)
	}
	m := got[0]
	if m.Importance != 0.7 {
		t.Fatalf("importance = %v, want 0.7", m.Importance)
	}
	if pinned, _ := m.Attributes["pinned"].(bool); !pinned {
		t.Fatalf("pinned not round-tripped: %v", m.Attributes["pinned"])
	}
	ids, ok := m.Attributes["source_ids"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "src1" || ids[1] != "src2" {
		t.Fatalf("source_ids not round-tripped: %v", m.Attributes["source_ids"])
	}
	if m.Attributes["refreshed_at"] == nil {
		t.Fatal("refreshed_at not stamped")
	}
}

func TestRememberModelBadSlug(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	// Empty slug -> "/mental-models/" -> ValidateKey rejects (empty
	// trailing segment): must error, and must not write a fact.
	if _, err := s.RememberModel(ctx, "ns", "", "body", nil, false); err == nil {
		t.Fatal("want error for empty slug, got nil")
	}
	got, err := s.ListModels(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no models written, got %d: %+v", len(got), got)
	}
}

func TestModelOutranksObservation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	// Observation written FIRST, model second. Search()'s FTS query has no
	// ORDER BY, so the FTS-arm rank tie-break is insertion-order — writing
	// the observation first gives IT any accidental rank edge, not the
	// model. Combined with matching Importance (0.7, tied with
	// RememberModel's hardcoded 0.7), this makes Components["model"]==1.15
	// the ONLY thing that can put the model on top: removing the model
	// multiply in score() must make this test fail.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/db-topology",
		Body: "postgres primary datastore", Writer: "consolidation", Importance: 0.7}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RememberModel(ctx, "ns", "db-topology", "postgres primary datastore", nil, false); err != nil {
		t.Fatal(err)
	}

	res, err := s.HybridSearchScored(ctx, "ns", "postgres primary datastore", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) < 2 {
		t.Fatalf("want at least 2 hits, got %d: %+v", len(res), res)
	}
	if res[0].Key != "/mental-models/db-topology" {
		t.Fatalf("top hit = %q, want the mental model to outrank the observation: %+v", res[0].Key, res)
	}
	if res[0].Components["model"] != 1.15 {
		t.Fatalf("model component = %v, want 1.15", res[0].Components["model"])
	}
	var obsHit *ScoredFact
	for i := range res {
		if res[i].Key == "/observations/db-topology" {
			obsHit = &res[i]
		}
	}
	if obsHit == nil {
		t.Fatal("observation not in results")
	}
	if _, ok := obsHit.Components["model"]; ok {
		t.Fatalf("observation should not carry a model component: %v", obsHit.Components)
	}
}

// TestModelSurvivesErroneousSupersession is the score.go defense-in-depth
// half of the fix: even if an /observations/* fact's source_ids cites a
// mental model's fact ID (which observe.go now prevents at write time, but
// this guards the read path independently), the model must never be
// dropped from search as "superseded".
func TestModelSurvivesErroneousSupersession(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	model, err := s.RememberModel(ctx, "ns", "db-topology", "postgres primary datastore", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// Body matches the query so this fact is itself a search candidate
	// (the supersede pass only runs over candidates in byID) — that's
	// what makes it actually exercise the superseded-drop path.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/bogus",
		Body: "postgres primary datastore", Writer: "consolidation",
		Attributes: map[string]any{"source_ids": []any{model.ID}, "proof_count": float64(1)}}); err != nil {
		t.Fatal(err)
	}

	res, err := s.HybridSearchScored(ctx, "ns", "postgres primary datastore", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sf := range res {
		if sf.Key == "/mental-models/db-topology" {
			return // survived — found it
		}
	}
	t.Fatalf("mental model dropped as superseded despite being cited in another fact's source_ids: %+v", res)
}
