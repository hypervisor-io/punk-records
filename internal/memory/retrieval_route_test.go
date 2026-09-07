package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAutoRouteYearInIdentifierRoutesExact is red proof 1: a bare year
// embedded in a version string, error code, hex literal, commit SHA,
// flag, path or file name is an IDENTIFIER, never a time filter. The
// legacy plain-search path still auto-parses "error 2024" into a window
// (untouched for compatibility); the routed surface must not.
func TestAutoRouteYearInIdentifierRoutesExact(t *testing.T) {
	queries := []string{
		"v2024.1",
		"2024.3.1 hotfix notes",
		"error 2024 connection pool exhausted",
		"exit code 2023 on startup",
		"0x2024deadbeef",
		"5dcc5ec9ab",
		"--release 2024 build",
		"/svc/db 2024 outage",
		"internal/memory/temporal.go",
	}
	for _, q := range queries {
		mode, reasons := AutoRoute(q)
		if mode != RouteExact {
			t.Errorf("AutoRoute(%q) = %s %v, want exact (identifier, not time filter)", q, mode, reasons)
		}
	}
}

// TestAutoRouteTemporalPhraseRoutesHistorical: real temporal language
// (relative windows, or a year bound by "in") routes historical.
func TestAutoRouteTemporalPhraseRoutesHistorical(t *testing.T) {
	queries := []string{
		"errors last month",
		"deploys last week",
		"what happened in 2024",
		"incidents in march 2025",
	}
	for _, q := range queries {
		mode, reasons := AutoRoute(q)
		if mode != RouteHistorical {
			t.Errorf("AutoRoute(%q) = %s %v, want historical", q, mode, reasons)
		}
	}
}

// TestRoutedSearchExplicitModeWinsBothDirections is red proof 2: an
// explicit caller mode overrides every auto signal, in both directions.
func TestRoutedSearchExplicitModeWinsBothDirections(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	// Written at the store's now, so "last week" would window it OUT:
	// an exact hit here proves no window was applied.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/outage", Body: "outage of the v2024.1 pipeline"}); err != nil {
		t.Fatal(err)
	}

	// Temporal language + explicit exact: no window, plain lexical hit.
	res, err := s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteExact, Query: "outage last week"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != RouteExact || res.RequestedMode != RouteExact {
		t.Fatalf("explicit exact: mode=%s requested=%s", res.Mode, res.RequestedMode)
	}
	if len(res.Hits) != 1 || res.Hits[0].Fact == nil || res.Hits[0].Fact.Key != "/outage" {
		t.Fatalf("explicit exact hits = %+v, want /outage (no time window)", res.Hits)
	}

	// Identifier text + explicit semantic: the identifier guard must not
	// pull the query back to exact.
	res, err = s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteSemantic, Query: "v2024.1 pipeline"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != RouteSemantic {
		t.Fatalf("explicit semantic on identifier text: mode=%s, want semantic", res.Mode)
	}
}

// TestAutoRouteAmbiguousFallsBackPredictable is red proof 3: language
// with no decisive signal falls back to the legacy-compatible lexical
// route, with the reason recorded.
func TestAutoRouteAmbiguousFallsBackPredictable(t *testing.T) {
	for _, q := range []string{"fluffy flamingo feathers", "the quick brown fox", "zed alpha"} {
		mode, reasons := AutoRoute(q)
		if mode != RouteExact {
			t.Errorf("AutoRoute(%q) = %s, want exact default", q, mode)
		}
		if len(reasons) == 0 || reasons[0] != "default:exact" {
			t.Errorf("AutoRoute(%q) reasons = %v, want [default:exact]", q, reasons)
		}
	}
}

