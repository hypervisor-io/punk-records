package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeObserver struct{}

func (fakeObserver) Observe(_ context.Context, facts []Fact) ([]Observation, error) {
	// consolidate every fact mentioning "postgres" into one belief
	var ids []string
	for _, f := range facts {
		if strings.Contains(f.Body, "postgres") {
			ids = append(ids, f.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return []Observation{{Slug: "postgres-ops", Body: "team runs postgres as primary datastore", SourceIDs: ids}}, nil
}

func TestConsolidateObservations(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for k, b := range map[string]string{
		"/a": "postgres primary on node7",
		"/b": "postgres failover drill passed",
		"/c": "redis cache on node9",
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ConsolidateObservations(ctx, "ns", fakeObserver{})
	if err != nil || n != 1 {
		t.Fatalf("wrote %d observations (err %v), want 1", n, err)
	}
	got, err := s.Recall(ctx, "ns", "/observations", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("recall observations: %v %v", got, err)
	}
	if got[0].Attributes["proof_count"].(float64) != 2 {
		t.Fatalf("proof_count = %v, want 2", got[0].Attributes["proof_count"])
	}
}

func TestPreferObservationsSupersedesSources(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "postgres primary on node7"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns",
		Key:  "/observations/postgres-ops",
		Body: "team runs postgres as primary datastore",
		Attributes: map[string]any{
			"source_ids": []any{f1.ID}, "proof_count": float64(1)},
		Writer: "consolidation"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.HybridSearchScored(ctx, "ns", "postgres primary", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sf := range res {
		if sf.ID == f1.ID {
			t.Fatalf("raw source fact surfaced alongside its observation: %+v", res)
		}
	}
	found := false
	for _, sf := range res {
		if sf.Key == "/observations/postgres-ops" {
			found = true
			if sf.Components["proof"] == 0 {
				t.Fatalf("observation missing proof component: %v", sf.Components)
			}
		}
	}
	if !found {
		t.Fatal("observation not in results")
	}
}

// TestConsolidateObservationsExcludesModels proves a curated mental model
// is never fed back in as raw evidence: fakeObserver folds every fact
// whose body mentions "postgres" into one belief, so if the model leaked
// into the raw pool its ID would show up in source_ids.
func TestConsolidateObservationsExcludesModels(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "postgres primary on node7"})
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.RememberModel(ctx, "ns", "db-topology", "postgres is the modeled primary", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.ConsolidateObservations(ctx, "ns", fakeObserver{})
	if err != nil || n != 1 {
		t.Fatalf("wrote %d observations (err %v), want 1", n, err)
	}
	got, err := s.Recall(ctx, "ns", "/observations", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("recall observations: %v %v", got, err)
	}
	ids, _ := got[0].Attributes["source_ids"].([]any)
	for _, id := range ids {
		if id == model.ID {
			t.Fatalf("model fact %q leaked into observation source_ids: %v", model.ID, ids)
		}
	}
	if len(ids) != 1 || ids[0] != f1.ID {
		t.Fatalf("source_ids = %v, want only the raw fact %q", ids, f1.ID)
	}
}

// writeRawPair writes the two facts fakeObserver folds into one belief.
func writeRawPair(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	for k, b := range map[string]string{
		"/a": "postgres primary on node7",
		"/b": "postgres failover drill passed",
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestObservationStaleFlag(t *testing.T) {
	s, clock := newTestStoreWithClock(t)
	ctx := t.Context()
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	// raw facts and consolidation all land in the same wall-clock second —
	// consolidated_at is ns-precision (store.TimeToDB), so it still orders
	// strictly after the facts it consolidated even without a full-second gap.
	clock.Set(base)
	writeRawPair(t, s, ctx)
	if n, err := s.ConsolidateObservations(ctx, "ns", fakeObserver{}); err != nil || n != 1 {
		t.Fatalf("consolidate: n=%d err=%v", n, err)
	}
	got, err := s.Recall(ctx, "ns", "/observations", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("recall observations: %v %v", got, err)
	}
	obs := got[0]
	if obs.Attributes["consolidated_at"] == nil {
		t.Fatal("consolidated_at not stamped")
	}

	stale, err := s.ObservationStale(ctx, "ns", obs)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("freshly consolidated observation reported stale (same-second false positive)")
	}

	// a raw fact newer than consolidation but already expired by the time
	// we check must not count as live evidence of staleness.
	exp := base.Add(500 * time.Millisecond)
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/expired", Body: "postgres blip",
		ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	clock.Set(base.Add(time.Second)) // now is past the expiry
	stale, err = s.ObservationStale(ctx, "ns", obs)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("expired raw fact should not mark observation stale")
	}

	// a genuinely newer, live raw fact does make it stale
	clock.Set(base.Add(2 * time.Second))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/c", Body: "postgres restore drill logged"}); err != nil {
		t.Fatal(err)
	}
	stale, err = s.ObservationStale(ctx, "ns", obs)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("observation with newer raw fact reported fresh")
	}
}

func TestRecallMarksStaleObservation(t *testing.T) {
	s, clock := newTestStoreWithClock(t)
	ctx := t.Context()
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	clock.Set(base)
	writeRawPair(t, s, ctx)
	if _, err := s.ConsolidateObservations(ctx, "ns", fakeObserver{}); err != nil {
		t.Fatal(err)
	}

	res, err := s.HybridSearchScored(ctx, "ns", "postgres primary", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sf := range res {
		if sf.Key == "/observations/postgres-ops" {
			found = true
			if sf.Components["stale"] != 0 {
				t.Fatalf("fresh observation marked stale: %v", sf.Components)
			}
		}
	}
	if !found {
		t.Fatal("fresh observation not in results")
	}

	// newer raw fact after consolidation => stale on the next recall
	clock.Set(base.Add(2 * time.Second))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/c", Body: "postgres restore drill logged"}); err != nil {
		t.Fatal(err)
	}
	res, err = s.HybridSearchScored(ctx, "ns", "postgres primary", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, sf := range res {
		if sf.Key == "/observations/postgres-ops" {
			found = true
			if sf.Components["stale"] != 1 {
				t.Fatalf("stale observation missing stale=1: %v", sf.Components)
			}
		}
	}
	if !found {
		t.Fatal("stale observation not in results")
	}
}

// kindObserver emits one observation per configured entry, for pinning
// the kind allowlist and the inductive two-source rule.
type kindObserver struct{ out []Observation }

func (k kindObserver) Observe(_ context.Context, _ []Fact) ([]Observation, error) {
	return k.out, nil
}

// TestObservationKinds pins the reasoning-kind contract:
// valid kinds persist as the "kind" attribute, unknown kinds drop to
// unlabeled (still written), and an inductive belief citing fewer than
// two sources is dropped entirely - one data point cannot exhibit a
// pattern.
func TestObservationKinds(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "postgres primary on node7"})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "postgres failover drill passed"})
	if err != nil {
		t.Fatal(err)
	}

	obs := kindObserver{out: []Observation{
		{Slug: "explicit-ok", Body: "b1", SourceIDs: []string{f1.ID}, Kind: "explicit"},
		{Slug: "inductive-ok", Body: "b2", SourceIDs: []string{f1.ID, f2.ID}, Kind: "inductive"},
		{Slug: "inductive-thin", Body: "b3", SourceIDs: []string{f1.ID}, Kind: "inductive"},
		{Slug: "weird-kind", Body: "b4", SourceIDs: []string{f2.ID}, Kind: "vibes"},
	}}
	n, err := s.ConsolidateObservations(ctx, "ns", obs)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("written = %d, want 3 (inductive-thin dropped)", n)
	}

	got, err := s.Recall(ctx, "ns", "/observations/", 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, f := range got {
		kind, _ := f.Attributes["kind"].(string)
		kinds[f.Key] = kind
	}
	if kinds["/observations/explicit-ok"] != "explicit" {
		t.Fatalf("explicit kind = %q", kinds["/observations/explicit-ok"])
	}
	if kinds["/observations/inductive-ok"] != "inductive" {
		t.Fatalf("inductive kind = %q", kinds["/observations/inductive-ok"])
	}
	if _, exists := kinds["/observations/inductive-thin"]; exists {
		t.Fatal("single-source inductive observation must be dropped")
	}
	if kinds["/observations/weird-kind"] != "" {
		t.Fatalf("unknown kind must drop to unlabeled, got %q", kinds["/observations/weird-kind"])
	}
}
