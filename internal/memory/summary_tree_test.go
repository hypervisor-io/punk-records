package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// keyRecordingSummarizer is a deterministic fake: it records how many times
// it was invoked and emits a body keyed on the children so tests can
// pin exactly which facts a summary covers.
type keyRecordingSummarizer struct {
	calls int
	facts int
}

func (c *keyRecordingSummarizer) Summarize(_ context.Context, facts []Fact) (string, error) {
	c.calls++
	c.facts += len(facts)
	var keys []string
	for _, f := range facts {
		keys = append(keys, f.Key)
	}
	sort.Strings(keys)
	return "summary[" + strings.Join(keys, ",") + "]", nil
}

// linkedObsSource writes one raw source fact and returns it, so fixtures
// can build source-linked observations (the only eligible summary leaves).
func linkedObsSource(t *testing.T, s *Store, ctx context.Context, key, body string) Fact {
	t.Helper()
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key, Body: body, Writer: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return *f
}

// writtenObs writes one source-linked observation leaf under a hierarchical
// or flat slug, grounded in src, and returns the written fact.
func writtenObs(t *testing.T, s *Store, ctx context.Context, slug, body string, src Fact) Fact {
	t.Helper()
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/" + slug,
		Body: body, Writer: "test",
		Attributes: map[string]any{"source_ids": []any{src.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	return *f
}

// summaryStore returns the live summary facts keyed by their key.
func summaryStore(t *testing.T, s *Store, ctx context.Context) map[string]Fact {
	t.Helper()
	keys, err := s.ListKeys(ctx, "ns", "/summaries")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Fact{}
	for _, k := range keys {
		fs, err := s.Recall(ctx, "ns", k, 1)
		if err != nil || len(fs) != 1 {
			t.Fatalf("recall summary %s: %v %v", k, fs, err)
		}
		out[k] = fs[0]
	}
	return out
}

// summaryChildren parses a summary fact's recorded child keys.
func summaryChildren(f Fact) []string {
	sm, ok := f.Attributes["summary"].(map[string]any)
	if !ok {
		return nil
	}
	children, _ := sm["children"].([]any)
	keys := make([]string, 0, len(children))
	for _, c := range children {
		cm, _ := c.(map[string]any)
		if k, _ := cm["key"].(string); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// siblingMap maps child summary key -> parent summary key for a set of
// summaries that reference other summaries as children.
func siblingMap(summaries map[string]Fact) map[string]string {
	parent := map[string]string{}
	for parentKey, f := range summaries {
		sm, ok := f.Attributes["summary"].(map[string]any)
		if !ok {
			continue
		}
		children, _ := sm["children"].([]any)
		for _, c := range children {
			cm, _ := c.(map[string]any)
			if k, _ := cm["key"].(string); strings.HasPrefix(k, "/summaries/") {
				parent[k] = parentKey
			}
		}
	}
	return parent
}

// ancestorPath returns the summary keys from leaf's parent up to the
// group root, following child->parent references.
func ancestorPath(summaries map[string]Fact, leafKey string) []string {
	parents := siblingMap(summaries)
	// find the deepest summary listing leafKey directly as a child
	cur := ""
	for k, f := range summaries {
		for _, ck := range summaryChildren(f) {
			if ck == leafKey {
				cur = k
				break
			}
		}
		if cur != "" {
			break
		}
	}
	if cur == "" {
		return nil
	}
	var path []string
	seen := map[string]bool{}
	for cur != "" && !seen[cur] {
		seen[cur] = true
		path = append(path, cur)
		cur = parents[cur]
	}
	return path
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// groupLeavesFor builds a deterministic single-group leaf set of n
// source-linked observations under /observations/<group>/..., each grounded
// in one shared raw source. Returns the raw source so a test can edit a
// leaf while preserving its linkage.
func groupLeavesFor(t *testing.T, s *Store, ctx context.Context, group string, n int) Fact {
	t.Helper()
	src := linkedObsSource(t, s, ctx, "/facts/source-"+group, "raw source for "+group)
	for i := 0; i < n; i++ {
		writtenObs(t, s, ctx, fmt.Sprintf("%s/obs-%03d", group, i), "body "+fmt.Sprintf("%02d", i), src)
	}
	return src
}

// TestSummaryTreeLeafEditInvalidatesOnlyAncestors: editing one leaf must
// rebuild only that leaf's summary and its ancestors; sibling branches do
// no model work and keep their revision.
func TestSummaryTreeLeafEditInvalidatesOnlyAncestors(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	src := groupLeavesFor(t, s, ctx, "g", 8)

	sum := &keyRecordingSummarizer{}
	res, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Built == 0 {
		t.Fatal("first build did no model work")
	}
	before := summaryStore(t, s, ctx)
	if len(before) < 2 {
		t.Fatalf("expected a tree, got %d summaries", len(before))
	}

	// capture the leaf's ancestor path before editing
	target := "/observations/g/obs-000"
	path := ancestorPath(before, target)
	if len(path) == 0 {
		t.Fatalf("leaf %s not found in any summary children", target)
	}

	// edit the leaf: new revision, same key, linkage preserved
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: target, Body: "edited", Writer: "test",
		Attributes: map[string]any{"source_ids": []any{src.ID}}}); err != nil {
		t.Fatal(err)
	}
	sum.calls = 0
	res2, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res2.ModelCalls != len(path) {
		t.Fatalf("ModelCalls = %d, want %d (only the ancestor chain)", res2.ModelCalls, len(path))
	}
	if res2.Built != len(path) {
		t.Fatalf("Built = %d, want %d", res2.Built, len(path))
	}
	after := summaryStore(t, s, ctx)
	// ancestors changed revision; siblings did not
	for k, fOld := range before {
		fNew, ok := after[k]
		if !ok {
			t.Fatalf("summary %s vanished on edit", k)
		}
		if contains(path, k) {
			if fNew.ID == fOld.ID {
				t.Fatalf("ancestor %s not rebuilt after edit", k)
			}
		} else if fNew.ID != fOld.ID {
			t.Fatalf("untouched branch %s rebuilt (id %s -> %s)", k, fOld.ID, fNew.ID)
		}
	}
	// the edited revision is recorded as live
	fs, _ := s.Recall(ctx, "ns", target, 1)
	if len(fs) != 1 {
		t.Fatalf("leaf gone")
	}
	stale, err := s.SummaryStale(ctx, "ns", after[path[len(path)-1]])
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatalf("freshly rebuilt root reported stale")
	}
}

// TestSummaryTreeLeafDeleteInvalidatesOnlyAncestors deletes one leaf: only
// the ancestor path changes (rebuilt or forgotten); untouched branches keep
// their revision and do no model work.
func TestSummaryTreeLeafDeleteInvalidatesOnlyAncestors(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	groupLeavesFor(t, s, ctx, "g", 8)

	sum := &keyRecordingSummarizer{}
	if _, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 3}); err != nil {
		t.Fatal(err)
	}
	before := summaryStore(t, s, ctx)

	target := "/observations/g/obs-000"
	path := ancestorPath(before, target)
	if len(path) == 0 {
		t.Fatalf("leaf %s not found", target)
	}
	if err := s.Forget(ctx, "ns", target, "test"); err != nil {
		t.Fatal(err)
	}

	sum.calls = 0
	res, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 3})
	if err != nil {
		t.Fatal(err)
	}
	after := summaryStore(t, s, ctx)
	// the deleted leaf must no longer be any summary's child
	for k, f := range after {
		for _, ck := range summaryChildren(f) {
			if ck == target {
				t.Fatalf("deleted leaf %s still a child of %s", target, k)
			}
		}
	}
	// untouched branches: not rebuilt, still present, no model work
	var rebuilt int
	for k, fOld := range before {
		fNew, ok := after[k]
		if !ok {
			if contains(path, k) {
				continue // ancestor disappeared (empty bucket) - fine
			}
			t.Fatalf("untouched summary %s vanished on delete", k)
		}
		if contains(path, k) {
			if fNew.ID != fOld.ID {
				rebuilt++
			}
		} else if fNew.ID != fOld.ID {
			t.Fatalf("untouched branch %s rebuilt after delete", k)
		}
	}
	if rebuilt != len(path) {
		t.Fatalf("rebuilt ancestors = %d, want %d", rebuilt, len(path))
	}
	if res.ModelCalls != rebuilt {
		t.Fatalf("ModelCalls = %d, want %d (ancestors only)", res.ModelCalls, rebuilt)
	}
}

