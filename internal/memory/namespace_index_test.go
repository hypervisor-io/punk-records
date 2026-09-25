package memory

import (
	"context"
	"testing"
)

func TestNamespaceSummaries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, w := range []struct{ ns, key, body string }{
		{"board", "/tasks/A", "first task"},
		{"board", "/tasks/A/status", "done: landed"},
		{"board", "/tasks/B", "second task"},
		{"board", "/decisions/x", "we chose sqlite"},
		{"notes", "/notes/one", "a note"},
		{"notes", "/notes/two", "another"},
	} {
		if _, err := s.Remember(ctx, w.ns, w.key, w.body, nil, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	// A tombstoned key drops out of both counts.
	if err := s.Forget(ctx, "notes", "/notes/two", "tester"); err != nil {
		t.Fatal(err)
	}

	sums, err := s.NamespaceSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]NamespaceSummary{}
	for _, sum := range sums {
		by[sum.Name] = sum
	}
	board, ok := by["board"]
	if !ok {
		t.Fatalf("board missing from %v", sums)
	}
	// /tasks/A, /tasks/A/status, /tasks/B, /decisions/x are live; only
	// /tasks/A and /tasks/B are task rows.
	if board.Facts != 4 || board.Tasks != 2 {
		t.Fatalf("board = %d facts, %d tasks; want 4, 2", board.Facts, board.Tasks)
	}
	if board.LastWrite.IsZero() {
		t.Fatal("board last_write is zero")
	}
	notes := by["notes"]
	if notes.Facts != 1 || notes.Tasks != 0 {
		t.Fatalf("notes = %d facts, %d tasks; want 1, 0", notes.Facts, notes.Tasks)
	}
	if len(sums) != 2 || sums[0].Name != "board" || sums[1].Name != "notes" {
		t.Fatalf("summaries not name-ordered: %v", sums)
	}
}
