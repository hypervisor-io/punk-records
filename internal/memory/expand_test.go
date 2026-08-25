package memory

import (
	"context"
	"errors"
	"testing"
)

type fakeExpander struct {
	out []string
	err error
}

func (f fakeExpander) Expand(context.Context, string) ([]string, error) { return f.out, f.err }

func TestHybridSearchExpanded(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "login tokens rotate hourly", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	// original query only matches /a; reformulation matches /b
	hits, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, fakeExpander{out: []string{"login tokens"}})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, h := range hits {
		keys[h.Key] = true
	}
	if !keys["/a"] || !keys["/b"] {
		t.Fatalf("expanded search must union variants: %v", keys)
	}
	// nil expander: identical to plain scored search
	plain, err := s.HybridSearchReranked(ctx, "ns", "authentication", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	nilExp, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, nil)
	if err != nil || len(nilExp) != len(plain) || nilExp[0].ID != plain[0].ID {
		t.Fatalf("nil expander must degrade to plain: %v vs %v", nilExp, plain)
	}
	// expander error: degrade to plain, no error
	if _, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, fakeExpander{err: errors.New("boom")}); err != nil {
		t.Fatalf("expander failure must degrade, not fail: %v", err)
	}
}

// TestHybridSearchExpandedTruncatesToThree pins the "up to 3 reformulations"
// contract: an expander returning 5 reformulations must only have its first
// 3 consulted. Reformulations 4 and 5 are the only way to reach /e, so /e's
// absence from the results proves refs 4-5 were never searched.
func TestHybridSearchExpandedTruncatesToThree(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/e", Body: "zephyr quokka narwhal marmoset", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	exp := fakeExpander{out: []string{
		"unrelated one",
		"unrelated two",
		"unrelated three",
		"zephyr quokka",    // 4th - must be dropped
		"narwhal marmoset", // 5th - must be dropped
	}}
	hits, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Key == "/e" {
			t.Fatalf("reformulation beyond the 3rd must not be searched, but /e (only reachable via ref 4/5) appeared: %v", hits)
		}
	}
}

// TestHybridSearchExpandedDedupesAndRespectsLimit pins two more contracts:
// a fact matched by both the original query and a reformulation appears at
// most once (max-merge, not concat), and the limit is still honored after
// the union.
func TestHybridSearchExpandedDedupesAndRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// /a matches both the original query ("authentication") and the
	// reformulation ("jwt tokens") via the shared word "jwt".
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "login tokens rotate hourly", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	exp := fakeExpander{out: []string{"jwt tokens"}}

	hits, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, h := range hits {
		seen[h.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate ID %s appeared %d times in expanded results: %v", id, n, hits)
		}
	}
	if len(hits) < 2 {
		t.Fatalf("expected both /a and /b to surface (union), got %v", hits)
	}

	// limit respected after union: two candidates exist, ask for 1.
	limited, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 1, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit=1 must be respected post-merge, got %d hits: %v", len(limited), limited)
	}
}

