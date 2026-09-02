package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

// ScoredFact is a search hit with its ranking explained. Components
// multiply/sum into Score exactly as documented on HybridSearchScored —
// evidence for why a fact surfaced (the search-side analogue of the
// findings evidence contract).
type ScoredFact struct {
	Fact
	Score      float64            `json:"score"`
	Components map[string]float64 `json:"score_components"`
}

// bridgeTypeBoost weights a link's bridge contribution by how strong a
// signal its type is: leads_to is causal (highest), evolved_into and
// derived_from are lineage (medium), everything else is a weak hint.
func bridgeTypeBoost(linkType string) float64 {
	switch linkType {
	case "leads_to":
		return 2.0
	case "evolved_into", "derived_from":
		return 1.5
	default:
		return 1.0
	}
}

// HybridSearchScored fuses FTS, vector, and entity ranks (RRF, k=60),
// adds a bridge-discovery component (multi-hop: a fact linked to
// >=2 of the top-20 seeds surfaces even with zero textual/vector match),
// and scales by bounded multiplicative salience boosts (design rule:
// a missing signal is exactly 1.0, boosts stay proportional to base
// relevance):
// Score = (fts+vector+entity+bridge) * recency * importance * access * feedback * reinforce,
// halved when the fact has an outgoing invalidated_by edge, with
// entity = RRF over entityArm's ranking (query tokens/bigrams fuzzy-match live /entities/ names, and
// each match's "mentions" in-edges pull the facts that mention it; absent
// entirely, not zero, when the namespace has no entities - see entityArm),
// bridge = (1/60)*tanh(0.5*cnt), cnt = sum of per-distinct-seed max
// (typeBoost(linkType)*link.Weight) — saturates at one top-rank RRF
// hit so a hub linked to everything cannot dominate relevance,
// recency = 1+0.2*(0.5^(age/halfLife)-0.5) (1 when halfLife=0),
// importance = 1+0.3*Importance,
// access = 1+0.1*(min(1, 0.5+ln(1+AccessCount)/10)-0.5),
// feedback = 1+0.3*(FeedbackWeight-0.5) (EWMA of user
// ratings recorded via RecordFeedback; neutral 1.0 at the 0.5 default,
// range [0.85,1.15]),
// reinforce = 1+0.1*(min(1, 0.5+ln(1+Reinforcements)/10)-0.5)
// (duplicate-content writes bump Reinforcements in
// place instead of no-oping; neutral 1.0 at zero, same bounded shape
// as access).
// Two observation-layer passes run after scoring, before truncation:
// prefer_observations drops any candidate whose ID is cited in another
// candidate's /observations/* source_ids (the belief supersedes its raw
// evidence), and observations get a proof component (same bounded-boost
// shape, keyed on proof_count) multiplied into Score. /mental-models/*
// hits get a flat model=1.15 component in the same pass, multiplied into
// Score, so a curated model outranks an observation tied on relevance.
// A mental model is never dropped as superseded, even if its ID appears
// in some source_ids (a curated top-tier fact must always survive).
// Hits get their access_count bumped best-effort after ranking.
// fusionPerSourceCap bounds each fusion arm before RRF/salience/bridge
// passes run, so their O(candidates) and O(candidates*links) cost stays
// bounded regardless of how many hits FTS/vector return.
// After scoring, results are diversified by key-subtree (see keySubtree):
// a first pass caps hits at maxPerSubtree (3) per subtree so one hot
// document's chunks cannot crowd out everything else, collecting
// skipped-only-for-cap candidates into an overflow list in ranked order;
// a second pass backfills from that overflow, in rank order, if the
// diversified set left room under limit. Consequence: output order is
// NOT strictly score-descending once backfill has run - the overflow
// tail is still rank-ordered internally, but interleaved after the
// capped set rather than merged back into one global sort.
const fusionPerSourceCap = 50

// keySubtree groups keys by their first two path segments so one hot
// document's chunks cannot crowd the whole result set (session
// diversification, keyed on punk's key hierarchy).
func keySubtree(key string) string {
	parts := strings.SplitN(strings.TrimPrefix(key, "/"), "/", 3)
	if len(parts) >= 2 {
		return "/" + parts[0] + "/" + parts[1]
	}
	return key
}

const maxPerSubtree = 3

// entityMatchCap bounds how many fuzzy-matched entities entityArm pulls
// edges for: every matched entity used to trigger its own Neighbors query,
// so an unbounded match set meant unbounded sequential queries (300
// matches measured -> 300 queries). Capping to the top-scored 20 (key-asc
// tie-break) before edge pulling, combined with the single batched
// linksForKeys call below, bounds the fan-out regardless of how many
// entities the namespace has.
const entityMatchCap = 20

