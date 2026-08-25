package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// txProbeEmbedder's Embed queries the store's own DB with a short deadline,
// standing in for an embedder's outbound HTTP call. sqlite runs
// SetMaxOpenConns(1) (internal/store/store.go), so if the caller still held
// the write transaction open while embedding, this probe query would have
// no connection to acquire and would time out; with the tx already closed
// (or not yet opened) it succeeds well within the deadline.
type txProbeEmbedder struct {
	s *Store
}

func (txProbeEmbedder) Dims() int { return 2 }

func (e txProbeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	probeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var one int
	if err := e.s.db.QueryRowContext(probeCtx, "SELECT 1").Scan(&one); err != nil {
		return nil, fmt.Errorf("probe query during embed (tx likely still open): %w", err)
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// TestAddLinkDescribedEmbedsOutsideTx covers finding 1 (round 2): the
// description embedding must be computed BEFORE AddLinkDescribed opens the
// write transaction, not from inside it (as memory.go's Remember already
// does for the fact body). If the embed call happened inside the tx,
// txProbeEmbedder's own DB query would deadlock against sqlite's single
// connection and time out, and AddLinkDescribed would propagate that error.
func TestAddLinkDescribedEmbedsOutsideTx(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"/a", "/b"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "x", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	s.SetEmbedder(txProbeEmbedder{s: s})
	if err := s.AddLinkDescribed(ctx, "ns", "/a", "/b", "relates_to", 1.0, "a relates to b"); err != nil {
		t.Fatalf("AddLinkDescribed with description must succeed (embed must not run inside the write tx): %v", err)
	}
}

func TestValidateLinkType(t *testing.T) {
	for _, ok := range []string{"relates_to", "leads_to", "occurred_before",
		"prefers_over", "exemplifies", "contradicts", "reinforces",
		"invalidated_by", "evolved_into", "derived_from", "part_of", "similar_to"} {
		if err := ValidateLinkType(ok); err != nil {
			t.Errorf("ValidateLinkType(%q) = %v, want nil", ok, err)
		}
	}
	if err := ValidateLinkType("buddy_of"); err == nil {
		t.Error("ValidateLinkType(buddy_of) = nil, want error")
	}
}

func TestAddLinkRejectsUnknownType(t *testing.T) {
	s, _, _ := newTest(t) // reuse memory_test.go's store helper
	ctx := context.Background()
	if err := s.AddLink(ctx, "ns", "/a", "/b", "buddy_of"); err == nil {
		t.Fatal("AddLink with unknown type = nil, want error")
	}
	// empty type still defaults to relates_to
	if err := s.AddLink(ctx, "ns", "/a", "/b", ""); err != nil {
		t.Fatalf("AddLink with empty type: %v", err)
	}
	links, err := s.Neighbors(ctx, "ns", "/a", "out")
	if err != nil || len(links) != 1 || links[0].LinkType != "relates_to" {
		t.Fatalf("Neighbors = %v, %v; want one relates_to link", links, err)
	}
}

// TestLinkValidityStampAndFilter proves AddLink stamps valid_at on create,
// and that closing an edge (invalid_at set) hides it from the default read
// paths - Neighbors and linksForKeys - while a still-live edge in the same
// namespace stays visible, so the filter is namespace/edge-scoped rather
// than blanket-hiding.
func TestLinkValidityStampAndFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"/a", "/b"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "x", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "relates_to"); err != nil {
		t.Fatal(err)
	}
	// select both columns and compare as strings: the fakeClock ticks one
	// millisecond per Now() call, so a regression that captured now() twice
	// (once for created_at, once for valid_at) would produce two different
	// timestamps here instead of one reused value.
	var validAt, createdAt sql.NullString
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT valid_at, created_at FROM memory_links WHERE namespace = $1 AND from_key = $2`), "ns", "/a").Scan(&validAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	if !validAt.Valid || validAt.String == "" {
		t.Fatal("AddLink must stamp valid_at")
	}
	if validAt.String != createdAt.String {
		t.Fatalf("valid_at = %q, want == created_at %q (single now() capture, not two)", validAt.String, createdAt.String)
	}

	// second, reverse edge that stays live throughout: proves the filter
	// only hides the specific closed edge, not every edge in the namespace.
	if err := s.AddLink(ctx, "ns", "/b", "/a", "relates_to"); err != nil {
		t.Fatal(err)
	}

	// manually close the /a->/b edge; default reads must hide it
	if _, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memory_links SET invalid_at = $1 WHERE namespace = $2 AND from_key = $3 AND to_key = $4`),
		store.TimeToDB(time.Now()), "ns", "/a", "/b"); err != nil {
		t.Fatal(err)
	}

	links, err := s.Neighbors(ctx, "ns", "/a", "out")
	if err != nil || len(links) != 0 {
		t.Fatalf("closed edge visible via Neighbors: %v %v", links, err)
	}
	live, err := s.Neighbors(ctx, "ns", "/b", "out")
	if err != nil || len(live) != 1 || live[0].ToKey != "/a" {
		t.Fatalf("live edge b->a hidden or wrong via Neighbors: %v %v", live, err)
	}

	lfk, err := s.linksForKeys(ctx, "ns", []string{"/a", "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(lfk) != 1 || lfk[0].FromKey != "/b" || lfk[0].ToKey != "/a" {
		t.Fatalf("linksForKeys = %+v, want only the live b->a edge (closed a->b hidden)", lfk)
	}
}