// TestSummaryTreeRebuildNoModelWork proves idempotence: an unchanged corpus
// rebuilds in zero Summarize calls and produces no new summary revisions.
func TestSummaryTreeRebuildNoModelWork(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	groupLeavesFor(t, s, ctx, "g", 8)

	sum := &keyRecordingSummarizer{}
	if _, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 3}); err != nil {
		t.Fatal(err)
	}
	first := summaryStore(t, s, ctx)

	sum.calls = 0
	res, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelCalls != 0 {
		t.Fatalf("unchanged corpus did %d model calls, want 0", res.ModelCalls)
	}
	if res.Fresh == 0 {
		t.Fatalf("expected fresh summaries, got 0")
	}
	second := summaryStore(t, s, ctx)
	if len(first) != len(second) {
		t.Fatalf("summary set changed: %d -> %d", len(first), len(second))
	}
	for k, f := range first {
		if second[k].ID != f.ID {
			t.Fatalf("unchanged summary %s got a new revision", k)
		}
	}
}

// TestSummaryTreeBeyondRecallCapFullCoverage writes 1200 observations:
// the union of every summary's children must equal all 1200 leaves, with no
// silent first-1000-facts ceiling.
func TestSummaryTreeBeyondRecallCapFullCoverage(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	groupLeavesFor(t, s, ctx, "g", 1200)

	sum := &keyRecordingSummarizer{}
	// A fanout above the default still forces multi-level bucketing over
	// 1200 leaves at a lower node count, keeping the run cheap while the
	// proof (full coverage, no cap) is unchanged.
	if _, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 40}); err != nil {
		t.Fatal(err)
	}
	all := summaryStore(t, s, ctx)
	if len(all) == 0 {
		t.Fatal("no summaries produced")
	}
	leaves := map[string]bool{}
	var collect func(Fact)
	collect = func(f Fact) {
		for _, ck := range summaryChildren(f) {
			if strings.HasPrefix(ck, "/summaries/") {
				sub, err := s.Recall(ctx, "ns", ck, 1)
				if err != nil {
					continue
				}
				if len(sub) == 1 {
					collect(sub[0])
				}
			} else {
				leaves[ck] = true
			}
		}
	}
	for _, f := range all {
		collect(f)
	}
	if len(leaves) != 1200 {
		t.Fatalf("union of children = %d leaves, want 1200", len(leaves))
	}
	for i := 0; i < 1200; i++ {
		if !leaves[fmt.Sprintf("/observations/g/obs-%03d", i)] {
			t.Fatalf("leaf obs-%03d missing from summary children", i)
		}
	}
}

