package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Reranker re-scores query/fact pairs with a cross-encoder. Returns a
// score per input fact, aligned by index. Nil reranker = no reranking.
type Reranker interface {
	Rerank(ctx context.Context, query string, bodies []string) ([]float64, error)
}

// HTTPReranker posts to a TEI-style cross-encoder endpoint:
// POST {URL} {"query": q, "texts": [...]} -> [{"index": i, "score": f}, ...]
// (bge-reranker / TEI /rerank shape). Mirrors OllamaEmbedder.Embed's HTTP
// shape (embed.go).
type HTTPReranker struct {
	URL  string
	HTTP *http.Client
}

func (h *HTTPReranker) Rerank(ctx context.Context, query string, bodies []string) ([]float64, error) {
	body, err := json.Marshal(map[string]any{"query": query, "texts": bodies})
	if err != nil {
		return nil, err
	}
	hc := h.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rerank: status %d", resp.StatusCode)
	}
	var out []struct {
		Index int     `json:"index"`
		Score float64 `json:"score"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	scores := make([]float64, len(bodies)) // default 0 for any index the server omits
	for _, o := range out {
		if o.Index >= 0 && o.Index < len(scores) {
			scores[o.Index] = o.Score
		}
	}
	return scores, nil
}

// rerankTopK bounds how many top fusion candidates get sent to the
// cross-encoder: a cost bound, not a ranking change (the fusion tail
// beyond it is already the weakest RRF/salience candidates).
const rerankTopK = 50

// HybridSearchReranked runs HybridSearchScored, then applies applyRerank
// against the same query. Without a reranker, or on a reranker error /
// index-count mismatch, it returns HybridSearchScored's result unchanged
// (byte-identical): reranking degrades gracefully, it never fails or
// explains a failed search.
func (s *Store) HybridSearchReranked(ctx context.Context, ns, query string, limit int, recencyHalfLife time.Duration) ([]ScoredFact, error) {
	return s.HybridSearchRerankedWith(ctx, ns, query, HybridOpts{Limit: limit, RecencyHalfLife: recencyHalfLife})
}

// HybridSearchRerankedWith is HybridSearchReranked with HybridOpts (anchors).
func (s *Store) HybridSearchRerankedWith(ctx context.Context, ns, query string, o HybridOpts) ([]ScoredFact, error) {
	scored, err := s.HybridSearchScoredWith(ctx, ns, query, o)
	if err != nil {
		return scored, err
	}
	return s.applyRerank(ctx, query, scored), nil
}

// applyRerank re-scores the top rerankTopK of scored (a caller-supplied
// candidate slice, already in some order) against query with the
// configured cross-encoder, writes each hit's Components["rerank"], and
// re-sorts that prefix by the rerank score while leaving the other
// components and the untouched tail intact. Without a reranker, or on a
// reranker error / index-count mismatch, scored is returned unchanged
// (byte-identical): reranking degrades gracefully, it never fails or
// explains a failed search. Shared by HybridSearchReranked (single-query
// search) and HybridSearchExpanded (merged multi-variant search), both of
// which need "call the reranker once, stash the score, re-sort the top-K"
// applied to a candidate set they've already assembled.
func (s *Store) applyRerank(ctx context.Context, query string, scored []ScoredFact) []ScoredFact {
	if s.reranker == nil || len(scored) == 0 {
		return scored
	}
	topN := rerankTopK
	if topN > len(scored) {
		topN = len(scored)
	}
	top := scored[:topN] // shares scored's backing array: sorting top in place reorders scored's prefix
	bodies := make([]string, len(top))
	for i, f := range top {
		bodies[i] = f.Body
	}
	scores, rerr := s.reranker.Rerank(ctx, query, bodies)
	if rerr != nil || len(scores) != len(top) {
		return scored
	}
	for i := range top {
		if top[i].Components == nil {
			top[i].Components = map[string]float64{}
		}
		top[i].Components["rerank"] = scores[i]
	}
	sort.SliceStable(top, func(a, b int) bool { return top[a].Components["rerank"] > top[b].Components["rerank"] })
	return scored
}
