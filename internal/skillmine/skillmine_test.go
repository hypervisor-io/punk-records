package skillmine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// fakeClock mirrors internal/task/ledger_test.go's fakeClock: List orders
// by created_at, so tests need strictly increasing, deterministic
// timestamps rather than relying on time.Now()'s real-clock granularity.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

func newTestLedger(t *testing.T) (*task.Ledger, *store.DB, *fakeClock) {
	t.Helper()
	db, err := store.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)}
	return task.NewLedger(db, clk.Now), db, clk
}

// mkAgentVersion inserts a row so Claim/AssignAgent's agent_version_id
// foreign key is satisfiable (tasks.agent_version_id REFERENCES
// agent_versions(id); no row, no valid non-null version id - see
// internal/task/ledger_test.go's identical pattern).
func mkAgentVersion(t *testing.T, db *store.DB, clk *fakeClock, agent string) int64 {
	t.Helper()
	var verID int64
	err := db.QueryRowContext(context.Background(), db.Rebind(`
		INSERT INTO agent_versions (name, version, content_hash, loaded_at, active)
		VALUES ($1, '0.1.0', 'testhash', $2, TRUE) RETURNING id`),
		agent, store.TimeToDB(clk.Now())).Scan(&verID)
	if err != nil {
		t.Fatal(err)
	}
	return verID
}

