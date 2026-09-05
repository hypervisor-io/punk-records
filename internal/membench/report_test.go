package membench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Focused tests for the E01 review fixes:
//
//  1. (r1) a reranker failure or fallback must be observable: memory
//     .applyRerank degrades silently, so the benchmark records degraded
//     runs and per-query fallback evidence instead of reporting an
//     available successful rerank ablation;
//  2. (r3) the report carries honest code-state provenance (Report
//     .SourceRevision): the binary's own embedded VCS revision
//     ("+modified" when the build tree was dirty) or explicit "unknown"
//     - never a build stamp like "dev", and never an unrelated
//     invocation repository's HEAD presented as the source identity.
//     The build-info resolution itself is covered in provenance_test.go
//     and end-to-end (same binary, unrelated invocation repo) in
//     cmd/punk/main_test.go.

// sourceRevisionRe matches a resolved provenance value: a 40-hex git
// SHA, optionally suffixed with the +modified build-tree marker.
var sourceRevisionRe = regexp.MustCompile(`^[0-9a-f]{40}(\+modified)?$`)

// failingReranker simulates the r1 repro: a wired cross-encoder whose
// every call errors.
type failingReranker struct{}

func (failingReranker) Rerank(context.Context, string, []string) ([]float64, error) {
	return nil, errors.New("membench test: simulated reranker failure")
}

// reversingReranker returns aligned ascending scores: applyRerank sorts
// the prefix by descending rerank score, so a working reranker observably
// reverses the candidate order.
type reversingReranker struct{}

func (reversingReranker) Rerank(_ context.Context, _ string, bodies []string) ([]float64, error) {
	scores := make([]float64, len(bodies))
	for i := range scores {
		scores[i] = float64(i)
	}
	return scores, nil
}

func findRun(t *testing.T, rep Report, name string) RunReport {
	t.Helper()
	for _, r := range rep.Runs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("run %q missing from the report", name)
	return RunReport{}
}