// entityArm is the third retrieval arm: query tokens and bigrams fuzzy-match live /entities/ names
// (nameSimilarity >= entityAliasThreshold); each matched entity's mentions
// in-edges pull the facts that mention it, ranked by matchScore*linkWeight.
// Deterministic, no LLM. Empty entity graph returns (nil, nil) - the arm is
// silent and scores stay byte-identical to an entity-less deployment. Real
// errors from Recall/linksForKeys/liveByKeys propagate to the caller
// (mirroring the vector arm in HybridSearchScored) rather than folding
// into "no entities" - only an empty result is silent.
func (s *Store) entityArm(ctx context.Context, ns, query string) ([]Fact, error) {
	// Tokenless query (blank, or all-whitespace): nothing can fuzzy-match,
	// so bail before the /entities Recall (up to 1000 rows) instead of
	// paying for it only to find matched empty. In practice
	// HybridSearchScored's own Search call already errors on a blank
	// query before entityArm is reached, so this guard's reachable path
	// is direct/unexported calls with a blank/whitespace-only query (the
	// public path already rejects those in Search); it still documents
	// and enforces the intended short-circuit order.
	toks := strings.Fields(strings.ToLower(query))
	if len(toks) == 0 {
		return nil, nil
	}
	ents, err := s.Recall(ctx, ns, "/entities", 1000)
	if err != nil {
		return nil, err
	}
	if len(ents) == 0 {
		return nil, nil
	}
	cands := append([]string(nil), toks...)
	for i := 0; i+1 < len(toks); i++ {
		cands = append(cands, toks[i]+" "+toks[i+1])
	}
	// Normalize each candidate once and each entity name once, outside the
	// double loop - normalizeName(e.Body) was previously recomputed per
	// candidate (26.5ms/search measured @1000 entities).
	normCands := make([]string, len(cands))
	for i, c := range cands {
		normCands[i] = normalizeName(c)
	}
	matched := map[string]float64{} // entity key -> best match score
	for _, e := range ents {
		normName := normalizeName(e.Body)
		for _, nc := range normCands {
			if sc := nameSimilarityNorm(nc, normName); sc >= entityAliasThreshold && sc > matched[e.Key] {
				matched[e.Key] = sc
			}
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}

	// Cap fan-out to the top entityMatchCap matches (key-asc tie-break)
	// before pulling any edges.
	matchedKeys := make([]string, 0, len(matched))
	for k := range matched {
		matchedKeys = append(matchedKeys, k)
	}
	sort.Slice(matchedKeys, func(a, b int) bool {
		if matched[matchedKeys[a]] != matched[matchedKeys[b]] {
			return matched[matchedKeys[a]] > matched[matchedKeys[b]]
		}
		return matchedKeys[a] < matchedKeys[b]
	})
	if len(matchedKeys) > entityMatchCap {
		matchedKeys = matchedKeys[:entityMatchCap]
	}

	// One batched query for every matched entity's incoming "mentions"
	// edges, instead of one Neighbors query per entity. linksForKeys
	// already covers to_key lookups (WHERE from_key IN (..) OR to_key IN
	// (..) over the same key list), so no new SQL is needed - filter to
	// mentions edges whose to_key is a matched entity (in-edges TO the
	// entity, i.e. facts that mention it) in code. entityMatchCap (20)
	// keeps the IN-list well under any chunking threshold.
	links, err := s.linksForKeys(ctx, ns, matchedKeys)
	if err != nil {
		return nil, err
	}
	factScore := map[string]float64{}
	for _, l := range links {
		if l.LinkType != "mentions" {
			continue
		}
		esc, ok := matched[l.ToKey]
		if !ok {
			continue // an outgoing edge FROM a matched entity, not an in-edge
		}
		if v := esc * l.Weight; v > factScore[l.FromKey] {
			factScore[l.FromKey] = v
		}
	}
	keys := make([]string, 0, len(factScore))
	for k := range factScore {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if factScore[keys[a]] != factScore[keys[b]] {
			return factScore[keys[a]] > factScore[keys[b]]
		}
		return keys[a] < keys[b]
	})
	if len(keys) > fusionPerSourceCap {
		keys = keys[:fusionPerSourceCap]
	}
	facts, err := s.liveByKeys(ctx, ns, keys)
	if err != nil {
		return nil, err
	}
	// liveByKeys returns key order; re-rank by factScore
	sort.Slice(facts, func(a, b int) bool {
		if factScore[facts[a].Key] != factScore[facts[b].Key] {
			return factScore[facts[a].Key] > factScore[facts[b].Key]
		}
		return facts[a].Key < facts[b].Key
	})
	return facts, nil
}

