package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// --- Bounded partition / recursion termination (reviewer fixture) ---

type reviewerH01PartitionSum struct{}

func (reviewerH01PartitionSum) Summarize(context.Context, []Fact) (string, error) {
	return "summary", nil
}

func TestReviewerH01PartitionCoversCollidingKeys(t *testing.T) {
	var leaves []Fact
	for i := 0; i < 17; i++ {
		suffix := []byte("aaaaa")
		for bit := 0; bit < 5; bit++ {
			if i&(1<<bit) != 0 {
				suffix[bit] = 'q'
			}
		}
		leaves = append(leaves, Fact{Key: "/observations/db/" + string(suffix)})
	}
	nodes, err := desiredSummaryNodes("db", leaves, 16)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, node := range nodes {
		if len(node.childKeys) > 16 {
			t.Fatalf("unbounded fanout %d", len(node.childKeys))
		}
		if node.leafItems {
			for _, key := range node.childKeys {
				seen[key]++
			}
		}
	}
	if len(seen) != len(leaves) {
		t.Fatalf("covered %d of %d leaves", len(seen), len(leaves))
	}
	for _, leaf := range leaves {
		if seen[leaf.Key] != 1 {
			t.Fatalf("leaf %s appears %d times", leaf.Key, seen[leaf.Key])
		}
	}
}

func TestReviewerH01PartitionRejectsFanoutOne(t *testing.T) {
	nodes, err := desiredSummaryNodes("db", []Fact{{Key: "a"}, {Key: "b"}}, 1)
	if err == nil || len(nodes) != 0 {
		t.Fatalf("want bounded explicit failure with no partial tree; nodes=%d err=%v", len(nodes), err)
	}
	res, err := newTestStore(t).BuildSummaryTree(t.Context(), "ns", reviewerH01PartitionSum{}, SummaryTreeOptions{Fanout: 1})
	if err == nil || res.ModelCalls != 0 || res.Built != 0 {
		t.Fatalf("invalid fanout must fail before model/write work: %+v err=%v", res, err)
	}
}

func TestReviewerH01PartitionGuardReturnsNoPartialTree(t *testing.T) {
	// Deliberately pathological input guarantees no hash can separate it.
	leaves := []Fact{{Key: "same"}, {Key: "same"}, {Key: "same"}}
	nodes, err := desiredSummaryNodes("db", leaves, 2)
	if err == nil || len(nodes) != 0 {
		t.Fatalf("collision guard must fail without a partial tree: nodes=%d err=%v", len(nodes), err)
	}
}

func TestReviewerH01PartitionLargeCorpusCoverage(t *testing.T) {
	leaves := make([]Fact, 1200)
	for i := range leaves {
		leaves[i].Key = fmt.Sprintf("/observations/db/%04d", i)
	}
	nodes, err := desiredSummaryNodes("db", leaves, 16)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, node := range nodes {
		if len(node.childKeys) > 16 {
			t.Fatalf("unbounded fanout %d", len(node.childKeys))
		}
		if node.leafItems {
			for _, key := range node.childKeys {
				seen[key]++
			}
		}
	}
	if len(seen) != len(leaves) {
		t.Fatalf("covered %d of %d leaves", len(seen), len(leaves))
	}
	for _, leaf := range leaves {
		if seen[leaf.Key] != 1 {
			t.Fatalf("leaf %s appears %d times", leaf.Key, seen[leaf.Key])
		}
	}
}

// --- Freshness / provenance / accounting / prune (reviewer fixture) ---

func reviewerH01Write(t *testing.T, s *Store, key, body string, attrs map[string]any) Fact {
	t.Helper()
	f, err := s.Write(t.Context(), WriteInput{Namespace: "ns", Key: key, Body: body, Attributes: attrs})
	if err != nil {
		t.Fatal(err)
	}
	return *f
}

func reviewerH01Attrs(child Fact) map[string]any {
	ids := child.Attributes["source_ids"]
	if nested, ok := child.Attributes["summary"].(map[string]any); ok {
		ids = nested["source_ids"]
	}
	return map[string]any{"summary": map[string]any{
		"stage_version": SummaryStageVersion, "group": "db",
		"children":   []any{map[string]any{"key": child.Key, "rev": child.ID}},
		"source_ids": ids,
	}}
}

type reviewerH01GoodSum struct{}

func (reviewerH01GoodSum) Summarize(context.Context, []Fact) (string, error) { return "summary", nil }

