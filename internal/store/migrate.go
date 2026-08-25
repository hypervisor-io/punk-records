package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationsFS embed.FS

// migration is one versioned pair of up/down SQL scripts.
type migration struct {
	Version int
	Name    string
	UpSQL   string
	DownSQL string
}

// MigrationStatus is one row of `punk migrate status`.
type MigrationStatus struct {
	Version   int
	Name      string
	Applied   bool
	AppliedAt string // empty when not applied
}

func loadMigrations(driver string) ([]migration, error) {
	dir := "migrations/" + driver
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	byVersion := map[int]*migration{}
	for _, e := range entries {
		name := e.Name() // e.g. 0001_core.up.sql
		base, isUp := strings.CutSuffix(name, ".up.sql")
		baseDown, isDown := strings.CutSuffix(name, ".down.sql")
		if !isUp && !isDown {
			return nil, fmt.Errorf("migration file %s: want *.up.sql or *.down.sql", name)
		}
		if isDown {
			base = baseDown
		}
		verStr, rest, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("migration file %s: want NNNN_name", name)
		}
		ver, err := strconv.Atoi(verStr)
		if err != nil {
			return nil, fmt.Errorf("migration file %s: bad version: %w", name, err)
		}
		raw, err := migrationsFS.ReadFile(dir + "/" + name)
		if err != nil {
			return nil, err
		}
		m := byVersion[ver]
		if m == nil {
			m = &migration{Version: ver, Name: rest}
			byVersion[ver] = m
		}
		if isUp {
			m.UpSQL = string(raw)
		} else {
			m.DownSQL = string(raw)
		}
	}
	out := make([]migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.UpSQL == "" || m.DownSQL == "" {
			return nil, fmt.Errorf("migration %04d_%s: missing up or down file", m.Version, m.Name)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (d *DB) ensureMigrationsTable(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`)
	return err
}

func (d *DB) appliedVersions(ctx context.Context) (map[int]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT version, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int]string{}
	for rows.Next() {
		var v int
		var at string
		if err := rows.Scan(&v, &at); err != nil {
			return nil, err
		}
		out[v] = at
	}
	return out, rows.Err()
}

// MigrateUp applies all pending migrations in order, each in its own
// transaction. Returns the number applied.
func (d *DB) MigrateUp(ctx context.Context) (int, error) {
	if err := d.ensureMigrationsTable(ctx); err != nil {
		return 0, err
	}
	migs, err := loadMigrations(d.Driver)
	if err != nil {
		return 0, err
	}
	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range migs {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		err := d.WithTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.UpSQL); err != nil {
				return fmt.Errorf("migration %04d_%s up: %w", m.Version, m.Name, err)
			}
			_, err := tx.ExecContext(ctx,
				d.Rebind(`INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`),
				m.Version, m.Name, TimeToDB(time.Now()))
			return err
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// MigrateDown reverts the single most recent applied migration.
func (d *DB) MigrateDown(ctx context.Context) (int, error) {
	if err := d.ensureMigrationsTable(ctx); err != nil {
		return 0, err
	}
	migs, err := loadMigrations(d.Driver)
	if err != nil {
		return 0, err
	}
	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	for i := len(migs) - 1; i >= 0; i-- {
		m := migs[i]
		if _, ok := applied[m.Version]; !ok {
			continue
		}
		err := d.WithTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.DownSQL); err != nil {
				return fmt.Errorf("migration %04d_%s down: %w", m.Version, m.Name, err)
			}
			_, err := tx.ExecContext(ctx, d.Rebind(`DELETE FROM schema_migrations WHERE version = $1`), m.Version)
			return err
		})
		if err != nil {
			return 0, err
		}
		return 1, nil
	}
	return 0, nil
}

// MigrateStatus lists every known migration and whether it is applied.
func (d *DB) MigrateStatus(ctx context.Context) ([]MigrationStatus, error) {
	if err := d.ensureMigrationsTable(ctx); err != nil {
		return nil, err
	}
	migs, err := loadMigrations(d.Driver)
	if err != nil {
		return nil, err
	}
	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MigrationStatus, 0, len(migs))
	for _, m := range migs {
		at, ok := applied[m.Version]
		out = append(out, MigrationStatus{Version: m.Version, Name: m.Name, Applied: ok, AppliedAt: at})
	}
	return out, nil
}
