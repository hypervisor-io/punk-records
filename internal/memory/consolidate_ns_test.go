package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// consolidationFixture writes a namespace with a done task, an open task,
// a decision with history, a tombstoned key, a session capture, a link
// and a convention.
func consolidationFixture(t *testing.T, s *Store, ns string) {
	t.Helper()
	ctx := context.Background()
	w := func(key, body string) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: ns, Key: key, Body: body, Author: "planner", Writer: "planner"}); err != nil {
			t.Fatal(err)
		}
	}
	w("/tasks/A", "Ship the board")
	w("/tasks/A/status", "done: abc123 shipped")
	w("/tasks/B", "Write docs")
	w("/tasks/B/status", "in_progress: drafting")
	w("/decisions/db", "use sqlite")
	w("/decisions/db", "use sqlite with WAL") // second revision: history moves too
	w("/conventions/repo", "go test ./...")
	w("/gone", "temporary")
	if err := s.Forget(ctx, ns, "/gone", "planner"); err != nil {
		t.Fatal(err)
	}
	w("/agent-sessions/s1/turn", "captured turn")
	if err := s.AddLink(ctx, ns, "/decisions/db", "/tasks/A", "relates_to"); err != nil {
		t.Fatal(err)
	}
}

func liveKeys(t *testing.T, s *Store, ns string) string {
	t.Helper()
	keys, err := s.ListKeys(context.Background(), ns, "/")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(keys, ",")
}

