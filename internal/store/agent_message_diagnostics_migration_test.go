package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// exerciseMessageDiagnosticsMigration proves 0026 on both engines: one
// latest row per (namespace, agent), nonnegative count checks, empty-string
// optional defaults, and a down that drops only snapshots while members
// and messages survive. Newer migrations may sit above it.
func exerciseMessageDiagnosticsMigration(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	for {
		st, err := d.MigrateStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tip := MigrationStatus{}
		for _, m := range st {
			if m.Applied {
				tip = m
			}
		}
		if tip.Version == 26 {
			if tip.Name != "agent_message_diagnostics" {
				t.Fatalf("0026 is %s", tip.Name)
			}
			break
		}
		if tip.Version < 26 {
			t.Fatalf("0026 not applied (tip %04d_%s)", tip.Version, tip.Name)
		}
		if _, err := d.MigrateDown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) error {
		_, err := d.ExecContext(ctx, d.Rebind(q), args...)
		return err
	}
	if err := exec(`INSERT INTO region_members (namespace, agent, role, joined_at) VALUES ('ns', 'a', '', '2026-09-28')`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO agent_messages (id, namespace, sender, recipient, body, created_at) VALUES ('m1', 'ns', 'a', 'a', 'b', '2026-09-28')`); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO agent_message_diagnostics (namespace, agent, client, delivery_mode, state, pending_ack_count, updated_at) VALUES ($1, $2, 'opencode', 'idle_wake', 'ready', $3, '2026-09-28')`
	if err := exec(ins, "ns", "a", 0); err != nil {
		t.Fatal(err)
	}
	if err := exec(ins, "ns", "a", 1); err == nil {
		t.Fatal("second snapshot for one member allowed; want primary key violation")
	}
	if err := exec(ins, "ns2", "a", 1); err != nil {
		t.Fatalf("same address in another namespace: %v", err)
	}
	if err := exec(ins, "ns3", "a", -1); err == nil {
		t.Fatal("negative count allowed; want check violation")
	}
	var last, next, reason string
	var wakes int
	if err := d.QueryRowContext(ctx, `SELECT last_attempt_at, next_attempt_at, last_error, wake_count FROM agent_message_diagnostics WHERE namespace = 'ns'`).
		Scan(&last, &next, &reason, &wakes); err != nil || last != "" || next != "" || reason != "" || wakes != 0 {
		t.Fatalf("defaults = %q %q %q %d err=%v", last, next, reason, wakes, err)
	}

	if _, err := d.MigrateDown(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM agent_message_diagnostics`).Scan(&n); err == nil {
		t.Fatal("agent_message_diagnostics survived down")
	}
	for _, table := range []string{"region_members", "agent_messages"} {
		if err := d.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("down touched %s: %d %v", table, n, err)
		}
	}
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM agent_message_diagnostics`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("re-up = %d %v, want empty table", n, err)
	}
}

func TestMessageDiagnosticsMigrationSQLite(t *testing.T) {
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "diag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exerciseMessageDiagnosticsMigration(t, d)
}

func TestMessageDiagnosticsMigrationPostgres(t *testing.T) {
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
	exerciseMessageDiagnosticsMigration(t, d)
}
