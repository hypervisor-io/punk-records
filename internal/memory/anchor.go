package memory

import (
	"context"
	"strings"
	"time"
)

// HybridOpts carries the optional knobs of HybridSearchScoredWith. Zero
// value means "exactly HybridSearchScored".
type HybridOpts struct {
	Limit           int
	RecencyHalfLife time.Duration
	// Anchors are exact identifiers, error strings, flags or file names
	// supplied by the caller. Each becomes a phrase-match FTS arm fused
	// by RRF under the "anchor" component. They are retrieval routes,
	// not filters: a hit never needs to contain an anchor to be returned.
	Anchors []string
}

// maxAnchors bounds the extra FTS round trips per search.
const maxAnchors = 8

func cleanAnchors(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		out = append(out, a)
		if len(out) == maxAnchors {
			break
		}
	}
	return out
}

// anchorArm runs one phrase search per anchor and returns, per fact ID,
// the summed RRF contribution across anchors (a fact matching two
// anchors outranks one matching one), plus the facts themselves.
func (s *Store) anchorArm(ctx context.Context, ns string, anchors []string, sourceCap int) (map[string]float64, map[string]Fact, error) {
	const k = 60.0
	contrib := map[string]float64{}
	facts := map[string]Fact{}
	for _, a := range cleanAnchors(anchors) {
		hits, err := s.SearchPhrase(ctx, ns, a, 100)
		if err != nil {
			return nil, nil, err
		}
		if len(hits) > sourceCap {
			hits = hits[:sourceCap]
		}
		for rank, f := range hits {
			contrib[f.ID] += 1.0 / (k + float64(rank+1))
			facts[f.ID] = f
		}
	}
	return contrib, facts, nil
}
