package membench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// newBenchStore opens an in-memory sqlite store, migrates it up, and
// wraps it in a memory.Store — the same wiring cmdMembench uses.
func newBenchStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "membench.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return memory.New(db, nil)
}

func TestRunScoresRecallAndMRR(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/svc/db", Body: "postgres primary lives on host7"},
		{Type: "fact", Key: "/svc/cache", Body: "redis cache cluster on host9"},
		// SQLite FTS5 MATCH ANDs every token together with no stopword
		// removal, so a query padded with "where/is/the" can never hit a
		// short fact body; keep the query to content words present in it.
		{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}},
		{Type: "query", Q: "entirely unrelated flamingo census", Expect: []string{"/svc/cache"}},
	}
	res, err := Run(t.Context(), s, "bench", recs, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queries != 2 {
		t.Fatalf("Queries = %d, want 2", res.Queries)
	}
	if res.RecallAtK != 0.5 { // first query hits, flamingo query cannot
		t.Fatalf("RecallAtK = %v, want 0.5", res.RecallAtK)
	}
	if res.MRR != 0.5 { // first query: expected key at rank 1 => 1.0; second: 0
		t.Fatalf("MRR = %v, want 0.5", res.MRR)
	}
}

func TestLoadParsesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.jsonl")
	content := `{"type":"fact","key":"/svc/db","body":"postgres primary on host7"}
{"type":"query","q":"where is postgres","expect":["/svc/db"]}

`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("len(recs) = %d, want 2 (blank line must be skipped)", len(recs))
	}
	fact := recs[0]
	if fact.Type != "fact" || fact.Key != "/svc/db" || fact.Body != "postgres primary on host7" {
		t.Fatalf("fact record = %+v, want type=fact key=/svc/db body=%q", fact, "postgres primary on host7")
	}
	query := recs[1]
	if query.Type != "query" || query.Q != "where is postgres" || len(query.Expect) != 1 || query.Expect[0] != "/svc/db" {
		t.Fatalf("query record = %+v, want type=query q=%q expect=[/svc/db]", query, "where is postgres")
	}
}

// The E01 tests below cover the versioned detailed report (report.go):
// per-query ranking/latency persistence, hit_at_k vs evidence recall_at_k
// vs mrr, unanswerable/failed accounting, namespace isolation, and the
// stable-manifest reproducibility proofs.