// TestHybridSearchExpandedRerankOverridesFusionOrder pins finding 1: with a
// reranker configured, the merged multi-variant output must follow the
// cross-encoder's scores against the ORIGINAL query, not revert to
// fusion-Score order. /b only surfaces via the reformulation (so it must
// be merged in), and the fake reranker scores /b above /a; a mutation
// that re-sorts the merge by fusion Score instead of applying the single
// rerank pass would put /a first.
func TestHybridSearchExpandedRerankOverridesFusionOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: bodyA, Importance: 1.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: bodyB}); err != nil {
		t.Fatal(err)
	}
	// original query only matches /a directly; the reformulation is what
	// pulls /b into the merge (mirrors seedAB's fusion baseline: /a ranks
	// ahead of /b on fusion score alone, via the importance boost).
	exp := fakeExpander{out: []string{"postgres failover checklist"}}

	base, err := s.HybridSearchExpanded(ctx, "ns", "postgres failover", 10, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 2 || base[0].Key != "/a" {
		t.Fatalf("fusion baseline = %+v, want /a first (importance boost, no reranker yet)", base)
	}

	s.SetReranker(&fakeReranker{scores: map[string]float64{
		bodyA: 0.1, // low cross-encoder score
		bodyB: 0.9, // high cross-encoder score, inverts fusion order
	}})

	res, err := s.HybridSearchExpanded(ctx, "ns", "postgres failover", 10, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	if res[0].Key != "/b" || res[1].Key != "/a" {
		t.Fatalf("order = [%s, %s], want [/b, /a] (single rerank pass over the merged set, against the original query, must win over fusion order)", res[0].Key, res[1].Key)
	}
	if res[0].Components["rerank"] != 0.9 || res[1].Components["rerank"] != 0.1 {
		t.Fatalf("rerank components = %v / %v, want 0.9/0.1", res[0].Components, res[1].Components)
	}
}

// TestHybridSearchExpandedMaxMergePicksHigherScore pins finding 2: the
// max-merge must keep the higher-scoring per-ID entry, not whichever
// search happened to run first or last. /a is reachable via both the
// original query and the reformulation; TouchAccess (called at the end of
// every HybridSearchScored) bumps /a's access count after the base
// search, so the variant search - which runs after - sees a strictly
// higher access count and therefore a strictly higher fusion Score for
// /a. A "first entry wins" merge bug would keep the base search's lower
// score instead.
func TestHybridSearchExpandedMaxMergePicksHigherScore(t *testing.T) {
	ctx := context.Background()
	write := func(t *testing.T, s *Store) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt tokens", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}

	// Twin store, never searched: /a's fusion score here reflects access
	// count 0 - exactly the state HybridSearchExpanded's own base search
	// (below) sees, since TouchAccess only bumps access AFTER scoring.
	twin := newTestStore(t)
	write(t, twin)
	baseline, err := twin.HybridSearchScored(ctx, "ns", "authentication", 10, 0)
	if err != nil || len(baseline) != 1 || baseline[0].Key != "/a" {
		t.Fatalf("twin baseline setup: %v, err=%v", baseline, err)
	}
	baseScore := baseline[0].Score

	// Real store: "jwt tokens" also matches /a. Inside HybridSearchExpanded
	// the base search runs first (scores /a at access 0, matching the
	// twin, then bumps access to 1); the variant search runs after
	// (scores /a at access 1) - strictly higher. Max-merge must keep it.
	s := newTestStore(t)
	write(t, s)
	hits, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, fakeExpander{out: []string{"jwt tokens"}})
	if err != nil {
		t.Fatal(err)
	}
	var got *ScoredFact
	for i := range hits {
		if hits[i].Key == "/a" {
			got = &hits[i]
		}
	}
	if got == nil {
		t.Fatalf("/a missing from expanded results: %v", hits)
	}
	if got.Score <= baseScore {
		t.Fatalf("merged /a Score = %v, want > base-search score %v (variant's higher-access score should win the max-merge)", got.Score, baseScore)
	}
}

