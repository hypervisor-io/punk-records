package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exerciseAgentInboxBindingSetter proves migration 0028 up/down on an
// already migrated store: the binding table gains set_by (who set the
// binding; ” = unknown, the default that keeps pre-0028 rows and
// setter-less call sites honest), one down from 0028 drops exactly that
// column and leaves the 0027 table - bindings included - and every
// other table intact, and re-up restores the column with every
// surviving row on the unknown default. Newer migrations may sit above
// it; step down to 0028 first.
func exerciseAgentInboxBindingSetter(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()

	for {
		st, err := d.MigrateStatus(ctx)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		tip := MigrationStatus{}
		for _, m := range st {
			if m.Applied {
				tip = m
			}
		}
		if tip.Version == 28 {
			if tip.Name != "agent_inbox_binding_setter" {
				t.Fatalf("0028 is %s, want agent_inbox_binding_setter", tip.Name)
			}
			break
		}
		if tip.Version < 28 {
			t.Fatalf("0028_agent_inbox_binding_setter not applied (applied tip %04d_%s)", tip.Version, tip.Name)
		}
		if _, err := d.MigrateDown(ctx); err != nil {
			t.Fatalf("step down past %04d_%s: %v", tip.Version, tip.Name, err)
		}
	}

	// A setter-named bind stores who and when.
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO agent_inbox_bindings (agent, namespace, set_by, set_at) VALUES ($1, $2, $3, $4)`),
		"claude-code:s1", "punk-pbs", "codex:operator", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert binding with setter: %v", err)
	}
	// The pre-0028 write shape (no set_by in the column list) still
	// works and lands on the documented '' = unknown default.
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO agent_inbox_bindings (agent, namespace, set_at) VALUES ($1, $2, $3)`),
		"opencode:s2", "repo-main", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert binding without setter: %v", err)
	}
	var setBy string
	if err := d.QueryRowContext(ctx, d.Rebind(
		`SELECT set_by FROM agent_inbox_bindings WHERE agent = $1`), "opencode:s2").Scan(&setBy); err != nil {
		t.Fatalf("read defaulted set_by: %v", err)
	}
	if setBy != "" {
		t.Fatalf("defaulted set_by = %q, want the '' = unknown default", setBy)
	}

	// Baseline row in an unrelated table proves down 0028 drops exactly
	// the setter column, never a table.
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO api_keys (name, token_hash, subject, created_at) VALUES ($1, $2, $3, $4)`),
		"baseline", "hash-baseline", "alice", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert baseline key: %v", err)
	}

	if _, err := d.MigrateDown(ctx); err != nil {
		t.Fatalf("down 0028: %v", err)
	}
	// The column is gone: naming it fails...
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO agent_inbox_bindings (agent, namespace, set_by, set_at) VALUES ($1, $2, $3, $4)`),
		"claude-code:s3", "punk-pbs", "codex:operator", TimeToDB(time.Now())); err == nil {
		t.Fatal("set_by insert worked after down 0028; want a no-such-column error")
	}
	// ...while the 0027 table itself survives with its original columns.
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO agent_inbox_bindings (agent, namespace, set_at) VALUES ($1, $2, $3)`),
		"claude-code:s3", "punk-pbs", TimeToDB(time.Now())); err != nil {
		t.Fatalf("binding table lost its 0027 shape after down 0028: %v", err)
	}
	var keys int
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM api_keys WHERE name = 'baseline'`).Scan(&keys); err != nil || keys != 1 {
		t.Fatalf("baseline key after down 0028: %d rows err=%v, want 1", keys, err)
	}

	// Re-up restores the column. A column rollback loses the setter
	// information, never a binding: all three rows survive the round
	// trip and report the '' = unknown default.
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatalf("re-up 0028: %v", err)
	}
	var rows int
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_inbox_bindings WHERE set_by = ''`).Scan(&rows); err != nil {
		t.Fatalf("count after re-up: %v", err)
	}
	if rows != 3 {
		t.Fatalf("re-up 0028: %d rows on the unknown default, want the 3 surviving bindings", rows)
	}
}

func TestAgentInboxBindingSetterMigrationSQLite(t *testing.T) {
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "setter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.MigrateUp(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	exerciseAgentInboxBindingSetter(t, d)
}

// Postgres coverage follows the repo pattern: runs only with
// PUNK_TEST_PG_DSN set; otherwise skipped, not passed.
func TestAgentInboxBindingSetterMigrationPostgres(t *testing.T) {
	dsn := os.Getenv("PUNK_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("PUNK_TEST_PG_DSN not set; skipping postgres migration test")
	}
	d, err := Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for {
		n, err := d.MigrateDown(context.Background())
		if err != nil {
			t.Fatalf("pg pre-clean: %v", err)
		}
		if n == 0 {
			break
		}
	}
	if _, err := d.MigrateUp(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	exerciseAgentInboxBindingSetter(t, d)
}