func TestReviewerH01RootDetectsStaleDescendant(t *testing.T) {
	s := newTestStore(t)
	raw := reviewerH01Write(t, s, "/facts/source", "underlying evidence", nil)
	attrs := map[string]any{"source_ids": []any{raw.ID}}
	var changed Fact
	// At fanout 2 these three keys occupy buckets 2+1, safely creating
	// two levels even in the original defective partition implementation.
	for _, slug := range []string{"a", "b", "c"} {
		f := reviewerH01Write(t, s, "/observations/db/"+slug, "old evidence", attrs)
		if slug == "a" {
			changed = f
		}
	}
	if _, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01GoodSum{}, SummaryTreeOptions{Fanout: 2}); err != nil {
		t.Fatal(err)
	}
	roots, err := s.liveByKeys(t.Context(), "ns", []string{"/summaries/db"})
	if err != nil || len(roots) != 1 {
		t.Fatalf("root: %v %v", roots, err)
	}
	root := roots[0]
	before, err := s.SummaryStale(t.Context(), "ns", root)
	if err != nil || before {
		t.Fatalf("unchanged hierarchy must initially be fresh: stale=%v err=%v", before, err)
	}
	reviewerH01Write(t, s, changed.Key, "new evidence", attrs)
	stale, err := s.SummaryStale(t.Context(), "ns", root)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("root presented as fresh after descendant leaf revision changed")
	}
}

type reviewerH01FailingSum struct{}

func (reviewerH01FailingSum) Summarize(context.Context, []Fact) (string, error) {
	return "", errors.New("upstream model failed")
}

func TestReviewerH01FailedAttemptCounted(t *testing.T) {
	s := newTestStore(t)
	raw := reviewerH01Write(t, s, "/facts/source", "underlying evidence", nil)
	reviewerH01Write(t, s, "/observations/db/leaf", "some evidence", map[string]any{"source_ids": []any{raw.ID}})
	res, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01FailingSum{}, SummaryTreeOptions{})
	if err == nil {
		t.Fatal("expected model failure")
	}
	if res.ModelCalls != 1 || res.InputTokens <= 0 {
		t.Fatalf("failed model attempt disappeared from workload: calls=%d estimated input=%d", res.ModelCalls, res.InputTokens)
	}
}

func TestReviewerH01NestedProvenanceRetained(t *testing.T) {
	leaf := Fact{Key: "/observations/db/leaf", ID: "obs-rev", Attributes: map[string]any{"source_ids": []any{"raw-source-revision"}}}
	child := Fact{Key: "/summaries/db/0", Attributes: reviewerH01Attrs(leaf)}
	ids := sourceIDList(child)
	if len(ids) != 1 || ids[0] != "raw-source-revision" {
		t.Fatalf("parent source union loses child-summary provenance: %v", ids)
	}
}

func TestReviewerH01PrunePreservesAdjacentGroup(t *testing.T) {
	s := newTestStore(t)
	raw := reviewerH01Write(t, s, "/facts/source", "underlying evidence", nil)
	leaf := reviewerH01Write(t, s, "/observations/db2/leaf", "current evidence", map[string]any{"source_ids": []any{raw.ID}})
	reviewerH01Write(t, s, "/summaries/db", "orphan", reviewerH01Attrs(Fact{Key: "/observations/db/gone", ID: "gone"}))
	survivor := reviewerH01Write(t, s, "/summaries/db2", "supported", reviewerH01Attrs(leaf))
	if err := s.pruneEmptySummaryGroups(t.Context(), "ns"); err != nil {
		t.Fatal(err)
	}
	facts, err := s.liveByKeys(t.Context(), "ns", []string{survivor.Key})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].ID != survivor.ID {
		t.Fatal("pruning empty db group deleted adjacent still-supported db2 group")
	}
}

type reviewerH01WriteFailSum struct{ s *Store }

func (c reviewerH01WriteFailSum) Summarize(context.Context, []Fact) (string, error) {
	if err := c.s.db.Close(); err != nil {
		return "", err
	}
	return "generated summary", nil
}

