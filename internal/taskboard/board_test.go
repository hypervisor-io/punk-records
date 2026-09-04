package taskboard

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func newTest(t *testing.T) (*memory.Store, *region.Store) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	return memory.New(db, now), region.New(db, now)
}

func TestBuildJoinsClaimsAndReadiness(t *testing.T) {
	mem, reg := newTest(t)
	ctx := context.Background()
	ns := "board"
	write := func(key, body string) {
		t.Helper()
		if _, err := mem.Write(ctx, memory.WriteInput{Namespace: ns, Key: key, Body: body, Writer: "planner"}); err != nil {
			t.Fatal(err)
		}
	}
	write("/tasks/A", "first")
	write("/tasks/B", "second\ndepends_on: A")
	write("/tasks/C", "third\ndepends_on: A, B")
	if err := reg.Register(ctx, ns, "worker-1", "worker"); err != nil {
		t.Fatal(err)
	}

	b, err := Build(ctx, mem, reg, ns)
	if err != nil {
		t.Fatal(err)
	}
	if b.Next != "A" || !b.Tasks[0].Ready || b.Tasks[1].Ready || b.Counts.Pending != 3 || len(b.Members) != 1 {
		t.Fatalf("fresh board = %+v", b)
	}

	if _, err := reg.ClaimWork(ctx, ns, "/tasks/A", "worker-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	b, _ = Build(ctx, mem, reg, ns)
	if b.Tasks[0].Holder != "worker-1" || b.Tasks[0].LeaseExpiresAt == "" || b.Tasks[0].Ready || b.Next != "" {
		t.Fatalf("claimed board = %+v", b.Tasks[0])
	}

	write("/tasks/A/status", "done: abc first landed; tests: go test ./...")
	if err := reg.ReleaseWork(ctx, ns, "/tasks/A", "worker-1"); err != nil {
		t.Fatal(err)
	}
	b, _ = Build(ctx, mem, reg, ns)
	if b.Tasks[0].State != "done" || !b.Tasks[1].Ready || b.Tasks[2].Ready || b.Next != "B" || b.Counts.Done != 1 || b.Counts.Pending != 2 {
		t.Fatalf("after done = %+v", b)
	}

	b, _ = Build(ctx, mem, nil, ns)
	if len(b.Members) != 0 || b.Next != "B" {
		t.Fatalf("nil region must degrade: %+v", b)
	}
}

func TestWaitForChangeReturnsOnTaskEvent(t *testing.T) {
	b := bus.New()
	done := make(chan []string, 1)
	go func() { done <- WaitForChange(context.Background(), b, "ns", 5*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	b.Publish(bus.Event{Kind: "memory", Key: "other:/tasks/T1/status"})
	b.Publish(bus.Event{Kind: "memory", Key: "ns:/notes/x"})
	b.Publish(bus.Event{Kind: "claim", Key: "ns:/tasks/T1", Data: map[string]string{"action": "claimed"}})
	select {
	case keys := <-done:
		if len(keys) != 1 || keys[0] != "/tasks/T1" {
			t.Fatalf("keys = %v", keys)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return on a claim event")
	}
	if keys := WaitForChange(context.Background(), b, "ns", 30*time.Millisecond); keys != nil {
		t.Fatalf("timeout must return nil, got %v", keys)
	}
	if keys := WaitForChange(context.Background(), nil, "ns", time.Second); keys != nil {
		t.Fatalf("nil bus must return nil at once, got %v", keys)
	}
}
