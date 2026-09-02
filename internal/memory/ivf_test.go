package memory

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// splitmix64 is a tiny deterministic PRNG for generating reproducible
// synthetic test vectors -- not math/rand, so builds/test runs never
// depend on stdlib RNG algorithm changes, and definitely not Math.random.
type splitmix64 struct{ s uint64 }

func newSplitmix64(seed uint64) *splitmix64 { return &splitmix64{s: seed} }

func (g *splitmix64) next() uint64 {
	g.s += 0x9E3779B97F4A7C15
	z := g.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// f32 returns a deterministic pseudo-random float32 in [-1, 1).
func (g *splitmix64) f32() float32 {
	return float32(g.next()>>40)/float32(1<<24)*2 - 1
}

func (g *splitmix64) vec(dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = g.f32()
	}
	return v
}

// vecEmbedder is a fixed lookup-table embedder for IVF tests: exact text
// -> vector, dims configurable (unlike fakeEmbedder's fixed 3).
type vecEmbedder struct {
	m    map[string][]float32
	dims int
}

func (e *vecEmbedder) Dims() int { return e.dims }
func (e *vecEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		// Document inputs are key-prefixed ("key: <key>\n<body>"); strip
		// the first line so the map (keyed by body) still matches. Queries
		// are plain text and pass through unchanged.
		key := t
		if strings.HasPrefix(t, "key: ") {
			key = t[strings.IndexByte(t, '\n')+1:]
		}
		out[i] = e.m[key]
	}
	return out, nil
}

