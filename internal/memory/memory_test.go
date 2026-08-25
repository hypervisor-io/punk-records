package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// fakeClock ticks one millisecond per call so revision ordering is
// deterministic without sleeps.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

// Set jumps the clock to t; the next Now() call ticks one millisecond
// past it, so tests can place facts at specific past dates while
// keeping revision ordering deterministic.
func (c *fakeClock) Set(t time.Time) { c.t = t }

func newTest(t *testing.T) (*Store, *store.DB, *fakeClock) {
	t.Helper()
	driver, dsn := "sqlite", filepath.Join(t.TempDir(), "mem.db")
	if pg := os.Getenv("PUNK_TEST_PG_DSN"); pg != "" {
		driver, dsn = "postgres", pg
	}
	db, err := store.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if driver == "postgres" {
		for {
			n, err := db.MigrateDown(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
	}
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)}
	return New(db, clk.Now), db, clk
}

func TestRememberRecallLatestWins(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()

	f1, err := s.Remember(ctx, "acme", "/service/payments/db", "primary is pg-1", nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if f1.Action != "add" {
		t.Errorf("first write action = %q, want add", f1.Action)
	}
	f2, err := s.Remember(ctx, "acme", "/service/payments/db", "primary is pg-2 after failover", nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if f2.Action != "update" {
		t.Errorf("second write action = %q, want update", f2.Action)
	}

	facts, err := s.Recall(ctx, "acme", "/service/payments", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("recall returned %d facts, want 1 (latest per key)", len(facts))
	}
	if facts[0].Body != "primary is pg-2 after failover" {
		t.Errorf("recall body = %q, want latest", facts[0].Body)
	}
}

func TestRecallUnknownNamespaceEmpty(t *testing.T) {
	s, _, _ := newTest(t)
	facts, err := s.Recall(context.Background(), "ghost", "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("want empty, got %d", len(facts))
	}
}

func TestForgetTombstones(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()

	if _, err := s.Remember(ctx, "ns", "/a/b", "x", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, "ns", "/a/b", "tester"); err != nil {
		t.Fatal(err)
	}
	facts, err := s.Recall(ctx, "ns", "/a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("tombstoned key still recalled: %+v", facts)
	}

	// forgetting a dead or never-lived key errors (duckbrain poisoning bug)
	if err := s.Forget(ctx, "ns", "/a/b", "tester"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double forget err = %v, want ErrNotFound", err)
	}
	if err := s.Forget(ctx, "ns", "/never/lived", "tester"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("forget missing err = %v, want ErrNotFound", err)
	}

	// re-adding after tombstone starts a new chain
	f, err := s.Remember(ctx, "ns", "/a/b", "back", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Action != "add" {
		t.Errorf("post-tombstone action = %q, want add", f.Action)
	}
}

func TestListKeys(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	for _, k := range []string{"/svc/a", "/svc/b", "/other/c"} {
		if _, err := s.Remember(ctx, "ns", k, "v", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Forget(ctx, "ns", "/svc/b", ""); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListKeys(ctx, "ns", "/svc")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "/svc/a" {
		t.Fatalf("keys = %v, want [/svc/a]", keys)
	}
}

func TestValidateKey(t *testing.T) {
	for _, bad := range []string{"", "nope", "/", "/a//b", "/a/"} {
		if err := ValidateKey(bad); err == nil {
			t.Errorf("ValidateKey(%q) accepted", bad)
		}
	}
	if err := ValidateKey("/a/b-c/d_e"); err != nil {
		t.Errorf("ValidateKey good key: %v", err)
	}
}

func TestQuarantineBadRowDoesNotBrickNamespace(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()

	if _, err := s.Remember(ctx, "ns", "/good", "fine", nil, ""); err != nil {
		t.Fatal(err)
	}
	// poison a row directly (bypassing the API, like a bug or bad import)
	var nsID int64
	if err := db.QueryRowContext(ctx, db.Rebind(`SELECT id FROM namespaces WHERE name = $1`), "ns").Scan(&nsID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, db.Rebind(
		`INSERT INTO memories (id, namespace_id, key, action, body, attributes, author, created_at)
		 VALUES ('poison1', $1, '/bad', 'add', 'x', 'NOT-JSON{', '', $2)`),
		nsID, store.TimeToDB(time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}

	facts, err := s.Recall(ctx, "ns", "/", 0)
	if err != nil {
		t.Fatalf("recall errored on poisoned row: %v", err)
	}
	if len(facts) != 1 || facts[0].Key != "/good" {
		t.Fatalf("facts = %+v, want only /good", facts)
	}

	var qn int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memories_quarantine`).Scan(&qn); err != nil {
		t.Fatal(err)
	}
	if qn != 1 {
		t.Fatalf("quarantine rows = %d, want 1", qn)
	}
	var left int
	if err := db.QueryRowContext(ctx, db.Rebind(`SELECT count(*) FROM memories WHERE id = $1`), "poison1").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatal("poisoned row still in memories")
	}
}

func TestSearch(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	if _, err := s.Remember(ctx, "ns", "/svc/db", "postgres replication lag spikes during vacuum", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "ns", "/svc/net", "packet loss on edge router", nil, ""); err != nil {
		t.Fatal(err)
	}

	facts, err := s.Search(ctx, "ns", "replication", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Key != "/svc/db" {
		t.Fatalf("search = %+v, want /svc/db", facts)
	}

	if _, err := s.Search(ctx, "ns", "   ", 10); err == nil {
		t.Fatal("empty query accepted")
	}
}

func TestSweepRetention(t *testing.T) {
	s, db, clk := newTest(t)
	ctx := context.Background()

	if _, err := s.Remember(ctx, "ns", "/k", "v1", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "ns", "/k", "v2", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "ns", "/dead", "x", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, "ns", "/dead", ""); err != nil {
		t.Fatal(err)
	}

	// jump the clock far past retention
	clk.t = clk.t.Add(48 * time.Hour)
	n, err := s.SweepRetention(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// v1 (superseded), /dead add (superseded), /dead tombstone = 3
	if n != 3 {
		t.Fatalf("swept %d rows, want 3", n)
	}

	facts, err := s.Recall(ctx, "ns", "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Body != "v2" {
		t.Fatalf("latest live must survive sweep, got %+v", facts)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("memories rows = %d, want 1", rows)
	}
}

func TestReinforcementsColumnRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/r", Body: "b", Writer: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Reinforcements != 0 {
		t.Fatalf("new fact reinforcements = %d, want 0", f.Reinforcements)
	}
	got, err := s.Recall(ctx, "ns", "/r", 1)
	if err != nil || len(got) != 1 {
		t.Fatal(err, got)
	}
	if got[0].Reinforcements != 0 {
		t.Fatalf("recalled reinforcements = %d, want 0", got[0].Reinforcements)
	}
}

func TestDedupWriteBumpsReinforcements(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	in := WriteInput{Namespace: "ns", Key: "/d", Body: "same", Writer: "a"}
	if _, err := s.Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // two duplicate writes
		f, err := s.Write(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if f.Reinforcements != int64(i+1) {
			t.Fatalf("after dup %d: reinforcements = %d, want %d", i+1, f.Reinforcements, i+1)
		}
	}
	// still exactly one revision
	var n int
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT count(*) FROM memories WHERE key = $1`), "/d").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("revisions = %d, want 1", n)
	}
	// no extra outbox rows beyond the first write's
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM memory_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
	// recall reads the bumped value back (proves the column is read, not defaulted)
	got, err := s.Recall(ctx, "ns", "/d", 1)
	if err != nil || len(got) != 1 {
		t.Fatal(err, got)
	}
	if got[0].Reinforcements != 2 {
		t.Fatalf("recalled reinforcements = %d, want 2", got[0].Reinforcements)
	}
}
