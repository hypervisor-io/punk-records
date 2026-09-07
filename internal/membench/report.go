package membench

// Versioned run report for the membench harness (E01). The mechanism
// follows the Cognee eval_framework (cognee/eval_framework/run_eval.py
// and cognee/eval_framework/evaluation/metrics/context_coverage.py,
// pinned at commit 78ff576559a7f75f65884c5bd90b22cdc790016e): separated
// corpus ingestion, retrieval and grading, per-query artifacts, aggregate
// metrics, and evaluators that cannot run being reported explicitly
// unavailable instead of silently skipped. Reimplemented from scratch in
// Go against punk's Store.HybridSearch; no Cognee code is copied.
//
// Like RunLoCoMo's evidence recall, everything here is RETRIEVAL scoring
// over gold fact keys, deliberately distinct from answer accuracy, which
// is a separate grading stage (E02). No run in this file makes model
// calls by itself: ablations that need an embedder or reranker only run
// when the caller has explicitly wired one.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// ReportSchema versions the machine-readable report format. Consumers
// must check it before trusting field semantics: the legacy Result's
// RecallAtK is an any-hit rate, while this report's HitAtK /
// EvidenceRecallAtK / MRR have the precisely-defined meanings documented
// on Summary.
const ReportSchema = "punk.membench.report/v1"

// Manifest records everything that determines a run's rankings: the
// corpus (by content hash), the seed, the build commit, the wired
// embedding/reranker model IDs, the retrieval settings and the warm/cold
// mode. Two runs with equal manifests over equal stores must produce
// equal rankings; a changed corpus or strategy must produce a different
// manifest.
type Manifest struct {
	CorpusSHA256 string `json:"corpus_sha256"`
	Seed         int64  `json:"seed"`
	// Commit is the caller-supplied build/commit label (the CLI defaults
	// it to the binary version). It is NOT the code-state provenance:
	// that is Report.SourceRevision, read from the binary's own embedded
	// VCS build info, so a generic "dev" build stamp is never the only
	// identity a report carries.
	Commit     string `json:"commit"`
	EmbedderID string `json:"embedder_id"` // "none" when no embedder is wired (FTS-only)
	RerankerID string `json:"reranker_id"` // "none" when no reranker is wired
	// Strategy names the retrieval pipeline: "hybrid" | "hybrid-reranked"
	// | "route-"+mode for R01 routed runs ("route-exact", "route-auto",
	// ...). Two runs differing only in strategy must differ here so
	// their manifests never compare equal.
	Strategy string `json:"strategy"`
	K        int    `json:"k"`
	// RecencyHalfLife is the recency half-life passed to search. membench
	// always disables recency ("0s") so ingestion time cannot affect
	// scoring; recorded so the setting is part of the reproducibility
	// claim rather than an assumption.
	RecencyHalfLife string `json:"recency_half_life"`
	Mode            string `json:"mode"` // "cold" (ingest then query) | "warm" (query an already-ingested corpus)
}

// QueryResult is the persisted per-query evidence of one run: the full
// retrieved ranking, the outcome metrics, the measured latency and, for a
// failed retrieval, the error. LatencyNanos is VOLATILE (excluded from
// stable comparisons by Report.Stable).
type QueryResult struct {
	Q              string   `json:"q"`
	Expect         []string `json:"expect"` // gold keys as written in the scenario (may contain duplicates)
	Ranking        []string `json:"ranking"`
	Hit            bool     `json:"hit"`
	EvidenceRecall float64  `json:"evidence_recall"`
	ReciprocalRank float64  `json:"reciprocal_rank"`
	LatencyNanos   int64    `json:"latency_ns"`
	Error          string   `json:"error,omitempty"`
	// RerankDegraded marks a query of a rerank run whose results carry no
	// rerank score component: memory.applyRerank degrades silently (a
	// reranker error or misaligned scores returns the baseline ranking
	// unchanged), so the ranking here is baseline FALLBACK, not a reranked
	// outcome. Distinct from Error: the retrieval itself succeeded. Set by
	// RunDetailed only for Rerank runs; see Summary.RerankDegraded.
	RerankDegraded bool `json:"rerank_degraded,omitempty"`
	// RoutedMode records the mode RoutedSearch actually ran for this
	// query on route-* runs (R01): for an auto run it is the router's
	// decision, the value compared against the scenario's
	// expect_strategy label. Empty on non-routed runs.
	RoutedMode string `json:"routed_mode,omitempty"`
	// RouteFallback is RoutedSearch's degradation evidence for this
	// query (e.g. "no-embedder:lexical", "identifier-guard:lexical"):
	// the nominal mode was selected but a CAPABILITY fallback ran part
	// of the pipeline lexically. Distinct from RouteMisroute, which is a
	// SELECTION error - E01 strategy comparisons must not read a
	// degraded ranking as a clean run of the nominal strategy.
	// RouteReasons persists the router's per-query reason log
	// (explicit-mode, rule names, vetoes, alias rewrites).
	RouteFallback string   `json:"route_fallback,omitempty"`
	RouteReasons  []string `json:"route_reasons,omitempty"`
	// RouteMisroute marks a routed query whose RoutedMode diverges from
	// its expect_strategy label - an observable auto-router mistake,
	// counted in Summary.RouteMisroutes. Orthogonal to Hit: a misrouted
	// query can still retrieve its gold keys. Never set on unlabeled
	// queries (no label, no judgment).
	RouteMisroute bool `json:"route_misroute,omitempty"`
}

