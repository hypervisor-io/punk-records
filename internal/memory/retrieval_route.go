package memory

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Retrieval routing: caller-selectable, inspectable search strategies
// plus a deterministic auto router. The mechanism is adapted from
// Cognee's rule-based query router (cognee/api/v1/recall/query_router.py
// at commit 78ff576559a7f75f65884c5bd90b22cdc790016e): regex heuristics
// with NO LLM call, the decision's reasons returned alongside the
// results, and misrouting surfaced as data rather than hidden (there:
// override_counts; here: Reasons, Fallback and membench's
// expect_strategy labels). Where Cognee accumulates weighted scores
// across graded completion types, punk's modes are capability-distinct
// pipelines (window filter, lexical, vector fusion, graph, skills), so
// the router uses a documented priority order with an explicit veto
// record instead - more predictable to audit, same determinism.
// Reimplemented from scratch against punk's Store boundaries; no Cognee
// code is copied.

// RouteMode names one retrieval strategy.
type RouteMode string

const (
	// RouteExact is lexical retrieval: Search, or (with Anchors) an RRF
	// fusion of the FTS arm plus one SearchPhrase arm per anchor. It never
	// calls an embedder, even when the store has one.
	RouteExact RouteMode = "exact"
	// RouteSemantic is HybridSearchScoredWith (FTS+vector+entity RRF).
	// Without an embedder it degrades to the lexical arms and reports
	// Fallback "no-embedder:lexical".
	RouteSemantic RouteMode = "semantic"
	// RouteHistorical runs WindowedSearch inside a time window: the
	// caller's explicit Window when given, else a temporal phrase parsed
	// from the query. An identifier-fused bare year (version suffix,
	// error-code prefix) never becomes a window - it degrades to lexical
	// with Fallback recorded - but identifier tokens elsewhere in the
	// query never veto an independent temporal phrase.
	RouteHistorical RouteMode = "historical"
	// RouteRelationship is TripletSearch over described links, after
	// canonicalizing merged entity aliases in the query through G02's
	// ResolveEntityKey. Without an embedder it degrades to lexical.
	RouteRelationship RouteMode = "relationship"
	// RouteProcedural is SearchSkills over the skill index (metadata
	// only; procedures load separately).
	RouteProcedural RouteMode = "procedural"
	// RouteAuto lets the deterministic router pick (see AutoRoute).
	RouteAuto RouteMode = "auto"
)

// RouteWindow is an explicit [From, To) time window supplied by the
// caller (the REST API's since/until). Only historical retrieval has
// window semantics, so a window with any other explicit mode is an
// error; auto resolves to historical when a window is given.
type RouteWindow struct {
	From, To time.Time
}

// RouteRequest is one routed search: the caller's mode ("" or auto lets
// the router decide), the query, the usual limit/token budget, the
// optional anchor identifiers the exact mode fuses lexically, and an
// optional explicit time window.
type RouteRequest struct {
	Mode      RouteMode
	Query     string
	Limit     int
	MaxTokens int // <=0: no budget (TokenBudget contract)
	Anchors   []string
	Window    *RouteWindow
}

// RouteResult is a routed search plus its inspection metadata: the mode
// that actually ran, the mode the caller asked for, why the mode was
// chosen (router rule names, vetoes, alias rewrites), and a non-empty
// Fallback when the requested pipeline degraded (no embedder, vetoed or
// missing temporal phrase). Hits are UnifiedHit: Kind "fact" wraps a
// ScoredFact (exact/historical carry no score components), "relation"
// wraps a Triplet, "skill" wraps SkillMeta.
type RouteResult struct {
	Mode          RouteMode    `json:"mode"`
	RequestedMode RouteMode    `json:"requested_mode"`
	Reasons       []string     `json:"reasons,omitempty"`
	Fallback      string       `json:"fallback,omitempty"`
	Hits          []UnifiedHit `json:"hits"`
}

