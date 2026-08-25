package memory

import (
	"fmt"
	"math"
	"testing"
)

func TestHybridSearchScoredComponents(t *testing.T) {
	s := newTestStore(t) // no embedder: vector component 0, fts drives
	ctx := t.Context()
	mustWrite := func(key, body string, imp float64) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key, Body: body, Importance: imp}); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("/a", "postgres primary failover runbook", 0)
	mustWrite("/b", "postgres primary failover checklist", 1.0)

	res, err := s.HybridSearchScored(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	// importance 1.0 => multiplier 1.3 beats equal-relevance sibling
	if res[0].Key != "/b" {
		t.Fatalf("top = %s, want /b (importance boost)", res[0].Key)
	}
	c := res[0].Components
	if c["fts"] <= 0 || c["importance"] != 1.3 || c["recency"] != 1.0 || c["access"] != 1.0 {
		t.Fatalf("components = %v", c)
	}
	want := (c["fts"] + c["vector"]) * c["recency"] * c["importance"] * c["access"]
	if math.Abs(res[0].Score-want) > 1e-12 {
		t.Fatalf("Score %v != product of components %v", res[0].Score, want)
	}
}

// TestHybridSearchScoredPerSourceFusionCap proves the FTS arm is capped
// BEFORE the RRF/salience passes run, not just truncated at the final
// limit. Search (sqlite FTS5, no ORDER BY) returns tied-relevance hits
// in write order, so write index == FTS rank deterministically (verified
// empirically against this schema). The fact one past fusionPerSourceCap
// gets the max salience boost (importance=1 -> 1.3x); by design that
// boost is proportional, not a leapfrog, so with a small limit (5) it
// could never out-rank the front of the field regardless of the cap --
// this is exactly what makes the cap a pure cost bound, not a ranking
// change. limit=30 is chosen so the boosted fact WOULD win a top-30 spot
// if it were a candidate at all; the cap's job is to make sure it never
// gets that far.
func TestHybridSearchScoredPerSourceFusionCap(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	const boundaryCap = 50 // must match fusionPerSourceCap in score.go
	const limit = 30
	var boundaryKey string
	for i := 0; i <= boundaryCap; i++ { // 0..50: one fact past the cap boundary
		key := fmt.Sprintf("/f%03d", i)
		imp := 0.0
		if i == boundaryCap {
			imp = 1.0
			boundaryKey = key
		}
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key,
			Body: fmt.Sprintf("widget report %03d", i), Importance: imp}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.HybridSearchScored(ctx, "ns", "widget", limit, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res {
		if f.Key == boundaryKey {
			t.Fatalf("fact ranked %d-th by FTS (beyond the %d cap) leaked into results despite max salience boost",
				boundaryCap+1, boundaryCap)
		}
	}
}

func TestHybridSearchScoredTouchesAccess(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "alpha beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HybridSearchScored(ctx, "ns", "alpha", 10, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Recall(ctx, "ns", "/a", 1)
	if len(got) != 1 || got[0].AccessCount != 1 {
		t.Fatalf("AccessCount after search = %v, want 1", got)
	}
}

func TestReinforceComponent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	in := WriteInput{Namespace: "ns", Key: "/rf", Body: "alpha beta", Writer: "a"}
	if _, err := s.Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, in); err != nil { // reinforce once
		t.Fatal(err)
	}
	hits, err := s.HybridSearchScored(ctx, "ns", "alpha", 5, 0)
	if err != nil || len(hits) != 1 {
		t.Fatal(err, hits)
	}
	c := hits[0].Components["reinforce"]
	if c <= 1.0 {
		t.Fatalf("reinforce component = %v, want > 1.0 after a duplicate write", c)
	}
	// neutral at zero: a never-reinforced fact scores component exactly 1.0
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/rf2", Body: "gamma delta", Writer: "a"}); err != nil {
		t.Fatal(err)
	}
	hits, err = s.HybridSearchScored(ctx, "ns", "gamma", 5, 0)
	if err != nil || len(hits) != 1 {
		t.Fatal(err, hits)
	}
	if hits[0].Components["reinforce"] != 1.0 {
		t.Fatalf("unreinforced component = %v, want exactly 1.0", hits[0].Components["reinforce"])
	}
}