// HybridSearchScored is HybridSearchScoredWith without anchors; kept as
// the stable signature every existing caller uses.
func (s *Store) HybridSearchScored(ctx context.Context, ns, query string, limit int, recencyHalfLife time.Duration) ([]ScoredFact, error) {
	return s.HybridSearchScoredWith(ctx, ns, query, HybridOpts{Limit: limit, RecencyHalfLife: recencyHalfLife})
}

func (s *Store) HybridSearchScoredWith(ctx context.Context, ns, query string, o HybridOpts) ([]ScoredFact, error) {
	limit, recencyHalfLife := o.Limit, o.RecencyHalfLife
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// ponytail: cost bound, not a ranking change -- RRF tails already
	// contribute negligibly via 1/(k+rank), so trimming them can't flip
	// results the caller asked for (sourceCap >= limit always).
	sourceCap := limit
	if fusionPerSourceCap > sourceCap {
		sourceCap = fusionPerSourceCap
	}
	const k = 60.0
	comp := map[string]map[string]float64{} // id -> component -> value
	byID := map[string]Fact{}
	get := func(id string) map[string]float64 {
		if comp[id] == nil {
			comp[id] = map[string]float64{}
		}
		return comp[id]
	}

	fts, err := s.Search(ctx, ns, query, 100)
	if err != nil {
		return nil, err
	}
	if len(fts) > sourceCap {
		fts = fts[:sourceCap]
	}
	for rank, f := range fts {
		get(f.ID)["fts"] += 1.0 / (k + float64(rank+1))
		byID[f.ID] = f
	}
	if s.embedder != nil {
		vec, err := s.VectorSearch(ctx, ns, query, 100)
		if err != nil {
			return nil, err
		}
		if len(vec) > sourceCap {
			vec = vec[:sourceCap]
		}
		for rank, f := range vec {
			get(f.ID)["vector"] += 1.0 / (k + float64(rank+1))
			byID[f.ID] = f
		}
	}
	// entityArm's result is already <= fusionPerSourceCap (<= sourceCap),
	// so no trim is needed here (see entityArm's own fusionPerSourceCap
	// cap on its returned fact keys).
	ent, err := s.entityArm(ctx, ns, query)
	if err != nil {
		return nil, err
	}
	for rank, f := range ent {
		get(f.ID)["entity"] += 1.0 / (k + float64(rank+1))
		byID[f.ID] = f
	}
	if len(o.Anchors) > 0 {
		contrib, anchored, err := s.anchorArm(ctx, ns, o.Anchors, sourceCap)
		if err != nil {
			return nil, err
		}
		for id, c := range contrib {
			get(id)["anchor"] += c
			byID[id] = anchored[id]
		}
	}
	// --- bridge discovery (multi-hop) ---
	// Seeds: top 20 candidates by base (fts+vector+entity), the RRF-strong
	// hits whose neighbors are worth pulling in. Key-asc tie-break: without
	// it, entity-only candidates (fts=vector=0, so tied whenever entity was
	// excluded from the sum) landed in the top-20 by map-iteration order,
	// not fact data - reviewer measured a downstream bridge fact present in
	// only 9/30 repeated searches instead of every time.
	seedIDs := make([]string, 0, len(byID))
	for id := range byID {
		seedIDs = append(seedIDs, id)
	}
	sort.Slice(seedIDs, func(a, b int) bool {
		ca, cb := comp[seedIDs[a]], comp[seedIDs[b]]
		sa, sb := ca["fts"]+ca["vector"]+ca["entity"]+ca["anchor"], cb["fts"]+cb["vector"]+cb["entity"]+cb["anchor"]
		if sa != sb {
			return sa > sb
		}
		return byID[seedIDs[a]].Key < byID[seedIDs[b]].Key
	})
	if len(seedIDs) > 20 {
		seedIDs = seedIDs[:20]
	}
	seedKeys := make([]string, 0, len(seedIDs))
	seedSet := map[string]bool{}
	for _, id := range seedIDs {
		key := byID[id].Key
		seedKeys = append(seedKeys, key)
		seedSet[key] = true
	}

	// contribution[neighborKey][seedKey] = max(typeBoost*weight) over
	// links from that seed to that neighbor.
	contribution := map[string]map[string]float64{}
	var seedLinks []Link
	if len(seedKeys) > 0 {
		// Degrade (skip bridging), never fail the search, on link-query error.
		if links, err := s.linksForKeys(ctx, ns, seedKeys); err == nil {
			seedLinks = links
			for _, l := range links {
				if l.LinkType == "invalidated_by" {
					continue
				}
				fromSeed, toSeed := seedSet[l.FromKey], seedSet[l.ToKey]
				if fromSeed == toSeed {
					continue // both seeds or neither: not a bridge edge
				}
				seedKey, neighborKey := l.FromKey, l.ToKey
				if toSeed {
					seedKey, neighborKey = l.ToKey, l.FromKey
				}
				c := bridgeTypeBoost(l.LinkType) * l.Weight
				if contribution[neighborKey] == nil {
					contribution[neighborKey] = map[string]float64{}
				}
				if c > contribution[neighborKey][seedKey] {
					contribution[neighborKey][seedKey] = c
				}
			}
		}
	}

	// Pull in bridge facts not already candidates: >=2 distinct seeds,
	// capped at 20 additions so one hub can't crowd the result set.
	existingKeys := map[string]bool{}
	for _, f := range byID {
		existingKeys[f.Key] = true
	}
	var newKeys []string
	for k, seeds := range contribution {
		if len(seeds) >= 2 && !existingKeys[k] {
			newKeys = append(newKeys, k)
		}
	}
	sumContribution := func(m map[string]float64) float64 {
		var sum float64
		for _, v := range m {
			sum += v
		}
		return sum
	}
	sort.Slice(newKeys, func(a, b int) bool {
		return sumContribution(contribution[newKeys[a]]) > sumContribution(contribution[newKeys[b]])
	})
	if len(newKeys) > 20 {
		newKeys = newKeys[:20] // ponytail: flat cap; per-source budgets if arms multiply
	}
	if len(newKeys) > 0 {
		if facts, err := s.liveByKeys(ctx, ns, newKeys); err == nil {
			for _, f := range facts {
				byID[f.ID] = f
				get(f.ID) // fts=vector=0, bridge assigned below
			}
		}
	}

	// Salience runs over the FULL candidate set, including bridge
	// additions, so they get recency/importance/access too.
	now := s.now()
	for id, f := range byID {
		c := get(id)
		c["recency"] = 1.0
		if recencyHalfLife > 0 {
			r := math.Pow(0.5, now.Sub(f.CreatedAt).Hours()/recencyHalfLife.Hours())
			c["recency"] = 1 + 0.2*(r-0.5)
		}
		c["importance"] = 1 + 0.3*f.Importance
		an := math.Min(1, 0.5+math.Log(1+float64(f.AccessCount))/10)
		c["access"] = 1 + 0.1*(an-0.5)
		c["feedback"] = 1 + 0.3*(f.FeedbackWeight-0.5)
		rn := math.Min(1, 0.5+math.Log(1+float64(f.Reinforcements))/10)
		c["reinforce"] = 1 + 0.1*(rn-0.5)
	}

	// Bridge component: every candidate (pre-existing or newly pulled
	// in) touching >=2 distinct seeds gets a saturating bonus.
	for id, f := range byID {
		seeds := contribution[f.Key]
		if len(seeds) < 2 {
			continue
		}
		get(id)["bridge"] = (1.0 / 60.0) * math.Tanh(0.5*sumContribution(seeds))
	}

	// Invalidation demotion: a candidate with an outgoing invalidated_by
	// edge is stale-superseded, not wrong — keep it visible, rank it low.
	allKeys := make([]string, 0, len(byID))
	for _, f := range byID {
		allKeys = append(allKeys, f.Key)
	}
	invalidatedFrom := map[string]bool{}
	for _, l := range seedLinks {
		if l.LinkType == "invalidated_by" {
			invalidatedFrom[l.FromKey] = true
		}
	}
	if allLinks, err := s.linksForKeys(ctx, ns, allKeys); err == nil {
		for _, l := range allLinks {
			if l.LinkType == "invalidated_by" {
				invalidatedFrom[l.FromKey] = true
			}
		}
	}
	for id, f := range byID {
		if invalidatedFrom[f.Key] {
			get(id)["invalidated"] = 1
		}
	}

	// prefer observations: a belief supersedes its raw evidence
	superseded := map[string]bool{}
	for id, f := range byID {
		if strings.HasPrefix(f.Key, "/mental-models/") {
			get(id)["model"] = 1.15
			continue
		}
		if !strings.HasPrefix(f.Key, "/observations/") {
			continue
		}
		if raw, ok := f.Attributes["source_ids"].([]any); ok {
			for _, sid := range raw {
				if sidStr, ok := sid.(string); ok {
					superseded[sidStr] = true
				}
			}
		}
		if pc, ok := f.Attributes["proof_count"].(float64); ok {
			an := math.Min(1, 0.5+math.Log(1+pc)/10)
			get(id)["proof"] = 1 + 0.1*(an-0.5)
		}
	}

	score := func(c map[string]float64) float64 {
		v := (c["fts"] + c["vector"] + c["entity"] + c["anchor"] + c["bridge"]) * c["recency"] * c["importance"] * c["access"] * c["feedback"] * c["reinforce"]
		if p := c["proof"]; p > 0 {
			v *= p
		}
		if m := c["model"]; m > 0 {
			v *= m
		}
		if c["invalidated"] == 1 {
			v *= 0.5
		}
		return v
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		sa, sb := score(comp[ids[a]]), score(comp[ids[b]])
		if sa != sb {
			return sa > sb
		}
		return byID[ids[a]].Key < byID[ids[b]].Key
	})
	// Diversification:
	// first pass caps hits per key-subtree (see keySubtree) so one hot
	// document's chunks can't crowd out everything else; candidates
	// skipped only for exceeding the cap are collected into overflow, in
	// ranked order, and backfilled after the pass if there's still room.
	out := []ScoredFact{}
	touched := []string{}
	subtreeCount := map[string]int{}
	var overflow []string
	for _, id := range ids {
		// A curated model is a top-tier synthesis: it must never be
		// dropped as superseded, even if some observation's source_ids
		// happens to cite its fact ID (defense in depth — observe.go
		// also excludes models from the raw pool that feeds source_ids).
		if superseded[id] && !strings.HasPrefix(byID[id].Key, "/mental-models/") {
			continue
		}
		sub := keySubtree(byID[id].Key)
		if subtreeCount[sub] >= maxPerSubtree {
			overflow = append(overflow, id)
			continue
		}
		subtreeCount[sub]++
		out = append(out, ScoredFact{Fact: byID[id], Score: score(comp[id]), Components: comp[id]})
		touched = append(touched, id)
		if len(out) >= limit {
			break
		}
	}
	for _, id := range overflow {
		if len(out) >= limit {
			break
		}
		out = append(out, ScoredFact{Fact: byID[id], Score: score(comp[id]), Components: comp[id]})
		touched = append(touched, id)
	}
	// Advisory staleness: decorates already-selected /observations/ hits
	// only — never feeds score() or re-ranks (relevance-first). Errors
	// are swallowed; leave the component absent rather than fail search.
	for i := range out {
		if !strings.HasPrefix(out[i].Key, "/observations/") {
			continue
		}
		if stale, err := s.ObservationStale(ctx, ns, out[i].Fact); err == nil && stale {
			if out[i].Components == nil {
				out[i].Components = map[string]float64{}
			}
			out[i].Components["stale"] = 1
		}
	}
	_ = s.TouchAccess(ctx, touched) // best-effort ranking signal
	return out, nil
}