func TestReviewerH01FailedWriteKeepsAttemptCount(t *testing.T) {
	s := newTestStore(t)
	raw := reviewerH01Write(t, s, "/facts/source", "underlying evidence", nil)
	reviewerH01Write(t, s, "/observations/db/leaf", "some evidence", map[string]any{"source_ids": []any{raw.ID}})
	res, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01WriteFailSum{s: s}, SummaryTreeOptions{})
	if err == nil {
		t.Fatal("expected closed-database write failure")
	}
	if res.ModelCalls != 1 || res.InputTokens <= 0 || res.Built != 0 {
		t.Fatalf("successful model work lost after failed write: %+v", res)
	}
}

// --- Leaf scope: hierarchy sits above source-linked observations only ---

func TestReviewerH01OnlySourceLinkedObservationLeaves(t *testing.T) {
	s := newTestStore(t)
	raw := reviewerH01Write(t, s, "/facts/source", "underlying source", nil)
	reviewerH01Write(t, s, "/tasks/current", "operational task status", nil)
	reviewerH01Write(t, s, "/observations/db/grounded", "source linked observation", map[string]any{"source_ids": []any{raw.ID}})
	reviewerH01Write(t, s, "/observations/db/unlinked", "unsupported synthesis", nil)
	res, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01GoodSum{}, SummaryTreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Leaves != 1 || res.Groups != 1 {
		t.Fatalf("summary hierarchy admitted non-source-linked-observation leaves: leaves=%d groups=%d, want 1 grounded observation in db", res.Leaves, res.Groups)
	}
	roots, err := s.liveByKeys(t.Context(), "ns", []string{"/summaries/db"})
	if err != nil || len(roots) != 1 {
		t.Fatalf("root: %v %v", roots, err)
	}
	refs, _, ok := summaryAttrs(roots[0])
	if !ok || len(refs) != 1 || refs[0].Key != "/observations/db/grounded" {
		t.Fatalf("wrong observation children: %+v", refs)
	}
}

// --- Source deletion / revision / membership freshness ---

func TestReviewerH01SourceAndMembershipFreshness(t *testing.T) {
	for _, change := range []string{"raw-source-revision", "raw-source-deletion", "new-observation"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			raw := reviewerH01Write(t, s, "/facts/source", "old source", nil)
			attrs := map[string]any{"source_ids": []any{raw.ID}}
			reviewerH01Write(t, s, "/observations/db/first", "derived from old source", attrs)
			if _, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01GoodSum{}, SummaryTreeOptions{}); err != nil {
				t.Fatal(err)
			}
			roots, err := s.liveByKeys(t.Context(), "ns", []string{"/summaries/db"})
			if err != nil || len(roots) != 1 {
				t.Fatalf("root: %v %v", roots, err)
			}
			root := roots[0]
			if stale, err := s.SummaryStale(t.Context(), "ns", root); err != nil || stale {
				t.Fatalf("unchanged root must be fresh: stale=%v err=%v", stale, err)
			}
			switch change {
			case "raw-source-revision":
				reviewerH01Write(t, s, raw.Key, "corrected source", nil)
			case "raw-source-deletion":
				if err := s.Forget(t.Context(), "ns", raw.Key, "reviewer"); err != nil {
					t.Fatal(err)
				}
			case "new-observation":
				reviewerH01Write(t, s, "/observations/db/second", "new relevant evidence", attrs)
			}
			stale, err := s.SummaryStale(t.Context(), "ns", root)
			if err != nil {
				t.Fatal(err)
			}
			if !stale {
				t.Fatalf("summary still fresh after %s", change)
			}
		})
	}
}

// --- Compact retrieval must preserve a stale-summary warning ---

func TestReviewerH01CompactRetrievalPreservesStaleSummaryWarning(t *testing.T) {
	s := newTestStore(t)
	raw := reviewerH01Write(t, s, "/facts/source", "old evidence", nil)
	reviewerH01Write(t, s, "/observations/db/leaf", "derived evidence", map[string]any{"source_ids": []any{raw.ID}})
	if _, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01GoodSum{}, SummaryTreeOptions{}); err != nil {
		t.Fatal(err)
	}
	roots, err := s.liveByKeys(t.Context(), "ns", []string{"/summaries/db"})
	if err != nil || len(roots) != 1 {
		t.Fatalf("root: %v %v", roots, err)
	}
	reviewerH01Write(t, s, raw.Key, "corrected evidence", nil)
	if stale, err := s.SummaryStale(t.Context(), "ns", roots[0]); err != nil || !stale {
		t.Fatalf("setup must have stale root: stale=%v err=%v", stale, err)
	}
	hits, err := s.UnifiedSearch(t.Context(), "ns", "summary", 50)
	if err != nil {
		t.Fatal(err)
	}

	// Both compact projections must carry the warning: the scored
	// unified path AND the plain recall path.
	recalled, err := s.Recall(t.Context(), "ns", "/summaries/db", 50)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string][]CompactHit{"unified": CompactUnified(hits, 0), "recall": CompactFacts(recalled, 0)} {
		t.Run(name, func(t *testing.T) {
			for _, hit := range path {
				if hit.Key != "/summaries/db" {
					continue
				}
				for _, flag := range hit.Flags {
					if flag == "stale" || flag == "unverified" || flag == "provisional" {
						return
					}
				}
				t.Fatalf("known stale summary reached compact %s without warning: flags=%v", name, hit.Flags)
			}
			t.Fatal("fixture summary was not retrieved")
		})
	}
}