func TestSubtreeDiversification(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	// 5 chunks in one document subtree + 2 facts elsewhere, all matching
	// "zebra". Distinct, strictly-descending Importance (0.45..0.25, all
	// above the /svc facts' default 0) pins the within-subtree rank order
	// deterministically: chunk-0001 > chunk-0002 > ... > chunk-0005 by
	// score, regardless of any FTS tie-break nuance, so the backfill
	// assertion below can name exactly which chunks get deferred.
	chunkKey := func(i int) string { return fmt.Sprintf("/docs/runbook/chunk-%04d", i) }
	for i := 1; i <= 5; i++ {
		imp := 0.5 - float64(i)*0.05
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns",
			Key: chunkKey(i), Body: "zebra fact", Importance: imp, Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"/svc/a", "/svc/b"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "zebra note", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.HybridSearchScored(ctx, "ns", "zebra", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 {
		t.Fatalf("want 5 hits, got %d", len(hits))
	}
	perSub := map[string]int{}
	for _, h := range hits {
		perSub[keySubtree(h.Key)]++
	}
	if perSub["/docs/runbook"] != 3 {
		t.Fatalf("subtree cap: got %d from /docs/runbook, want 3 (both /svc facts must surface)", perSub["/docs/runbook"])
	}
	// the capped-in three must be the highest-importance chunks (1,2,3),
	// not an arbitrary subset - the cap keeps the ranked-best, not the
	// first-written.
	for _, want := range []string{chunkKey(1), chunkKey(2), chunkKey(3)} {
		found := false
		for _, h := range hits {
			if h.Key == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected capped-in chunk %s among the 5 hits, got %+v", want, hits)
		}
	}
	// backfill: limit larger than diversified set refills from the capped
	// remainder. With limit=7 all 5 chunks + both /svc facts fit, but the
	// diversification pass still runs first (out=[chunk1,chunk2,chunk3,
	// svc/a,svc/b], all under the per-subtree cap or in a different
	// subtree) before the overflow backfill appends the two capped-out
	// chunks (4 and 5) in their ranked order - so they land LAST, as
	// hits[5] and hits[6], not interleaved back into score order.
	hits, err = s.HybridSearchScored(ctx, "ns", "zebra", 7, 0)
	if err != nil || len(hits) != 7 {
		t.Fatalf("backfill: want 7 hits, got %d err %v", len(hits), err)
	}
	if hits[5].Key != chunkKey(4) || hits[6].Key != chunkKey(5) {
		t.Fatalf("backfill order: hits[5:7] = [%s, %s], want [%s, %s] (rank-4 then rank-5 chunk, in order)",
			hits[5].Key, hits[6].Key, chunkKey(4), chunkKey(5))
	}
}

// TestBridgeSeedSelectionDeterministic is the reviewer's round-1 repro
// shape: entity-arm-only candidates (fts=vector=0) used to enter the
// bridge seed sort with NO tie-break and, pre-fix, an identical base
// (entity wasn't even folded into the base sum) - so with more candidates
// than the top-20 seed cap, WHICH ones made the cut was decided by Go's
// randomized map-iteration order feeding an unstable sort, not by the
// fact data. The reviewer measured a downstream bridge fact present in
// only 9/30 repeated searches instead of every time. 25 tied entity-only
// candidates (5 more than the seed cap) force an actual cutoff; two
// disjoint pairs of them each feed a distinct bridge candidate so the test
// can observe, deterministically, which side of the cut each pair lands
// on and that the outcome never changes across repeated searches.
func TestBridgeSeedSelectionDeterministic(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/zeta", Body: "Zeta",
		Attributes: map[string]any{"mention_count": 1.0}, Writer: "enricher"}); err != nil {
		t.Fatal(err)
	}

	const n = 25
	keyOf := func(i int) string { return fmt.Sprintf("/notes/e%02d", i) }
	for i := 0; i < n; i++ {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: keyOf(i),
			Body: fmt.Sprintf("candidate content %02d", i), Writer: "t"}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddLinkWeighted(ctx, "ns", keyOf(i), "/entities/zeta", "mentions", 1.0); err != nil {
			t.Fatal(err)
		}
	}

	// Two disjoint bridge candidates, each fed by two of the 25 tied
	// entity-only candidates via a non-mentions link (relates_to) - a
	// bridge fact only surfaces once >=2 of its distinct seed feeders
	// survive the seed cut. b-low is fed by the two lowest-keyed (e00,
	// e01) candidates, which sort first and so must always survive;
	// b-high is fed by the two highest-keyed (e23, e24), which must
	// always be the ones cut.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/bridge/b-low", Body: "unrelated bridge content low", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/bridge/b-high", Body: "unrelated bridge content high", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 1} {
		if err := s.AddLinkWeighted(ctx, "ns", keyOf(i), "/bridge/b-low", "relates_to", 1.0); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []int{n - 2, n - 1} {
		if err := s.AddLinkWeighted(ctx, "ns", keyOf(i), "/bridge/b-high", "relates_to", 1.0); err != nil {
			t.Fatal(err)
		}
	}

	bridgePresence := func() (lowPresent, highPresent bool) {
		t.Helper()
		// limit=30 comfortably exceeds the <=27 possible candidates (25
		// entity matches + 2 bridge facts) so a qualifying bridge fact is
		// never trimmed by the final limit - this isolates the assertion
		// to whether each bridge fact clears the >=2-distinct-seed bridge
		// threshold at all, not whether it also wins a place in a small
		// top-K.
		hits, err := s.HybridSearchScored(ctx, "ns", "zeta", 30, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.Key == "/bridge/b-low" {
				lowPresent = true
			}
			if h.Key == "/bridge/b-high" {
				highPresent = true
			}
		}
		return
	}

	wantLow, wantHigh := bridgePresence()
	if !wantLow || wantHigh {
		t.Fatalf("expected only the low-keyed feeder pair's bridge fact to survive the seed cut: b-low present=%v, b-high present=%v",
			wantLow, wantHigh)
	}
	for i := 0; i < 10; i++ {
		gotLow, gotHigh := bridgePresence()
		if gotLow != wantLow || gotHigh != wantHigh {
			t.Fatalf("bridge presence unstable across repeated searches: run 0 = (low=%v,high=%v), run %d = (low=%v,high=%v)",
				wantLow, wantHigh, i, gotLow, gotHigh)
		}
	}
}

