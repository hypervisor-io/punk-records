package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// contradictStubEmbedder assigns identical vectors to bodies sharing the
// "db timeout is" prefix and an orthogonal vector to everything else, so
// cosine similarity lines up with what the contradiction test needs
// without depending on a real embedding model.
type contradictStubEmbedder struct{}

func (contradictStubEmbedder) Dims() int { return 2 }

func (contradictStubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if strings.HasPrefix(t, "db timeout is") {
			out[i] = []float32{1, 0}
		} else {
			out[i] = []float32{0, 1}
		}
	}
	return out, nil
}

// fakeContradictJudge always answers yes/no per the fixed field, tracking
// how many times it was invoked so tests can assert the dedup guard skips
// already-linked (or already-closed) pairs instead of re-judging them. If
// err is set, it is returned (with ok=false) on the first invocation only;
// every later call answers normally from yes - this lets a test prove a
// judge error on one pair doesn't stop the pass from reaching the next.
type fakeContradictJudge struct {
	yes   bool
	err   error
	calls int
}

func (j *fakeContradictJudge) Contradicts(_ context.Context, a, b Fact) (bool, error) {
	j.calls++
	if j.err != nil && j.calls == 1 {
		return false, j.err
	}
	return j.yes, nil
}

func TestDetectContradictions(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(contradictStubEmbedder{})
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/one", Body: "db timeout is 5s", Writer: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/two", Body: "db timeout is 30s", Writer: "b"}); err != nil {
		t.Fatal(err)
	}
	// excluded prefix must never be judged, even though its body embeds
	// identically to /f/one's.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/o", Body: "db timeout is 5s", Writer: "c"}); err != nil {
		t.Fatal(err)
	}
	j := &fakeContradictJudge{yes: true}
	n, err := s.DetectContradictions(ctx, "ns", 0.8, 0, j)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("linked pairs = %d, want 1", n)
	}
	if j.calls != 1 {
		t.Fatalf("judge calls = %d, want 1 (excluded prefix must never be judged)", j.calls)
	}
	out, err := s.Neighbors(ctx, "ns", "/f/one", "out")
	if err != nil {
		t.Fatal(err)
	}
	var hasContradicts, hasInvalidated bool
	for _, l := range out {
		if l.LinkType == "contradicts" && l.ToKey == "/f/two" {
			hasContradicts = true
		}
		if l.LinkType == "invalidated_by" && l.ToKey == "/f/two" {
			hasInvalidated = true // /f/one is older -> invalidated_by newer
		}
	}
	if !hasContradicts || !hasInvalidated {
		t.Fatalf("links missing: %+v", out)
	}
	outTwo, err := s.Neighbors(ctx, "ns", "/f/two", "out")
	if err != nil {
		t.Fatal(err)
	}
	var twoHasContradicts bool
	for _, l := range outTwo {
		if l.LinkType == "contradicts" && l.ToKey == "/f/one" {
			twoHasContradicts = true
		}
	}
	if !twoHasContradicts {
		t.Fatalf("contradicts link missing the reverse direction: %+v", outTwo)
	}

	// re-run: already-linked pair skipped, judge not called again
	before := j.calls
	if n, err := s.DetectContradictions(ctx, "ns", 0.8, 0, j); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if j.calls != before {
		t.Fatal("already-linked pair must not be re-judged")
	}

	// nil judge: no-op
	if n, err := s.DetectContradictions(ctx, "ns", 0.8, 0, nil); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

// TestDetectContradictionsClosedPairNotResurrected proves the dedup preload
// (contradictLinkedPairs) is deliberately validity-unfiltered: once a user
// closes every edge DetectContradictions wrote for a pair - both
// contradicts directions and invalidated_by - DetectContradictions must not
// re-judge that pair or create any new live edge for it. A guard that only
// looked at live edges would resurrect the noise every pass; closing only
// one of the three edges (leaving the reverse contradicts or invalidated_by
// live) would still leave the pair's filtered count above zero and wouldn't
// exercise the validity-unfiltered guard at all, so all three are closed
// here.
func TestDetectContradictionsClosedPairNotResurrected(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(contradictStubEmbedder{})
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/one", Body: "db timeout is 5s", Writer: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/two", Body: "db timeout is 30s", Writer: "b"}); err != nil {
		t.Fatal(err)
	}
	j := &fakeContradictJudge{yes: true}
	if n, err := s.DetectContradictions(ctx, "ns", 0.8, 0, j); err != nil || n != 1 {
		t.Fatalf("first pass = %d, %v; want 1, nil", n, err)
	}

	// close all three edges the first pass wrote: both contradicts
	// directions plus invalidated_by (/f/one is older -> invalidated_by
	// /f/two).
	if err := s.InvalidateLink(ctx, "ns", "/f/one", "/f/two", "contradicts"); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateLink(ctx, "ns", "/f/two", "/f/one", "contradicts"); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateLink(ctx, "ns", "/f/one", "/f/two", "invalidated_by"); err != nil {
		t.Fatal(err)
	}

	before := j.calls
	n, err := s.DetectContradictions(ctx, "ns", 0.8, 0, j)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("re-run after closing all three edges = %d, want 0 (pair must stay skipped)", n)
	}
	if j.calls != before {
		t.Fatalf("judge was re-called on a closed pair: calls %d -> %d", before, j.calls)
	}

	out, err := s.Neighbors(ctx, "ns", "/f/one", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range out {
		if l.LinkType == "contradicts" && l.ToKey == "/f/two" {
			t.Fatalf("closed contradicts edge was resurrected: %+v", l)
		}
	}
}