func suiteFixtureRecs(t *testing.T) []Record {
	t.Helper()
	recs, err := Load(filepath.Join("..", "..", "scenarios", "membench", "baseline.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func suiteOpts() SuiteOptions {
	return SuiteOptions{K: 5, Seed: 1, Commit: "test", EmbedderID: "none", RerankerID: "none", Fixture: "baseline.jsonl"}
}

// TestRunDetailedPartialMultiHop is the E01 red-proof metric case: a
// multi-hop query with two required facts of which only one is retrievable
// must score hit_at_k=1 and evidence recall_at_k=0.5: Hit is any-hit,
// EvidenceRecall is retrieved unique gold IDs / all unique gold IDs. The
// query matches only /deploy/api ("argocd", "repo", "punk", "api");
// /deploy/rollout shares no token with it and there are no links for the
// bridge arm to pull it in.
func TestRunDetailedPartialMultiHop(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/deploy/api", Body: "api service deploys from repo punk-api via argocd"},
		{Type: "fact", Key: "/deploy/rollout", Body: "rollout policy: canary at 10 percent, then full fleet"},
		{Type: "query", Q: "argocd repo punk-api", Expect: []string{"/deploy/api", "/deploy/rollout"}},
	}
	run, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{Name: "baseline", K: 5, Mode: "cold", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Queries) != 1 {
		t.Fatalf("len(Queries) = %d, want 1", len(run.Queries))
	}
	q := run.Queries[0]
	if !q.Hit {
		t.Fatalf("Hit = false, want true (one of two gold facts was retrieved)")
	}
	if q.EvidenceRecall != 0.5 {
		t.Fatalf("EvidenceRecall = %v, want 0.5 (1 of 2 unique gold IDs retrieved)", q.EvidenceRecall)
	}
	if q.ReciprocalRank != 1.0 {
		t.Fatalf("ReciprocalRank = %v, want 1.0", q.ReciprocalRank)
	}
	if run.Summary.HitAtK != 1.0 {
		t.Fatalf("HitAtK = %v, want 1.0", run.Summary.HitAtK)
	}
	if run.Summary.EvidenceRecallAtK != 0.5 {
		t.Fatalf("EvidenceRecallAtK = %v, want 0.5", run.Summary.EvidenceRecallAtK)
	}
	if run.Summary.MRR != 1.0 {
		t.Fatalf("MRR = %v, want 1.0", run.Summary.MRR)
	}
}

// TestRunDetailedDuplicateExpectDoesNotInflateRecall: duplicate IDs in a
// query's expect list are one gold fact, not two: the recall denominator
// is the unique gold set (a naive count would score 1/3 here).
func TestRunDetailedDuplicateExpectDoesNotInflateRecall(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/deploy/api", Body: "api service deploys from repo punk-api via argocd"},
		{Type: "fact", Key: "/deploy/rollout", Body: "rollout policy: canary at 10 percent, then full fleet"},
		{Type: "query", Q: "argocd repo punk-api", Expect: []string{"/deploy/api", "/deploy/api", "/deploy/rollout"}},
	}
	run, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{Name: "baseline", K: 5, Mode: "cold", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.Queries[0].EvidenceRecall; got != 0.5 {
		t.Fatalf("EvidenceRecall = %v, want 0.5 (unique gold {api, rollout}, one retrieved)", got)
	}
}

// TestRunDetailedUnanswerableCountedNotDropped: an empty-gold query has a
// separate denominator: it must appear in the per-query report and in the
// Unanswerable count, and the metric means must stay 0 (no division by
// zero, no NaN).
func TestRunDetailedUnanswerableCountedNotDropped(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/svc/db", Body: "postgres primary on host7"},
		{Type: "query", Q: "quantum flux capacitor calibration"}, // no expect: unanswerable
	}
	run, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{Name: "baseline", K: 5, Mode: "cold", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if run.Summary.Queries != 1 || run.Summary.Answerable != 0 || run.Summary.Unanswerable != 1 {
		t.Fatalf("summary = %+v, want queries=1 answerable=0 unanswerable=1", *run.Summary)
	}
	if run.Summary.HitAtK != 0 || run.Summary.EvidenceRecallAtK != 0 || run.Summary.MRR != 0 {
		t.Fatalf("summary = %+v, want zeroed means (no division by zero)", *run.Summary)
	}
	if len(run.Queries) != 1 {
		t.Fatal("unanswerable query disappeared from the per-query report")
	}
	if len(run.Queries[0].Ranking) != 0 {
		t.Fatalf("Ranking = %v, want empty (no fixture fact matches these tokens)", run.Queries[0].Ranking)
	}
	if run.Queries[0].Error != "" {
		t.Fatalf("Error = %q, want empty (a no-match search is not an error)", run.Queries[0].Error)
	}
}

// TestRunDetailedFailedQueryCounted: a real retrieval error (a
// whitespace-only query is rejected by Store.Search) must be recorded on
// the per-query result and counted in Failed, never silently dropped, and
// never aborting the run. A failed answerable query stays in the metric
// denominators as a miss, so failures depress the metrics instead of
// vanishing from them.
func TestRunDetailedFailedQueryCounted(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/svc/db", Body: "postgres primary on host7"},
		{Type: "query", Q: "   ", Expect: []string{"/svc/db"}},
	}
	run, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{Name: "baseline", K: 5, Mode: "cold", Seed: 1})
	if err != nil {
		t.Fatalf("query errors must be recorded per-query, not abort the run: %v", err)
	}
	if run.Summary.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", run.Summary.Failed)
	}
	if run.Summary.Queries != 1 || run.Summary.Answerable != 1 {
		t.Fatalf("summary = %+v, want queries=1 answerable=1 (failed query still counted)", *run.Summary)
	}
	q := run.Queries[0]
	if q.Error == "" {
		t.Fatal("Error = empty, want the recorded retrieval error")
	}
	if !strings.Contains(q.Error, "empty search query") {
		t.Fatalf("Error = %q, want it to mention the empty search query", q.Error)
	}
	if q.Hit || run.Summary.HitAtK != 0 || run.Summary.MRR != 0 {
		t.Fatalf("failed query scored as a hit: %+v", *run.Summary)
	}
	if len(run.Queries) != 1 {
		t.Fatal("failed query disappeared from the per-query report")
	}
}