// TestRerankFailureRecordedAsDegradedNotSuccessful is the Suite-level
// proof of r1 finding 1: a wired reranker whose calls fail makes
// applyRerank serve the baseline ranking silently. The rerank ablation
// must then be recorded as degraded baseline fallback - per query, in the
// summary count and in the run Reason - never as an available successful
// rerank with Failed=0 and no failure/fallback evidence. The retrieval
// itself succeeded, so Failed stays 0 and the fallback ranking stays in
// the metrics: degradation is not a retrieval failure.
func TestRerankFailureRecordedAsDegradedNotSuccessful(t *testing.T) {
	s := newBenchStore(t)
	s.SetReranker(failingReranker{})
	recs := []Record{
		{Type: "fact", Key: "/svc/db", Body: "postgres primary on host7"},
		{Type: "fact", Key: "/svc/cache", Body: "redis cache cluster on host9"},
		{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}},
		{Type: "query", Q: "quantum flux capacitor", Expect: []string{"/svc/cache"}}, // no candidates: neutral, not degraded
	}
	rep, err := Suite(t.Context(), s, "bench", recs, SuiteOptions{K: 5, Seed: 1, Commit: "test", RerankerID: "failing-cross-encoder", Fixture: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	rr := findRun(t, rep, "rerank")
	if !rr.Available || rr.Summary == nil || rr.Manifest == nil {
		t.Fatalf("rerank run = %+v, want available with manifest and summary", rr)
	}
	if rr.Manifest.Strategy != "hybrid-reranked" || rr.Manifest.RerankerID != "failing-cross-encoder" {
		t.Fatalf("manifest = %+v, want strategy=hybrid-reranked reranker_id=failing-cross-encoder", *rr.Manifest)
	}
	if rr.Summary.Failed != 0 {
		t.Fatalf("Failed = %d, want 0: the retrievals themselves succeeded on fallback", rr.Summary.Failed)
	}
	if rr.Summary.RerankDegraded != 1 {
		t.Fatalf("RerankDegraded = %d, want 1 (the query with candidates fell back; the empty-candidate query is neutral)", rr.Summary.RerankDegraded)
	}
	if rr.Reason == "" {
		t.Fatal("Reason = empty: a degraded rerank must carry an explicit fallback reason")
	}
	if !strings.Contains(rr.Reason, "baseline fallback") {
		t.Fatalf("Reason = %q, want it to name the baseline fallback", rr.Reason)
	}
	q := rr.Queries[0]
	if !q.RerankDegraded {
		t.Fatal("per-query RerankDegraded = false, want the fallback marked per query")
	}
	if q.Error != "" {
		t.Fatalf("Error = %q, want empty: the retrieval succeeded", q.Error)
	}
	if len(q.Ranking) == 0 || q.Ranking[0] != "/svc/db" {
		t.Fatalf("Ranking = %v, want the baseline fallback ranking with /svc/db first", q.Ranking)
	}
	if rr.Queries[1].RerankDegraded {
		t.Fatal("empty-candidate query marked degraded: with nothing to rerank there is nothing to misattribute")
	}
}

// TestRerankSuccessAppliesNotDegraded is the other direction: a working
// reranker must observably apply (its scores reverse the candidate order)
// and must never be marked degraded, so the degradation telemetry cannot
// be dismissed as always-on noise.
func TestRerankSuccessAppliesNotDegraded(t *testing.T) {
	s := newBenchStore(t)
	s.SetReranker(reversingReranker{})
	recs := []Record{
		// Deliberately lopsided token frequency: the bm25 gap between the
		// two facts is far larger than the access-count salience drift
		// between the cold and warm runs, so the pre-rerank candidate
		// order is stable and the reversal is exact.
		{Type: "fact", Key: "/svc/db", Body: "postgres postgres postgres primary host7"},
		{Type: "fact", Key: "/svc/cache", Body: "postgres client connections redis cache"},
		{Type: "query", Q: "postgres", Expect: []string{"/svc/db"}},
	}
	rep, err := Suite(t.Context(), s, "bench", recs, SuiteOptions{K: 5, Seed: 1, Commit: "test", RerankerID: "reversing", Fixture: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	base := findRun(t, rep, "baseline")
	if got := base.Queries[0].Ranking; len(got) != 2 || got[0] != "/svc/db" || got[1] != "/svc/cache" {
		t.Fatalf("baseline ranking = %v, want [/svc/db /svc/cache]", got)
	}
	rr := findRun(t, rep, "rerank")
	if !rr.Available || rr.Summary == nil {
		t.Fatalf("rerank run = %+v, want available with a summary", rr)
	}
	if rr.Reason != "" {
		t.Fatalf("Reason = %q, want empty: the reranker demonstrably ran", rr.Reason)
	}
	if rr.Summary.RerankDegraded != 0 {
		t.Fatalf("RerankDegraded = %d, want 0", rr.Summary.RerankDegraded)
	}
	q := rr.Queries[0]
	if q.RerankDegraded {
		t.Fatal("RerankDegraded = true on a successfully reranked query")
	}
	if len(q.Ranking) != 2 || q.Ranking[0] != "/svc/cache" || q.Ranking[1] != "/svc/db" {
		t.Fatalf("rerank ranking = %v, want [/svc/cache /svc/db]: the ascending cross-encoder scores must reverse the baseline order", q.Ranking)
	}
}

// TestRerankDeclaredButUnwiredIsDegraded: a reranker ID declared in the
// options but never wired into the store leaves applyRerank a no-op; the
// ablation must be recorded as degraded fallback, never as a successful
// reranked run.
func TestRerankDeclaredButUnwiredIsDegraded(t *testing.T) {
	s := newBenchStore(t) // no SetReranker: applyRerank is a silent no-op
	recs := []Record{
		{Type: "fact", Key: "/svc/db", Body: "postgres primary on host7"},
		{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}},
	}
	rep, err := Suite(t.Context(), s, "bench", recs, SuiteOptions{K: 5, Seed: 1, Commit: "test", RerankerID: "declared-but-never-wired", Fixture: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	rr := findRun(t, rep, "rerank")
	if !rr.Available || rr.Summary == nil {
		t.Fatalf("rerank run = %+v, want available with a summary", rr)
	}
	if rr.Reason == "" {
		t.Fatal("Reason = empty: a declared reranker that never applied must be recorded as degraded fallback")
	}
	if rr.Summary.RerankDegraded != 1 || !rr.Queries[0].RerankDegraded {
		t.Fatalf("summary %+v / query %+v, want the unwired fallback counted and flagged", *rr.Summary, rr.Queries[0])
	}
}

// TestCommittedBaselineReportProvenance: the committed artifact must
// carry honest code-state provenance - the generating binary's embedded
// VCS revision ("+modified" when built from a dirty tree) or explicit
// "unknown" - never "dev" or another build stamp presented as a
// reproducibility identity, and never an invocation repository's HEAD.
// Format-only by design: the value is NOT compared against the current
// HEAD, so regenerating the report at a later commit is fine (no
// self-referential commit requirement). The measurement content is
// verified separately by TestCommittedBaselineReportMatches via the
// explicit ResultOnly projection.
func TestCommittedBaselineReportProvenance(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scenarios", "membench", "baseline-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var committed Report
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatal(err)
	}
	if committed.SourceRevision == "" || committed.SourceRevision == "dev" {
		t.Fatalf("committed source_revision = %q, want an embedded VCS revision, revision+modified, or explicit %q", committed.SourceRevision, SourceRevisionUnknown)
	}
	if committed.SourceRevision != SourceRevisionUnknown && !sourceRevisionRe.MatchString(committed.SourceRevision) {
		t.Fatalf("committed source_revision = %q, want <40-hex>[+modified] or %q", committed.SourceRevision, SourceRevisionUnknown)
	}
	// The stable serialization of the committed artifact must retain the
	// identity: provenance is part of the reproducibility claim.
	stable, err := committed.StableJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stable), `"source_revision": "`+committed.SourceRevision+`"`) {
		t.Fatalf("committed report's stable serialization lost its source identity %q", committed.SourceRevision)
	}
}