// TestSummaryTreeEmptySubtreeDisappears: deleting every leaf of one group
// removes that group's whole prefix; an untouched group remains.
func TestSummaryTreeEmptySubtreeDisappears(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	groupLeavesFor(t, s, ctx, "a", 4)
	groupLeavesFor(t, s, ctx, "b", 4)

	sum := &keyRecordingSummarizer{}
	if _, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recall(ctx, "ns", "/summaries/a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recall(ctx, "ns", "/summaries/b", 1); err != nil {
		t.Fatal(err)
	}

	// delete all of group a
	for i := 0; i < 4; i++ {
		if err := s.Forget(ctx, "ns", fmt.Sprintf("/observations/a/obs-%03d", i), "test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 10}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListKeys(ctx, "ns", "/summaries/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("empty group a subtree still present: %v", keys)
	}
	b, err := s.ListKeys(ctx, "ns", "/summaries/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatalf("untouched group b subtree disappeared")
	}
}

// TestSummaryTreeSeparateFromMentalModels: summaries are never generated
// under /mental-models and curated models are never fed in as leaves.
func TestSummaryTreeSeparateFromMentalModels(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	groupLeavesFor(t, s, ctx, "g", 8)
	if _, err := s.RememberModel(ctx, "ns", "db", "curated postgres model", nil, false); err != nil {
		t.Fatal(err)
	}

	sum := &keyRecordingSummarizer{}
	if _, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 3}); err != nil {
		t.Fatal(err)
	}
	for k, f := range summaryStore(t, s, ctx) {
		if strings.HasPrefix(k, "/mental-models/") {
			t.Fatalf("summary %s generated under /mental-models", k)
		}
		if !strings.HasPrefix(k, "/summaries/") {
			t.Fatalf("summary key %s not under /summaries", k)
		}
		for _, ck := range summaryChildren(f) {
			if strings.HasPrefix(ck, "/mental-models/") {
				t.Fatalf("curated model %s fed in as a summary leaf", ck)
			}
		}
	}
	// curated models listing is untouched
	models, err := s.ListModels(ctx, "ns")
	if err != nil || len(models) != 1 || models[0].Key != "/mental-models/db" {
		t.Fatalf("curated models changed by summary build: %v %v", models, err)
	}
}

