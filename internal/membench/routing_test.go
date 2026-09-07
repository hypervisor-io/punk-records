package membench

import (
	"strings"
	"testing"
)

// TestRunDetailedRouteAutoReportsMisroutes is the R01/E01 reporting
// hook: a routed run records the strategy in its manifest, the routed
// mode per query, and counts auto-router decisions that diverge from
// the scenario's expected-strategy labels. A router regression can
// never masquerade as a clean run.
func TestRunDetailedRouteAutoReportsMisroutes(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/v2024", Body: "release notes for v2024.1"},
		// Auto: identifier guard -> exact; label agrees, no misroute.
		{Type: "query", Q: "v2024.1 release notes", Expect: []string{"/v2024"}, ExpectStrategy: "exact"},
		// Auto: no decisive signal -> default exact; label says semantic:
		// a routing mistake that must be counted.
		{Type: "query", Q: "fluffy flamingo feathers", ExpectStrategy: "semantic"},
		// Unlabeled query: routed mode recorded, never a misroute.
		{Type: "query", Q: "release notes"},
	}
	run, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{
		Name: "auto", RouteStrategy: "auto", K: 5, Seed: 1,
		Commit: "route-fixture", EmbedderID: "none", RerankerID: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Manifest == nil || run.Manifest.Strategy != "route-auto" {
		t.Fatalf("manifest strategy = %+v, want route-auto", run.Manifest)
	}
	if len(run.Queries) != 3 {
		t.Fatalf("queries = %d, want 3", len(run.Queries))
	}
	if got := run.Queries[0].RoutedMode; got != "exact" {
		t.Fatalf("q0 routed mode = %q, want exact (identifier guard)", got)
	}
	if run.Queries[0].RouteMisroute {
		t.Fatal("q0 labeled exact, routed exact: misroute must be false")
	}
	if got := run.Queries[1].RoutedMode; got != "exact" {
		t.Fatalf("q1 routed mode = %q, want exact (default)", got)
	}
	if !run.Queries[1].RouteMisroute {
		t.Fatal("q1 labeled semantic, routed exact: misroute must be true")
	}
	if run.Queries[2].RoutedMode == "" || run.Queries[2].RouteMisroute {
		t.Fatalf("q2 unlabeled: routed=%q misroute=%v, want recorded mode, no misroute",
			run.Queries[2].RoutedMode, run.Queries[2].RouteMisroute)
	}
	if run.Summary == nil || run.Summary.RouteMisroutes != 1 {
		t.Fatalf("summary route_misroutes = %+v, want 1", run.Summary)
	}
	if run.Reason == "" || !strings.Contains(run.Reason, "misrout") {
		t.Fatalf("run reason = %q, want a misroute explanation", run.Reason)
	}
}

// TestRunDetailedRouteStrategyRejectsRerankCombo: rerank and routed
// retrieval are mutually exclusive run shapes - combining them would
// attribute a ranking to two pipelines at once.
func TestRunDetailedRouteStrategyRejectsRerankCombo(t *testing.T) {
	s := newBenchStore(t)
	_, err := RunDetailed(t.Context(), s, "bench", []Record{{Type: "query", Q: "x"}}, RunOptions{
		Name: "bad", Rerank: true, RouteStrategy: "exact",
	})
	if err == nil {
		t.Fatal("rerank + route strategy: want an error, got nil")
	}
}

