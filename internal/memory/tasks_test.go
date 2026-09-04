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