// Summary is the aggregate over one run's query records. Denominator
// contract: HitAtK, EvidenceRecallAtK and MRR average over ANSWERABLE
// queries only (>=1 unique expected key); unanswerable queries have their
// own denominator and never divide by zero. Failed retrievals are counted
// in Failed AND remain in the answerable denominators as misses, so a
// broken retrieval depresses the metrics instead of silently shrinking
// them. Queries = Answerable + Unanswerable always holds; Failed is
// orthogonal (a failed query may be answerable or not).
type Summary struct {
	Queries      int `json:"queries"`
	Answerable   int `json:"answerable"`
	Unanswerable int `json:"unanswerable"`
	Failed       int `json:"failed"`
	// RerankDegraded counts the queries of a rerank run whose configured
	// reranker did not actually apply and whose rankings are baseline
	// fallback (see QueryResult.RerankDegraded). Orthogonal like Failed,
	// but NOT a retrieval failure: a degraded query retrieved valid
	// (fallback) results and stays in the metric denominators with them.
	// Always 0 outside rerank runs, so a silent reranker fallback can
	// never be read as a successful reranked ablation.
	RerankDegraded int `json:"rerank_degraded,omitempty"`
	// RouteMisroutes counts route-* run queries whose routed mode
	// diverged from the scenario's expect_strategy label (R01's
	// routing-mistake metric). Orthogonal like Failed: a misrouted query
	// still retrieves and stays in the metric denominators. Always 0 on
	// non-routed runs, so a misrouting auto router can never read as a
	// clean ablation.
	RouteMisroutes int `json:"route_misroutes,omitempty"`
	// RouteDegraded counts route-* run queries whose retrieval degraded
	// to a capability fallback (RoutedSearch Fallback != "", e.g.
	// semantic/relationship without an embedder, or an identifier-year
	// veto of historical). The rerank-degradation honesty contract
	// applied to routed runs: a degraded query's ranking is lexical
	// fallback, not evidence of the nominal strategy's quality.
	// Orthogonal to RouteMisroutes (selection error vs capability
	// fallback); always 0 on non-routed runs.
	RouteDegraded int `json:"route_degraded,omitempty"`
	// HitAtK is the fraction of answerable queries with >=1 expected key
	// in the top-k. EvidenceRecallAtK is the mean per-answerable-query
	// fraction of unique expected keys retrieved in the top-k (duplicate
	// results or duplicate expect IDs cannot inflate it). MRR is the mean
	// reciprocal rank of the first expected key.
	HitAtK            float64 `json:"hit_at_k"`
	EvidenceRecallAtK float64 `json:"evidence_recall_at_k"`
	MRR               float64 `json:"mrr"`
	// LegacyRecallAtK reproduces the legacy Run's Result.RecallAtK
	// semantics exactly: the any-hit rate over ALL query records,
	// unanswerable and failed included. Present so old and new numbers
	// can be compared, and so no feature is ever declared an improvement
	// from the legacy any-hit metric alone.
	LegacyRecallAtK float64 `json:"legacy_recall_at_k"`
}