// --- identifier guard -------------------------------------------------
// A bare 20xx year inside a version, error code, hex literal, commit
// SHA, flag, path or file name is an IDENTIFIER, never a time filter.
// The legacy plain-search auto-parse keeps treating "error 2024" as a
// window (compatibility); the routed surface refuses to.

var (
	identifierVersionRe   = regexp.MustCompile(`\bv?\d+\.\d+(?:\.\d+)*(?:[-+][0-9A-Za-z.]+)?`)
	identifierHexRe       = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	identifierSHARe       = regexp.MustCompile(`\b[0-9a-f]{7,64}\b`)
	identifierFlagRe      = regexp.MustCompile(`(?:^|\s)--[a-zA-Z][\w-]*`)
	identifierPathRe      = regexp.MustCompile(`(?:^|\s)/[\w./-]+`)
	identifierFileRe      = regexp.MustCompile(`(?i)\b[\w-]+\.(?:go|py|js|jsx|ts|tsx|json|ya?ml|toml|md|sql|log|txt|sh|rs|c|h|cc|cpp|java|rb|php|css|html)\b`)
	identifierErrorYearRe = regexp.MustCompile(`(?i)\b(?:error|err|errno|code|exit|status|build|release|version|panic|fatal)\s*#?\s*-?\s*20\d{2}\b`)
	identifierQuotedRe    = regexp.MustCompile(`^"[^"]+"$`)
)

// identifierSignal reports the first identifier pattern the query
// matches, named for the reason log. A SHA candidate must contain a
// digit so pure-hex-letter words ("deadbeef") don't count.
func identifierSignal(q string) (string, bool) {
	if identifierQuotedRe.MatchString(q) {
		return "quoted-phrase", true
	}
	if identifierErrorYearRe.MatchString(q) {
		return "error-code-year", true
	}
	if identifierVersionRe.MatchString(q) {
		return "version", true
	}
	if identifierHexRe.MatchString(q) {
		return "hex-literal", true
	}
	for _, m := range identifierSHARe.FindAllString(q, -1) {
		if strings.ContainsAny(m, "0123456789") {
			return "commit-sha", true
		}
	}
	if identifierFlagRe.MatchString(q) {
		return "flag", true
	}
	if identifierPathRe.MatchString(q) {
		return "path", true
	}
	if identifierFileRe.MatchString(q) {
		return "file-name", true
	}
	return "", false
}

var (
	relationshipRe = regexp.MustCompile(`(?i)\b(related|connected|link(?:ed|s)? to|depends? on|dependency|path between|what connects|relationship)\b`)
	proceduralRe   = regexp.MustCompile(`(?i)\b(how to|how do (?:i|we)|steps? to|step-by-step|procedure|runbook|playbook|checklist|workflow)\b`)
	questionRe     = regexp.MustCompile(`(?i)\b(what|why|who|whose|which|where|when|explain|describe)\b`)
)

// AutoRoute classifies a query into a RouteMode with pure regex/string
// heuristics - deterministic, no model call, no store access. Priority
// order (first match wins): identifier guard -> temporal phrase ->
// relationship language -> procedural language -> question language ->
// lexical default. When the identifier guard vetoes a temporal phrase
// that ParseTemporal would have matched, the veto is recorded as a
// reason so the misroute-prevention is visible, not silent.
func AutoRoute(query string) (RouteMode, []string) {
	return autoRoute(query, time.Now())
}

func autoRoute(query string, now time.Time) (RouteMode, []string) {
	q := strings.TrimSpace(query)
	if q == "" {
		return RouteExact, []string{"default:exact"}
	}
	if sig, ok := identifierSignal(q); ok {
		reasons := []string{"identifier:" + sig}
		if _, _, _, tok := ParseTemporal(q, now); tok {
			reasons = append(reasons, "temporal-vetoed:identifier-guard")
		}
		return RouteExact, reasons
	}
	if _, _, _, ok := ParseTemporal(q, now); ok {
		return RouteHistorical, []string{"temporal-phrase"}
	}
	if relationshipRe.MatchString(q) {
		return RouteRelationship, []string{"relationship-language"}
	}
	if proceduralRe.MatchString(q) {
		return RouteProcedural, []string{"procedural-language"}
	}
	if questionRe.MatchString(q) {
		return RouteSemantic, []string{"natural-language-question"}
	}
	return RouteExact, []string{"default:exact"}
}

