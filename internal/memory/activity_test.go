package memory

import (
	"context"
	"testing"
	"time"
)

// TestWriteActivity pins the consolidation scheduler's trigger source:
// revision counts and the latest write time since a cutoff, with a
// missing namespace reporting zero activity rather than an error.
func TestWriteActivity(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()

	// Missing namespace: zero activity, no error.
	n, last, err := s.WriteActivity(ctx, "nope", time.Time{})
	if err != nil || n != 0 || !last.IsZero() {
		t.Fatalf("missing ns: n=%d last=%v err=%v", n, last, err)
	}

	t0 := clk.t
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	clk.Set(t0.Add(time.Minute))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "two"}); err != nil {
		t.Fatal(err)
	}

	n, last, err = s.WriteActivity(ctx, "ns", time.Time{})
	if err != nil || n != 2 {
		t.Fatalf("all writes: n=%d err=%v", n, err)
	}
	// The fake clock ticks 1ms per Now() call, so the second write lands
	// a few ms past the Set point - assert a tight window, not equality.
	if last.Before(t0.Add(time.Minute)) || last.After(t0.Add(time.Minute+time.Second)) {
		t.Fatalf("last write = %v, want ~%v", last, t0.Add(time.Minute).UTC())
	}

	// Cutoff between the two writes: only the second counts.
	n, _, err = s.WriteActivity(ctx, "ns", t0.Add(30*time.Second))
	if err != nil || n != 1 {
		t.Fatalf("since mid: n=%d err=%v", n, err)
	}
	// Cutoff after both: zero, zero-time last.
	n, last, err = s.WriteActivity(ctx, "ns", t0.Add(2*time.Minute))
	if err != nil || n != 0 || !last.IsZero() {
		t.Fatalf("since after: n=%d last=%v err=%v", n, last, err)
	}
}

// TestDiagnoseConsolidationFields pins the consolidation observability:
// -1 sentinel before any pass is recorded, then last_consolidated_at
// plus writes-since once MarkConsolidated has run.
func TestDiagnoseConsolidationFields(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "one"}); err != nil {
		t.Fatal(err)
	}

	d, err := s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if d.LastConsolidatedAt != "" || d.WritesSinceConsolidation != -1 {
		t.Fatalf("before any pass: %q %d", d.LastConsolidatedAt, d.WritesSinceConsolidation)
	}

	mark := clk.t.Add(time.Minute)
	s.MarkConsolidated("ns", mark)
	clk.Set(mark.Add(time.Minute))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "two"}); err != nil {
		t.Fatal(err)
	}

	d, err = s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if d.LastConsolidatedAt != mark.UTC().Format(time.RFC3339) {
		t.Fatalf("last_consolidated_at = %q", d.LastConsolidatedAt)
	}
	if d.WritesSinceConsolidation != 1 {
		t.Fatalf("writes_since_consolidation = %d, want 1", d.WritesSinceConsolidation)
	}
}
