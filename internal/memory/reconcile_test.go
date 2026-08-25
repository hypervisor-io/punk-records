package memory

import (
	"context"
	"strings"
	"testing"
)

// fakeMergeJudge merges only observations that both mention postgres,
// mirroring the pair the fake embedder makes near-identical.
type fakeMergeJudge struct{}

func (fakeMergeJudge) ShouldMerge(_ context.Context, a, b Observation) (bool, string, error) {
	if strings.Contains(a.Body, "postgres") && strings.Contains(b.Body, "postgres") {
		return true, "team runs postgres as primary datastore (reconciled)", nil
	}
	return false, "", nil
}

func writeObservation(t *testing.T, s *Store, ctx context.Context, key, body string, sourceIDs []any) {
	t.Helper()
	if _, err := s.Write(ctx, WriteInput{
		Namespace: "ns", Key: key, Body: body,
		Attributes: map[string]any{"source_ids": sourceIDs, "proof_count": float64(len(sourceIDs))},
		Writer:     "consolidation",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileObservationsMergesNearDuplicates(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"postgres is the primary datastore":                    {1, 0, 0},
		"team already runs postgres as primary":                {0.99, 0.05, 0},
		"redis handles the cache layer":                        {0, 1, 0},
		"team runs postgres as primary datastore (reconciled)": {0.97, 0.05, 0},
		"postgres is also the system of record":                {0.98, 0.05, 0},
	}})
	ctx := t.Context()
	writeObservation(t, s, ctx, "/observations/postgres-a", "postgres is the primary datastore", []any{"f1"})
	writeObservation(t, s, ctx, "/observations/postgres-b", "team already runs postgres as primary", []any{"f2"})
	writeObservation(t, s, ctx, "/observations/redis-cache", "redis handles the cache layer", []any{"f3"})

	n, err := s.ReconcileObservations(ctx, "ns", 0.9, fakeMergeJudge{})
	if err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v; want 1, nil", n, err)
	}

	got, err := s.Recall(ctx, "ns", "/observations", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("live observations = %d, want 2 (merged + orthogonal): %+v", len(got), got)
	}
	var merged *Fact
	var sawRedis bool
	for i := range got {
		switch got[i].Key {
		case "/observations/redis-cache":
			sawRedis = true
		case "/observations/postgres-a-merged":
			merged = &got[i]
		}
	}
	if !sawRedis {
		t.Fatalf("orthogonal observation was touched: %+v", got)
	}
	if merged == nil {
		t.Fatalf("merged observation not found: %+v", got)
	}
	if merged.Attributes["proof_count"].(float64) != 2 {
		t.Fatalf("proof_count = %v, want 2", merged.Attributes["proof_count"])
	}
	gotIDs := map[string]bool{}
	for _, id := range merged.Attributes["source_ids"].([]any) {
		gotIDs[id.(string)] = true
	}
	if !gotIDs["f1"] || !gotIDs["f2"] {
		t.Fatalf("source_ids = %v, want union of f1,f2", merged.Attributes["source_ids"])
	}

	// len(got)==2 above plus the key checks already prove both originals
	// (postgres-a, postgres-b) were tombstoned, not just superseded.

	// idempotent: nothing left above threshold on a second pass
	n2, err := s.ReconcileObservations(ctx, "ns", 0.9, fakeMergeJudge{})
	if err != nil || n2 != 0 {
		t.Fatalf("second reconcile = %d, %v; want 0, nil", n2, err)
	}

	// A third near-duplicate arrives and gets merged into the survivor on
	// the next tick: the key must stay postgres-a-merged, not restack to
	// postgres-a-merged-merged.
	writeObservation(t, s, ctx, "/observations/postgres-c", "postgres is also the system of record", []any{"f4"})

	n3, err := s.ReconcileObservations(ctx, "ns", 0.9, fakeMergeJudge{})
	if err != nil || n3 != 1 {
		t.Fatalf("third reconcile = %d, %v; want 1, nil", n3, err)
	}

	got3, err := s.Recall(ctx, "ns", "/observations", 10)
	if err != nil {
		t.Fatal(err)
	}
	var merged2 *Fact
	for i := range got3 {
		if got3[i].Key == "/observations/postgres-a-merged" {
			merged2 = &got3[i]
		}
		if got3[i].Key == "/observations/postgres-a-merged-merged" {
			t.Fatalf("merged key restacked a suffix: %+v", got3[i])
		}
	}
	if merged2 == nil {
		t.Fatalf("merged observation not found after third merge: %+v", got3)
	}
	if merged2.Attributes["proof_count"].(float64) != 3 {
		t.Fatalf("proof_count = %v, want 3 (union of f1,f2,f4)", merged2.Attributes["proof_count"])
	}
	gotIDs3 := map[string]bool{}
	for _, id := range merged2.Attributes["source_ids"].([]any) {
		gotIDs3[id.(string)] = true
	}
	if !gotIDs3["f1"] || !gotIDs3["f2"] || !gotIDs3["f4"] {
		t.Fatalf("source_ids = %v, want union of f1,f2,f4", merged2.Attributes["source_ids"])
	}
	for _, k := range got3 {
		if k.Key == "/observations/postgres-c" {
			t.Fatalf("newest near-duplicate original was not tombstoned: %+v", k)
		}
	}
}

func TestReconcileObservationsNoJudgeIsNoop(t *testing.T) {
	s := newTestStore(t)
	n, err := s.ReconcileObservations(t.Context(), "ns", 0.9, nil)
	if err != nil || n != 0 {
		t.Fatalf("nil judge reconcile = %d, %v; want 0, nil", n, err)
	}
}

func TestReconcileObservationsThresholdAtOneIsNoop(t *testing.T) {
	s := newTestStore(t)
	n, err := s.ReconcileObservations(t.Context(), "ns", 1, fakeMergeJudge{})
	if err != nil || n != 0 {
		t.Fatalf("threshold>=1 reconcile = %d, %v; want 0, nil", n, err)
	}
}
