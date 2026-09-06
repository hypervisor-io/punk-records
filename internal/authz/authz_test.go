package authz

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "authz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestGrantAllowExactMatch(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	if err := a.Grant(ctx, "alice", "ns-a", OpRead); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !a.Allow(ctx, "alice", "ns-a", OpRead) {
		t.Fatal("alice read ns-a denied, want allowed")
	}
	if a.Allow(ctx, "alice", "ns-a", OpWrite) {
		t.Fatal("alice write ns-a allowed on a read grant, want denied")
	}
	if a.Allow(ctx, "alice", "ns-a", OpAdmin) {
		t.Fatal("alice admin ns-a allowed on a read grant, want denied")
	}
	if a.Allow(ctx, "alice", "ns-b", OpRead) {
		t.Fatal("alice read ns-b allowed with only an ns-a grant, want denied")
	}
	if a.Allow(ctx, "bob", "ns-a", OpRead) {
		t.Fatal("bob read ns-a allowed without a grant, want denied")
	}
	// exact match is case-sensitive; near-names must not leak
	if a.Allow(ctx, "Alice", "ns-a", OpRead) {
		t.Fatal("subject match must be case-sensitive")
	}
	if a.Allow(ctx, "alice", "NS-A", OpRead) {
		t.Fatal("namespace match must be case-sensitive")
	}
}

func TestGrantRevocation(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	if err := a.Grant(ctx, "alice", "ns-a", OpWrite); err != nil {
		t.Fatal(err)
	}
	if !a.Allow(ctx, "alice", "ns-a", OpWrite) {
		t.Fatal("grant not in effect")
	}
	if err := a.Revoke(ctx, "alice", "ns-a", OpWrite); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if a.Allow(ctx, "alice", "ns-a", OpWrite) {
		t.Fatal("revoked grant still allows")
	}
	if err := a.Revoke(ctx, "alice", "ns-a", OpWrite); err == nil {
		t.Fatal("double revoke should error")
	}
	// re-grant after revoke works
	if err := a.Grant(ctx, "alice", "ns-a", OpWrite); err != nil {
		t.Fatalf("re-grant after revoke: %v", err)
	}
	if !a.Allow(ctx, "alice", "ns-a", OpWrite) {
		t.Fatal("re-grant not in effect")
	}
}

func TestEmptySubjectDenied(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	if a.Allow(ctx, "", "ns-a", OpRead) {
		t.Fatal("empty subject allowed, want deny-by-default")
	}
	if err := a.Grant(ctx, "", "ns-a", OpRead); err == nil {
		t.Fatal("grant with empty subject should error")
	}
	if err := a.Grant(ctx, "   ", "ns-a", OpRead); err == nil {
		t.Fatal("grant with blank subject should error")
	}
}

func TestAdminIsDistinct(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	if err := a.Grant(ctx, "root", "ns-a", OpAdmin); err != nil {
		t.Fatal(err)
	}
	if !a.Allow(ctx, "root", "ns-a", OpAdmin) {
		t.Fatal("admin grant should allow admin")
	}
	// no implicit inheritance: admin alone is not read or write
	if a.Allow(ctx, "root", "ns-a", OpRead) {
		t.Fatal("admin grant implies read, want distinct ops")
	}
	if a.Allow(ctx, "root", "ns-a", OpWrite) {
		t.Fatal("admin grant implies write, want distinct ops")
	}
}

func TestWildcardRejected(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	// v1 grants are exact-match only; '*' is rejected so a grant can
	// never be mistaken for a glob or prefix rule.
	for _, ns := range []string{"*", "ns-*", "*-a", "ns%"} {
		if ns == "ns%" {
			continue // '%' is a plain character here, not special
		}
		if err := a.Grant(ctx, "alice", ns, OpRead); err == nil {
			t.Fatalf("grant namespace %q should be rejected", ns)
		}
	}
	if err := a.Grant(ctx, "*", "ns-a", OpRead); err == nil {
		t.Fatal("wildcard subject should be rejected")
	}
	if a.Allow(ctx, "alice", "*", OpRead) {
		t.Fatal("wildcard namespace lookup must be denied")
	}
}

// TestColonRejectedInNamespace pins the A02-review fix: bus keys are
// built as namespace+":"+key and the SSE handlers split on the first
// ':', so a namespace containing ':' would be indistinguishable on the
// bus from a shorter namespace sharing its prefix (e.g. "a" vs
// "a:private"). Grants and lookups must reject it outright.
func TestColonRejectedInNamespace(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	if err := a.Grant(ctx, "alice", "a:private", OpRead); err == nil {
		t.Fatal("grant on a namespace containing ':' should be rejected")
	}
	if a.Allow(ctx, "alice", "a:private", OpRead) {
		t.Fatal("allow on a namespace containing ':' should be denied")
	}
}