func mkTask(t *testing.T, l *task.Ledger, db *store.DB, clk *fakeClock, agent, kind string, tools []string, summary string) string {
	t.Helper()
	ctx := context.Background()
	verID := mkAgentVersion(t, db, clk, agent)
	tk, _, err := l.Submit(ctx, task.SubmitInput{Source: "test", Kind: kind, Actor: "t",
		Budget: task.Budget{Tokens: 1000, ToolCalls: 10, WallMS: 60000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.AssignAgent(ctx, tk.ID, agent, verID, "t", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Claim(ctx, tk.ID, agent, verID, 60_000_000_000); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if _, err := l.Append(ctx, tk.ID, task.EventToolCall, "agent:"+agent, map[string]any{"tool": tool, "status": "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Append(ctx, tk.ID, task.EventFinding, "agent:"+agent,
		map[string]any{"finding": map[string]any{"summary": summary, "confidence": "high"}}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetStatus(ctx, tk.ID, task.StatusCompleted, "agent:"+agent, "done"); err != nil {
		t.Fatal(err)
	}
	return tk.ID
}

func TestMineGroupsByTrajectory(t *testing.T) {
	l, db, clk := newTestLedger(t)
	traj := []string{"get_logs", "query_metrics"}
	id1 := mkTask(t, l, db, clk, "db", "investigate", traj, "s1")
	id2 := mkTask(t, l, db, clk, "db", "investigate", []string{"query_metrics", "get_logs"}, "s2") // same multiset, different order
	id3 := mkTask(t, l, db, clk, "db", "investigate", traj, "s3")
	idOther := mkTask(t, l, db, clk, "db", "investigate", []string{"get_traces"}, "other")
	idSubtask := mkTask(t, l, db, clk, "db", "subtask", traj, "ignored kind")

	groups, err := Mine(context.Background(), l, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(groups), groups)
	}
	g := groups[0]
	if g.Agent != "db" || len(g.TaskIDs) != 3 || len(g.Summaries) != 3 {
		t.Fatalf("group: %+v", g)
	}

	// Discriminating: List orders newest-first (ORDER BY created_at DESC),
	// and Mine preserves that iteration order when building group members.
	// The fake clock makes creation order strictly increasing, so the
	// surviving group's members come back in reverse creation order:
	// id3, id2, id1. Pin both the ID order and the Summaries order to it -
	// a plain set-equality check would pass under either List order and
	// miss a regression if List's ORDER BY were ever dropped or reversed.
	wantIDs := []string{id3, id2, id1}
	for i, want := range wantIDs {
		if g.TaskIDs[i] != want {
			t.Fatalf("TaskIDs[%d] = %s, want %s (full: %v)", i, g.TaskIDs[i], want, g.TaskIDs)
		}
	}
	wantSummaries := []string{"s3", "s2", "s1"}
	for i, want := range wantSummaries {
		if g.Summaries[i] != want {
			t.Fatalf("Summaries[%d] = %s, want %s (full: %v)", i, g.Summaries[i], want, g.Summaries)
		}
	}
	if len(g.Trajectory) != 2 {
		t.Fatalf("Trajectory = %v, want len 2", g.Trajectory)
	}

	// The different-trajectory task and the non-investigate-kind task must
	// not appear anywhere in the surviving group's membership.
	for _, id := range g.TaskIDs {
		if id == idOther || id == idSubtask {
			t.Fatalf("group contains excluded task %s: %+v", id, g)
		}
	}

	// agent filter excludes everything
	groups, err = Mine(context.Background(), l, "net", 3)
	if err != nil || len(groups) != 0 {
		t.Fatal(groups, err)
	}
}

func TestMineMinCountDropsSmallGroups(t *testing.T) {
	l, db, clk := newTestLedger(t)
	traj := []string{"get_logs"}
	mkTask(t, l, db, clk, "db", "investigate", traj, "s1")
	mkTask(t, l, db, clk, "db", "investigate", traj, "s2")

	groups, err := Mine(context.Background(), l, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %+v, want none (only 2 members, minCount 3)", groups)
	}

	// minCount <= 0 defaults to 3, same as an explicit 3.
	groups, err = Mine(context.Background(), l, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %+v, want none (default minCount 3)", groups)
	}
}

// TestMineGroupsByAgentDimension pins that agent is part of the group key,
// not just the trajectory: two agents each running the identical tool
// multiset three times must produce two distinct groups (one per agent),
// not one merged group of six. A grouping bug that matched on trajectory
// alone would collapse these into a single group of 6.
func TestMineGroupsByAgentDimension(t *testing.T) {
	l, db, clk := newTestLedger(t)
	traj := []string{"get_logs", "query_metrics"}
	dbIDs := []string{
		mkTask(t, l, db, clk, "db", "investigate", traj, "db1"),
		mkTask(t, l, db, clk, "db", "investigate", traj, "db2"),
		mkTask(t, l, db, clk, "db", "investigate", traj, "db3"),
	}
	netIDs := []string{
		mkTask(t, l, db, clk, "net", "investigate", traj, "net1"),
		mkTask(t, l, db, clk, "net", "investigate", traj, "net2"),
		mkTask(t, l, db, clk, "net", "investigate", traj, "net3"),
	}

	groups, err := Mine(context.Background(), l, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (one per agent): %+v", len(groups), groups)
	}
	byAgent := map[string]Group{}
	for _, g := range groups {
		byAgent[g.Agent] = g
	}
	if len(byAgent) != 2 {
		t.Fatalf("groups not keyed one-per-agent: %+v", groups)
	}
	dbGroup, ok := byAgent["db"]
	if !ok || len(dbGroup.TaskIDs) != 3 {
		t.Fatalf("db group: %+v", dbGroup)
	}
	netGroup, ok := byAgent["net"]
	if !ok || len(netGroup.TaskIDs) != 3 {
		t.Fatalf("net group: %+v", netGroup)
	}
	for _, id := range dbGroup.TaskIDs {
		for _, netID := range netIDs {
			if id == netID {
				t.Fatalf("db group contains net task %s", id)
			}
		}
	}
	for _, id := range netGroup.TaskIDs {
		for _, dbID := range dbIDs {
			if id == dbID {
				t.Fatalf("net group contains db task %s", id)
			}
		}
	}

	// Filtering by agent keeps only that agent's group.
	filtered, err := Mine(context.Background(), l, "db", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Agent != "db" {
		t.Fatalf("filtered = %+v, want only db's group", filtered)
	}
	for _, id := range filtered[0].TaskIDs {
		for _, netID := range netIDs {
			if id == netID {
				t.Fatalf("agent-filtered group leaked net task %s", id)
			}
		}
	}
}

// TestFindingSummaryLastWins pins that a task with multiple finding events
// contributes its LATEST summary to the group, not its first. A prior
// implementation could plausibly grab the first EventFinding in
// chronological (seq) order; this pins the actual "last wins" contract
// documented on findingSummary.
func TestFindingSummaryLastWins(t *testing.T) {
	l, db, clk := newTestLedger(t)
	traj := []string{"get_logs"}
	// mkTask already appends one finding ("first") + completes the task;
	// append a second, later finding directly afterwards is not possible
	// since the task is already completed, so build this task by hand
	// with two findings before completion.
	ctx := context.Background()
	verID := mkAgentVersion(t, db, clk, "db")
	tk, _, err := l.Submit(ctx, task.SubmitInput{Source: "test", Kind: "investigate", Actor: "t",
		Budget: task.Budget{Tokens: 1000, ToolCalls: 10, WallMS: 60000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.AssignAgent(ctx, tk.ID, "db", verID, "t", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Claim(ctx, tk.ID, "db", verID, 60_000_000_000); err != nil {
		t.Fatal(err)
	}
	for _, tool := range traj {
		if _, err := l.Append(ctx, tk.ID, task.EventToolCall, "agent:db", map[string]any{"tool": tool, "status": "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Append(ctx, tk.ID, task.EventFinding, "agent:db",
		map[string]any{"finding": map[string]any{"summary": "first", "confidence": "low"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ctx, tk.ID, task.EventFinding, "agent:db",
		map[string]any{"finding": map[string]any{"summary": "second-and-final", "confidence": "high"}}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetStatus(ctx, tk.ID, task.StatusCompleted, "agent:db", "done"); err != nil {
		t.Fatal(err)
	}
	// Two more members with a single finding each, so the group clears minCount.
	mkTask(t, l, db, clk, "db", "investigate", traj, "s2")
	mkTask(t, l, db, clk, "db", "investigate", traj, "s3")

	groups, err := Mine(context.Background(), l, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want 1", groups)
	}
	found := false
	for i, id := range groups[0].TaskIDs {
		if id == tk.ID {
			found = true
			if groups[0].Summaries[i] != "second-and-final" {
				t.Fatalf("Summaries[%d] = %q, want the LATER finding's summary, not the first", i, groups[0].Summaries[i])
			}
		}
	}
	if !found {
		t.Fatalf("multi-finding task %s missing from group: %+v", tk.ID, groups)
	}
}

// TestMineSkipsZeroTrajectoryTasks pins the skip guard: a completed
// investigate task with zero tool calls (empty trajectory) must never
// form or join a group, even at the loosest possible threshold
// (minCount=1, where every other task type or count would pass). Without
// the len(traj)==0 guard, three such tasks would form a degenerate
// "empty trajectory" group and Mine would draft a skill from nothing.
func TestMineSkipsZeroTrajectoryTasks(t *testing.T) {
	l, db, clk := newTestLedger(t)
	mkTask(t, l, db, clk, "db", "investigate", nil, "s1")
	mkTask(t, l, db, clk, "db", "investigate", nil, "s2")
	mkTask(t, l, db, clk, "db", "investigate", nil, "s3")

	groups, err := Mine(context.Background(), l, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %+v, want none (zero-trajectory tasks must never group)", groups)
	}
}

// TestMineSkipsEmptyAgentName pins the skip guard for tk.AgentName == "":
// Claim's agent parameter is written straight into the agent_name column
// with no non-empty validation, so a task can reach "completed" with an
// empty agent name via Claim(ctx, id, "", verID, lease) (submitted ->
// working still only requires a valid agent_version_id FK, not a named
// agent). At the loosest threshold (minCount=1) such tasks must still
// never form or join a group.
func TestMineSkipsEmptyAgentName(t *testing.T) {
	l, db, clk := newTestLedger(t)
	ctx := context.Background()
	traj := []string{"get_logs"}
	for i := 0; i < 3; i++ {
		verID := mkAgentVersion(t, db, clk, "") // version row need not carry the agent's own name
		tk, _, err := l.Submit(ctx, task.SubmitInput{Source: "test", Kind: "investigate", Actor: "t",
			Budget: task.Budget{Tokens: 1000, ToolCalls: 10, WallMS: 60000}})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Claim(ctx, tk.ID, "", verID, 60_000_000_000); err != nil {
			t.Fatal(err)
		}
		for _, tool := range traj {
			if _, err := l.Append(ctx, tk.ID, task.EventToolCall, "agent:", map[string]any{"tool": tool, "status": "ok"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := l.Append(ctx, tk.ID, task.EventFinding, "agent:",
			map[string]any{"finding": map[string]any{"summary": "s", "confidence": "high"}}); err != nil {
			t.Fatal(err)
		}
		if err := l.SetStatus(ctx, tk.ID, task.StatusCompleted, "agent:", "done"); err != nil {
			t.Fatal(err)
		}
	}

	groups, err := Mine(context.Background(), l, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %+v, want none (empty AgentName tasks must never group)", groups)
	}
}

// fakeInsightDrafter records what it saw and answers canned markdown.
type fakeInsightDrafter struct {
	sawNS    string
	sawFacts int
	body     string
}

func (f *fakeInsightDrafter) DraftInsights(_ context.Context, ns string, facts []memory.Fact) (string, error) {
	f.sawNS = ns
	f.sawFacts = len(facts)
	return f.body, nil
}

// TestWriteInsights pins the insights proposal path: mental models,
// observations, and important raw facts feed the drafter (session
// capture and entities never do), the proposal lands under
// outDir/<ns>/CLAUDE-additions.md with a provenance header, and an
// empty namespace or an empty draft writes nothing.
func TestWriteInsights(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "insights.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	mem := memory.New(db, nil)
	ctx := context.Background()
	out := t.TempDir()

	// Empty namespace: no file.
	d := &fakeInsightDrafter{body: "## Style\n- rule"}
	path, err := WriteInsights(ctx, mem, "agent-empty", d, out)
	if err != nil || path != "" {
		t.Fatalf("empty ns: %q %v", path, err)
	}

	for key, body := range map[string]string{
		"/mental-models/arch":        "hexagonal, adapters at the edge",
		"/observations/style":        "tests are table-driven",
		"/agent-sessions/s1/tool-t1": "capture noise",
		"/entities/alice":            "Alice",
	} {
		if _, err := mem.Write(ctx, memory.WriteInput{Namespace: "agent-proj", Key: key, Body: body, Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mem.Write(ctx, memory.WriteInput{Namespace: "agent-proj", Key: "/decisions/db", Body: "sqlite by default", Writer: "t", Importance: 0.8}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Write(ctx, memory.WriteInput{Namespace: "agent-proj", Key: "/notes/minor", Body: "unimportant", Writer: "t", Importance: 0.1}); err != nil {
		t.Fatal(err)
	}

	path, err = WriteInsights(ctx, mem, "agent-proj", d, out)
	if err != nil {
		t.Fatal(err)
	}
	if d.sawNS != "agent-proj" || d.sawFacts != 3 {
		t.Fatalf("drafter saw ns=%q facts=%d, want agent-proj/3 (model+observation+important raw)", d.sawNS, d.sawFacts)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.Contains(content, "## Style\n- rule") || !strings.Contains(content, "punk never edits CLAUDE.md itself") {
		t.Fatalf("proposal content: %s", content)
	}
	if filepath.Base(path) != "CLAUDE-additions.md" || !strings.Contains(path, "agent-proj") {
		t.Fatalf("path: %s", path)
	}

	// Empty draft: no file.
	d2 := &fakeInsightDrafter{body: "  \n"}
	path, err = WriteInsights(ctx, mem, "agent-proj", d2, t.TempDir())
	if err != nil || path != "" {
		t.Fatalf("empty draft: %q %v", path, err)
	}
}
