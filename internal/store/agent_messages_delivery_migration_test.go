package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func exerciseMessageDeliveryMigration(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := d.MigrateStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last := st[len(st)-1]; last.Version != 25 || last.Name != "agent_messages_delivery" || !last.Applied {
		t.Fatalf("missing migration 0025: %+v", last)
	}
	for _, id := range []string{"decoy", "target"} {
		if _, err := d.ExecContext(ctx, d.Rebind(`INSERT INTO agent_messages (id, namespace, sender, recipient, body, created_at, leased_by, leased_until) VALUES ($1,$2,'a','b','body','2026-09-01','owner','2026-09-02')`), id, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.MigrateDown(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("down destroyed messages: %d %v", n, err)
	}
	if err := d.QueryRowContext(ctx, `SELECT leased_by FROM agent_messages LIMIT 1`).Scan(new(string)); err == nil {
		t.Fatal("lease column survived down")
	}
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages WHERE leased_by IS NULL AND leased_until IS NULL`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("re-up lost rows or null defaults: %d %v", n, err)
	}
}

func TestMessageDeliveryMigrationSQLite(t *testing.T) {
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "delivery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exerciseMessageDeliveryMigration(t, d)
}

func TestMessageDeliveryMigrationPostgres(t *testing.T) {
	dsn := os.Getenv("PUNK_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("PUNK_TEST_PG_DSN not set")
	}
	d, err := Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for {
		n, err := d.MigrateDown(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	exerciseMessageDeliveryMigration(t, d)
}