// TestHybridSearchExpandedMaxMergeKeepsHigherBaseScore pins the opposite
// direction of the max-merge check above: when the base search's score for
// a shared ID is HIGHER than the variant search's score for that same ID,
// the merge must keep the base's higher score, not overwrite it with the
// variant's lower one just because the variant ran later ("last wins").
//
// /a matches the base query "authentication" as the sole FTS hit (rank 1).
// The reformulation "jwt" also matches /a, but three short, jwt-repeating
// decoy facts outrank /a's single "jwt" mention under FTS5 bm25 (higher
// term frequency, shorter body), pushing /a down to rank 4 in the variant
// search. The small AccessCount bump /a picks up between the base and
// variant searches (TouchAccess runs after the base search, before the
// variant search) is not enough to offset that rank drop, so /a's variant
// score is strictly lower than its base score - a last-wins merge bug
// would keep the (lower) variant score instead of the (higher) base one.
func TestHybridSearchExpandedMaxMergeKeepsHigherBaseScore(t *testing.T) {
	ctx := context.Background()
	write := func(t *testing.T, s *Store) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt tokens", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/decoy/1", Body: "jwt jwt jwt", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/decoy/2", Body: "jwt jwt jwt jwt", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/decoy/3", Body: "jwt jwt jwt jwt jwt", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}

	// Twin store, never searched with the variant query: /a's base-query
	// score here reflects access count 0 - exactly the state
	// HybridSearchExpanded's own base search sees.
	twin := newTestStore(t)
	write(t, twin)
	baseline, err := twin.HybridSearchScored(ctx, "ns", "authentication", 10, 0)
	if err != nil || len(baseline) != 1 || baseline[0].Key != "/a" {
		t.Fatalf("twin baseline setup: %v, err=%v", baseline, err)
	}
	baseScore := baseline[0].Score

	// Second twin, replaying the exact sequencing HybridSearchExpanded
	// applies: run the base query first (bumps /a's access count to 1,
	// exactly like the real base search inside HybridSearchExpanded), then
	// run the variant query and confirm /a's score there is strictly lower
	// than baseScore - proving this fixture genuinely exercises the "base
	// higher than variant" direction rather than coincidentally tying or
	// inverting it.
	twin2 := newTestStore(t)
	write(t, twin2)
	if _, err := twin2.HybridSearchScored(ctx, "ns", "authentication", 10, 0); err != nil {
		t.Fatal(err)
	}
	variantHits, err := twin2.HybridSearchScored(ctx, "ns", "jwt", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var variantScore float64
	found := false
	for _, h := range variantHits {
		if h.Key == "/a" {
			variantScore = h.Score
			found = true
		}
	}
	if !found {
		t.Fatalf("fixture assumption broken: /a must still match the variant query: %v", variantHits)
	}
	if variantScore >= baseScore {
		t.Fatalf("fixture assumption broken: variant score %v must be strictly lower than base score %v (decoys must outrank /a on the variant query)", variantScore, baseScore)
	}

	// Real store: exercise the actual merge inside HybridSearchExpanded.
	s := newTestStore(t)
	write(t, s)
	hits, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, fakeExpander{out: []string{"jwt"}})
	if err != nil {
		t.Fatal(err)
	}
	var got *ScoredFact
	for i := range hits {
		if hits[i].Key == "/a" {
			got = &hits[i]
		}
	}
	if got == nil {
		t.Fatalf("/a missing from expanded results: %v", hits)
	}
	if got.Score != baseScore {
		t.Fatalf("merged /a Score = %v, want base score %v unchanged (base's higher score must survive the merge, not be overwritten by the variant's lower one)", got.Score, baseScore)
	}
}

// TestHybridSearchExpandedTiesSortByKeyAscending pins finding 3: facts
// tied on fusion Score sort by key ascending, not by map iteration order.
// Four facts, each a standalone single-word document uniquely matched by
// exactly one of {original query, ref1, ref2, ref3}, are symmetric in
// every score component (each is the sole FTS hit in its own search, none
// are touched before their own search, no links/entities/importance
// differences) - the reviewer confirmed this fixture yields exact ties.
func TestHybridSearchExpandedTiesSortByKeyAscending(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	bodies := map[string]string{"/a": "alpha", "/b": "bravo", "/c": "charlie", "/d": "delta"}
	// write out of key order so a passing test can't be explained by
	// write/insertion order leaking into the result order.
	for _, k := range []string{"/d", "/b", "/a", "/c"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: bodies[k], Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}

	// Insertion order into the merge map is base-query first, then refs
	// in list order - deliberately the REVERSE of the desired ascending
	// output (original query matches /d, refs match /c, /b, /a in that
	// order) so that a broken sort relying on map/insertion order (no
	// real key tie-break) reliably disagrees with key-ascending, instead
	// of coincidentally matching it.
	exp := fakeExpander{out: []string{"charlie", "bravo", "alpha"}}
	hits, err := s.HybridSearchExpanded(ctx, "ns", "delta", 10, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("want all 4 tied facts, got %d: %v", len(hits), hits)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score != hits[i].Score {
			t.Fatalf("fixture assumption broken, scores not tied: %+v", hits)
		}
	}
	want := []string{"/a", "/b", "/c", "/d"}
	for i, w := range want {
		if hits[i].Key != w {
			t.Fatalf("tie order[%d] = %s, want %s (key-ascending tie-break among ties): %v", i, hits[i].Key, w, hits)
		}
	}
}

