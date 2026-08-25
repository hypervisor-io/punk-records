package memory

import (
	"context"
	"math"
	"testing"
)

// TestRecordFeedbackEWMA proves the EWMA update: rating 1.0 raises weight
// toward 1 by alpha each call, rating 0 lowers it, and both ends are
// clamped to [0,1]. An unknown ID (or namespace) is a no-op, not an error.
func TestRecordFeedbackEWMA(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/k", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if f.FeedbackWeight != 0.5 {
		t.Fatalf("default FeedbackWeight = %v, want 0.5", f.FeedbackWeight)
	}

	if err := s.RecordFeedback(ctx, "ns", []string{f.ID}, 1.0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Recall(ctx, "ns", "/k", 1)
	want := 0.5 + feedbackAlpha*(1.0-0.5) // 0.65
	if len(got) != 1 || math.Abs(got[0].FeedbackWeight-want) > 1e-9 {
		t.Fatalf("FeedbackWeight after +1 rating = %v, want %v", got[0].FeedbackWeight, want)
	}

	// repeated high ratings keep climbing toward 1, never overshoot. Gap
	// shrinks by (1-alpha) per call, so 100 calls closes it well under
	// float64 epsilon at 1.0.
	for i := 0; i < 100; i++ {
		if err := s.RecordFeedback(ctx, "ns", []string{f.ID}, 1.0); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.Recall(ctx, "ns", "/k", 1)
	if got[0].FeedbackWeight > 1 || math.Abs(got[0].FeedbackWeight-1) > 1e-6 {
		t.Fatalf("FeedbackWeight after repeated +1 = %v, want ~1 and clamped", got[0].FeedbackWeight)
	}

	if err := s.RecordFeedback(ctx, "ns", []string{f.ID}, 0.0); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Recall(ctx, "ns", "/k", 1)
	if got[0].FeedbackWeight >= 1 {
		t.Fatalf("FeedbackWeight after a 0 rating = %v, want a drop below 1", got[0].FeedbackWeight)
	}

	// unknown id: no-op, no error
	if err := s.RecordFeedback(ctx, "ns", []string{"does-not-exist"}, 1.0); err != nil {
		t.Fatal(err)
	}
	// unknown namespace: no-op, no error
	if err := s.RecordFeedback(ctx, "ghost-ns", []string{f.ID}, 1.0); err != nil {
		t.Fatal(err)
	}

	// rating out of range clamps rather than errors or overshooting [0,1]
	if err := s.RecordFeedback(ctx, "ns", []string{f.ID}, 5.0); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Recall(ctx, "ns", "/k", 1)
	if got[0].FeedbackWeight > 1 {
		t.Fatalf("FeedbackWeight after out-of-range rating = %v, want clamped to <=1", got[0].FeedbackWeight)
	}
}

// TestFeedbackComponentInScore proves the feedback salience component: a
// fact with feedback_weight 1.0 outranks an equal-relevance sibling at the
// default 0.5, and the component is present at the documented 1.15 value.
func TestFeedbackComponentInScore(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "postgres primary failover runbook"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "postgres primary failover checklist"}); err != nil {
		t.Fatal(err)
	}
	// converge feedback_weight to (float64-indistinguishable-from) 1.0
	for i := 0; i < 100; i++ {
		if err := s.RecordFeedback(ctx, "ns", []string{a.ID}, 1.0); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.HybridSearchScored(ctx, "ns", "postgres failover", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	if res[0].Key != "/a" {
		t.Fatalf("top = %s, want /a (feedback boost)", res[0].Key)
	}
	c := res[0].Components
	if math.Abs(c["feedback"]-1.15) > 1e-9 {
		t.Fatalf("feedback component = %v, want 1.15", c["feedback"])
	}

	// the untouched sibling stays at the neutral default: no ranking change
	for _, r := range res {
		if r.Key == "/b" && r.Components["feedback"] != 1.0 {
			t.Fatalf("default-weight fact feedback component = %v, want 1.0", r.Components["feedback"])
		}
	}
}
