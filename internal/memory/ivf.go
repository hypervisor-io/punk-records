package memory

import (
	"context"
	"math"
	"sort"
)

// factRef pairs a live fact with its raw (tag/legacy-encoded) embedding
// blob, so IVF postings can score directly via scoreStored (Task 1's
// shared scorer) without a decode round-trip.
type factRef struct {
	fact Fact
	raw  []byte
}

// ivfIndex is an in-memory inverted-file coarse quantizer: k-means
// centroids over a namespace's live vectors at build time, each vector
// assigned to its nearest centroid's posting list. builtKeys maps a fact
// key to the fact ID that was indexed for it, so VectorSearch's tail scan
// can tell whether a key's current revision is the one already built into
// the index (skip it, already covered by the postings probe) or not
// (score it in the tail, so it's never missed). builtKeys is a snapshot
// from build time, though, so it can never see a tombstone or a later
// update by itself -- VectorSearch instead compares postings against a
// fresh live snapshot taken at query time to decide what's still current.
//
// Advisory only: VectorSearch never depends on this for correctness, only
// for speed. Centroid/posting lookup only changes which candidates get
// scored; the tail brute force (see VectorSearch) guarantees every live
// vector is always reachable regardless of nprobe.
type ivfIndex struct {
	centroids  [][]float32
	postings   [][]factRef
	builtKeys  map[string]string // fact key -> indexed fact ID
	builtCount int
}

// BuildIVF (re)builds the approximate index for ns from its current live
// vectors. Chooses k = round(sqrt(n)) centroids, seeded by evenly-strided
// sampling of the vector list (index i*n/k) -- deterministic, not
// randomness, so builds are reproducible -- then refined by up to 10
// rounds of Lloyd's k-means using cosine assignment (the same metric
// VectorSearch scores with). Cheap enough to run on the consolidation
// ticker.
func (s *Store) BuildIVF(ctx context.Context, ns string) error {
	facts, raws, err := s.liveVectorRows(ctx, ns)
	if err != nil {
		return err
	}
	var refs []factRef
	var vecs [][]float32
	keys := make(map[string]string, len(facts))
	for i, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		v := decodeVector(raw)
		if len(v) == 0 {
			continue
		}
		refs = append(refs, factRef{fact: facts[i], raw: raw})
		vecs = append(vecs, v)
		keys[facts[i].Key] = facts[i].ID
	}

	idx := &ivfIndex{builtKeys: keys, builtCount: len(refs)}
	if len(refs) > 0 {
		k := int(math.Round(math.Sqrt(float64(len(refs)))))
		if k < 1 {
			k = 1
		}
		if k > len(refs) {
			k = len(refs)
		}
		idx.centroids = seedCentroids(vecs, k)
		assign := kmeansAssign(vecs, idx.centroids)
		idx.postings = make([][]factRef, k)
		for i, c := range assign {
			idx.postings[c] = append(idx.postings[c], refs[i])
		}
	}

	s.ivfMu.Lock()
	if s.ivf == nil {
		s.ivf = map[string]*ivfIndex{}
	}
	s.ivf[ns] = idx
	s.ivfMu.Unlock()
	return nil
}

// seedCentroids picks k initial centroids by evenly-strided sampling of
// vecs (index i*n/k each) -- deterministic and reproducible, unlike
// random initialization (banned here: it would break reproducible builds).
func seedCentroids(vecs [][]float32, k int) [][]float32 {
	n := len(vecs)
	centroids := make([][]float32, k)
	for i := range centroids {
		centroids[i] = append([]float32(nil), vecs[i*n/k]...)
	}
	return centroids
}

// kmeansAssign runs Lloyd's algorithm (cosine assignment, mean-vector
// centroid update) for up to 10 iterations, mutating centroids in place,
// and returns each vector's final cluster index.
func kmeansAssign(vecs [][]float32, centroids [][]float32) []int {
	n, k, dims := len(vecs), len(centroids), len(vecs[0])
	assign := make([]int, n)
	for iter := 0; iter < 10; iter++ {
		changed := false
		for i, v := range vecs {
			best, bestSim := 0, -2.0
			for c, cen := range centroids {
				if sim := cosine(v, cen); sim > bestSim {
					bestSim, best = sim, c
				}
			}
			if iter == 0 || assign[i] != best {
				changed = true
			}
			assign[i] = best
		}
		if !changed {
			break
		}
		sums := make([][]float64, k)
		counts := make([]int, k)
		for c := range sums {
			sums[c] = make([]float64, dims)
		}
		for i, v := range vecs {
			c := assign[i]
			counts[c]++
			for d, x := range v {
				sums[c][d] += float64(x)
			}
		}
		for c := range centroids {
			if counts[c] == 0 {
				continue // empty cluster: keep its previous centroid
			}
			nc := make([]float32, dims)
			for d := range nc {
				nc[d] = float32(sums[c][d] / float64(counts[c]))
			}
			centroids[c] = nc
		}
	}
	return assign
}

// nearestCentroids returns up to nprobe centroid indices ordered by
// descending cosine similarity to q.
func nearestCentroids(q []float32, centroids [][]float32, nprobe int) []int {
	type cs struct {
		i int
		s float64
	}
	scored := make([]cs, len(centroids))
	for i, c := range centroids {
		scored[i] = cs{i, cosine(q, c)}
	}
	sort.Slice(scored, func(a, b int) bool { return scored[a].s > scored[b].s })
	if nprobe > len(scored) {
		nprobe = len(scored)
	}
	out := make([]int, nprobe)
	for i := 0; i < nprobe; i++ {
		out[i] = scored[i].i
	}
	return out
}

// ivfNprobeOrDefault and ivfMinFactsOrDefault apply the documented
// defaults (8, 2000) whenever the config knob is unset (<=0).
// ponytail: threshold constants inline; promote to config-validated
// fields if a deployment ever needs per-namespace overrides.
func (s *Store) ivfNprobeOrDefault() int {
	if s.ivfNprobe > 0 {
		return s.ivfNprobe
	}
	return 8
}

func (s *Store) ivfMinFactsOrDefault() int {
	if s.ivfMinFacts > 0 {
		return s.ivfMinFacts
	}
	return 2000
}
