package policy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

func testStore(t *testing.T) (*Proposals, *task.Ledger) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "prop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	return NewProposals(db, now), task.NewLedger(db, now)
}

func TestProposalLifecycle(t *testing.T) {
	props, ledger := testStore(t)
	ctx := context.Background()
	tk, _, err := ledger.Submit(ctx, task.SubmitInput{Source: "acme"})
	if err != nil {
		t.Fatal(err)
	}

	pr, err := props.Create(ctx, tk.ID, "mutate", "runbooks__execute_restart", `{"service":"api"}`)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Status != "proposed" {
		t.Fatalf("status = %s", pr.Status)
	}

	upd, err := props.UpdateStatus(ctx, pr.ID, "approved", "susanoo:execution:55")
	if err != nil {
		t.Fatal(err)
	}
	if upd.Status != "approved" || upd.ExternalRef != "susanoo:execution:55" {
		t.Fatalf("upd = %+v", upd)
	}

	if _, err := props.UpdateStatus(ctx, pr.ID, "vaporized", ""); err == nil {
		t.Fatal("bad status accepted")
	}
	if _, err := props.UpdateStatus(ctx, "ghost", "approved", ""); err == nil {
		t.Fatal("missing proposal accepted")
	}

	list, err := props.ListByTask(ctx, tk.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v err=%v", list, err)
	}
}

func TestExpireStale(t *testing.T) {
	props, ledger := testStore(t)
	ctx := context.Background()
	tk, _, err := ledger.Submit(ctx, task.SubmitInput{Source: "s"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := props.Create(ctx, tk.ID, "mutate", "x", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	// age the clock far past the cutoff, then create a fresh one
	props.now = func() time.Time { return time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC) }
	fresh, err := props.Create(ctx, tk.ID, "mutate", "y", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	n, err := props.ExpireStale(ctx, 72*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("expired %d err=%v, want 1", n, err)
	}
	o, _ := props.Get(ctx, old.ID)
	f, _ := props.Get(ctx, fresh.ID)
	if o.Status != "expired" || f.Status != "proposed" {
		t.Fatalf("old=%s fresh=%s", o.Status, f.Status)
	}
}