// TestSummaryStaleRetentionSafe: a swept/superseded recorded revision marks
// a summary stale (never an error); the latest live revision survives
// retention so a fresh summary stays fresh.
func TestSummaryStaleRetentionSafe(t *testing.T) {
	s, clock := newTestStoreWithClock(t)
	ctx := t.Context()
	base := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	clock.Set(base)
	src, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/facts/source-g", Body: "raw source"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/g/obs-000", Body: "original",
		Attributes: map[string]any{"source_ids": []any{src.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	sum := &keyRecordingSummarizer{}
	if _, err := s.BuildSummaryTree(ctx, "ns", sum, SummaryTreeOptions{Fanout: 10}); err != nil {
		t.Fatal(err)
	}
	root, err := s.Recall(ctx, "ns", "/summaries/g", 1)
	if err != nil || len(root) != 1 {
		t.Fatalf("recall root: %v %v", root, err)
	}
	if stale, err := s.SummaryStale(ctx, "ns", root[0]); err != nil || stale {
		t.Fatalf("fresh summary marked stale (err %v)", err)
	}

	// supersede the recorded revision: edit the leaf behind an advancing clock
	clock.Set(base.Add(2 * time.Second))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/g/obs-000", Body: "edited",
		Attributes: map[string]any{"source_ids": []any{src.ID}}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recall(ctx, "ns", "/summaries/g", 1)
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	if stale, err := s.SummaryStale(ctx, "ns", got[0]); err != nil || !stale {
		t.Fatalf("summary with superseded child reported fresh (stale=%v err=%v)", stale, err)
	}

	// retention sweeps the superseded revision: still stale, never an error
	clock.Set(base.Add(3 * time.Hour))
	if _, err := s.SweepRetention(ctx, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err = s.Recall(ctx, "ns", "/summaries/g", 1)
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	if stale, err := s.SummaryStale(ctx, "ns", got[0]); err != nil || !stale {
		t.Fatalf("retention-swept child not reported stale (stale=%v err=%v)", stale, err)
	}
	_ = leaf // keep the build-time revision for clarity
}

// capObserver returns a single observation whose source_ids span every raw
// fact, so a silent 1000 cap would be visible in the source_ids count.
type capObserver struct{}

func (capObserver) Observe(_ context.Context, facts []Fact) ([]Observation, error) {
	ids := make([]string, len(facts))
	for i, f := range facts {
		ids[i] = f.ID
	}
	return []Observation{{Slug: "everything", Body: "all facts", SourceIDs: ids}}, nil
}

// TestConsolidateObservationsBeyondCap proves the paginated observation scan
// is uncapped: 1200 raw facts all reach the observer, not only 1000.
func TestConsolidateObservationsBeyondCap(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for i := 0; i < 1200; i++ {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: fmt.Sprintf("/raw/f-%04d", i), Body: fmt.Sprintf("fact %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	written, err := s.ConsolidateObservations(ctx, "ns", capObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}
	got, err := s.Recall(ctx, "ns", "/observations/everything", 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("recall observation: %v %v", got, err)
	}
	ids, _ := got[0].Attributes["source_ids"].([]any)
	if len(ids) != 1200 {
		t.Fatalf("source_ids = %d, want 1200 (silent 1000 cap still present)", len(ids))
	}
}
