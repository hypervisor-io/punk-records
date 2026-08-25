package memory

import (
	"fmt"
	"testing"
	"time"
)

// newTestStoreWithClock wraps newTest (memory_test.go), exposing the
// settable fake clock for tests that need facts at specific past dates.
func newTestStoreWithClock(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	s, _, clk := newTest(t)
	return s, clk
}

func TestWindowedSearchSpreadsAcrossBuckets(t *testing.T) {
	// three clusters of facts, months apart; all match the query.
	// naive rank-order would return one cluster; bucket spreading
	// must return one from each.
	s, clock := newTestStoreWithClock(t)
	ctx := t.Context()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for month := 0; month < 3; month++ {
		clock.Set(base.AddDate(0, month, 0))
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("/m%d/f%d", month, i)
			if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key,
				Body: "deploy incident retro notes"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := s.WindowedSearch(ctx, "ns", "deploy incident",
		base, base.AddDate(0, 3, 0), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	months := map[string]bool{}
	for _, f := range got {
		months[f.Key[:3]] = true // "/m0", "/m1", "/m2"
	}
	if len(months) != 3 {
		t.Fatalf("results clustered: %v, want one per month", months)
	}
}

func TestWindowedSearchNarrowWindowReturnsAllInRange(t *testing.T) {
	s, clock := newTestStoreWithClock(t)
	ctx := t.Context()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock.Set(base)
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a",
		Body: "deploy incident retro notes"}); err != nil {
		t.Fatal(err)
	}
	// outside the window: must not be returned
	clock.Set(base.AddDate(1, 0, 0))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b",
		Body: "deploy incident retro notes"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.WindowedSearch(ctx, "ns", "deploy incident",
		base, base.AddDate(0, 1, 0), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "/a" {
		t.Fatalf("got %v, want just /a", got)
	}
}

// TestWindowedSearchSubBucketWindowNoPanic covers the finding that
// span := to.Sub(from) / windowBuckets floors to 0 for windows under
// 8ns, which then panics on division by zero when bucketing. Facts
// share one CreatedAt (clock reset before each write) so a 4ns window
// with len(in) > limit exercises the bucket path.
func TestWindowedSearchSubBucketWindowNoPanic(t *testing.T) {
	s, clock := newTestStoreWithClock(t)
	ctx := t.Context()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var last time.Time
	for i := 0; i < 3; i++ {
		clock.Set(base)
		f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: fmt.Sprintf("/f%d", i),
			Body: "deploy incident retro notes"})
		if err != nil {
			t.Fatal(err)
		}
		last = f.CreatedAt
	}
	from := last
	to := from.Add(4 * time.Nanosecond)
	got, err := s.WindowedSearch(ctx, "ns", "deploy incident", from, to, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestWindowedSearchRejectsBadRange(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.WindowedSearch(ctx, "ns", "q", now, now, 10); err == nil {
		t.Fatal("want error when to == from")
	}
	if _, err := s.WindowedSearch(ctx, "ns", "q", now, now.Add(-time.Hour), 10); err == nil {
		t.Fatal("want error when to is before from")
	}
}