// TestRunDetailedRouteSemanticNoEmbedderRecordsDegraded: route-semantic
// without an embedder degrades every query to the lexical arms. The
// report must record that as capability fallback (route_fallback +
// route_degraded), distinct from a router selection mistake
// (route_misroute): the router picked semantic correctly, the pipeline
// just could not run it. E01 strategy comparisons need the distinction.
func TestRunDetailedRouteSemanticNoEmbedderRecordsDegraded(t *testing.T) {
	s := newBenchStore(t) // no embedder
	recs := []Record{
		{Type: "fact", Key: "/a", Body: "postgres primary failover"},
		{Type: "query", Q: "what is the postgres primary", Expect: []string{"/a"}, ExpectStrategy: "semantic"},
	}
	run, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{
		Name: "sem", RouteStrategy: "semantic", K: 5, Seed: 1,
		Commit: "route-fixture", EmbedderID: "none", RerankerID: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Queries) != 1 {
		t.Fatalf("queries = %d, want 1", len(run.Queries))
	}
	q := run.Queries[0]
	if q.RoutedMode != "semantic" {
		t.Fatalf("routed mode = %q, want semantic (degradation is metadata, not a mode swap)", q.RoutedMode)
	}
	if q.RouteFallback != "no-embedder:lexical" {
		t.Fatalf("route fallback = %q, want no-embedder:lexical", q.RouteFallback)
	}
	if len(q.RouteReasons) == 0 {
		t.Fatal("route reasons empty: the selection evidence must persist per query")
	}
	if q.RouteMisroute {
		t.Fatal("mode matched the label: misroute must be false (degraded != misrouted)")
	}
	if run.Summary == nil || run.Summary.RouteDegraded != 1 {
		t.Fatalf("summary route_degraded = %+v, want 1", run.Summary)
	}
	if run.Summary.RouteMisroutes != 0 {
		t.Fatalf("summary route_misroutes = %d, want 0", run.Summary.RouteMisroutes)
	}
	if run.Reason == "" || !strings.Contains(run.Reason, "degrad") {
		t.Fatalf("run reason = %q, want the degradation aggregate explained", run.Reason)
	}
}

// TestRunDetailedRouteHistoricalIdentifierGuardRecordsDegraded:
// route-historical on an identifier-only query ("error 2024") never
// windows the identifier year; it degrades to lexical and the report
// says so per query and in aggregate.
func TestRunDetailedRouteHistoricalIdentifierGuardRecordsDegraded(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/err", Body: "error 2024 pool exhausted"},
		{Type: "query", Q: "error 2024", Expect: []string{"/err"}, ExpectStrategy: "historical"},
	}
	run, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{
		Name: "hist", RouteStrategy: "historical", K: 5, Seed: 1,
		Commit: "route-fixture", EmbedderID: "none", RerankerID: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	q := run.Queries[0]
	if q.RoutedMode != "historical" {
		t.Fatalf("routed mode = %q, want historical", q.RoutedMode)
	}
	if q.RouteFallback != "identifier-guard:lexical" {
		t.Fatalf("route fallback = %q, want identifier-guard:lexical", q.RouteFallback)
	}
	if q.RouteMisroute {
		t.Fatal("historical label, historical mode: misroute must be false")
	}
	if run.Summary == nil || run.Summary.RouteDegraded != 1 || run.Summary.RouteMisroutes != 0 {
		t.Fatalf("summary = %+v, want degraded=1 misroutes=0", run.Summary)
	}
	// The identifier year never became a window: /err was written at the
	// harness's now (outside year 2024), so only the lexical fallback
	// could retrieve it.
	if !q.Hit {
		t.Fatalf("identifier-guard query missed its gold key: %+v (a year-2024 window would exclude it)", q)
	}
}

// TestRunDetailedNonRoutedRunsCarryNoRouteDegradation: the new route_*
// fields are omitempty-additive: hybrid/rerank runs emit none of them,
// keeping Stable() byte-compatible with pre-R01 reports.
func TestRunDetailedNonRoutedRunsCarryNoRouteDegradation(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/a", Body: "postgres primary"},
		{Type: "query", Q: "postgres", Expect: []string{"/a"}},
	}
	run, err := RunDetailed(t.Context(), s, "bench", recs, RunOptions{
		Name: "base", K: 5, Seed: 1, Commit: "route-fixture", EmbedderID: "none", RerankerID: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Summary.RouteDegraded != 0 || run.Summary.RouteMisroutes != 0 {
		t.Fatalf("non-routed summary = %+v, want zero route counters", run.Summary)
	}
	q := run.Queries[0]
	if q.RoutedMode != "" || q.RouteFallback != "" || q.RouteReasons != nil || q.RouteMisroute {
		t.Fatalf("non-routed query carries route evidence: %+v", q)
	}
}