// TestTripletSearchHidesInvalidatedEdge proves TripletSearch, like
// Neighbors and linksForKeys, filters invalid_at IS NULL: an edge that
// scores highest before closing must disappear from results after.
func TestTripletSearchHidesInvalidatedEdge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"fact alpha":          {1, 0, 0},
		"fact beta":           {1, 0, 0},
		"alpha leads to beta": {1, 0, 0},
		"query about alpha":   {1, 0, 0},
	}})
	for k, b := range map[string]string{"/a": "fact alpha", "/b": "fact beta"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLinkDescribed(ctx, "ns", "/a", "/b", "leads_to", 1.0, "alpha leads to beta"); err != nil {
		t.Fatal(err)
	}
	got, err := s.TripletSearch(ctx, "ns", "query about alpha", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].From.Key != "/a" || got[0].To.Key != "/b" {
		t.Fatalf("TripletSearch before closing = %+v, want one a->b triplet", got)
	}

	if _, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memory_links SET invalid_at = $1 WHERE namespace = $2`),
		store.TimeToDB(time.Now()), "ns"); err != nil {
		t.Fatal(err)
	}

	got, err = s.TripletSearch(ctx, "ns", "query about alpha", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("closed edge visible via TripletSearch: %+v", got)
	}
}

// TestInvalidateLinkAndAsOf proves InvalidateLink closes the live edge's
// validity window (soft-delete, never a row DELETE) and that NeighborsAsOf
// reads the edges live at a past instant the same way RecallAsOf reads
// facts: gone from the live view (Neighbors), still visible as-of an
// instant before the close, and a second invalidate (no live edge left)
// errors.
//
// Uses newTest's fakeClock directly (not time.Now()): the store's now()
// is bound to the fakeClock, which starts fixed at 2026-07-06 and ticks
// one millisecond per call, so a real time.Now() reading has no defined
// relationship to the stored valid_at/invalid_at instants. mid is taken
// from the same clock right after AddLink and before InvalidateLink, so
// it is guaranteed to fall inside the edge's open validity window,
// mirroring how TestRecallAsOf brackets writes with clk.t.
func TestInvalidateLinkAndAsOf(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	for _, k := range []string{"/a", "/b"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "x", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "leads_to"); err != nil {
		t.Fatal(err)
	}
	mid := clk.Now() // ticks past AddLink's stamp, still before InvalidateLink's
	if err := s.InvalidateLink(ctx, "ns", "/a", "/b", "leads_to"); err != nil {
		t.Fatal(err)
	}
	// live view: gone
	links, err := s.Neighbors(ctx, "ns", "/a", "out")
	if err != nil || len(links) != 0 {
		t.Fatal(links, err)
	}
	// as-of before invalidation: visible
	links, err = s.NeighborsAsOf(ctx, "ns", "/a", "out", mid)
	if err != nil || len(links) != 1 {
		t.Fatalf("as-of: %v %v", links, err)
	}
	// second invalidate: no live edge -> error
	if err := s.InvalidateLink(ctx, "ns", "/a", "/b", "leads_to"); err == nil {
		t.Fatal("re-invalidate must error")
	} else if !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-invalidate err = %v, want ErrNotFound", err)
	}
}

// TestNeighborsAsOfWindowBoundaries pins an edge's validity window to
// known, fixed instants (rather than deriving them from the fakeClock) so
// the half-open interval comparisons - valid_at <= asOf and asOf <
// invalid_at - are exercised exactly at their edges: a boundary mutation
// (<= -> <, or > -> >=) flips one of these three assertions.
func TestNeighborsAsOfWindowBoundaries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"/a", "/b"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "x", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "relates_to"); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateLink(ctx, "ns", "/a", "/b", "relates_to"); err != nil {
		t.Fatal(err)
	}

	validAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	invalidAt := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memory_links SET valid_at = $1, invalid_at = $2 WHERE namespace = $3 AND from_key = $4 AND to_key = $5`),
		store.TimeToDB(validAt), store.TimeToDB(invalidAt), "ns", "/a", "/b"); err != nil {
		t.Fatal(err)
	}

	if links, err := s.NeighborsAsOf(ctx, "ns", "/a", "out", validAt); err != nil || len(links) != 1 {
		t.Fatalf("as-of valid_at exactly = %v, %v; want visible (inclusive lower bound)", links, err)
	}
	if links, err := s.NeighborsAsOf(ctx, "ns", "/a", "out", invalidAt); err != nil || len(links) != 0 {
		t.Fatalf("as-of invalid_at exactly = %v, %v; want hidden (exclusive upper bound)", links, err)
	}
	if links, err := s.NeighborsAsOf(ctx, "ns", "/a", "out", invalidAt.Add(-time.Nanosecond)); err != nil || len(links) != 1 {
		t.Fatalf("as-of invalid_at minus 1ns = %v, %v; want visible", links, err)
	}
}