func TestGrantValidationAndIdempotence(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	if err := a.Grant(ctx, "alice", "", OpRead); err == nil {
		t.Fatal("grant with empty namespace should error")
	}
	if err := a.Grant(ctx, "alice", "ns-a", Op("bogus")); err == nil {
		t.Fatal("grant with unknown op should error")
	}
	if err := a.Grant(ctx, "alice", "ns-a", OpRead); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(ctx, "alice", "ns-a", OpRead); err != nil {
		t.Fatalf("duplicate grant should be idempotent: %v", err)
	}
	grants, err := a.Grants(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1 after duplicate grant", len(grants))
	}
	if grants[0].Subject != "alice" || grants[0].Namespace != "ns-a" || grants[0].Op != OpRead {
		t.Fatalf("grant row = %+v", grants[0])
	}
	// revoked grants are not listed as active
	if err := a.Revoke(ctx, "alice", "ns-a", OpRead); err != nil {
		t.Fatal(err)
	}
	grants, err = a.Grants(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("grants after revoke = %d, want 0", len(grants))
	}
}

// Concurrent readers and writers must not race (run with -race);
// writers use disjoint subject/namespace pairs so the unique index is
// never contended.
func TestConcurrentAllowAndGrant(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	if err := a.Grant(ctx, "seed", "ns-a", OpRead); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			subject := fmt.Sprintf("reader-%d", i)
			for j := 0; j < 20; j++ {
				if !a.Allow(ctx, "seed", "ns-a", OpRead) {
					t.Error("seed read denied")
				}
				if a.Allow(ctx, subject, "ns-a", OpRead) {
					t.Errorf("%s read allowed without grant", subject)
				}
				if _, err := a.Grants(ctx, "seed"); err != nil {
					t.Errorf("grants: %v", err)
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			subject := fmt.Sprintf("writer-%d", i)
			for j := 0; j < 5; j++ {
				ns := fmt.Sprintf("ns-w-%d-%d", i, j)
				if err := a.Grant(ctx, subject, ns, OpWrite); err != nil {
					t.Errorf("grant: %v", err)
					return
				}
				if !a.Allow(ctx, subject, ns, OpWrite) {
					t.Errorf("fresh grant not in effect")
				}
				if err := a.Revoke(ctx, subject, ns, OpWrite); err != nil {
					t.Errorf("revoke: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestConcurrentGrantSameTupleNoRaceOneActiveRow pins the fix for
// Grant's count-then-insert race on the namespace_grants_active partial
// unique index: many goroutines granting the identical
// (subject, namespace, op) concurrently must never error and must leave
// exactly one active row.
func TestConcurrentGrantSameTupleNoRaceOneActiveRow(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = a.Grant(ctx, "concurrent", "ns-race", OpWrite)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: grant error: %v", i, err)
		}
	}
	var count int
	if err := a.db.QueryRowContext(ctx, `
		SELECT count(*) FROM namespace_grants
		WHERE subject = 'concurrent' AND namespace = 'ns-race' AND op = 'write' AND revoked_at IS NULL`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active rows = %d, want exactly 1", count)
	}
	if !a.Allow(ctx, "concurrent", "ns-race", OpWrite) {
		t.Fatal("grant not in effect after concurrent grants")
	}
}

// TestAllowLogsWarnOnDBErrorFailClosed pins the fix for Allow silently
// swallowing DB errors as deny: a real query failure (not sql.ErrNoRows)
// must still deny (fail-closed) but must also be visible via the
// exported Log field - and must never log the subject (only op and
// namespace).
func TestAllowLogsWarnOnDBErrorFailClosed(t *testing.T) {
	db := testDB(t)
	var buf bytes.Buffer
	a := New(db, nil)
	a.Log = slog.New(slog.NewTextHandler(&buf, nil))

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if a.Allow(context.Background(), "secret-subject-token", "ns-a", OpRead) {
		t.Fatal("allow on a closed db should deny (fail-closed)")
	}
	logged := buf.String()
	if !strings.Contains(logged, "WARN") {
		t.Fatalf("log = %q, want a warn line", logged)
	}
	if !strings.Contains(logged, "ns-a") || !strings.Contains(logged, "read") {
		t.Fatalf("log = %q, want op and namespace", logged)
	}
	if strings.Contains(logged, "secret-subject-token") {
		t.Fatalf("log = %q, must never include the subject", logged)
	}
}

// TestAllowNilLogDoesNotPanic pins that a nil Log (the default) discards
// instead of panicking.
func TestAllowNilLogDoesNotPanic(t *testing.T) {
	db := testDB(t)
	a := New(db, nil) // Log left nil
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if a.Allow(context.Background(), "alice", "ns-a", OpRead) {
		t.Fatal("allow on a closed db should deny")
	}
}

func TestAllowRejectsUnknownOp(t *testing.T) {
	a := New(testDB(t), nil)
	ctx := context.Background()
	if err := a.Grant(ctx, "alice", "ns-a", OpRead); err != nil {
		t.Fatal(err)
	}
	if a.Allow(ctx, "alice", "ns-a", Op("bogus")) {
		t.Fatal("unknown op allowed")
	}
}
