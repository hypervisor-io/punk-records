package task

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Task S02: the skill_run event anchors a procedural-skill run outcome
// to its originating task. Every event constant gets a payload struct
// and a round-trip test (package convention).
func TestAppendSkillRunEventRoundTrip(t *testing.T) {
	l, _, _ := newTest(t)
	ctx := context.Background()

	tk, _, err := l.Submit(ctx, SubmitInput{Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	p := SkillRunPayload{
		SkillName:     "db-connection-triage",
		SkillVersion:  "0.1.0",
		Outcome:       "failed",
		SuccessScore:  0.25,
		ErrorType:     "tool_failure",
		ErrorMessage:  "pool exhausted after 3 retries",
		ResultSummary: "2 of 5 triage checks failed",
	}
	seq, err := l.AppendSkillRun(ctx, tk.ID, "agent:db", p)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 { // after the submitted event
		t.Fatalf("seq = %d, want 2", seq)
	}

	_, events, err := l.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found *Event
	for i := range events {
		if events[i].Type == EventSkillRun && events[i].Seq == seq {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no skill_run event at seq %d: %+v", seq, events)
	}
	if found.Actor != "agent:db" {
		t.Fatalf("actor = %q, want agent:db", found.Actor)
	}
	var back SkillRunPayload
	if err := json.Unmarshal(found.Payload, &back); err != nil {
		t.Fatal(err)
	}
	if back != p {
		t.Fatalf("round trip: %+v != %+v", back, p)
	}
}

// AppendSkillRunOnce is the atomic anchor boundary for one execution
// identity - (namespace, task, execution id): an exact-content retry
// reuses the first event's sequence without appending, changed content
// for the identity is ErrSkillRunConflict without appending, and
// another namespace's event is never reused (same id, distinct
// execution, own event).
func TestAppendSkillRunOnceAnchorSemantics(t *testing.T) {
	l, _, _ := newTest(t)
	ctx := context.Background()

	tk, _, err := l.Submit(ctx, SubmitInput{Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	p := SkillRunPayload{
		SkillName:     "db-connection-triage",
		SkillVersion:  "0.1.0",
		Namespace:     "ns-a",
		ExecutionID:   "exec-1",
		Outcome:       "failed",
		SuccessScore:  0.1,
		ErrorType:     "tool_failure",
		ErrorMessage:  "pool exhausted after 3 retries",
		ResultSummary: "2 of 5 triage checks failed",
	}
	seq, reused, err := l.AppendSkillRunOnce(ctx, tk.ID, "agent:db", p)
	if err != nil || reused {
		t.Fatalf("first append: seq=%d reused=%t err=%v, want a fresh event", seq, reused, err)
	}
	if seq != 2 { // after the submitted event
		t.Fatalf("seq = %d, want 2", seq)
	}

	// Exact retry: reuse the anchor, append nothing.
	seq2, reused2, err := l.AppendSkillRunOnce(ctx, tk.ID, "agent:db", p)
	if err != nil || !reused2 || seq2 != seq {
		t.Fatalf("exact retry: seq=%d reused=%t err=%v, want reuse of seq %d", seq2, reused2, err, seq)
	}

	// Changed outcome content for the same identity: conflict.
	changed := p
	changed.Outcome, changed.SuccessScore = "success", 1
	if _, _, err := l.AppendSkillRunOnce(ctx, tk.ID, "agent:db", changed); !errors.Is(err, ErrSkillRunConflict) {
		t.Fatalf("changed retry: err = %v, want ErrSkillRunConflict", err)
	}
	// A different recording actor is a content conflict too.
	if _, _, err := l.AppendSkillRunOnce(ctx, tk.ID, "agent:other", p); !errors.Is(err, ErrSkillRunConflict) {
		t.Fatalf("actor-changed retry: err = %v, want ErrSkillRunConflict", err)
	}

	// Another namespace's execution with the same id: distinct anchor,
	// own event, never a reuse of ns-a's.
	other := p
	other.Namespace = "ns-b"
	seq3, reused3, err := l.AppendSkillRunOnce(ctx, tk.ID, "agent:db", other)
	if err != nil || reused3 || seq3 <= seq {
		t.Fatalf("other-namespace append: seq=%d reused=%t err=%v, want a fresh event after %d", seq3, reused3, err, seq)
	}

	// Without an execution id there is no identity to dedup on: refused.
	if _, _, err := l.AppendSkillRunOnce(ctx, tk.ID, "agent:db", SkillRunPayload{
		SkillName: p.SkillName, SkillVersion: p.SkillVersion,
	}); err == nil {
		t.Fatal("AppendSkillRunOnce without an execution id accepted")
	}

	_, events, err := l.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == EventSkillRun {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d skill_run events, want exactly 2 (ns-a and ns-b)", n)
	}
}