// InterleaveSearch round-robins the FTS and vector arms: each arm's #1,
// then each arm's #2, deduped by ID. Use over RRF when a hit that is
// strong in ONE arm must not be averaged down (dedup-style recalls).
// Without an embedder, degrades to FTS order.
func (s *Store) InterleaveSearch(ctx context.Context, ns, query string, limit int) ([]ScoredFact, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	arms := [][]Fact{}
	fts, err := s.Search(ctx, ns, query, 100)
	if err != nil {
		return nil, err
	}
	arms = append(arms, fts)
	if s.embedder != nil {
		vec, err := s.VectorSearch(ctx, ns, query, 100)
		if err != nil {
			return nil, err
		}
		arms = append(arms, vec)
	}
	seen := map[string]bool{}
	out := []ScoredFact{}
	for rank := 0; ; rank++ {
		progressed := false
		for ai, arm := range arms {
			if rank >= len(arm) {
				continue
			}
			progressed = true
			f := arm[rank]
			if seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			out = append(out, ScoredFact{Fact: f,
				Score:      1.0 / float64(len(out)+1),
				Components: map[string]float64{"arm": float64(ai), "arm_rank": float64(rank + 1)}})
			if len(out) >= limit {
				return out, nil
			}
		}
		if !progressed {
			return out, nil
		}
	}
}