// RunReport is one named run inside a Report: either an executed run
// (Available, with manifest, summary and per-query results) or an
// ablation that could not run offline (Reason says why, never silently
// absent). An Available run also carries a Reason when it DEGRADED: the
// configured reranker failed, returned misaligned scores or was not
// actually wired, so (some) rankings are baseline fallback - a degraded
// rerank can never masquerade as a successful reranked ablation.
// DurationNanos is VOLATILE (excluded by Report.Stable).
type RunReport struct {
	Name          string        `json:"name"`
	Available     bool          `json:"available"`
	Reason        string        `json:"reason,omitempty"`
	Manifest      *Manifest     `json:"manifest,omitempty"`
	Summary       *Summary      `json:"summary,omitempty"`
	Queries       []QueryResult `json:"queries,omitempty"`
	DurationNanos int64         `json:"duration_ns,omitempty"`
}

// Report is the machine-readable artifact written by "punk membench
// --report": the baseline and ablation runs over one fixture.
// GeneratedAt is VOLATILE (excluded by Stable).
type Report struct {
	Schema  string `json:"schema"`
	Fixture string `json:"fixture"`
	// SourceRevision is the code-state provenance of the binary that
	// produced the report (see BuildSourceRevision): the VCS revision
	// embedded in the binary at build time, "+modified" suffixed when
	// the build tree had uncommitted changes, or explicit "unknown" when
	// the binary carries no VCS stamp. Deliberately separate from
	// Manifest.Commit, which stays a caller-supplied build label: a
	// generic "dev" build stamp is never presented as the
	// reproducibility identity, and an invocation-time git repository
	// is never recorded as the source. NOT volatile: code identity is
	// part of the reproducibility claim, so Stable keeps it; only
	// ResultOnly strips it, explicitly, for cross-revision measurement
	// comparison.
	SourceRevision string      `json:"source_revision,omitempty"`
	GeneratedAt    string      `json:"generated_at,omitempty"`
	Runs           []RunReport `json:"runs"`
	// Answers carries the optional E02 answer stage (answers.go) when
	// SuiteOptions.Answers configured it: answer accuracy, citation
	// existence/support and abstention graded per case, kept separate
	// from the retrieval evidence metrics above. Nil (omitted) when the
	// stage was not configured, so answer-less reports keep their exact
	// v1 shape.
	Answers *AnswerReport `json:"answers,omitempty"`
}

// Stable returns the report with every VOLATILE field removed:
// generated_at, runs[].duration_ns and runs[].queries[].latency_ns (the
// report carries no run IDs and measures no cost), plus the answer
// stage's measured token cost (answers.summary.tokens/judge_tokens and
// answers.cases[].tokens/judge_tokens) and the answer cases' fact-ID
// material (retrieved_ids, cited_ids: generated per store instance, like
// latency); the deterministic retrieved keys and all answer grading
// outcomes stay. Source
// identity is NOT volatile: a reproducibility manifest must preserve the
// code the results came from, so Stable keeps SourceRevision. Two runs of
// the same binary, fixture, seed and strategy have byte-identical
// StableJSON; anything else in the artifact is part of the
// reproducibility claim.
func (r Report) Stable() Report {
	out := r
	out.GeneratedAt = ""
	out.Runs = make([]RunReport, len(r.Runs))
	for i, run := range r.Runs {
		cp := run
		cp.DurationNanos = 0
		if run.Queries != nil {
			qs := make([]QueryResult, len(run.Queries))
			copy(qs, run.Queries)
			for j := range qs {
				qs[j].LatencyNanos = 0
			}
			cp.Queries = qs
		}
		out.Runs[i] = cp
	}
	if r.Answers != nil {
		a := *r.Answers
		a.Summary.Tokens = 0
		a.Summary.JudgeTokens = 0
		a.Summary.UsageUnknown = 0
		if a.Cases != nil {
			cs := make([]AnswerCaseResult, len(a.Cases))
			copy(cs, a.Cases)
			for j := range cs {
				cs[j].Tokens = 0
				cs[j].JudgeTokens = 0
				// Fact IDs are generated per store instance, so like
				// latency they cannot be byte-compared across runs:
				// the stable form keeps the deterministic KEYS and all
				// grading outcomes; the full report keeps the IDs. A
				// grader Note may carry model text, so like usage it is
				// volatile and stripped.
				cs[j].RetrievedIDs = nil
				cs[j].CitedIDs = nil
				cs[j].Note = ""
			}
			a.Cases = cs
		}
		out.Answers = &a
	}
	return out
}

