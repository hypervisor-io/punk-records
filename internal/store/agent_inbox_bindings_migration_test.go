package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exerciseAgentInboxBindings proves migration 0027 up/down on an already
// migrated store: the binding table exists and enforces one row per
// agent address (agent is the primary key), one down from 0027 drops
// exactly that table, and re-up restores it empty. Newer migrations may
// sit above it; step down to 0027 first.
func exerciseAgentInboxBindings(t *testing.T, d *DB) {
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
		if tip.Version == 27 {
			if tip.Name != "agent_inbox_bindings" {
				t.Fatalf("0027 is %s, want agent_inbox_bindings", tip.Name)
			}
			break
		}
		if tip.Version < 27 {
			t.Fatalf("0027_agent_inbox_bindings not applied (applied tip %04d_%s)", tip.Version, tip.Name)
		}
		if _, err := d.MigrateDown(ctx); err != nil {
			t.Fatalf("step down past %04d_%s: %v", tip.Version, tip.Name, err)
		}
	}

	insert := func(agent, ns string) error {
		_, err := d.ExecContext(ctx, d.Rebind(
			`INSERT INTO agent_inbox_bindings (agent, namespace, set_at) VALUES ($1, $2, $3)`),
			agent, ns, TimeToDB(time.Now()))
		return err
	}
	if err := insert("claude-code:s1", "punk-pbs"); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
	// One binding per agent address: the primary key rejects a second row.
	if err := insert("claude-code:s1", "other"); err == nil {
		t.Fatal("duplicate binding agent allowed; want primary key violation")
	}
	// Different agents bind independently.
	if err := insert("opencode:s2", "repo-main"); err != nil {
		t.Fatalf("insert second agent binding: %v", err)
	}

	// Baseline row in an unrelated table proves down 0027 drops exactly
	// the binding table.
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO api_keys (name, token_hash, subject, created_at) VALUES ($1, $2, $3, $4)`),
		"baseline", "hash-baseline", "alice", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert baseline key: %v", err)
	}
	assertBaselineKey := func(stage string) {
		t.Helper()
		var keys int
		if err := d.QueryRowContext(ctx,
			`SELECT count(*) FROM api_keys WHERE name = 'baseline'`).Scan(&keys); err != nil || keys != 1 {
			t.Fatalf("baseline key %s: %d rows err=%v, want 1", stage, keys, err)
		}
	}

	if _, err := d.MigrateDown(ctx); err != nil {
		t.Fatalf("down 0027: %v", err)
	}
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM agent_inbox_bindings`).Scan(&n); err == nil {
		t.Fatal("agent_inbox_bindings still exists after down 0027")
	}
	assertBaselineKey("after down")
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatalf("re-up 0027: %v", err)
	}
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM agent_inbox_bindings`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("agent_inbox_bindings after re-up = %d err=%v, want empty table", n, err)
	}
	assertBaselineKey("after re-up")
}

func TestAgentInboxBindingsMigrationSQLite(t *testing.T) {
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.MigrateUp(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	exerciseAgentInboxBindings(t, d)
}

// Postgres coverage follows the repo pattern: runs only with
// PUNK_TEST_PG_DSN set; otherwise skipped, not passed.
func TestAgentInboxBindingsMigrationPostgres(t *testing.T) {
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
	exerciseAgentInboxBindings(t, d)
}
