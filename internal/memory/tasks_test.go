package memory

import (
	"context"
	"testing"
)

func TestTaskCounts(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()

	got, err := s.TaskCounts(ctx, "missing")
	if err != nil || got != (TaskCounts{}) {
		t.Fatalf("missing ns: %+v %v", got, err)
	}

	write := func(key, body string) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	write("/tasks/T1", "task one")
	write("/tasks/T2", "task two")
	write("/tasks/T3", "task three")
	write("/tasks/T4", "task four")
	write("/tasks/T1/status", "done: abc123 shipped")
	write("/tasks/T2/status", "blocked: waiting on answer")
	write("/tasks/T3/status", "in progress, not a terminal status")
	write("/tasks/T9/status", "done: status without a task fact")
	write("/plan/overview", "not a task")
	write("/tasks/T4/notes/extra", "deeper key is neither task nor status")

	got, err = s.TaskCounts(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	want := TaskCounts{Total: 4, Done: 2, Blocked: 1, Pending: 1}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}

	// A tombstoned task drops out of total; its status flips to done.
	if err := s.Forget(ctx, "ns", "/tasks/T4", "tester"); err != nil {
		t.Fatal(err)
	}
	write("/tasks/T3/status", "done: now finished")
	got, err = s.TaskCounts(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	want = TaskCounts{Total: 3, Done: 3, Blocked: 1, Pending: 0}
	if got != want {
		t.Fatalf("after forget: got %+v want %+v", got, want)
	}
}

func TestListTasksRows(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	ns := "board"
	write := func(key, body string, attrs map[string]any) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: ns, Key: key, Body: body, Attributes: attrs, Writer: "planner"}); err != nil {
			t.Fatal(err)
		}
	}
	write("/tasks/A", "Add the board\nfiles: x.go", nil)
	write("/tasks/B", "Wire the tool\ndepends_on: A", nil)
	write("/tasks/C", "Docs", map[string]any{"depends_on": []any{"A", "B"}})
	write("/tasks/A/status", "done: abc123 board added; tests: go test ./internal/memory/", map[string]any{"state": "done", "sha": "abc123"})
	write("/tasks/B/status", "in_progress: red writing the failing test", nil)
	write("/tasks/Z/status", "blocked: no task fact", nil)

	rows, err := s.ListTasks(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]TaskRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (A, B, C, orphan Z)", len(rows))
	}
	if a := byID["A"]; a.Title != "Add the board" || a.State != "done" || a.Status != "done: abc123 board added; tests: go test ./internal/memory/" || a.By != "planner" {
		t.Fatalf("A = %+v", a)
	}
	if b := byID["B"]; b.State != "in_progress" || len(b.DependsOn) != 1 || b.DependsOn[0] != "A" {
		t.Fatalf("B = %+v", b)
	}
	if c := byID["C"]; c.State != "pending" || len(c.DependsOn) != 2 || c.Status != "" {
		t.Fatalf("C = %+v", c)
	}
	if z := byID["Z"]; z.State != "blocked" || z.Title != "" {
		t.Fatalf("Z = %+v", z)
	}
	if rows[0].ID != "A" || rows[3].ID != "Z" {
		t.Fatalf("rows must be sorted by id: %v %v", rows[0].ID, rows[3].ID)
	}
}

func TestParseTaskState(t *testing.T) {
	cases := []struct {
		body  string
		attrs map[string]any
		want  string
	}{
		{"done: x", nil, "done"},
		{"Done: x", nil, "done"},
		{"blocked: y", nil, "blocked"},
		{"review: please gate", nil, "review"},
		{"in_progress: green", nil, "in_progress"},
		{"working on it", map[string]any{"state": "in_progress"}, "in_progress"},
		{"", nil, "pending"},
		{"some prose", nil, "pending"},
	}
	for _, c := range cases {
		if got := ParseTaskState(c.body, c.attrs); got != c.want {
			t.Errorf("ParseTaskState(%q, %v) = %q, want %q", c.body, c.attrs, got, c.want)
		}
	}
}
