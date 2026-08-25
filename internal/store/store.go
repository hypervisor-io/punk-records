// Package store owns database access: opening, migrations, transactions,
// and timestamp encoding. Repos in other packages receive *store.DB.
//
// One source of truth: this database. No parallel write paths.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver, no CGO
)

// DB wraps sql.DB with the driver name so dialect-specific SQL (FTS) can
// branch in exactly one place per query.
type DB struct {
	*sql.DB
	Driver string // "sqlite" | "postgres"
}

// Open connects and pings. SQLite gets WAL + foreign keys + busy timeout;
// these are correctness settings, not tuning.
func Open(driver, dsn string) (*DB, error) {
	var d *sql.DB
	var err error
	switch driver {
	case "sqlite":
		d, err = sql.Open("sqlite", dsn+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
		if err == nil {
			// modernc/sqlite serializes writes; a single connection avoids
			// SQLITE_BUSY between concurrent write txs in-process.
			d.SetMaxOpenConns(1)
		}
	case "postgres":
		// Simple protocol allows multi-statement migration scripts; pgx
		// still binds $N args safely client-side in this mode.
		if !strings.Contains(dsn, "default_query_exec_mode") {
			sep := " "
			if strings.Contains(dsn, "://") {
				sep = "?"
				if strings.Contains(dsn, "?") {
					sep = "&"
				}
			}
			dsn += sep + "default_query_exec_mode=simple_protocol"
		}
		d, err = sql.Open("pgx", dsn)
	default:
		return nil, fmt.Errorf("unknown db driver %q", driver)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.PingContext(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("ping %s: %w", driver, err)
	}
	return &DB{DB: d, Driver: driver}, nil
}

// WithTx runs fn in a transaction: rollback on error, commit otherwise.
// Any write that touches 2+ tables goes through here. No exceptions.
func (d *DB) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}
	return tx.Commit()
}

// Rebind rewrites $N placeholders to SQLite's ?N positional form. All SQL
// in the codebase is written with $N; call Rebind at the query site.
func (d *DB) Rebind(query string) string {
	if d.Driver != "sqlite" {
		return query
	}
	var b strings.Builder
	b.Grow(len(query))
	for i := 0; i < len(query); i++ {
		if query[i] == '$' && i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9' {
			b.WriteByte('?')
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// dbTimeFormat is fixed-width RFC3339 with nanoseconds so stored strings
// sort lexicographically identically on both engines.
const dbTimeFormat = "2006-01-02T15:04:05.000000000Z"

// TimeToDB encodes a timestamp for storage. Always UTC.
func TimeToDB(t time.Time) string { return t.UTC().Format(dbTimeFormat) }

// TimeFromDB decodes a stored timestamp.
func TimeFromDB(s string) (time.Time, error) {
	t, err := time.Parse(dbTimeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad db timestamp %q: %w", s, err)
	}
	return t, nil
}

// BackupSQLite writes a consistent snapshot of a SQLite database using
// VACUUM INTO. State lives in the DB and specs live in git, so this is
// the whole disaster-recovery story for the sqlite deployment shape.
// Postgres deployments use pg_dump; calling this there is an error.
func (d *DB) BackupSQLite(ctx context.Context, dst string) error {
	if d.Driver != "sqlite" {
		return fmt.Errorf("backup: driver %s not supported here; use pg_dump", d.Driver)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup: %s already exists", dst)
	}
	_, err := d.ExecContext(ctx, "VACUUM INTO "+sqliteQuote(dst))
	return err
}

func sqliteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