// TestBridgeIDsTieBreakStable covers the final ranking pass's ("Part B")
// ids sort: two bridge-only facts with an IDENTICAL contribution (same two
// seeds, same link type and weight) tie exactly on Score, and the
// tie-break (key ascending) must place them in a stable, repeatable order
// rather than however an unstable sort happens to leave exact ties.
func TestBridgeIDsTieBreakStable(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/seed/s1", Body: "gizmo primary", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/seed/s2", Body: "gizmo secondary", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/bridge/b-a", Body: "unrelated bridge content a", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/bridge/b-b", Body: "unrelated bridge content b", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []string{"/seed/s1", "/seed/s2"} {
		for _, bridge := range []string{"/bridge/b-a", "/bridge/b-b"} {
			if err := s.AddLinkWeighted(ctx, "ns", seed, bridge, "relates_to", 1.0); err != nil {
				t.Fatal(err)
			}
		}
	}

	order := func() (idxA, idxB int) {
		t.Helper()
		idxA, idxB = -1, -1
		hits, err := s.HybridSearchScored(ctx, "ns", "gizmo", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		for i, h := range hits {
			if h.Key == "/bridge/b-a" {
				idxA = i
			}
			if h.Key == "/bridge/b-b" {
				idxB = i
			}
		}
		return
	}

	idxA, idxB := order()
	if idxA < 0 || idxB < 0 {
		t.Fatalf("both tied bridge facts must surface: idxA=%d idxB=%d", idxA, idxB)
	}
	if idxA >= idxB {
		t.Fatalf("tied bridge facts must break ties key-ascending: /bridge/b-a (idx %d) should rank before /bridge/b-b (idx %d)", idxA, idxB)
	}
	for i := 0; i < 10; i++ {
		gotA, gotB := order()
		if gotA != idxA || gotB != idxB {
			t.Fatalf("tie-break order unstable across repeated searches: run 0 = (%d,%d), run %d = (%d,%d)", idxA, idxB, i, gotA, gotB)
		}
	}
}

func TestKeySubtree(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/docs/runbook/chunk-0001", "/docs/runbook"},
		{"/docs/runbook", "/docs/runbook"},
		{"/a", "/a"},
	}
	for _, c := range cases {
		if got := keySubtree(c.in); got != c.want {
			t.Errorf("keySubtree(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