// TestRoutedSearchNoEmbedderSemanticDegradesLexical is red proof 4:
// semantic mode without an embedder must not error and must not pretend
// to be vector search - it returns the lexical ranking and says so.
func TestRoutedSearchNoEmbedderSemanticDegradesLexical(t *testing.T) {
	s := newTestStore(t) // no embedder
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "postgres primary failover runbook"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteSemantic, Query: "postgres failover"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != RouteSemantic {
		t.Fatalf("mode = %s, want semantic (degradation is metadata, not a mode swap)", res.Mode)
	}
	if res.Fallback != "no-embedder:lexical" {
		t.Fatalf("fallback = %q, want no-embedder:lexical", res.Fallback)
	}
	lex, err := s.Search(ctx, "ns", "postgres failover", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != len(lex) {
		t.Fatalf("hits = %d, want %d (the lexical ranking)", len(res.Hits), len(lex))
	}
	for i, h := range res.Hits {
		if h.Fact == nil || h.Fact.Key != lex[i].Key {
			t.Fatalf("hit %d = %+v, want lexical hit %s", i, h, lex[i].Key)
		}
	}
}

// TestRoutedSearchProceduralHitsSkills: procedural mode (explicit, or
// auto from how-do-i language) answers from the skill index, as
// metadata-only skill hits.
func TestRoutedSearchProceduralHitsSkills(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	err := s.IndexSkill(ctx, "ns", SkillMeta{
		Name: "conn-sat", Version: "1.0.0",
		Description: "connection saturation triage",
		Source:      SkillSourceAuthored, Active: true,
	}, "step one: drain the pool")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteProcedural, Query: "connection saturation"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 || res.Hits[0].Kind != "skill" || res.Hits[0].Skill == nil {
		t.Fatalf("procedural hits = %+v, want a skill hit", res.Hits)
	}
	if res.Hits[0].Skill.Name != "conn-sat" {
		t.Fatalf("skill = %+v, want conn-sat", res.Hits[0].Skill)
	}

	mode, _ := AutoRoute("how do i triage connection saturation")
	if mode != RouteProcedural {
		t.Fatalf("AutoRoute(how do i...) = %s, want procedural", mode)
	}
}

// TestRoutedSearchPreservesNamespaceAndTokenBudget: routed retrieval is
// scoped to exactly the requested namespace and obeys the same
// first-non-fitting-hit token budget as the legacy surface.
func TestRoutedSearchPreservesNamespaceAndTokenBudget(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	body := strings.Repeat("alpha bravo charlie delta ", 10) // ~65 tokens
	for _, ns := range []string{"ns-a", "ns-b"} {
		for _, key := range []string{"/one", "/two", "/three"} {
			if _, err := s.Write(ctx, WriteInput{Namespace: ns, Key: key, Body: body}); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := s.RoutedSearch(ctx, "ns-a", RouteRequest{Mode: RouteExact, Query: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 3 {
		t.Fatalf("ns-a hits = %d, want 3", len(res.Hits))
	}
	// Same query in ns-b returns ns-b rows; ns-a can never leak across.
	resB, err := s.RoutedSearch(ctx, "ns-b", RouteRequest{Mode: RouteExact, Query: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resB.Hits) != 3 {
		t.Fatalf("ns-b hits = %d, want 3", len(resB.Hits))
	}
	if res.Hits[0].Fact.ID == resB.Hits[0].Fact.ID {
		t.Fatal("ns-a and ns-b returned the same fact row: namespace boundary broken")
	}

	// 70 tokens fits the first ~65-token body, not the second.
	res, err = s.RoutedSearch(ctx, "ns-a", RouteRequest{Mode: RouteExact, Query: "alpha", MaxTokens: 70})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("budgeted hits = %d, want 1 (first-non-fitting cutoff)", len(res.Hits))
	}
}

// TestAutoRouteDeterministicNoModelCall: the router is a pure function
// of the query string - repeated calls give identical decisions, and its
// signature takes no context/store, so it structurally cannot make a
// model or embedder call.
func TestAutoRouteDeterministicNoModelCall(t *testing.T) {
	queries := []string{
		"v2024.1", "errors last month", "how do i rotate logs",
		"what connects api and db", "what is the queue depth",
		"fluffy flamingo feathers", "",
	}
	type decision struct {
		mode    RouteMode
		reasons string
	}
	first := map[string]decision{}
	for _, q := range queries {
		m, r := AutoRoute(q)
		first[q] = decision{m, strings.Join(r, "|")}
	}
	for i := 0; i < 50; i++ {
		for _, q := range queries {
			m, r := AutoRoute(q)
			if d := first[q]; d.mode != m || d.reasons != strings.Join(r, "|") {
				t.Fatalf("iteration %d: AutoRoute(%q) = %s %v, first was %s %s", i, q, m, r, d.mode, d.reasons)
			}
		}
	}
}

// TestRoutedSearchRelationshipCanonicalizesAliases: relationship mode
// resolves merged entity aliases in the query to their canonical keys
// (G02 lineage) before embedding, and records the rewrite as a reason.
func TestRoutedSearchRelationshipCanonicalizesAliases(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"/entities/person/alice-chen":     {1, 0, 0},
		"alice chen mentioned in note n2": {1, 0, 0},
	}})
	aliceFixture(t, s, ctx)
	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(props) == 0 {
		t.Fatal("fixture produced no merge proposal")
	}
	if _, err := s.ApplyEntityMerge(ctx, "ns", props[0], MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLinkDescribed(ctx, "ns", "/entities/person/alice-chen", "/notes/n2", "mentions", 1.0, "alice chen mentioned in note n2"); err != nil {
		t.Fatal(err)
	}

	res, err := s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteRelationship, Query: "/entities/person/alice"})
	if err != nil {
		t.Fatal(err)
	}
	var sawCanon bool
	for _, r := range res.Reasons {
		if r == "alias-canonicalized:/entities/person/alice->/entities/person/alice-chen" {
			sawCanon = true
		}
	}
	if !sawCanon {
		t.Fatalf("reasons = %v, want the alias canonicalization recorded", res.Reasons)
	}
	if len(res.Hits) == 0 || res.Hits[0].Kind != "relation" || res.Hits[0].Triplet == nil {
		t.Fatalf("relationship hits = %+v, want a relation hit", res.Hits)
	}
	if res.Hits[0].Triplet.From.Key != "/entities/person/alice-chen" {
		t.Fatalf("triplet from = %s, want the canonical key", res.Hits[0].Triplet.From.Key)
	}
}

// spyEmbedder records embedder calls and fails them: exact mode is
// advertised as lexical, so any embedding from its retrieval arms is a
// bug (reviewer preflight repro 1).
type spyEmbedder struct{ calls int }

func (e *spyEmbedder) Dims() int { return 3 }
func (e *spyEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	e.calls++
	return nil, errors.New("unexpected model call from exact route")
}

// TestRoutedSearchExactAnchorsStayLexical: exact mode with anchors fuses
// LEXICAL arms only (FTS + one phrase arm per anchor). Even with an
// embedder configured on the store, the model is never called.
func TestRoutedSearchExactAnchorsStayLexical(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/facts/alpha", Body: "alpha deployment"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/facts/beta", Body: "beta mentions alpha"}); err != nil {
		t.Fatal(err)
	}
	e := &spyEmbedder{}
	s.SetEmbedder(e)
	res, err := s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteExact, Query: "alpha", Anchors: []string{"alpha"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if e.calls != 0 {
		t.Fatalf("exact anchored retrieval invoked the model %d times; exact is lexical-only", e.calls)
	}
	if len(res.Hits) == 0 || res.Hits[0].Fact == nil {
		t.Fatalf("exact anchored hits = %+v, want lexical hits", res.Hits)
	}
	var sawAlpha bool
	for _, h := range res.Hits {
		if h.Fact != nil && h.Fact.Key == "/facts/alpha" {
			sawAlpha = true
		}
	}
	if !sawAlpha {
		t.Fatalf("exact anchored hits = %+v, want /facts/alpha surfaced", res.Hits)
	}
}

