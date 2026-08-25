package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func appendToBranch(t *testing.T, dir, line string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "region.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestBranchExperimentMerge(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()

	// live brain: two facts in region "repo"
	if _, err := s.Remember(ctx, "repo", "/a", "original a", nil, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "repo", "/b", "original b", nil, "main"); err != nil {
		t.Fatal(err)
	}

	// satellite branches the region to a git worktree
	dir := filepath.Join(t.TempDir(), "experiment")
	if err := s.BranchRegion(ctx, "repo", dir, "exp-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "region.jsonl")); err != nil {
		t.Fatalf("branch export missing: %v", err)
	}

	// meanwhile the satellite discovers a new fact and appends it to the
	// branch file (simulating offline work), then merges back
	appendToBranch(t, dir, `{"id":"exp-new-1","key":"/c","action":"add","body":"discovered on the branch","created_at":"2026-07-06T01:00:00.000000000Z"}`)
	imported, skipped, blocked, err := s.MergeBranch(ctx, "repo", dir, "exp-1")
	if err != nil {
		t.Fatal(err)
	}
	// the two originals dedup by id, the new one imports
	if imported != 1 || skipped != 2 || blocked != 0 {
		t.Fatalf("merge = %d/%d/%d, want 1/2/0", imported, skipped, blocked)
	}
	facts, err := s.Recall(ctx, "repo", "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("after merge = %d facts, want 3", len(facts))
	}
}
