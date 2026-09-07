package memory

// R02 bounded neighbor read: the expansion path must bound its database
// work, so Store gains a deterministic bounded first-set neighbor query
// with observable HasMore instead of materializing every live link of a
// high-degree key. Neighbors keeps its exact legacy behavior for
// existing callers (only "in" selects incoming; every other direction is
// outgoing; empty results serialize []) and delegates to the same query.
// The API is a bounded FIRST SET contract: no cursor, just the first
// Limit links in deterministic (opposite-key, link-type) order plus the
// HasMore signal.

import (
	"encoding/json"
	"math"
	"testing"
)

func TestNeighborsBoundedLimitsWithObservableHasMore(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	const degree = 8
	for i := 0; i < degree; i++ {
		if err := s.AddLink(ctx, "ns", "/hub", "/node/"+string(rune('a'+i)), "relates_to"); err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.NeighborsBounded(ctx, "ns", "/hub", NeighborQuery{Direction: "out", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Links) != 3 {
		t.Fatalf("links = %d, want the bounded 3 (the old read materialized all %d)", len(page.Links), degree)
	}
	if !page.HasMore {
		t.Fatal("HasMore = false, want true: truncation must be observable, never silent")
	}
	for i, l := range page.Links {
		if want := "/node/" + string(rune('a'+i)); l.ToKey != want {
			t.Fatalf("link %d = %s, want %s (key-ordered)", i, l.ToKey, want)
		}
	}

	// The final bound: a limit covering the whole neighborhood reports
	// HasMore false.
	all, err := s.NeighborsBounded(ctx, "ns", "/hub", NeighborQuery{Direction: "out", Limit: degree})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Links) != degree || all.HasMore {
		t.Fatalf("full bounded read = %d links HasMore=%v, want %d links with HasMore false", len(all.Links), all.HasMore, degree)
	}
}

// TestNeighborsBoundedTypedEdgeOrder pins the deterministic order:
// opposite-end key first, then link type, so two typed edges between the
// same pair come back in a stable order on both directions.
func TestNeighborsBoundedTypedEdgeOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for _, typ := range []string{"relates_to", "leads_to"} {
		if err := s.AddLink(ctx, "ns", "/a", "/b", typ); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"out", "in"} {
		key := "/a"
		if dir == "in" {
			key = "/b"
		}
		first, err := s.NeighborsBounded(ctx, "ns", key, NeighborQuery{Direction: dir, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Links) != 1 || !first.HasMore || first.Links[0].LinkType != "leads_to" {
			t.Fatalf("unstable bounded result %s: %+v", dir, first)
		}
		both, err := s.NeighborsBounded(ctx, "ns", key, NeighborQuery{Direction: dir, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(both.Links) != 2 || both.HasMore || both.Links[1].LinkType != "relates_to" {
			t.Fatalf("typed edges lost %s: %+v", dir, both)
		}
	}
}

func TestNeighborsBoundedMatchesNeighborsAndDirections(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for i := 0; i < 5; i++ {
		if err := s.AddLink(ctx, "ns", "/hub", "/node/"+string(rune('a'+i)), "relates_to"); err != nil {
			t.Fatal(err)
		}
		if err := s.AddLink(ctx, "ns", "/in/"+string(rune('a'+i)), "/hub", "relates_to"); err != nil {
			t.Fatal(err)
		}
	}

	full, err := s.Neighbors(ctx, "ns", "/hub", "out")
	if err != nil {
		t.Fatal(err)
	}
	unbounded, err := s.NeighborsBounded(ctx, "ns", "/hub", NeighborQuery{Direction: "out"})
	if err != nil {
		t.Fatal(err)
	}
	if unbounded.HasMore {
		t.Fatal("unbounded read reported HasMore")
	}
	if len(unbounded.Links) != len(full) {
		t.Fatalf("unbounded read = %d links, Neighbors = %d", len(unbounded.Links), len(full))
	}
	for i := range full {
		if unbounded.Links[i] != full[i] {
			t.Fatalf("link %d diverges from Neighbors: %+v vs %+v", i, unbounded.Links[i], full[i])
		}
	}

	in, err := s.NeighborsBounded(ctx, "ns", "/hub", NeighborQuery{Direction: "in", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Links) != 2 || !in.HasMore {
		t.Fatalf("in-page = %+v hasMore=%v, want 2 key-ordered links with HasMore", in.Links, in.HasMore)
	}
	for _, l := range in.Links {
		if l.ToKey != "/hub" {
			t.Fatalf("in-direction link %+v does not point at /hub", l)
		}
	}

	if _, err := s.NeighborsBounded(ctx, "ns", "/hub", NeighborQuery{Direction: "sideways", Limit: 2}); err == nil {
		t.Fatal("invalid direction accepted")
	}
}

// TestNeighborsLegacyCompatibility pins the legacy API surface: only
// "in" selects incoming, every other direction string is outgoing and
// must not error, and an empty result serializes as [] not null.
func TestNeighborsLegacyCompatibility(t *testing.T) {
	s := newTestStore(t)
	for _, dir := range []string{"out", ""} {
		links, err := s.Neighbors(t.Context(), "ns", "/none", dir)
		if err != nil {
			t.Errorf("legacy direction %q now errors: %v", dir, err)
			continue
		}
		b, err := json.Marshal(links)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "[]" {
			t.Errorf("legacy empty direction %q serialized %s, want []", dir, b)
		}
	}
}

// TestNeighborsBoundedRejectsLimitOverflow: limit+1 lookahead must fail
// loudly instead of wrapping into an unbounded or negative read.
func TestNeighborsBoundedRejectsLimitOverflow(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.NeighborsBounded(t.Context(), "ns", "/a", NeighborQuery{Direction: "out", Limit: math.MaxInt}); err == nil {
		t.Fatal("overflowing limit must fail instead of becoming unbounded")
	}
}