// --- Boundary deletion: a shrink across the fanout threshold must not
// retire a non-empty untouched sibling (established split topology retained) ---

func TestReviewerH01BoundaryDeletionPreservesUntouchedSibling(t *testing.T) {
	s := newTestStore(t)
	raw := reviewerH01Write(t, s, "/facts/source", "underlying source", nil)
	attrs := map[string]any{"source_ids": []any{raw.ID}}
	groups := [2][]string{}
	for n := 0; len(groups[0]) < 2 || len(groups[1]) < 1; n++ {
		if n > 1000 {
			t.Fatal("could not build deterministic two-bucket fixture")
		}
		key := fmt.Sprintf("/observations/db/leaf-%03d", n)
		b := bucketIndex(key, 2, 0)
		want := 1
		if b == 0 {
			want = 2
		}
		if len(groups[b]) < want {
			groups[b] = append(groups[b], key)
		}
	}
	for _, keys := range groups {
		for _, key := range keys {
			reviewerH01Write(t, s, key, "source-linked observation", attrs)
		}
	}
	if _, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01GoodSum{}, SummaryTreeOptions{Fanout: 2}); err != nil {
		t.Fatal(err)
	}
	siblingKey := "/summaries/db/1"
	before, err := s.liveByKeys(t.Context(), "ns", []string{siblingKey})
	if err != nil || len(before) != 1 {
		t.Fatalf("untouched sibling setup: %+v %v", before, err)
	}
	if err := s.Forget(t.Context(), "ns", groups[0][0], "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01GoodSum{}, SummaryTreeOptions{Fanout: 2}); err != nil {
		t.Fatal(err)
	}
	after, err := s.liveByKeys(t.Context(), "ns", []string{siblingKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("deleting bucket-0 leaf retired nonempty untouched bucket-1 sibling at fanout threshold: before=%s after_count=%d", before[0].ID, len(after))
	}
}

// TestReviewerH01FreshSummaryUnverifiedOnRecall pins that a generated
// summary surfaced without a computed stale marker is conservatively
// flagged unverified, not silently presented as current: an unchanged
// summary from the plain recall path (no verified freshness marker) stays
// provisional. A computed stale marker keeps the stronger 'stale' flag.
func TestReviewerH01FreshSummaryUnverifiedOnRecall(t *testing.T) {
	s := newTestStore(t)
	raw := reviewerH01Write(t, s, "/facts/source", "underlying source", nil)
	reviewerH01Write(t, s, "/observations/db/leaf", "derived evidence", map[string]any{"source_ids": []any{raw.ID}})
	if _, err := s.BuildSummaryTree(t.Context(), "ns", reviewerH01GoodSum{}, SummaryTreeOptions{}); err != nil {
		t.Fatal(err)
	}
	recalled, err := s.Recall(t.Context(), "ns", "/summaries/db", 10)
	if err != nil || len(recalled) != 1 {
		t.Fatalf("recall: %v %v", recalled, err)
	}
	hits := CompactFacts(recalled, 0)
	found := false
	for _, h := range hits {
		if h.Key != "/summaries/db" {
			continue
		}
		found = true
		unverified, stale := false, false
		for _, flag := range h.Flags {
			if flag == "unverified" {
				unverified = true
			}
			if flag == "stale" {
				stale = true
			}
		}
		if stale {
			t.Fatalf("unchanged fresh summary wrongly flagged stale: %v", h.Flags)
		}
		if !unverified {
			t.Fatalf("fresh summary from recall path presented current without unverified marker: %v", h.Flags)
		}
	}
	if !found {
		t.Fatal("summary not retrieved")
	}
}
