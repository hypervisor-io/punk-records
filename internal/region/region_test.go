package region

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

func newTest(t *testing.T) *Store {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "region.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	return New(db, func() time.Time { clk = clk.Add(time.Second); return clk })
}

func TestRegisterMembersRegions(t *testing.T) {
	s := newTest(t)
	ctx := context.Background()

	if err := s.Register(ctx, "incident-42", "database", "diagnostician"); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, "incident-42", "network", "diagnostician"); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, "repo-main", "database", "reader"); err != nil {
		t.Fatal(err)
	}

	members, err := s.Members(ctx, "incident-42")
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %v err=%v", members, err)
	}
	regions, err := s.Regions(ctx, "database")
	if err != nil || len(regions) != 2 {
		t.Fatalf("regions = %v err=%v", regions, err)
	}

	// idempotent + role update
	if err := s.Register(ctx, "incident-42", "database", "lead"); err != nil {
		t.Fatal(err)
	}
	members, _ = s.Members(ctx, "incident-42")
	if len(members) != 2 {
		t.Fatalf("re-register duplicated: %v", members)
	}
	var dbRole string
	for _, m := range members {
		if m.Agent == "database" {
			dbRole = m.Role
		}
	}
	if dbRole != "lead" {
		t.Fatalf("role not updated: %q", dbRole)
	}

	if err := s.Deregister(ctx, "incident-42", "network"); err != nil {
		t.Fatal(err)
	}
	members, _ = s.Members(ctx, "incident-42")
	if len(members) != 1 {
		t.Fatalf("deregister failed: %v", members)
	}
}

func TestSyncFromSpecs(t *testing.T) {
	s := newTest(t)
	ctx := context.Background()
	err := s.SyncFromSpecs(ctx, map[string][]string{
		"database": {"repo-main", "incident-pool"},
		"network":  {"incident-pool"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.Members(ctx, "incident-pool")
	if len(m) != 2 {
		t.Fatalf("incident-pool members = %v", m)
	}
	if m[0].Role != "declared" {
		t.Fatalf("declared role missing: %+v", m[0])
	}
}

func TestTouchUpdatesLastSeen(t *testing.T) {
	s, _ := newClaimTest(t)
	ctx := context.Background()
	if err := s.Register(ctx, "repo", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	m, err := s.Members(ctx, "repo")
	if err != nil || len(m) != 1 || m[0].LastSeenAt == "" || m[0].LastSeenAt != m[0].JoinedAt {
		t.Fatalf("register must set last_seen_at = joined_at: %+v %v", m, err)
	}
	if err := s.Touch(ctx, "repo", "w1"); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Members(ctx, "repo")
	if m[0].LastSeenAt <= m[0].JoinedAt {
		t.Fatalf("touch must advance last_seen_at: %+v", m[0])
	}
	if err := s.Touch(ctx, "repo", "ghost"); err != nil {
		t.Fatalf("touch on an unregistered agent is a no-op, got %v", err)
	}
}
