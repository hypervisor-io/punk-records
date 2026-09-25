package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exerciseAgentMessages proves migration 0024 up/down on an already
// migrated store: the durable inbox table works and enforces the
// per-(namespace, sender) idempotency key, one down from 0024 reverts
// exactly it, and re-up restores it. Newer migrations may sit above it;
// step down to 0024 first.
func exerciseAgentMessages(t *testing.T, d *DB) {
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
		if tip.Version == 24 {
			if tip.Name != "agent_messages" {
				t.Fatalf("0024 is %s, want agent_messages", tip.Name)
			}
			break
		}
		if tip.Version < 24 {
			t.Fatalf("0024_agent_messages not applied (applied tip %04d_%s)", tip.Version, tip.Name)
		}
		if _, err := d.MigrateDown(ctx); err != nil {
			t.Fatalf("step down past %04d_%s: %v", tip.Version, tip.Name, err)
		}
	}

	insert := func(id, ns, sender, key string) error {
		var k any
		if key != "" {
			k = key
		}
		_, err := d.ExecContext(ctx, d.Rebind(
			`INSERT INTO agent_messages (id, namespace, sender, recipient, body, idempotency_key, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`),
			id, ns, sender, "bob", "hi", k, TimeToDB(time.Now()))
		return err
	}
	if err := insert("m1", "ns", "alice", "k1"); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := insert("m1", "ns", "alice", ""); err == nil {
		t.Fatal("duplicate message id allowed; want primary key violation")
	}
	if err := insert("m2", "ns", "alice", "k1"); err == nil {
		t.Fatal("duplicate (namespace, sender, idempotency_key) allowed; want unique violation")
	}
	// the key is scoped: other sender / other namespace may reuse it, and
	// keyless messages never collide
	for _, c := range []struct{ id, ns, sender, key string }{
		{"m3", "ns", "carol", "k1"}, {"m4", "ns2", "alice", "k1"}, {"m5", "ns", "alice", ""}, {"m6", "ns", "alice", ""},
	} {
		if err := insert(c.id, c.ns, c.sender, c.key); err != nil {
			t.Fatalf("insert %s: %v", c.id, err)
		}
	}
	var n int
	if err := d.QueryRowContext(ctx, d.Rebind(
		`SELECT count(*) FROM agent_messages WHERE namespace = $1 AND recipient = $2 AND acked_at IS NULL`),
		"ns", "bob").Scan(&n); err != nil || n != 4 {
		t.Fatalf("unread = %d err=%v, want 4", n, err)
	}

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
		t.Fatalf("down 0024: %v", err)
	}
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages`).Scan(&n); err == nil {
		t.Fatal("agent_messages still exists after down 0024")
	}
	assertBaselineKey("after down")
	if _, err := d.MigrateUp(ctx); err != nil {
		t.Fatalf("re-up 0024: %v", err)
	}
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("agent_messages after re-up = %d err=%v, want empty table", n, err)
	}
	assertBaselineKey("after re-up")
}

func TestAgentMessagesMigrationSQLite(t *testing.T) {
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.MigrateUp(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	exerciseAgentMessages(t, d)
}

// Postgres coverage follows the repo pattern: runs only with
// PUNK_TEST_PG_DSN set; otherwise skipped, not passed.
func TestAgentMessagesMigrationPostgres(t *testing.T) {
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
	exerciseAgentMessages(t, d)
}