// TestIVFRecallVsBruteForce is the correctness floor: the approximate
// index must recover at least 90% of brute force's top-10 neighbors on
// average, at the default nprobe (8). If this fails, the index is wrong
// -- fix the clustering, don't lower the threshold.
func TestIVFRecallVsBruteForce(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()

	const n = 3000
	const dims = 8
	const nq = 50

	gen := newSplitmix64(1)
	vecs := make([][]float32, n)
	m := map[string][]float32{}
	for i := 0; i < n; i++ {
		v := gen.vec(dims)
		vecs[i] = v
		m[fmt.Sprintf("v%d", i)] = v
	}
	qgen := newSplitmix64(2)
	queries := make([][]float32, nq)
	for i := 0; i < nq; i++ {
		v := qgen.vec(dims)
		queries[i] = v
		m[fmt.Sprintf("q%d", i)] = v
	}

	s.SetEmbedder(&vecEmbedder{m: m, dims: dims})
	for i := 0; i < n; i++ {
		if _, err := s.Remember(ctx, "ns", fmt.Sprintf("/k%d", i), fmt.Sprintf("v%d", i), nil, "w"); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.BuildIVF(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	var totalRecall float64
	for qi := 0; qi < nq; qi++ {
		q := queries[qi]
		type sc struct {
			key   string
			score float64
		}
		gt := make([]sc, n)
		for i := 0; i < n; i++ {
			gt[i] = sc{fmt.Sprintf("/k%d", i), cosine(q, vecs[i])}
		}
		sort.Slice(gt, func(a, b int) bool { return gt[a].score > gt[b].score })
		truth := map[string]bool{}
		for i := 0; i < 10; i++ {
			truth[gt[i].key] = true
		}

		got, err := s.VectorSearch(ctx, "ns", fmt.Sprintf("q%d", qi), 10)
		if err != nil {
			t.Fatal(err)
		}
		hit := 0
		for _, f := range got {
			if truth[f.Key] {
				hit++
			}
		}
		totalRecall += float64(hit) / 10
	}
	meanRecall := totalRecall / float64(nq)
	t.Logf("mean recall@10 (n=%d, nprobe=default 8) = %.4f", n, meanRecall)
	if meanRecall < 0.90 {
		t.Fatalf("mean recall@10 = %.4f, want >= 0.90", meanRecall)
	}
}

// TestIVFUnindexedTail proves a vector written after BuildIVF is still
// found: the tail brute force in VectorSearch must catch it regardless of
// which centroids get probed.
func TestIVFUnindexedTail(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	const dims = 8
	const n = 30

	gen := newSplitmix64(3)
	m := map[string][]float32{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("v%d", i)] = gen.vec(dims)
	}
	s.SetEmbedder(&vecEmbedder{m: m, dims: dims})
	// low threshold so this small namespace still takes the index path
	s.SetIVFConfig(2, 5)

	for i := 0; i < n; i++ {
		if _, err := s.Remember(ctx, "ns", fmt.Sprintf("/k%d", i), fmt.Sprintf("v%d", i), nil, "w"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BuildIVF(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	// 5 new facts written after the build; tail2's vector is identical to
	// the query's, guaranteeing it's the true (and only) top match.
	nn := gen.vec(dims)
	m["tail0"], m["tail1"] = gen.vec(dims), gen.vec(dims)
	m["tail2"] = nn
	m["tail3"], m["tail4"] = gen.vec(dims), gen.vec(dims)
	m["q"] = nn
	for i := 0; i < 5; i++ {
		body := fmt.Sprintf("tail%d", i)
		if _, err := s.Remember(ctx, "ns", "/tailkey"+fmt.Sprint(i), body, nil, "w"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.VectorSearch(ctx, "ns", "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range got {
		if f.Key == "/tailkey2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tail nearest-neighbor (/tailkey2) not found in results: %+v", got)
	}
}

// TestIVFBelowThresholdBruteForce: below IVFMinFacts, an index existing
// makes no difference -- results before and after BuildIVF must match
// exactly (brute force is byte-identical to today's behavior).
func TestIVFBelowThresholdBruteForce(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	const dims = 8
	const n = 50 // well below the default IVFMinFacts (2000)

	gen := newSplitmix64(4)
	m := map[string][]float32{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("v%d", i)] = gen.vec(dims)
	}
	m["q"] = gen.vec(dims)
	s.SetEmbedder(&vecEmbedder{m: m, dims: dims})

	for i := 0; i < n; i++ {
		if _, err := s.Remember(ctx, "ns", fmt.Sprintf("/k%d", i), fmt.Sprintf("v%d", i), nil, "w"); err != nil {
			t.Fatal(err)
		}
	}

	before, err := s.VectorSearch(ctx, "ns", "q", 10)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.BuildIVF(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	after, err := s.VectorSearch(ctx, "ns", "q", 10)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(before, after) {
		t.Fatalf("below IVFMinFacts: results changed after building index\nbefore=%+v\nafter=%+v", before, after)
	}
}

// TestIVFTombstonedNotReturned reproduces Finding 1: builtKeys was
// populated from the same snapshot as the postings, so the staleness
// guard could never fire and a Forgotten fact's stale posting kept
// scoring. Forget a fact after BuildIVF, then query with its own vector
// (guaranteed nearest match) -- it must not come back.
func TestIVFTombstonedNotReturned(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	const dims = 8
	const n = 30

	gen := newSplitmix64(5)
	m := map[string][]float32{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("v%d", i)] = gen.vec(dims)
	}
	s.SetEmbedder(&vecEmbedder{m: m, dims: dims})
	s.SetIVFConfig(2, 5) // low threshold so this small namespace uses the index path

	for i := 0; i < n; i++ {
		if _, err := s.Remember(ctx, "ns", fmt.Sprintf("/k%d", i), fmt.Sprintf("v%d", i), nil, "w"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BuildIVF(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	if err := s.Forget(ctx, "ns", "/k7", "w"); err != nil {
		t.Fatal(err)
	}
	m["q"] = m["v7"] // query with the tombstoned fact's own vector: guaranteed nearest

	got, err := s.VectorSearch(ctx, "ns", "q", n)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if f.Key == "/k7" {
			t.Fatalf("tombstoned fact /k7 resurfaced from a stale posting: %+v", got)
		}
	}
}

// TestIVFUpdatedReturnedOnce reproduces the other half of Finding 1: an
// updated fact's stale posting (indexed at build time) must not be
// returned alongside its current revision (found via the tail scan).
// Query with the fact's OLD vector -- if the stale posting were still
// scored, the key would appear twice.
func TestIVFUpdatedReturnedOnce(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	const dims = 8
	const n = 30

	gen := newSplitmix64(6)
	m := map[string][]float32{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("v%d", i)] = gen.vec(dims)
	}
	s.SetEmbedder(&vecEmbedder{m: m, dims: dims})
	s.SetIVFConfig(2, 5)

	for i := 0; i < n; i++ {
		if _, err := s.Remember(ctx, "ns", fmt.Sprintf("/k%d", i), fmt.Sprintf("v%d", i), nil, "w"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BuildIVF(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	oldVec := m["v7"]
	m["q"] = oldVec // query toward the OLD vector, exactly where the stale posting lives
	m["v7-new"] = gen.vec(dims)
	updated, err := s.Remember(ctx, "ns", "/k7", "v7-new", nil, "w")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.VectorSearch(ctx, "ns", "q", n)
	if err != nil {
		t.Fatal(err)
	}
	count, gotID := 0, ""
	for _, f := range got {
		if f.Key == "/k7" {
			count++
			gotID = f.ID
		}
	}
	if count != 1 {
		t.Fatalf("/k7 appeared %d times, want exactly 1 (stale posting + current tail would double it): %+v", count, got)
	}
	if gotID != updated.ID {
		t.Fatalf("/k7 returned with id %s, want current id %s (stale posting revision leaked)", gotID, updated.ID)
	}
}
