package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exercisePipelineRuns proves migration 0023 up/down on an already
// migrated store: the runs table enforces the idempotent work key, one
// down from 0023 reverts exactly it, and re-up restores it.
func exercisePipelineRuns(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()

	// newer migrations (0024+) may sit above it; step down to 0023 first
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
		if tip.Version == 23 {
			if tip.Name != "pipeline_runs" {
				t.Fatalf("0023 is %s, want pipeline_runs", tip.Name)
			}
			break
		}
		if tip.Version < 23 {
			t.Fatalf("0023_pipeline_runs not applied (applied tip %04d_%s)", tip.Version, tip.Name)
		}
		if _, err := d.MigrateDown(ctx); err != nil {
			t.Fatalf("step down past %04d_%s: %v", tip.Version, tip.Name, err)
		}
	}

	// table is usable: record a run, read it back
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO memory_pipeline_runs
			(namespace, stage, stage_version, source_key, source_revision, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`),
		"ns", "embed_link", 1, "/a", "rev-1", "pending", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	var n int
	if err := d.QueryRowContext(ctx, d.Rebind(
		`SELECT count(*) FROM memory_pipeline_runs WHERE namespace = $1 AND status = $2`),
		"ns", "pending").Scan(&n); err != nil {
		t.Fatalf("select run: %v", err)
	}
	if n != 1 {
		t.Fatalf("pending runs = %d, want 1", n)
	}
	// the work key is unique: a redelivery of the same
	// (stage, version, key, revision) unit of work must conflict
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO memory_pipeline_runs
			(namespace, stage, stage_version, source_key, source_revision, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`),
		"ns", "embed_link", 1, "/a", "rev-1", "pending", TimeToDB(time.Now())); err == nil {
		t.Fatal("duplicate work key allowed; want unique violation")
	}
	// a changed source revision is a new row
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO memory_pipeline_runs
			(namespace, stage, stage_version, source_key, source_revision, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`),
		"ns", "embed_link", 1, "/a", "rev-2", "pending", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert second revision run: %v", err)
	}

	// baseline data (pre-0023 tables) must survive the down/up cycle
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
			t.Fatalf("baseline key %s: %d rows, want 1 (down/up of 0023 must not touch baseline data)", stage, keys)
		}
	}

	if _, err := d.MigrateDown(ctx); err != nil {
		t.Fatalf("down 0023: %v", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_pipeline_runs`).Scan(&n); err == nil {
		t.Fatal("memory_pipeline_runs still exists after down 0023")
	}
	assertBaselineKey("after down")
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatalf("re-up 0023: %v", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_pipeline_runs`).Scan(&n); err != nil {
		t.Fatalf("memory_pipeline_runs missing after re-up: %v", err)
	}
	assertBaselineKey("after re-up")
}

func TestPipelineRunsMigrationSQLite(t *testing.T) {
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.MigrateUp(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	exercisePipelineRuns(t, d)
}

// Postgres coverage follows the repo pattern: it runs only when CI (or a
// developer) points at a database via PUNK_TEST_PG_DSN, same gate as
// openTest. Without it this test is skipped, not passed.
func TestPipelineRunsMigrationPostgres(t *testing.T) {
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
	exercisePipelineRuns(t, d)
}
