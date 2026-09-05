package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exerciseNamespaceGrants proves migration 0022 up/down on an already
// migrated store: the grants table works at the tip, one down reverts
// exactly 0022, and re-up restores it.
func exerciseNamespaceGrants(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()

	st, err := d.MigrateStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if tip := st[len(st)-1]; tip.Version != 22 || tip.Name != "namespace_grants" {
		t.Fatalf("migration tip = %04d_%s, want 0022_namespace_grants", tip.Version, tip.Name)
	}

	// table is usable: grant, read back, soft-revoke shape
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO namespace_grants (subject, namespace, op, created_at) VALUES ($1, $2, $3, $4)`),
		"alice", "ns-a", "read", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	var n int
	if err := d.QueryRowContext(ctx, d.Rebind(
		`SELECT count(*) FROM namespace_grants WHERE subject = $1 AND revoked_at IS NULL`),
		"alice").Scan(&n); err != nil {
		t.Fatalf("select grant: %v", err)
	}
	if n != 1 {
		t.Fatalf("active grants = %d, want 1", n)
	}
	// duplicate active grant violates the partial unique index
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO namespace_grants (subject, namespace, op, created_at) VALUES ($1, $2, $3, $4)`),
		"alice", "ns-a", "read", TimeToDB(time.Now())); err == nil {
		t.Fatal("duplicate active grant allowed; want unique violation")
	}

	// baseline data (pre-0022 tables) must survive the down/up cycle
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO api_keys (name, token_hash, subject, created_at) VALUES ($1, $2, $3, $4)`),
		"baseline", "hash-baseline", "alice", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert baseline key: %v", err)
	}
	assertBaselineKey := func(stage string) {
		t.Helper()
		var keys int
		if err := d.QueryRowContext(ctx,
			`SELECT count(*) FROM api_keys WHERE name = 'baseline'`).Scan(&keys); err != nil {
			t.Fatalf("baseline key check %s: %v", stage, err)
		}
		if keys != 1 {
			t.Fatalf("baseline key %s: %d rows, want 1 (down/up of 0022 must not touch baseline data)", stage, keys)
		}
	}

	if _, err := d.MigrateDown(ctx); err != nil {
		t.Fatalf("down 0022: %v", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM namespace_grants`).Scan(&n); err == nil {
		t.Fatal("namespace_grants still exists after down 0022")
	}
	assertBaselineKey("after down")
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatalf("re-up 0022: %v", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM namespace_grants`).Scan(&n); err != nil {
		t.Fatalf("namespace_grants missing after re-up: %v", err)
	}
	assertBaselineKey("after re-up")
}

func TestNamespaceGrantsMigrationSQLite(t *testing.T) {
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "authz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.MigrateUp(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	exerciseNamespaceGrants(t, d)
}

// Postgres coverage follows the repo pattern: it runs only when CI (or a
// developer) points at a database via PUNK_TEST_PG_DSN, same gate as
// openTest. Without it this test is skipped, not passed.
func TestNamespaceGrantsMigrationPostgres(t *testing.T) {
	dsn := os.Getenv("PUNK_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("PUNK_TEST_PG_DSN not set; skipping postgres migration test")
	}
	d, err := Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	// fresh schema per run, same as openTest
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
	exerciseNamespaceGrants(t, d)
}
