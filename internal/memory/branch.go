package memory

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Branch/merge/federation give the duckbrain "fork a region, experiment,
// merge back" superpower on top of the DB brain: a region exports to a
// git repo (JSONL, the derived-data substrate), agents work a branch in
// isolation, and merge replays the branch's appends back into the live DB
// (append-only + content dedup makes merge conflict-free). The DB stays
// the live brain; git is the experiment + federation layer (B5).

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// BranchRegion exports a region to a git repo at dir on a new branch:
// the region's full history as region.jsonl, committed. Safe to hand to
// an agent that works the branch offline.
func (s *Store) BranchRegion(ctx context.Context, ns, dir, branch string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if _, err := git(ctx, dir, "init", "-q"); err != nil {
			return err
		}
		// deterministic identity so commits work in CI/headless
		_, _ = git(ctx, dir, "config", "user.email", "brain@punkrecords")
		_, _ = git(ctx, dir, "config", "user.name", "punk-records")
	}
	if branch != "" {
		if _, err := git(ctx, dir, "checkout", "-q", "-B", branch); err != nil {
			return err
		}
	}
	f, err := os.Create(filepath.Join(dir, "region.jsonl"))
	if err != nil {
		return err
	}
	if err := s.ExportJSONL(ctx, ns, f); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Close()
	if _, err := git(ctx, dir, "add", "region.jsonl"); err != nil {
		return err
	}
	// nothing to commit is fine (idempotent re-branch)
	_, _ = git(ctx, dir, "commit", "-q", "-m", "export region "+ns)
	return nil
}

// MergeBranch replays a branch's region.jsonl back into the live DB.
// Append-only + content dedup means this is conflict-free: new facts land,
// duplicates collapse, and the returned counts show what actually merged
// (blocked counts records dropped by write-time defense in block mode).
func (s *Store) MergeBranch(ctx context.Context, ns, dir, branch string) (imported, skipped, blocked int, err error) {
	if branch != "" {
		if _, err := git(ctx, dir, "checkout", "-q", branch); err != nil {
			return 0, 0, 0, err
		}
	}
	f, err := os.Open(filepath.Join(dir, "region.jsonl"))
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = f.Close() }()
	return s.ImportJSONL(ctx, ns, f)
}

// PushRegion federates a branched region to a remote (git push). Sharing
// a brain region = pushing its git repo (duckbrain federation).
func (s *Store) PushRegion(ctx context.Context, dir, remote, branch string) error {
	if _, err := git(ctx, dir, "remote", "add", "origin", remote); err != nil {
		// remote may already exist; set-url instead
		if _, err2 := git(ctx, dir, "remote", "set-url", "origin", remote); err2 != nil {
			return err
		}
	}
	_, err := git(ctx, dir, "push", "-q", "origin", branch)
	return err
}

// PullRegion fetches a federated region and merges it into the live DB.
func (s *Store) PullRegion(ctx context.Context, ns, dir, remote, branch string) (imported, skipped, blocked int, err error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if _, err := git(ctx, filepath.Dir(dir), "clone", "-q", remote, filepath.Base(dir)); err != nil {
			return 0, 0, 0, err
		}
	} else {
		if _, err := git(ctx, dir, "pull", "-q", "origin", branch); err != nil {
			return 0, 0, 0, err
		}
	}
	return s.MergeBranch(ctx, ns, dir, branch)
}
