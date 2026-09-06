package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// recordingSummarizer answers with a deterministic digest and records
// what it was shown, so tests can pin the recursion contract.
type recordingSummarizer struct {
	calls  int
	priors []string
	counts []int
}

func (r *recordingSummarizer) Summarize(_ context.Context, prior string, facts []Fact) (string, error) {
	r.calls++
	r.priors = append(r.priors, prior)
	r.counts = append(r.counts, len(facts))
	return fmt.Sprintf("summary v%d of %d events", r.calls, len(facts)), nil
}

func writeCapture(t *testing.T, s *Store, sid, sub, body string) {
	t.Helper()
	if _, err := s.Write(context.Background(), WriteInput{
		Namespace: "ns", Key: "/agent-sessions/" + sid + "/" + sub, Body: body,
		Writer: "agent-hook",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSummarizeSessions pins the rolling-summary contract: below the
// token threshold nothing happens; above it the summarizer runs once
// per session with the prior summary and only the events after it, and
// the summary supersedes itself under a stable key.
func TestSummarizeSessions(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	sum := &recordingSummarizer{}

	writeCapture(t, s, "s1", "prompt-p1", strings.Repeat("alpha ", 30))
	writeCapture(t, s, "s1", "tool-t1", strings.Repeat("beta ", 30))
	writeCapture(t, s, "s1", "injected", "id1 id2") // bookkeeping: never summarized

	// Threshold above the accumulated tokens: no-op.
	n, err := s.SummarizeSessions(ctx, "ns", 10_000, sum, time.Hour)
	if err != nil || n != 0 || sum.calls != 0 {
		t.Fatalf("below threshold: n=%d calls=%d err=%v", n, sum.calls, err)
	}

	// Low threshold: one summary from two capture events (not the
	// bookkeeping fact).
	n, err = s.SummarizeSessions(ctx, "ns", 10, sum, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("first pass: n=%d err=%v", n, err)
	}
	if sum.calls != 1 || sum.priors[0] != "" || sum.counts[0] != 2 {
		t.Fatalf("first call: %+v", sum)
	}
	sf, err := s.LatestSessionSummary(ctx, "ns")
	if err != nil || sf == nil || sf.Body != "summary v1 of 2 events" {
		t.Fatalf("latest summary: %+v err=%v", sf, err)
	}

	// No new events: nothing to do, summarizer not called again.
	n, err = s.SummarizeSessions(ctx, "ns", 10, sum, time.Hour)
	if err != nil || n != 0 || sum.calls != 1 {
		t.Fatalf("idle pass: n=%d calls=%d err=%v", n, sum.calls, err)
	}

	// A new event after the summary: recursion sees the prior summary
	// plus ONLY the fresh event.
	clk.Set(clk.t.Add(time.Minute))
	writeCapture(t, s, "s1", "prompt-p2", strings.Repeat("gamma ", 30))
	n, err = s.SummarizeSessions(ctx, "ns", 10, sum, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("second pass: n=%d err=%v", n, err)
	}
	if sum.calls != 2 || sum.priors[1] != "summary v1 of 2 events" || sum.counts[1] != 1 {
		t.Fatalf("second call: %+v", sum)
	}
	sf, _ = s.LatestSessionSummary(ctx, "ns")
	if sf == nil || sf.Body != "summary v2 of 1 events" {
		t.Fatalf("summary must supersede in place: %+v", sf)
	}

	// Nil summarizer / zero threshold: hard no-ops.
	if n, err := s.SummarizeSessions(ctx, "ns", 0, sum, time.Hour); err != nil || n != 0 {
		t.Fatalf("zero threshold: %d %v", n, err)
	}
	if n, err := s.SummarizeSessions(ctx, "ns", 10, nil, time.Hour); err != nil || n != 0 {
		t.Fatalf("nil summarizer: %d %v", n, err)
	}
}

// TestSummarizeSessionsIgnoresDeliveryBookkeeping pins the C04 delivery
// bookkeeping facts - the delivery marker ("<event> <rev> issued") and
// the delivered-turns pid set - as session metadata: like injected and
// summary, they never count toward the summarization threshold and are
// never fed to the summarizer.
func TestSummarizeSessionsIgnoresDeliveryBookkeeping(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	sum := &recordingSummarizer{}

	writeCapture(t, s, "s1", "prompt-p1", strings.Repeat("alpha ", 30))
	writeCapture(t, s, "s1", "tool-t1", strings.Repeat("beta ", 30))
	writeCapture(t, s, "s1", "delivery", "startup 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef issued")
	writeCapture(t, s, "s1", "delivered-turns", "p1 p2")

	n, err := s.SummarizeSessions(ctx, "ns", 10, sum, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if sum.calls != 1 || sum.priors[0] != "" || sum.counts[0] != 2 {
		t.Fatalf("delivery bookkeeping must never reach the summarizer: %+v", sum)
	}
}
