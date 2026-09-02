package memory

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"time"
)

// QueryExpander produces LLM reformulations of a query.
// Implementations live in cmd; nil disables.
type QueryExpander interface {
	Expand(ctx context.Context, query string) ([]string, error)
}

// isNilExpander reports whether exp is unusable as a nil expander would
// be: either the untyped nil interface, or a non-nil interface holding a
// nil value (e.g. a nil *T satisfying QueryExpander via a pointer
// receiver), a case the plain `exp == nil` check misses and that would
// otherwise panic the first time Expand runs.
func isNilExpander(exp QueryExpander) bool {
	if exp == nil {
		return true
	}
	v := reflect.ValueOf(exp)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface, reflect.UnsafePointer:
		return v.IsNil()
	default:
		return false
	}
}

// filterRefs drops blank reformulations and duplicates (case-insensitive,
// trimmed), checked against the original query and against each other in
// encounter order, BEFORE the caller truncates to 3. Filtering before
// truncating matters: an expander emitting ["","","","useful"] must still
// reach "useful" instead of losing it to three dropped blanks, and an LLM
// echoing the original query back as a "reformulation" must not consume a
// search slot that a real reformulation could have used.
func filterRefs(query string, refs []string) []string {
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(query)): true}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		key := strings.ToLower(strings.TrimSpace(r))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// HybridSearchExpanded runs the original query plus up to 3 reformulations
// through HybridSearchScored, no per-variant reranking, then max-merges the
// per-query result sets by fact ID (highest fusion Score wins, its
// Components kept), then applies a single rerank pass (via applyRerank,
// shared with HybridSearchReranked) over the merged candidates against the
// ORIGINAL query. That ordering matters: reranking each variant
// separately and then re-sorting the merge by fusion Score, as an earlier
// version of this function did, silently discards the cross-encoder's
// verdict and leaves a stray Components["rerank"] attributed to whichever
// variant happened to produce the winning fact. Reranking once, after the
// merge, against the query the caller actually asked, means the returned
// order always reflects the reranker (when one is configured) or the
// fusion-score/key-ascending sort (when it isn't), never a discarded
// per-variant ordering.
//
// nil expander (including a typed-nil interface value, e.g. (*T)(nil),
// see isNilExpander), expansion error, or zero reformulations after
// filtering degrade to the plain reranked search on the original query,
// never an error. limit is clamped the same way HybridSearchScored clamps
// it (<=0 or >200 becomes 50), so a caller passing limit<=0 still gets a
// bounded result instead of truncating to zero.
//
// Merging up to 4 per-query result sets (original + up to 3
// reformulations) relaxes per-query guarantees: HybridSearchScored's
// subtree diversification cap applies independently per variant, so the
// merged output can exceed 3 facts per subtree. Cross-variant RRF scores
// are also rank-based and not strictly comparable across differently-
// worded queries; the single final rerank pass (when a reranker is
// configured) mitigates that by re-scoring the merged set against one
// shared query. Both relaxations are deliberate.
// HybridSearchExpanded is HybridSearchExpandedWith without anchors; kept as
// the stable signature every existing caller uses.
func (s *Store) HybridSearchExpanded(ctx context.Context, ns, query string, limit int, recencyHalfLife time.Duration, exp QueryExpander) ([]ScoredFact, error) {
	return s.HybridSearchExpandedWith(ctx, ns, query, HybridOpts{Limit: limit, RecencyHalfLife: recencyHalfLife}, exp)
}

func (s *Store) HybridSearchExpandedWith(ctx context.Context, ns, query string, o HybridOpts, exp QueryExpander) ([]ScoredFact, error) {
	limit := o.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	base, err := s.HybridSearchScoredWith(ctx, ns, query, HybridOpts{Limit: limit, RecencyHalfLife: o.RecencyHalfLife, Anchors: o.Anchors})
	if err != nil {
		return base, err
	}
	if isNilExpander(exp) {
		return s.applyRerank(ctx, query, base), nil
	}
	refs, rerr := exp.Expand(ctx, query)
	if rerr == nil {
		refs = filterRefs(query, refs)
	}
	if rerr != nil || len(refs) == 0 {
		return s.applyRerank(ctx, query, base), nil
	}
	if len(refs) > 3 {
		refs = refs[:3]
	}

	best := map[string]ScoredFact{}
	for _, sf := range base {
		best[sf.ID] = sf
	}
	var tops []string
	if len(base) > 0 {
		tops = append(tops, base[0].ID)
	}
	for _, q := range refs {
		hits, herr := s.HybridSearchScoredWith(ctx, ns, q, HybridOpts{Limit: limit, RecencyHalfLife: o.RecencyHalfLife, Anchors: o.Anchors})
		if herr != nil {
			if cerr := ctx.Err(); cerr != nil {
				// A canceled/deadline-exceeded ctx must propagate as an
				// error, not silently look like "expansion found
				// nothing"; the caller needs to know the search was
				// interrupted, not that no reformulation matched.
				return nil, cerr
			}
			continue // one bad variant (non-ctx error) never fails the whole search
		}
		if len(hits) > 0 {
			tops = append(tops, hits[0].ID)
		}
		for _, sf := range hits {
			if cur, ok := best[sf.ID]; !ok || sf.Score > cur.Score {
				best[sf.ID] = sf
			}
		}
	}
	out := make([]ScoredFact, 0, len(best))
	if len(best) > limit {
		// Coverage first: the top hit of the base query and of every
		// reformulation is kept, in that order, so a variant that reached
		// something the others missed is never truncated away by the sum
		// of the others' scores. The rest is filled by score. Coverage
		// only reorders when truncation would actually drop candidates;
		// otherwise the deterministic score-then-key order is kept.
		seen := map[string]bool{}
		for _, id := range tops {
			if sf, ok := best[id]; ok && !seen[id] {
				seen[id] = true
				out = append(out, sf)
			}
		}
		rest := make([]ScoredFact, 0, len(best))
		for id, sf := range best {
			if !seen[id] {
				rest = append(rest, sf)
			}
		}
		sort.Slice(rest, func(a, b int) bool {
			if rest[a].Score != rest[b].Score {
				return rest[a].Score > rest[b].Score
			}
			return rest[a].Key < rest[b].Key
		})
		out = append(out, rest...)
	} else {
		for _, sf := range best {
			out = append(out, sf)
		}
		sort.Slice(out, func(a, b int) bool {
			if out[a].Score != out[b].Score {
				return out[a].Score > out[b].Score
			}
			return out[a].Key < out[b].Key
		})
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return s.applyRerank(ctx, query, out), nil
}
