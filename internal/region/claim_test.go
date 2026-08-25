package region

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

func newClaimTest(t *testing.T) (*Store, *store.DB) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "claim.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	return New(db, func() time.Time { clk = clk.Add(time.Millisecond); return clk }), db
}

func TestClaimBlocksSecondHolder(t *testing.T) {
	s, _ := newClaimTest(t)
	ctx := context.Background()

	if _, err := s.ClaimWork(ctx, "repo", "/file/a.go", "agent-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	// second agent on same key blocked
	_, err := s.ClaimWork(ctx, "repo", "/file/a.go", "agent-2", time.Hour)
	var claimed *ErrClaimed
	if !errors.As(err, &claimed) || claimed.Holder != "agent-1" {
		t.Fatalf("second claim err = %v, want ErrClaimed by agent-1", err)
	}
	// different key is free
	if _, err := s.ClaimWork(ctx, "repo", "/file/b.go", "agent-2", time.Hour); err != nil {
		t.Fatal(err)
	}
	// holder re-claiming its own key extends, no error
	if _, err := s.ClaimWork(ctx, "repo", "/file/a.go", "agent-1", time.Hour); err != nil {
		t.Fatalf("self re-claim: %v", err)
	}

	claims, err := s.ListClaims(ctx, "repo")
	if err != nil || len(claims) != 2 {
		t.Fatalf("live claims = %v err=%v", claims, err)
	}

	// release frees it for agent-2
	if err := s.ReleaseWork(ctx, "repo", "/file/a.go", "agent-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimWork(ctx, "repo", "/file/a.go", "agent-2", time.Hour); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

func TestExpiredClaimReclaimable(t *testing.T) {
	s, _ := newClaimTest(t)
	ctx := context.Background()
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clk }

	if _, err := s.ClaimWork(ctx, "repo", "/x", "a1", time.Minute); err != nil {
		t.Fatal(err)
	}
	clk = clk.Add(2 * time.Minute) // lease lapsed
	if _, err := s.ClaimWork(ctx, "repo", "/x", "a2", time.Minute); err != nil {
		t.Fatalf("reclaim after expiry: %v", err)
	}
	claims, _ := s.ListClaims(ctx, "repo")
	if len(claims) != 1 || claims[0].Holder != "a2" {
		t.Fatalf("claims = %+v", claims)
	}
}

// Concurrency: N goroutines race the same key; exactly one wins.
func TestConcurrentClaimSingleWinner(t *testing.T) {
	s, _ := newClaimTest(t)
	s.now = time.Now // real clock for the race
	ctx := context.Background()

	const n = 12
	var wins int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if _, err := s.ClaimWork(ctx, "repo", "/hot", holderName(id), time.Hour); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
}

func holderName(i int) string { return "agent-" + string(rune('a'+i)) }