// TestRunDetailedNamespaceIsolation: a warm run against a namespace that
// was never ingested must retrieve nothing: fixture facts in ns A can
// never satisfy queries scored in ns B (no cross-namespace leakage), and
// the isolated queries are still counted.
func TestRunDetailedNamespaceIsolation(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/svc/db", Body: "postgres primary on host7"},
		{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}},
	}
	control, err := RunDetailed(t.Context(), s, "bench-a", recs, RunOptions{Name: "ingest-a", K: 5, Mode: "cold", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !control.Queries[0].Hit {
		t.Fatal("control run in bench-a should hit its own fact")
	}
	isolated, err := RunDetailed(t.Context(), s, "bench-b", recs, RunOptions{Name: "query-b", K: 5, Mode: "warm", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Queries[0].Hit || len(isolated.Queries[0].Ranking) != 0 {
		t.Fatalf("cross-namespace leakage: bench-b saw ranking %v", isolated.Queries[0].Ranking)
	}
	if isolated.Summary.Queries != 1 || isolated.Summary.HitAtK != 0 {
		t.Fatalf("isolated summary = %+v, want queries=1 hit_at_k=0", *isolated.Summary)
	}
}

// TestSuiteStableReportReproducible is the E01 reproducibility red proof:
// a fixed fixture and seed must yield byte-identical stable report content
// (everything except the documented volatile fields: generated_at,
// runs[].duration_ns, runs[].queries[].latency_ns) across two full suite
// runs on two fresh stores. Source identity is NOT volatile: both runs
// share one binary, so their identical BuildSourceRevision stays in the
// stable bytes. The test also pins the suite shape: baseline (cold,
// k=5) + top1 (warm, k=1) execute offline, and both model-based ablations
// are explicitly marked unavailable with reasons, never silently absent.
func TestSuiteStableReportReproducible(t *testing.T) {
	recs := suiteFixtureRecs(t)
	rep1, err := Suite(t.Context(), newBenchStore(t), "bench", recs, suiteOpts())
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := Suite(t.Context(), newBenchStore(t), "bench", recs, suiteOpts())
	if err != nil {
		t.Fatal(err)
	}
	if rep1.Schema != ReportSchema || rep2.Schema != ReportSchema {
		t.Fatalf("Schema = %q / %q, want %q", rep1.Schema, rep2.Schema, ReportSchema)
	}
	if rep1.GeneratedAt == "" {
		t.Fatal("GeneratedAt = empty, want a timestamp on the full (non-stable) report")
	}
	a, err := rep1.StableJSON()
	if err != nil {
		t.Fatal(err)
	}
	b, err := rep2.StableJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("stable report differs across identical runs:\n%s\n--- vs ---\n%s", a, b)
	}
	if !strings.Contains(string(a), `"source_revision": "`+rep1.SourceRevision+`"`) {
		t.Fatal("stable bytes lost the source identity: Stable must preserve source_revision, only ResultOnly strips it")
	}
	if len(rep1.Runs) != 4 {
		t.Fatalf("len(Runs) = %d, want 4 (baseline, top1, rerank, embed-hybrid)", len(rep1.Runs))
	}
	base, top1, rr, emb := rep1.Runs[0], rep1.Runs[1], rep1.Runs[2], rep1.Runs[3]
	if base.Name != "baseline" || !base.Available || base.Manifest == nil {
		t.Fatalf("baseline run = %+v, want available with a manifest", base)
	}
	m := *base.Manifest
	if m.Mode != "cold" || m.K != 5 || m.Seed != 1 || m.Commit != "test" {
		t.Fatalf("baseline manifest = %+v, want mode=cold k=5 seed=1 commit=test", m)
	}
	if m.Strategy != "hybrid" || m.RecencyHalfLife != "0s" {
		t.Fatalf("baseline manifest = %+v, want strategy=hybrid recency_half_life=0s", m)
	}
	if m.EmbedderID != "none" || m.RerankerID != "none" {
		t.Fatalf("baseline manifest = %+v, want embedder/reranker IDs none (offline run)", m)
	}
	if len(m.CorpusSHA256) != 64 {
		t.Fatalf("CorpusSHA256 = %q, want a 64-char hex sha256", m.CorpusSHA256)
	}
	if top1.Name != "top1" || !top1.Available || top1.Manifest == nil || top1.Manifest.K != 1 || top1.Manifest.Mode != "warm" {
		t.Fatalf("top1 run = %+v, want available warm k=1", top1)
	}
	if rr.Name != "rerank" || rr.Available || rr.Reason == "" {
		t.Fatalf("rerank run = %+v, want unavailable with an explicit reason", rr)
	}
	if emb.Name != "embed-hybrid" || emb.Available || emb.Reason == "" {
		t.Fatalf("embed-hybrid run = %+v, want unavailable with an explicit reason", emb)
	}
}

// TestManifestChangesWithCorpusAndStrategy is the second half of the red
// proof: changing the corpus or the retrieval strategy must change the
// recorded manifest, while identical inputs must produce an identical one.
func TestManifestChangesWithCorpusAndStrategy(t *testing.T) {
	recs := suiteFixtureRecs(t)
	h1, err := CorpusHash(recs)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]Record(nil), recs...)
	changed[0] = Record{Type: changed[0].Type, Key: changed[0].Key, Body: changed[0].Body + " (changed)"}
	h2, err := CorpusHash(changed)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("corpus change did not change CorpusSHA256")
	}

	s := newBenchStore(t)
	base, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{Name: "baseline", K: 5, Mode: "cold", Seed: 1, Commit: "test"})
	if err != nil {
		t.Fatal(err)
	}
	reranked, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{Name: "rerank", K: 5, Mode: "warm", Rerank: true, Seed: 1, Commit: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if reranked.Manifest.Strategy != "hybrid-reranked" {
		t.Fatalf("Strategy = %q, want hybrid-reranked", reranked.Manifest.Strategy)
	}
	if reflect.DeepEqual(base.Manifest, reranked.Manifest) {
		t.Fatal("strategy change did not change the recorded manifest")
	}
	top1, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{Name: "top1", K: 1, Mode: "warm", Seed: 1, Commit: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(base.Manifest, top1.Manifest) {
		t.Fatal("k/mode change did not change the recorded manifest")
	}
	// Same corpus + same settings on a fresh store => identical manifest.
	base2, err := RunDetailed(t.Context(), newBenchStore(t), "bench", recs, RunOptions{Name: "baseline", K: 5, Mode: "cold", Seed: 1, Commit: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base.Manifest, base2.Manifest) {
		t.Fatalf("identical runs produced different manifests:\n%+v\n--- vs ---\n%+v", *base.Manifest, *base2.Manifest)
	}
}

// TestBaselineFixtureReport pins the deterministic offline baseline over
// scenarios/membench/baseline.jsonl: every contract category (correction,
// exact identifier, multi-hop evidence full and partial, alias, time
// window, missing answer, failed retrieval) has a designed outcome under
// FTS-only hybrid search at k=5, seed 1. If retrieval behavior changes,
// this test and the committed report artifact must be consciously updated.
func TestBaselineFixtureReport(t *testing.T) {
	recs := suiteFixtureRecs(t)
	rep, err := Suite(t.Context(), newBenchStore(t), "bench", recs, suiteOpts())
	if err != nil {
		t.Fatal(err)
	}
	base := rep.Runs[0]
	sum := *base.Summary
	if sum.Queries != 9 || sum.Answerable != 8 || sum.Unanswerable != 1 || sum.Failed != 1 {
		t.Fatalf("summary = %+v, want queries=9 answerable=8 unanswerable=1 failed=1", sum)
	}
	if sum.HitAtK != 0.875 { // 7 of 8 answerable hit; the failed query counts as a miss
		t.Fatalf("HitAtK = %v, want 0.875", sum.HitAtK)
	}
	if sum.EvidenceRecallAtK != 0.8125 { // (6*1.0 + 0.5 partial multi-hop + 0 failed) / 8
		t.Fatalf("EvidenceRecallAtK = %v, want 0.8125", sum.EvidenceRecallAtK)
	}
	if sum.MRR != 0.875 { // all 7 hits at rank 1
		t.Fatalf("MRR = %v, want 0.875", sum.MRR)
	}
	if sum.LegacyRecallAtK != 7.0/9.0 { // legacy any-hit over ALL queries, unanswerable included
		t.Fatalf("LegacyRecallAtK = %v, want %v", sum.LegacyRecallAtK, 7.0/9.0)
	}
	wantRank1 := map[string]string{
		"correction postgres primary moved ceph-node-09": "/svc/db/correction",
		"failover promote replica haproxy":               "/svc/db/failover",
		"ERR_POOL_EXHAUSTED intake pool":                 "/incidents/err-pool",
		"argocd canary rollout":                          "/deploy/rollout",
		"argocd repo punk-api":                           "/deploy/api",
		"kubernetes control plane":                       "/infra/k8s",
		"maintenance window ceph upgrade":                "/ops/window",
	}
	seen := 0
	for _, q := range base.Queries {
		if want, ok := wantRank1[q.Q]; ok {
			seen++
			if !q.Hit {
				t.Fatalf("query %q Hit = false, want true", q.Q)
			}
			if len(q.Ranking) == 0 || q.Ranking[0] != want {
				t.Fatalf("query %q ranking = %v, want rank 1 = %s", q.Q, q.Ranking, want)
			}
			if q.Q == "argocd repo punk-api" && q.EvidenceRecall != 0.5 {
				t.Fatalf("partial multi-hop EvidenceRecall = %v, want 0.5", q.EvidenceRecall)
			}
			if q.Q == "argocd canary rollout" && q.EvidenceRecall != 1.0 {
				t.Fatalf("full multi-hop EvidenceRecall = %v, want 1.0 (both gold facts in top-5)", q.EvidenceRecall)
			}
			continue
		}
		switch q.Q {
		case "quantum flux capacitor calibration": // missing answer
			seen++
			if len(q.Expect) != 0 {
				t.Fatalf("unanswerable query Expect = %v, want empty", q.Expect)
			}
			if len(q.Ranking) != 0 {
				t.Fatalf("unanswerable query ranking = %v, want empty", q.Ranking)
			}
		case "   ": // failed retrieval, counted not dropped
			seen++
			if q.Error == "" {
				t.Fatal("failed query lost its recorded error")
			}
		default:
			t.Fatalf("unexpected query in fixture report: %q", q.Q)
		}
	}
	if seen != 9 {
		t.Fatalf("saw %d of 9 fixture queries in the report", seen)
	}
	// The k=1 ablation truncates multi-gold evidence: same corpus, changed
	// strategy, measurably different (lower) evidence recall.
	top1 := rep.Runs[1]
	if top1.Summary.EvidenceRecallAtK >= sum.EvidenceRecallAtK {
		t.Fatalf("top1 EvidenceRecallAtK = %v, want below baseline %v", top1.Summary.EvidenceRecallAtK, sum.EvidenceRecallAtK)
	}
}

// TestCommittedBaselineReportMatches ties the committed offline artifact
// (scenarios/membench/baseline-report.json) to what the one-command suite
// still produces. The comparison is the EXPLICIT ResultOnly projection -
// measurement content only - because the two sides legitimately carry
// different source identities: the committed artifact records the
// VCS-stamped binary that generated it, while this test process is an
// unstamped go test binary reporting "unknown". That is a conscious
// cross-revision metric comparison, not a provenance check: provenance
// honesty is pinned separately by TestCommittedBaselineReportProvenance
// (format, never a build stamp, retained by Stable). Any corpus or
// retrieval change still forces a conscious regeneration of the
// committed baseline, and nothing here compares the artifact's recorded
// revision against the current HEAD, so the test is not self-referential.
func TestCommittedBaselineReportMatches(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scenarios", "membench", "baseline-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var committed Report
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Schema != ReportSchema {
		t.Fatalf("committed report schema = %q, want %q", committed.Schema, ReportSchema)
	}
	if len(committed.Runs) == 0 || committed.Runs[0].Manifest == nil {
		t.Fatal("committed report has no baseline run manifest")
	}
	opts := suiteOpts()
	opts.Commit = committed.Runs[0].Manifest.Commit // the build stamp recorded at generation time
	rep, err := Suite(t.Context(), newBenchStore(t), "bench", suiteFixtureRecs(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	a, err := committed.ResultOnlyJSON()
	if err != nil {
		t.Fatal(err)
	}
	b, err := rep.ResultOnlyJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("committed baseline report no longer reproduces; regenerate scenarios/membench/baseline-report.json and review the delta:\ncommitted:\n%s\ngot:\n%s", a, b)
	}
}