// StableJSON marshals Stable() with indentation: the canonical bytes for
// comparing two runs of the same binary or verifying a committed report
// artifact. The source identity is present in these bytes.
func (r Report) StableJSON() ([]byte, error) {
	return json.MarshalIndent(r.Stable(), "", "  ")
}

// ResultOnly returns the Stable report further stripped of source
// identity: measurement content only. It exists for ONE explicit job -
// comparing archived metrics across code revisions (e.g. verifying that
// a regenerated committed artifact still matches what the suite
// produces) where the two sides legitimately come from different code
// states. It must never be used to claim two reports came from the same
// code: that claim is what SourceRevision is for, and Stable keeps it.
func (r Report) ResultOnly() Report {
	out := r.Stable()
	out.SourceRevision = ""
	return out
}

// ResultOnlyJSON marshals ResultOnly() with indentation: the canonical
// bytes for an explicitly provenance-blind cross-revision measurement
// comparison.
func (r Report) ResultOnlyJSON() ([]byte, error) {
	return json.MarshalIndent(r.ResultOnly(), "", "  ")
}

// CorpusHash returns the hex sha256 of the canonical JSON encoding of
// recs (facts AND queries): any record-level change to the scenario
// changes the hash, while whitespace-only differences in the source file
// do not. It is the corpus fingerprint recorded in every Manifest.
func CorpusHash(recs []Record) (string, error) {
	b, err := json.Marshal(recs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// RunOptions configures one detailed run.
type RunOptions struct {
	Name   string // report name ("baseline", "top1", ...)
	K      int    // top-k per query (<=0 defaults to 5)
	Rerank bool   // route queries through HybridSearchReranked
	// RouteStrategy routes queries through memory.RoutedSearch with this
	// mode ("exact"|"semantic"|"historical"|"relationship"|"procedural"|
	// "auto"), making the run manifest "route-"+mode and recording the
	// routed mode per query (R01's per-strategy quality/cost evidence and
	// auto-router misroute counting). Mutually exclusive with Rerank: a
	// ranking must be attributable to exactly one pipeline.
	RouteStrategy string
	Mode          string // "cold" (ingest then query; default) | "warm" (query only, reuse an already-ingested corpus)
	Seed          int64  // recorded in the manifest; retrieval itself is deterministic
	Commit        string // build/commit identifier recorded in the manifest
	EmbedderID    string // model ID recorded in the manifest ("none" when unwired)
	RerankerID    string // model/endpoint ID recorded in the manifest ("none" when unwired)
}

// RunDetailed ingests the fact records (cold mode only) and scores every
// query record, persisting per-query rankings, latency and outcome, and
// aggregating the versioned Summary. Unlike the legacy Run it never
// aborts on a query error: the error is recorded on that query's result
// and counted in Failed (see Summary's denominator contract). Scoring:
//
//   - Hit: >=1 expected key in the top-k (any-hit, like legacy).
//   - EvidenceRecall: |retrieved unique expected keys| / |unique expected
//     keys|; duplicates on either side are collapsed first.
//   - ReciprocalRank: 1/rank of the first expected key in the ranking, 0
//     when absent.
//   - Empty Expect: unanswerable; searched, reported per-query, counted
//     in Unanswerable, excluded from the metric means (no division by
//     zero).
//
// Ingest errors are run-fatal (the corpus itself is broken); retrieval
// errors are per-query outcomes.
//
// Rerank runs additionally observe the configured reranker's effect:
// memory.applyRerank degrades silently (a reranker error, misaligned
// scores or an unwired reranker returns the baseline ranking unchanged),
// so every query whose non-empty results carry no rerank score component
// is marked RerankDegraded (baseline fallback, counted in
// Summary.RerankDegraded) and the run gets an explicit degradation
// Reason. A failed reranker can never be reported as a successful
// reranked ablation.
func RunDetailed(ctx context.Context, s *memory.Store, ns string, recs []Record, o RunOptions) (RunReport, error) {
	k := o.K
	if k <= 0 {
		k = 5
	}
	mode := o.Mode
	if mode == "" {
		mode = "cold"
	}
	if mode != "cold" && mode != "warm" {
		return RunReport{}, fmt.Errorf("membench: mode %q: want cold or warm", mode)
	}
	if o.Rerank && o.RouteStrategy != "" {
		return RunReport{}, fmt.Errorf("membench: rerank and route strategy %q are mutually exclusive", o.RouteStrategy)
	}
	hash, err := CorpusHash(recs)
	if err != nil {
		return RunReport{}, err
	}
	strategy := "hybrid"
	if o.Rerank {
		strategy = "hybrid-reranked"
	}
	if o.RouteStrategy != "" {
		strategy = "route-" + o.RouteStrategy
	}
	run := RunReport{
		Name:      o.Name,
		Available: true,
		Manifest: &Manifest{
			CorpusSHA256:    hash,
			Seed:            o.Seed,
			Commit:          o.Commit,
			EmbedderID:      o.EmbedderID,
			RerankerID:      o.RerankerID,
			Strategy:        strategy,
			K:               k,
			RecencyHalfLife: "0s",
			Mode:            mode,
		},
	}
	if mode == "cold" {
		for _, r := range recs {
			if r.Type != "fact" {
				continue
			}
			if _, err := s.Write(ctx, memory.WriteInput{Namespace: ns, Key: r.Key, Body: r.Body, Writer: "membench"}); err != nil {
				return RunReport{}, fmt.Errorf("ingest %s: %w", r.Key, err)
			}
		}
	}

	start := time.Now()
	sum := Summary{}
	var hitSum, recallSum, mrrSum float64
	var rerankApplied int
	for _, r := range recs {
		if r.Type != "query" {
			continue
		}
		sum.Queries++
		qr := QueryResult{Q: r.Q, Expect: r.Expect}
		t0 := time.Now()
		var facts []memory.Fact
		if o.RouteStrategy != "" {
			res, err := s.RoutedSearch(ctx, ns, memory.RouteRequest{
				Mode: memory.RouteMode(o.RouteStrategy), Query: r.Q, Limit: k,
			})
			if err != nil {
				qr.Error = err.Error()
			} else {
				qr.RoutedMode = string(res.Mode)
				qr.RouteFallback = res.Fallback
				qr.RouteReasons = res.Reasons
				if res.Fallback != "" {
					sum.RouteDegraded++
				}
				for _, h := range res.Hits {
					facts = append(facts, memory.Fact{Key: routedHitKey(h)})
				}
			}
		} else if o.Rerank {
			scored, err := s.HybridSearchReranked(ctx, ns, r.Q, k, 0)
			if err != nil {
				qr.Error = err.Error()
			} else {
				facts = make([]memory.Fact, len(scored))
				for i, sf := range scored {
					facts[i] = sf.Fact
				}
				// applyRerank degrades silently, so the rerank score
				// component it stamps on every reranked hit is the only
				// observable evidence that the configured reranker ran.
				// Empty results are neutral: no candidates, nothing to
				// rerank, nothing to misattribute.
				switch {
				case len(scored) == 0:
				case hasRerankComponent(scored):
					rerankApplied++
				default:
					qr.RerankDegraded = true
					sum.RerankDegraded++
				}
			}
		} else {
			var err error
			facts, err = s.HybridSearch(ctx, ns, r.Q, k, 0)
			if err != nil {
				qr.Error = err.Error()
			}
		}
		qr.LatencyNanos = time.Since(t0).Nanoseconds()
		if o.RouteStrategy != "" && r.ExpectStrategy != "" && qr.RoutedMode != "" && qr.RoutedMode != r.ExpectStrategy {
			qr.RouteMisroute = true
			sum.RouteMisroutes++
		}

		expect := map[string]bool{}
		for _, e := range r.Expect {
			expect[e] = true
		}
		seen := map[string]bool{}
		found := 0
		for rank, f := range facts {
			qr.Ranking = append(qr.Ranking, f.Key)
			if seen[f.Key] {
				continue // duplicate results cannot inflate recall
			}
			seen[f.Key] = true
			if !expect[f.Key] {
				continue
			}
			found++
			if !qr.Hit {
				qr.Hit = true
				qr.ReciprocalRank = 1.0 / float64(rank+1)
			}
		}
		if len(expect) > 0 {
			sum.Answerable++
			qr.EvidenceRecall = float64(found) / float64(len(expect))
		} else {
			sum.Unanswerable++ // empty gold: separate denominator, never 0/0
		}
		if qr.Error != "" {
			sum.Failed++
		}
		if qr.Hit {
			hitSum++
			mrrSum += qr.ReciprocalRank
		}
		recallSum += qr.EvidenceRecall
		run.Queries = append(run.Queries, qr)
	}
	if sum.Answerable > 0 {
		sum.HitAtK = hitSum / float64(sum.Answerable)
		sum.EvidenceRecallAtK = recallSum / float64(sum.Answerable)
		sum.MRR = mrrSum / float64(sum.Answerable)
	}
	if sum.Queries > 0 {
		sum.LegacyRecallAtK = hitSum / float64(sum.Queries)
	}
	if o.Rerank {
		switch {
		case sum.RerankDegraded > 0:
			run.Reason = fmt.Sprintf("reranker degraded on %d of %d queries: results carry no rerank score component (reranker error, misaligned scores, or reranker not wired into the store); the affected rankings are baseline fallback, not reranked", sum.RerankDegraded, sum.Queries)
		case rerankApplied == 0 && sum.Queries > 0:
			run.Reason = "no reranked results: every query errored or returned no candidates, so there is no evidence the configured reranker ran"
		}
	}
	if o.RouteStrategy != "" {
		var notes []string
		if sum.RouteMisroutes > 0 {
			notes = append(notes, fmt.Sprintf("auto router misrouted %d of %d queries: routed mode diverged from the scenario's expect_strategy label (see queries[].route_misroute)", sum.RouteMisroutes, sum.Queries))
		}
		if sum.RouteDegraded > 0 {
			notes = append(notes, fmt.Sprintf("%d of %d queries degraded to a capability fallback (see queries[].route_fallback): the nominal strategy did not fully run, so their rankings are lexical fallback, not strategy-quality evidence", sum.RouteDegraded, sum.Queries))
		}
		run.Reason = strings.Join(notes, "; ")
	}
	run.Summary = &sum
	run.DurationNanos = time.Since(start).Nanoseconds()
	return run, nil
}

// routedHitKey maps a routed hit to the ranking key evidence scoring
// compares against Expect: fact hits use the fact key; relation and
// skill hits use their CompactUnified-style rendering so the ranking is
// honest about what was surfaced (gold keys are fact keys, so a relation
// or skill hit simply never inflates recall).
func routedHitKey(h memory.UnifiedHit) string {
	switch {
	case h.Fact != nil:
		return h.Fact.Key
	case h.Triplet != nil:
		return h.Triplet.From.Key + " -> " + h.Triplet.LinkType + " -> " + h.Triplet.To.Key
	case h.Skill != nil:
		return "/skills/" + h.Skill.Name + "/" + h.Skill.Version
	}
	return ""
}

// hasRerankComponent reports whether memory.applyRerank actually applied
// the configured reranker to scored: on success it stamps every reranked
// hit's Components with its "rerank" score; on a reranker error or
// index-count mismatch it returns the baseline ranking without the
// component.
func hasRerankComponent(scored []memory.ScoredFact) bool {
	for _, sf := range scored {
		if _, ok := sf.Components["rerank"]; ok {
			return true
		}
	}
	return false
}

// SuiteOptions configures the baseline+ablation suite.
type SuiteOptions struct {
	K          int    // baseline top-k (<=0 defaults to 5)
	Seed       int64  // recorded in every manifest (fixed 1 for the offline fixture report)
	Commit     string // build/commit identifier recorded in every manifest
	EmbedderID string // "none" (or empty) when no embedder is wired
	RerankerID string // "none" (or empty) when no reranker is wired; any other value enables the rerank ablation
	Fixture    string // fixture identifier recorded in the report
	// Answers, when non-nil, attaches the E02 answer stage (answers.go):
	// after the retrieval runs it composes and grades an answer per query
	// in warm mode over the ingested corpus, recorded as Report.Answers.
	// Nil (the default) keeps the report's retrieval-only v1 shape.
	Answers *AnswerOptions
}

// Suite runs the reproducible offline suite against one store and
// returns the machine-readable Report: baseline (cold, k=K), top1 (warm,
// k=1, a deterministic settings ablation over the already-ingested
// corpus), and the model-based ablations. Rerank executes only when a
// reranker is explicitly wired; the embed-hybrid (vector-arm) ablation is
// recorded with its availability reason either way. Unavailable
// ablations are explicit report entries, never silent omissions, a
// configured reranker that fails (or is not actually wired) is recorded
// as a degraded rerank run with an explicit fallback Reason, never as a
// successful reranked ablation, and no suite path makes paid/external
// model calls by default. With SuiteOptions.Answers set the optional
// answer stage runs warm over the ingested corpus after the retrieval
// runs and lands in Report.Answers (see answers.go for its grading and
// budget contract). The report's SourceRevision records the
// running binary's embedded VCS provenance (see BuildSourceRevision):
// the code the results came from, independent of the directory the
// binary was invoked in.
func Suite(ctx context.Context, s *memory.Store, ns string, recs []Record, o SuiteOptions) (Report, error) {
	arecs := make([]AnswerRecord, len(recs))
	for i, r := range recs {
		arecs[i] = AnswerRecord{Record: r}
	}
	return suite(ctx, s, ns, arecs, o)
}

// SuiteWithAnswers is Suite over an answer-labeled scenario: the
// retrieval runs see exactly the base Records (identical corpus hash and
// rankings), while the expected answers feed the answer stage's
// exact/structured grading when SuiteOptions.Answers is set.
func SuiteWithAnswers(ctx context.Context, s *memory.Store, ns string, arecs []AnswerRecord, o SuiteOptions) (Report, error) {
	return suite(ctx, s, ns, arecs, o)
}

func suite(ctx context.Context, s *memory.Store, ns string, arecs []AnswerRecord, o SuiteOptions) (Report, error) {
	recs := BaseRecords(arecs)
	k := o.K
	if k <= 0 {
		k = 5
	}
	base := RunOptions{
		Name:       "baseline",
		K:          k,
		Mode:       "cold",
		Seed:       o.Seed,
		Commit:     o.Commit,
		EmbedderID: o.EmbedderID,
		RerankerID: o.RerankerID,
	}
	if base.EmbedderID == "" {
		base.EmbedderID = "none"
	}
	if base.RerankerID == "" {
		base.RerankerID = "none"
	}
	rep := Report{
		Schema:         ReportSchema,
		Fixture:        o.Fixture,
		SourceRevision: BuildSourceRevision(),
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	baseline, err := RunDetailed(ctx, s, ns, recs, base)
	if err != nil {
		return Report{}, err
	}
	rep.Runs = append(rep.Runs, baseline)

	top1 := base
	top1.Name, top1.K, top1.Mode = "top1", 1, "warm"
	t1, err := RunDetailed(ctx, s, ns, recs, top1)
	if err != nil {
		return Report{}, err
	}
	rep.Runs = append(rep.Runs, t1)

	if base.RerankerID != "none" {
		rr := base
		rr.Name, rr.Mode, rr.Rerank = "rerank", "warm", true
		run, err := RunDetailed(ctx, s, ns, recs, rr)
		if err != nil {
			return Report{}, err
		}
		rep.Runs = append(rep.Runs, run)
	} else {
		rep.Runs = append(rep.Runs, RunReport{
			Name:   "rerank",
			Reason: "reranker not configured: model-based ablation unavailable (no external model calls by default)",
		})
	}
	reason := "embedder not configured: --file runs are offline FTS-only, vector-arm ablation unavailable (no paid model calls by default)"
	if base.EmbedderID != "none" {
		reason = "embedder configured: the vector arm is active in every run; an FTS-only ablation requires an unwired store"
	}
	rep.Runs = append(rep.Runs, RunReport{Name: "embed-hybrid", Reason: reason})
	if o.Answers != nil {
		ao := *o.Answers
		ao.Mode = "warm" // the baseline run already ingested the corpus
		if ao.K <= 0 {
			ao.K = k
		}
		ar, err := RunAnswers(ctx, s, ns, arecs, ao)
		if err != nil {
			return Report{}, err
		}
		rep.Answers = &ar
	}
	return rep, nil
}