func TestDetectContradictionsNilEmbedderIsNoop(t *testing.T) {
	s := newTestStore(t)
	n, err := s.DetectContradictions(context.Background(), "ns", 0.8, 0, &fakeContradictJudge{yes: true})
	if err != nil || n != 0 {
		t.Fatalf("no embedder = %d, %v; want 0, nil", n, err)
	}
}

// TestDetectContradictionsThresholdBoundaries covers finding 7: threshold
// <= 0 defaults to 0.85 (so a caller who passes the zero value still gets a
// working pass), and threshold >= 1 is a no-op, mirroring
// ReconcileObservations' "no pair can ever reach a similarity of 1"
// disable-switch convention.
func TestDetectContradictionsThresholdBoundaries(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(contradictStubEmbedder{})
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/one", Body: "db timeout is 5s", Writer: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/two", Body: "db timeout is 30s", Writer: "b"}); err != nil {
		t.Fatal(err)
	}

	// threshold <= 0 defaults to 0.85; the stub gives these two facts an
	// identical vector (cosine 1.0), so the default threshold still lets
	// the pair through.
	j := &fakeContradictJudge{yes: true}
	n, err := s.DetectContradictions(ctx, "ns", 0, 0, j)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("threshold=0 (default 0.85) linked = %d, want 1", n)
	}

	// threshold >= 1 is a no-op: judge must not be called even though the
	// pair above was never linked... use a fresh pair so the dedup guard
	// isn't what's suppressing it.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/three", Body: "db timeout is 45s", Writer: "c"}); err != nil {
		t.Fatal(err)
	}
	j2 := &fakeContradictJudge{yes: true}
	n2, err := s.DetectContradictions(ctx, "ns", 1, 0, j2)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("threshold=1 linked = %d, want 0 (no-op)", n2)
	}
	if j2.calls != 0 {
		t.Fatalf("threshold=1 judge calls = %d, want 0 (no-op)", j2.calls)
	}
}

// TestDetectContradictionsMaxPairsBudget covers finding 3: maxPairs caps
// judge invocations per pass, not links written. Three facts sharing the
// stub's "db timeout is" prefix give three eligible above-threshold pairs
// ((one,three), (one,two), (three,two) in key-ascending scan order); a
// budget of 1 must stop the scan after the first judge call.
func TestDetectContradictionsMaxPairsBudget(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(contradictStubEmbedder{})
	ctx := context.Background()
	for _, k := range []string{"/f/one", "/f/two", "/f/three"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "db timeout is 5s", Writer: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	j := &fakeContradictJudge{yes: true}
	n, err := s.DetectContradictions(ctx, "ns", 0.8, 1, j)
	if err != nil {
		t.Fatal(err)
	}
	if j.calls != 1 {
		t.Fatalf("judge calls = %d, want exactly 1 (maxPairs=1 budget)", j.calls)
	}
	if n != 1 {
		t.Fatalf("linked = %d, want 1 (the one pair the budget allowed)", n)
	}
}

// TestDetectContradictionsJudgeErrorSkipsPair covers finding 4: a judge
// error on one pair must not fail the pass or block later pairs. Three
// facts sharing the stub's embedding prefix give two candidate pairs in
// scan order after the first (erroring) one; the second must still get
// judged and linked, and the pass must return a nil error.
func TestDetectContradictionsJudgeErrorSkipsPair(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(contradictStubEmbedder{})
	ctx := context.Background()
	for _, k := range []string{"/f/one", "/f/two", "/f/three"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "db timeout is 5s", Writer: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	j := &fakeContradictJudge{yes: true, err: errors.New("judge unavailable")}
	n, err := s.DetectContradictions(ctx, "ns", 0.8, 0, j)
	if err != nil {
		t.Fatalf("pass must return nil error even though the judge errored on one pair: %v", err)
	}
	if j.calls < 2 {
		t.Fatalf("judge calls = %d, want >= 2 (later pair must still be judged after the first errors)", j.calls)
	}
	if n < 1 {
		t.Fatalf("linked = %d, want >= 1 (later good pair must still be linked)", n)
	}
}

// TestDetectContradictionsReverseDirectionDeduped covers finding 5: the
// dedup preload must catch an existing edge regardless of which direction
// it was written in. Pre-linking only /f/two -> /f/one contradicts (the
// reverse of the scan order, since /f/one sorts first) must still suppress
// the judge call for the (/f/one, /f/two) pair.
func TestDetectContradictionsReverseDirectionDeduped(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(contradictStubEmbedder{})
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/one", Body: "db timeout is 5s", Writer: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/two", Body: "db timeout is 30s", Writer: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "ns", "/f/two", "/f/one", "contradicts"); err != nil {
		t.Fatal(err)
	}
	j := &fakeContradictJudge{yes: true}
	n, err := s.DetectContradictions(ctx, "ns", 0.8, 0, j)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("linked = %d, want 0 (reverse-direction edge must dedup the pair)", n)
	}
	if j.calls != 0 {
		t.Fatalf("judge calls = %d, want 0 (reverse-direction edge must dedup the pair)", j.calls)
	}
}