// RoutedSearch runs one explicit or auto-routed retrieval strategy. It
// only ever reuses the existing search functions (Search, SearchPhrase,
// HybridSearchScoredWith, WindowedSearch, TripletSearch, SearchSkills);
// the routing layer adds selection, metadata and the token budget, never
// a new ranking. The namespace passes straight through to the chosen
// function, and MaxTokens applies the same first-non-fitting-hit budget
// as the legacy surface. The legacy default (plain handleSearch /
// search with no strategy) is untouched; callers opt in by naming a
// strategy.
func (s *Store) RoutedSearch(ctx context.Context, ns string, r RouteRequest) (RouteResult, error) {
	requested := r.Mode
	if requested == "" {
		requested = RouteAuto
	}
	res := RouteResult{RequestedMode: requested}
	var err error
	switch requested {
	case RouteAuto:
		if r.Window != nil {
			// An explicit window IS the temporal intent: auto resolves to
			// historical without consulting the query text.
			res.Mode = RouteHistorical
			res.Reasons = []string{"explicit-window"}
		} else {
			res.Mode, res.Reasons = autoRoute(r.Query, s.now())
		}
	case RouteExact, RouteSemantic, RouteHistorical, RouteRelationship, RouteProcedural:
		if r.Window != nil && requested != RouteHistorical {
			return res, fmt.Errorf("memory: route mode %q does not support an explicit since/until window (only historical; auto resolves to historical when a window is given)", string(requested))
		}
		res.Mode = requested
		res.Reasons = []string{"explicit-mode"}
	default:
		return res, fmt.Errorf("memory: unknown route mode %q (want exact|semantic|historical|relationship|procedural|auto)", string(r.Mode))
	}

	switch res.Mode {
	case RouteExact:
		res.Hits, err = s.routedExact(ctx, ns, r.Query, r.Limit, r.Anchors)
		if err == nil && len(cleanAnchors(r.Anchors)) > 0 {
			res.Reasons = append(res.Reasons, "anchors-fused")
		}
	case RouteSemantic:
		var scored []ScoredFact
		scored, err = s.HybridSearchScoredWith(ctx, ns, r.Query, HybridOpts{Limit: r.Limit, Anchors: r.Anchors})
		res.Hits = scoredHits(scored)
		if s.embedder == nil {
			res.Fallback = "no-embedder:lexical"
		}
	case RouteHistorical:
		res.Hits, err = s.routedHistorical(ctx, ns, r, &res)
	case RouteRelationship:
		res.Hits, err = s.routedRelationship(ctx, ns, r, &res)
	case RouteProcedural:
		var skills []SkillMeta
		skills, err = s.SearchSkills(ctx, ns, r.Query, r.Limit)
		res.Hits = skillHits(skills)
		if s.embedder == nil {
			res.Fallback = "no-embedder:lexical"
		}
	}
	if err != nil {
		return res, err
	}
	res.Hits = budgetUnifiedHits(res.Hits, r.MaxTokens)
	return res, nil
}