// TestHybridSearchExpandedFiltersBlanksBeforeTruncating pins finding 4:
// blank reformulations are dropped BEFORE the 3-reformulation cap, so an
// expander emitting three blanks and one useful reformulation still
// reaches the useful one instead of losing it to the cap.
func TestHybridSearchExpandedFiltersBlanksBeforeTruncating(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "login tokens rotate hourly", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	exp := fakeExpander{out: []string{"", "  ", "", "login tokens"}}
	hits, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, h := range hits {
		keys[h.Key] = true
	}
	if !keys["/b"] {
		t.Fatalf("blank reformulations must be dropped before the 3-cap, so the 4th (useful) one still runs: %v", hits)
	}
}

// TestFilterRefs pins filterRefs' contract directly, independent of
// HybridSearchExpanded's own tests: blank/whitespace-only entries are
// dropped, entries equal to the (trimmed, case-folded) original query are
// dropped, later entries that duplicate an earlier surviving entry
// (case/whitespace-insensitive) are dropped, and surviving entries keep
// their original casing/spacing in first-encounter order.
func TestFilterRefs(t *testing.T) {
	got := filterRefs("  Authentication  ", []string{"", "  ", "authentication", "JWT Tokens", "jwt tokens", " jwt tokens ", "real"})
	want := []string{"JWT Tokens", "real"}
	if len(got) != len(want) {
		t.Fatalf("filterRefs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("filterRefs() = %v, want %v", got, want)
		}
	}
}

// TestHybridSearchExpandedDedupesQueryText pins finding 5: a reformulation
// that's the original query re-worded only in case/whitespace, or a
// reformulation repeated by the expander, must not burn a search call - a
// bad expander returning ["Authentication", "jwt tokens", "jwt tokens"]
// should behave like it returned exactly ["jwt tokens"].
func TestHybridSearchExpandedDedupesQueryText(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/e", Body: "zephyr quokka narwhal marmoset", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	// "Authentication" (dupe of original, different case) and a repeated
	// "jwt tokens" both waste the truncate-to-3 budget under the old
	// behavior, which would consume the two real slots left before ever
	// reaching a reformulation that could find /e. Dedup must free that
	// slot up so a 3rd, real reformulation still runs.
	exp := fakeExpander{out: []string{"Authentication", "jwt tokens", "jwt tokens", "zephyr quokka"}}
	hits, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, h := range hits {
		keys[h.Key] = true
	}
	if !keys["/e"] {
		t.Fatalf("deduping the echoed/repeated refs should free a slot to reach 'zephyr quokka' (4th distinct ref, 2nd after dedup): %v", hits)
	}
}

// typedNilExpander's Expand panics if ever called - it exists only to
// prove HybridSearchExpanded never calls Expand on a typed-nil
// QueryExpander (finding 6).
type typedNilExpander struct{}

func (*typedNilExpander) Expand(context.Context, string) ([]string, error) {
	panic("typed-nil expander guard failed: Expand must not be called")
}

func TestHybridSearchExpandedTypedNilGuard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	var p *typedNilExpander
	var exp QueryExpander = p // non-nil interface value wrapping a nil pointer
	hits, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Key != "/a" {
		t.Fatalf("typed-nil expander must degrade like a plain nil expander: %v", hits)
	}
}

// cancelingExpander cancels ctx from inside Expand, simulating a caller
// whose deadline/cancellation fires mid-expansion (after the base search
// already completed, before the variant searches run).
type cancelingExpander struct {
	cancel context.CancelFunc
	refs   []string
}

func (c cancelingExpander) Expand(context.Context, string) ([]string, error) {
	c.cancel()
	return c.refs, nil
}

// TestHybridSearchExpandedCtxCancellationPropagates pins finding 7: a ctx
// canceled mid-expansion must surface as an error, not silently degrade
// to "no reformulation matched anything".
func TestHybridSearchExpandedCtxCancellationPropagates(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "authentication uses jwt", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	exp := cancelingExpander{cancel: cancel, refs: []string{"login tokens"}}
	_, err := s.HybridSearchExpanded(ctx, "ns", "authentication", 10, 0, exp)
	if err == nil {
		t.Fatalf("expected an error after mid-expansion ctx cancellation, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