// TestNeighborsAsOfNullValidAtFallback proves NeighborsAsOf falls back to
// created_at when valid_at is NULL (a pre-0019 row, migrated without a
// validity stamp): visible as-of created_at and any later instant
// (including far-future), hidden strictly before it. A COALESCE-drop
// mutation compares NULL <= asOf directly, which is never true, and would
// hide the row at every instant tested here, including the far-future one.
func TestNeighborsAsOfNullValidAtFallback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"/a", "/b"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "x", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "relates_to"); err != nil {
		t.Fatal(err)
	}

	createdAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memory_links SET valid_at = NULL, created_at = $1 WHERE namespace = $2 AND from_key = $3 AND to_key = $4`),
		store.TimeToDB(createdAt), "ns", "/a", "/b"); err != nil {
		t.Fatal(err)
	}

	if links, err := s.NeighborsAsOf(ctx, "ns", "/a", "out", createdAt); err != nil || len(links) != 1 {
		t.Fatalf("as-of created_at (NULL valid_at fallback) = %v, %v; want visible", links, err)
	}
	farFuture := createdAt.Add(100 * 365 * 24 * time.Hour)
	if links, err := s.NeighborsAsOf(ctx, "ns", "/a", "out", farFuture); err != nil || len(links) != 1 {
		t.Fatalf("as-of far future = %v, %v; want visible (edge is still live)", links, err)
	}
	if links, err := s.NeighborsAsOf(ctx, "ns", "/a", "out", createdAt.Add(-time.Nanosecond)); err != nil || len(links) != 0 {
		t.Fatalf("as-of created_at minus 1ns = %v, %v; want hidden", links, err)
	}
}

// TestInvalidateLinkDefaultTypeIsRelatesTo proves InvalidateLink's ""
// default closes only the relates_to edge of a pair that has both a
// relates_to and a leads_to edge live - the default must not blanket-close
// every edge between the two keys.
func TestInvalidateLinkDefaultTypeIsRelatesTo(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"/a", "/b"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "x", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "relates_to"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "leads_to"); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateLink(ctx, "ns", "/a", "/b", ""); err != nil {
		t.Fatal(err)
	}
	links, err := s.Neighbors(ctx, "ns", "/a", "out")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].LinkType != "leads_to" {
		t.Fatalf("Neighbors after default-type invalidate = %+v, want only leads_to still live", links)
	}
}

// TestNeighborsAsOfOrdering proves NeighborsAsOf orders results like
// Neighbors (by the "other" key column), not by insertion order: /a->/c
// inserted before /a->/b must still come back [/b, /c]. An ORDER BY-drop
// mutation leaves rows in scan (insertion) order, which is [/c, /b] here.
func TestNeighborsAsOfOrdering(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	for _, k := range []string{"/a", "/b", "/c"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "x", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink(ctx, "ns", "/a", "/c", "relates_to"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "relates_to"); err != nil {
		t.Fatal(err)
	}
	asOf := clk.Now()
	links, err := s.NeighborsAsOf(ctx, "ns", "/a", "out", asOf)
	if err != nil || len(links) != 2 {
		t.Fatalf("NeighborsAsOf = %v, %v; want 2 live edges", links, err)
	}
	if links[0].ToKey != "/b" || links[1].ToKey != "/c" {
		t.Fatalf("NeighborsAsOf order = [%s, %s], want [/b, /c]", links[0].ToKey, links[1].ToKey)
	}
}
