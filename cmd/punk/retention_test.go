package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func TestMessageRetentionMaintenanceEntrypoint(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	reg := region.New(db, func() time.Time { return now })
	mem := memory.New(db, func() time.Time { return now })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var keep []string
	for _, ns := range []string{"decoy", "target"} {
		for _, a := range []string{"a", "b"} {
			if err := reg.Register(ctx, ns, a, ""); err != nil {
				t.Fatal(err)
			}
		}
		for _, kind := range []string{"old-acked", "unread", "recent-acked", "boundary"} {
			m, err := reg.SendMessage(ctx, region.MessageInput{Namespace: ns, Sender: "a", Recipient: "b", Body: kind})
			if err != nil {
				t.Fatal(err)
			}
			var ack any
			switch kind {
			case "old-acked":
				ack = store.TimeToDB(now.Add(-31 * 24 * time.Hour))
			case "recent-acked":
				ack = store.TimeToDB(now)
			case "boundary":
				ack = store.TimeToDB(now.Add(-30 * 24 * time.Hour))
			}
			if _, err := db.ExecContext(ctx, db.Rebind(`UPDATE agent_messages SET created_at=$1,acked_at=$2 WHERE id=$3`), store.TimeToDB(now.Add(-90*24*time.Hour)), ack, m.ID); err != nil {
				t.Fatal(err)
			}
			if kind != "old-acked" {
				keep = append(keep, m.ID)
			}
		}
	}
	// Scoped sweep must not delete the older decoy namespace's ACKed row.
	if n, err := reg.SweepMessageRetention(ctx, "target"); err != nil || n != 1 {
		t.Fatalf("scoped = %d %v", n, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages WHERE namespace='decoy'`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("decoy changed = %d %v", count, err)
	}
	// Actual cmdServe maintenance tick body, with memory retention OFF.
	runRetentionSweeps(ctx, log, mem, reg, 0)
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("maintenance = %d %v", count, err)
	}
	for _, id := range keep {
		if err := db.QueryRowContext(ctx, db.Rebind(`SELECT count(*) FROM agent_messages WHERE id=$1`), id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("preserved %s = %d %v", id, count, err)
		}
	}
	reg.MessageRetention = 0
	now = now.Add(100 * 24 * time.Hour)
	runRetentionSweeps(ctx, log, mem, reg, 0)
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("disabled = %d %v", count, err)
	}
}
