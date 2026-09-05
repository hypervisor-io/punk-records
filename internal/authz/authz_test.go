package authz

import (
	"context"
	"fmt"
	"path/filepath"
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