// TestAddLinkTxAtomicRollback covers finding 1: DetectContradictions writes
// a pair's three edges (contradicts both ways, invalidated_by) inside one
// s.db.WithTx call, each edge via addLinkTx against the shared *sql.Tx -
// exactly the mechanism this test exercises directly. There's no
// deterministic DB-level constraint two ordinary contradicts/invalidated_by
// inserts can trip (the unique constraint is ON CONFLICT DO NOTHING, not an
// error), so atomicity is proven here by making the third of three
// addLinkTx calls fail validation (bad link type) and asserting the first
// two - already Exec'd against the tx - never made it to the table: the
// whole transaction rolled back, not just the failing statement.
func TestAddLinkTxAtomicRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/one", Body: "a", Writer: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f/two", Body: "b", Writer: "b"}); err != nil {
		t.Fatal(err)
	}
	now := s.now()
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.addLinkTx(ctx, tx, "ns", "/f/one", "/f/two", "contradicts", 1.0, "", nil, now); err != nil {
			return err
		}
		if err := s.addLinkTx(ctx, tx, "ns", "/f/two", "/f/one", "contradicts", 1.0, "", nil, now); err != nil {
			return err
		}
		// deliberately invalid: forces rollback of the two inserts above.
		return s.addLinkTx(ctx, tx, "ns", "/f/one", "/f/two", "not_a_real_link_type", 1.0, "", nil, now)
	})
	if err == nil {
		t.Fatal("expected the tx to fail on the invalid link type")
	}
	out, nErr := s.Neighbors(ctx, "ns", "/f/one", "out")
	if nErr != nil {
		t.Fatal(nErr)
	}
	for _, l := range out {
		if l.LinkType == "contradicts" {
			t.Fatalf("partial commit: contradicts edge survived a failed tx: %+v", l)
		}
	}
	outTwo, nErr := s.Neighbors(ctx, "ns", "/f/two", "out")
	if nErr != nil {
		t.Fatal(nErr)
	}
	for _, l := range outTwo {
		if l.LinkType == "contradicts" {
			t.Fatalf("partial commit: reverse contradicts edge survived a failed tx: %+v", l)
		}
	}
}

// TestDetectContradictionsMaxPairsBudgetCountsJudgeErrors covers finding 3
// (round 2): a judge error must count against the maxPairs budget the same
// as a yes/no answer - TestDetectContradictionsJudgeErrorSkipsPair proves an
// error doesn't stop the scan, and TestDetectContradictionsMaxPairsBudget
// proves the budget counts judge calls, but neither proves an erroring call
// is deducted from the budget itself. maxPairs=1 with an always-erroring
// judge over three facts sharing the stub's embedding prefix (three
// candidate pairs in scan order) must stop after exactly one judge call: if
// errors didn't count against the budget, the scan would keep going past
// the erroring first pair and judge (and link) a second one.
func TestDetectContradictionsMaxPairsBudgetCountsJudgeErrors(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(contradictStubEmbedder{})
	ctx := context.Background()
	for _, k := range []string{"/f/one", "/f/two", "/f/three"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "db timeout is 5s", Writer: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	// err on every call (not just the first): fakeContradictJudge only
	// special-cases calls == 1, so a persistent error needs its own judge
	// rather than reusing fakeContradictJudge's err field, which lets later
	// calls succeed - here there must be no "later call" to reach at all.
	j := &alwaysErrJudge{}
	n, err := s.DetectContradictions(ctx, "ns", 0.8, 1, j)
	if err != nil {
		t.Fatalf("pass must return nil error even though the judge errored: %v", err)
	}
	if j.calls != 1 {
		t.Fatalf("judge calls = %d, want exactly 1 (maxPairs=1 must stop the scan even though the one call errored)", j.calls)
	}
	if n != 0 {
		t.Fatalf("linked = %d, want 0 (the only judged pair errored, so nothing could be linked)", n)
	}
}

// alwaysErrJudge always errors, unlike fakeContradictJudge which only
// errors on its first call - needed by
// TestDetectContradictionsMaxPairsBudgetCountsJudgeErrors so a maxPairs=1
// budget can't be satisfied by a second, non-erroring call.
type alwaysErrJudge struct {
	calls int
}

func (j *alwaysErrJudge) Contradicts(_ context.Context, a, b Fact) (bool, error) {
	j.calls++
	return false, errors.New("judge unavailable")
}