// TestConsolidationPlanKeepsOpenWork: open tasks (with children),
// /consolidated/ and keep prefixes stay; everything else, including
// tombstoned and done-task keys, moves.
func TestConsolidationPlanKeepsOpenWork(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	consolidationFixture(t, s, "proj")
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	plan, err := s.PlanConsolidation(ctx, "proj", []string{"/conventions/"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan.KeepKeys, ","); got != "/conventions/repo,/tasks/B,/tasks/B/status" {
		t.Fatalf("keep = %s", got)
	}
	// /agent-sessions/s1/turn, /decisions/db, /gone, /tasks/A, /tasks/A/status
	if plan.MoveKeys != 5 {
		t.Fatalf("move keys = %d, want 5", plan.MoveKeys)
	}
	if plan.Archive != "proj-archive-20261028" || strings.Join(plan.OpenTasks, ",") != "B" {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := s.PlanConsolidation(ctx, "proj", []string{"conventions"}, now); err == nil {
		t.Fatal("a keep prefix without a leading / must be rejected")
	}
}

// TestArchiveNamespaceMovesHistoryAndRestores: the move keeps every
// revision and link in the archive, leaves only the kept keys live, and
// RestoreArchive brings it all back.
func TestArchiveNamespaceMovesHistoryAndRestores(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	consolidationFixture(t, s, "proj")
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	plan, err := s.PlanConsolidation(ctx, "proj", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "proj", Key: ConsolidatedSummaryKey, Body: "# Summary", Writer: "me"}); err != nil {
		t.Fatal(err)
	}
	keep := append(plan.KeepKeys, ConsolidatedSummaryKey)
	moved, err := s.ArchiveNamespace(ctx, "proj", plan.Archive, keep)
	if err != nil {
		t.Fatal(err)
	}
	// 11 revisions written (the fixture's 10, counting two for
	// /decisions/db and add+tombstone for /gone, plus the summary) minus
	// the 3 kept (/tasks/B, its status, the summary).
	if moved != 8 {
		t.Fatalf("moved = %d, want 8", moved)
	}
	if got := liveKeys(t, s, "proj"); got != "/consolidated/summary,/tasks/B,/tasks/B/status" {
		t.Fatalf("live after archive = %s", got)
	}
	if got := liveKeys(t, s, plan.Archive); got != "/agent-sessions/s1/turn,/conventions/repo,/decisions/db,/tasks/A,/tasks/A/status" {
		t.Fatalf("archive live keys = %s", got)
	}
	// History moved with the key: both revisions sit in the archive.
	var revs int
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM memories m JOIN namespaces n ON n.id = m.namespace_id
		WHERE n.name = $1 AND m.key = '/decisions/db'`), plan.Archive).Scan(&revs); err != nil || revs != 2 {
		t.Fatalf("archive revisions of /decisions/db = %d %v, want 2", revs, err)
	}
	links, err := s.Neighbors(ctx, plan.Archive, "/decisions/db", "out")
	if err != nil || len(links) == 0 {
		t.Fatalf("archive links = %v %v, want the moved link", links, err)
	}
	// A second consolidation the same day picks a fresh archive name.
	if name, err := s.ArchiveName(ctx, "proj", now); err != nil || name != "proj-archive-20261028-2" {
		t.Fatalf("second archive name = %q %v", name, err)
	}
	if _, err := s.ArchiveNamespace(ctx, "proj", plan.Archive, keep); err == nil {
		t.Fatal("archiving into a non-empty archive must fail")
	}

	if err := s.RecordConsolidation(ctx, "proj", ConsolidationStatus{At: now, By: "me", Archive: plan.Archive, MovedFacts: moved}); err != nil {
		t.Fatal(err)
	}
	marks, err := s.NamespaceMarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := marks["proj"]; m.ConsolidatedAt.IsZero() || m.Archive {
		t.Fatalf("proj mark = %+v, want consolidated", m)
	}
	if m := marks[plan.Archive]; !m.Archive {
		t.Fatalf("archive mark = %+v, want archive", m)
	}

	src, back, err := s.RestoreArchive(ctx, plan.Archive, "me")
	if err != nil || src != "proj" || back != moved {
		t.Fatalf("restore = %q %d %v, want proj %d", src, back, err, moved)
	}
	if got := liveKeys(t, s, "proj"); !strings.Contains(got, "/decisions/db") || !strings.Contains(got, "/consolidated/summary") {
		t.Fatalf("live after restore = %s", got)
	}
	if got := liveKeys(t, s, plan.Archive); got != "" {
		t.Fatalf("archive after restore = %s, want empty", got)
	}
	marks, _ = s.NamespaceMarks(ctx)
	if marks[plan.Archive].Archive {
		t.Fatal("a restored archive must no longer read as an archive")
	}
	if !marks["proj"].ConsolidatedAt.IsZero() {
		t.Fatal("a restored namespace must no longer read as consolidated")
	}
}

// TestLiveFactsPageSkipsSessionsAndPages: key-ordered pages with a
// cursor, session captures skipped unless asked for.
func TestLiveFactsPageSkipsSessionsAndPages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"/a", "/agent-sessions/x", "/agent-sessions/y", "/b", "/c"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "pg", Key: k, Body: "v", Writer: "w"}); err != nil {
			t.Fatal(err)
		}
	}
	keys := func(fs []Fact) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Key)
		}
		return strings.Join(out, ",")
	}
	p1, next, err := s.LiveFactsPage(ctx, "pg", "/", "", 2, false)
	if err != nil || keys(p1) != "/a,/b" || next != "/b" {
		t.Fatalf("page 1 = %s next %q %v", keys(p1), next, err)
	}
	p2, next, err := s.LiveFactsPage(ctx, "pg", "/", next, 2, false)
	if err != nil || keys(p2) != "/c" || next != "" {
		t.Fatalf("page 2 = %s next %q %v", keys(p2), next, err)
	}
	all, _, err := s.LiveFactsPage(ctx, "pg", "/", "", 10, true)
	if err != nil || keys(all) != "/a,/agent-sessions/x,/agent-sessions/y,/b,/c" {
		t.Fatalf("with sessions = %s %v", keys(all), err)
	}
	counts, live, err := s.LivePrefixCounts(ctx, "pg")
	if err != nil || live != 5 || counts[0].Prefix != "/agent-sessions/" || counts[0].Keys != 2 {
		t.Fatalf("prefix counts = %+v live %d %v", counts, live, err)
	}
}
