package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openTest returns a migrated store. With PUNK_TEST_PG_DSN set it also
// runs the whole suite against Postgres (used by CI's postgres job).
func openTest(t *testing.T) *DB {
	t.Helper()
	driver, dsn := "sqlite", filepath.Join(t.TempDir(), "test.db")
	if pg := os.Getenv("PUNK_TEST_PG_DSN"); pg != "" {
		driver, dsn = "postgres", pg
	}
	d, err := Open(driver, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if driver == "postgres" {
		// fresh schema per test run
		for {
			n, err := d.MigrateDown(context.Background())
			if err != nil {
				t.Fatalf("pg pre-clean: %v", err)
			}
			if n == 0 {
				break
			}
		}
	}
	if _, err := d.MigrateUp(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return d
}

func TestMigrateUpDownStatus(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	st, err := d.MigrateStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st) == 0 {
		t.Fatal("no migrations found")
	}
	for _, m := range st {
		if !m.Applied {
			t.Errorf("migration %04d_%s not applied after up", m.Version, m.Name)
		}
	}

	// schema is usable
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO namespaces (name, created_at) VALUES ($1, $2)`),
		"test", TimeToDB(time.Now())); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}

	// down reverts one migration at a time until none remain
	downs := 0
	for {
		n, err := d.MigrateDown(ctx)
		if err != nil {
			t.Fatalf("down #%d: %v", downs+1, err)
		}
		if n == 0 {
			break
		}
		downs++
	}
	if downs != len(st) {
		t.Fatalf("downs = %d, want %d", downs, len(st))
	}
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM namespaces`).Scan(new(int)); err == nil {
		t.Fatal("namespaces still exists after full down")
	}
	if n, err := d.MigrateUp(ctx); err != nil || n != len(st) {
		t.Fatalf("re-up: n=%d err=%v", n, err)
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := d.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, d.Rebind(
			`INSERT INTO namespaces (name, created_at) VALUES ($1, $2)`),
			"doomed", TimeToDB(time.Now())); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx err = %v, want boom", err)
	}

	var n int
	if err := d.QueryRowContext(ctx,
		d.Rebind(`SELECT count(*) FROM namespaces WHERE name = $1`), "doomed").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rollback failed: %d rows survived", n)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	now := time.Now()
	s := TimeToDB(now)
	back, err := TimeFromDB(s)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(now.UTC().Truncate(time.Nanosecond)) {
		t.Fatalf("round trip: %v != %v", back, now.UTC())
	}
	// fixed width so lexicographic order == chronological order
	later := TimeToDB(now.Add(time.Millisecond))
	if s >= later {
		t.Fatalf("lexicographic ordering broken: %q !< %q", s, later)
	}
}

func TestBackupSQLite(t *testing.T) {
	d := openTest(t)
	if d.Driver != "sqlite" {
		t.Skip("sqlite-only")
	}
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, d.Rebind(
		`INSERT INTO namespaces (name, created_at) VALUES ($1, $2)`),
		"backup-me", TimeToDB(time.Now())); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := d.BackupSQLite(ctx, dst); err != nil {
		t.Fatal(err)
	}
	snap, err := Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	var n int
	if err := snap.QueryRowContext(ctx, snap.Rebind(
		`SELECT count(*) FROM namespaces WHERE name = $1`), "backup-me").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("snapshot rows = %d, want 1", n)
	}
	if err := d.BackupSQLite(ctx, dst); err == nil {
		t.Fatal("overwrite allowed")
	}
}