// TestRoutedSearchHistoricalIdentifierSpanKeepsIndependentWindow: the
// identifier guard targets identifier spans, not the whole query. A file
// name alongside a real temporal phrase keeps its window (reviewer
// preflight repro 2).
func TestRoutedSearchHistoricalIdentifierSpanKeepsIndependentWindow(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	for _, f := range []struct {
		key string
		at  time.Time
	}{
		{"/facts/last-week", time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)},
		{"/facts/this-week", time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)},
	} {
		clk.Set(f.at)
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: f.key, Body: "config.go changes"}); err != nil {
			t.Fatal(err)
		}
	}
	clk.Set(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC))
	res, err := s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteHistorical, Query: "config.go changes last week", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "" || len(res.Hits) != 1 || res.Hits[0].Fact == nil || res.Hits[0].Fact.Key != "/facts/last-week" {
		t.Fatalf("historical filename query lost its legitimate window: fallback=%q hits=%+v", res.Fallback, res.Hits)
	}
}

// TestRoutedSearchHistoricalIdentifierYearNeverWindows: a bare year
// fused to an identifier span ("error 2024", version "2024.3") is not a
// time filter; with no independent temporal phrase the mode degrades to
// lexical with the guard recorded. A legit temporal phrase alongside an
// identifier year still windows.
func TestRoutedSearchHistoricalIdentifierYearNeverWindows(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	clk.Set(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)) // outside year 2024
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/err", Body: "error 2024 pool exhausted"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteHistorical, Query: "error 2024", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "identifier-guard:lexical" {
		t.Fatalf("error 2024: fallback = %q, want identifier-guard:lexical (no year-2024 window)", res.Fallback)
	}
	if len(res.Hits) != 1 || res.Hits[0].Fact == nil || res.Hits[0].Fact.Key != "/err" {
		t.Fatalf("error 2024: hits = %+v, want the lexical hit (a 2024 window would exclude it)", res.Hits)
	}

	// Version year: "2024.3" must not become a year-2024 window either.
	res, err = s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteHistorical, Query: "2024.3 hotfix", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "identifier-guard:lexical" {
		t.Fatalf("2024.3 hotfix: fallback = %q, want identifier-guard:lexical", res.Fallback)
	}

	// Mixed: identifier year AND an independent temporal phrase -> the
	// phrase still windows (guard strips only the entangled year).
	clk.Set(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/old", Body: "error 2024 pool drained"}); err != nil {
		t.Fatal(err)
	}
	clk.Set(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC))
	res, err = s.RoutedSearch(ctx, "ns", RouteRequest{Mode: RouteHistorical, Query: "error 2024 last week", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "" {
		t.Fatalf("error 2024 last week: fallback = %q, want the last-week window kept", res.Fallback)
	}
	// Window from July 8 = Jun 29..Jul 6: /old (Jun 30) is in, /err (Jul 8) is out.
	var sawOld, sawErr bool
	for _, h := range res.Hits {
		if h.Fact != nil && h.Fact.Key == "/old" {
			sawOld = true
		}
		if h.Fact != nil && h.Fact.Key == "/err" {
			sawErr = true
		}
	}
	if !sawOld || sawErr {
		t.Fatalf("error 2024 last week: hits = %+v, want /old only (window kept, entangled year not a filter)", res.Hits)
	}
}