// routedExact is lexical retrieval: plain Search, or - when the caller
// named anchor identifiers - an RRF (k=60) fusion of the FTS arm plus
// one SearchPhrase arm per anchor, the same fusion the skill index uses.
// Exact NEVER routes through HybridSearchScoredWith: that function calls
// VectorSearch when the store has an embedder, and exact mode's contract
// is lexical-only (spy-embedder regression test pins this).
func (s *Store) routedExact(ctx context.Context, ns, query string, limit int, anchors []string) ([]UnifiedHit, error) {
	anchors = cleanAnchors(anchors)
	if len(anchors) == 0 {
		facts, err := s.Search(ctx, ns, query, limit)
		if err != nil {
			return nil, err
		}
		return factHits(facts), nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	arms := make([][]Fact, 0, len(anchors)+1)
	fts, err := s.Search(ctx, ns, query, 100)
	if err != nil {
		return nil, err
	}
	arms = append(arms, fts)
	for _, a := range anchors {
		phrase, err := s.SearchPhrase(ctx, ns, a, 100)
		if err != nil {
			return nil, err
		}
		arms = append(arms, phrase)
	}
	fused := rrfFuseFacts(arms...)
	if len(fused) > limit {
		fused = fused[:limit]
	}
	return factHits(fused), nil
}

// identifierYearPrefixRe marks a bare year fused to an error/code
// context word ("error 2024", "exit code 2023"): the year is part of
// the identifier, not a time filter.
var identifierYearPrefixRe = regexp.MustCompile(`(?i)(?:error|err|errno|code|exit|status|build|release|version|panic|fatal)\s*#?\s*-?\s*$`)

// bareYearRe finds the bare-year tokens ParseTemporal's last matcher can
// interpret as a window.
var bareYearRe = regexp.MustCompile(`\b20\d{2}\b`)

// yearEntangled reports whether the bare-year span at loc is fused to an
// identifier: an error/code prefix, or a version suffix ("2024.3").
func yearEntangled(query string, loc []int) bool {
	if identifierYearPrefixRe.MatchString(query[:loc[0]]) {
		return true
	}
	return loc[1]+1 < len(query) && query[loc[1]] == '.' && query[loc[1]+1] >= '0' && query[loc[1]+1] <= '9'
}

// temporalVetoedByIdentifier reports whether the query's ONLY temporal
// signal is identifier-fused bare years. The guard is span-targeted:
// entangled years are stripped, and any remaining temporal phrase
// ("error 2024 last week", file names beside "last week") keeps its
// window; only when nothing survives ("error 2024" alone) is the window
// vetoed.
func temporalVetoedByIdentifier(query string, now time.Time) bool {
	var b strings.Builder
	last := 0
	entangledAny := false
	for _, loc := range bareYearRe.FindAllStringIndex(query, -1) {
		if !yearEntangled(query, loc) {
			continue
		}
		entangledAny = true
		b.WriteString(query[last:loc[0]])
		b.WriteByte(' ')
		last = loc[1]
	}
	if !entangledAny {
		return false
	}
	b.WriteString(query[last:])
	_, _, _, ok := ParseTemporal(b.String(), now)
	return !ok
}

// routedHistorical windows the query. An explicit caller Window is used
// verbatim (no phrase parsing, no guard - the caller delimited time
// themselves). Otherwise the window comes from ParseTemporal, except
// when the only temporal signal is an identifier-fused bare year (the
// guard) or there is no phrase at all: both degrade to lexical with a
// Fallback note rather than silently misrouting.
func (s *Store) routedHistorical(ctx context.Context, ns string, r RouteRequest, res *RouteResult) ([]UnifiedHit, error) {
	if r.Window != nil {
		facts, err := s.WindowedSearch(ctx, ns, r.Query, r.Window.From, r.Window.To, r.Limit)
		if err != nil {
			return nil, err
		}
		res.Reasons = append(res.Reasons, "explicit-window")
		return factHits(facts), nil
	}
	from, to, cleaned, ok := ParseTemporal(r.Query, s.now())
	if !ok {
		res.Fallback = "no-temporal-phrase:lexical"
		return s.routedExact(ctx, ns, r.Query, r.Limit, nil)
	}
	if temporalVetoedByIdentifier(r.Query, s.now()) {
		res.Reasons = append(res.Reasons, "temporal-vetoed:identifier-guard")
		res.Fallback = "identifier-guard:lexical"
		return s.routedExact(ctx, ns, r.Query, r.Limit, nil)
	}
	facts, err := s.WindowedSearch(ctx, ns, cleaned, from, to, r.Limit)
	if err != nil {
		return nil, err
	}
	return factHits(facts), nil
}

// routedRelationship canonicalizes merged entity aliases in the query
// (G02 ResolveEntityKey) before embedding, so a reference written before
// a merge still reaches the canonical entity's relations. Without an
// embedder, TripletSearch can rank nothing, so it degrades to lexical.
func (s *Store) routedRelationship(ctx context.Context, ns string, r RouteRequest, res *RouteResult) ([]UnifiedHit, error) {
	query, notes, err := s.canonicalizeQueryKeys(ctx, ns, r.Query)
	if err != nil {
		return nil, err
	}
	res.Reasons = append(res.Reasons, notes...)
	if s.embedder == nil {
		res.Fallback = "no-embedder:lexical"
		return s.routedExact(ctx, ns, query, r.Limit, nil)
	}
	trips, err := s.TripletSearch(ctx, ns, query, r.Limit)
	if err != nil {
		return nil, err
	}
	return tripletHits(trips), nil
}

// canonicalizeQueryKeys resolves every absolute-key token in the query
// through merged_into lineage. Rewrites are returned as inspectable
// reasons; a lineage cycle is an error, never silently dropped.
func (s *Store) canonicalizeQueryKeys(ctx context.Context, ns, query string) (string, []string, error) {
	toks := strings.Fields(query)
	var notes []string
	changed := false
	for i, tok := range toks {
		if !strings.HasPrefix(tok, "/") {
			continue
		}
		canon, err := s.ResolveEntityKey(ctx, ns, tok)
		if err != nil {
			return "", nil, err
		}
		if canon != tok {
			toks[i] = canon
			notes = append(notes, "alias-canonicalized:"+tok+"->"+canon)
			changed = true
		}
	}
	if !changed {
		return query, nil, nil
	}
	return strings.Join(toks, " "), notes, nil
}

func factHits(facts []Fact) []UnifiedHit {
	out := make([]UnifiedHit, 0, len(facts))
	for i := range facts {
		sf := ScoredFact{Fact: facts[i], Components: map[string]float64{}}
		out = append(out, UnifiedHit{Kind: "fact", Fact: &sf})
	}
	return out
}

func scoredHits(scored []ScoredFact) []UnifiedHit {
	out := make([]UnifiedHit, 0, len(scored))
	for i := range scored {
		sf := scored[i]
		out = append(out, UnifiedHit{Kind: "fact", Fact: &sf, Score: sf.Score})
	}
	return out
}

func tripletHits(trips []Triplet) []UnifiedHit {
	out := make([]UnifiedHit, 0, len(trips))
	for i := range trips {
		tp := trips[i]
		out = append(out, UnifiedHit{Kind: "relation", Triplet: &tp, Score: tp.Score})
	}
	return out
}

func skillHits(skills []SkillMeta) []UnifiedHit {
	out := make([]UnifiedHit, 0, len(skills))
	for i := range skills {
		m := skills[i]
		out = append(out, UnifiedHit{Kind: "skill", Skill: &m})
	}
	return out
}

// unifiedHitBody is the text a hit spends from the token budget.
func unifiedHitBody(h UnifiedHit) string {
	switch {
	case h.Fact != nil:
		return h.Fact.Body
	case h.Triplet != nil:
		return h.Triplet.Description
	case h.Skill != nil:
		return h.Skill.Name + " " + h.Skill.Description
	}
	return ""
}

// budgetUnifiedHits applies the TokenBudget contract (ranked order kept,
// stop at the first hit that does not fit) across all hit kinds.
func budgetUnifiedHits(hits []UnifiedHit, maxTokens int) []UnifiedHit {
	if maxTokens <= 0 {
		return hits
	}
	used := 0
	for i, h := range hits {
		used += EstimateTokens(unifiedHitBody(h))
		if used > maxTokens {
			return hits[:i]
		}
	}
	return hits
}
