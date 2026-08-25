package memory

import (
	"context"
	"sort"
)

// UnifiedHit is one entry of a unified recall: either a ranked fact or a
// ranked relation-triplet, with a fused rank score. Exactly one of Fact
// / Triplet is set.
type UnifiedHit struct {
	Kind    string      `json:"kind"` // "fact" | "relation"
	Fact    *ScoredFact `json:"fact,omitempty"`
	Triplet *Triplet    `json:"triplet,omitempty"`
	Score   float64     `json:"score"` // reciprocal-rank-fused
}

// UnifiedSearch fuses HybridSearchScored (facts/observations/entities/
// mental-models) and TripletSearch (relations) into one ranked list via
// Reciprocal Rank Fusion (k=60): each source list contributes 1/(60+rank)
// per item; facts and relations are distinct hits (a triplet's endpoint
// facts may also appear separately as fact hits — the relation is a
// distinct surfacing, not a duplicate). Returns the top-limit.
// No embedder → TripletSearch yields nothing, so this degrades to the
// HybridSearchScored ranking wrapped as UnifiedHit (byte-identical order).
func (s *Store) UnifiedSearch(ctx context.Context, ns, query string, limit int) ([]UnifiedHit, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const k = 60.0

	facts, err := s.HybridSearchScored(ctx, ns, query, limit, 0)
	if err != nil {
		return nil, err
	}
	trips, _ := s.TripletSearch(ctx, ns, query, limit) // no embedder / error -> degrade to facts-only

	out := make([]UnifiedHit, 0, len(facts)+len(trips))
	for i := range facts {
		out = append(out, UnifiedHit{Kind: "fact", Fact: &facts[i], Score: 1.0 / (k + float64(i+1))})
	}
	for i := range trips {
		out = append(out, UnifiedHit{Kind: "relation", Triplet: &trips[i], Score: 1.0 / (k + float64(i+1))})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
