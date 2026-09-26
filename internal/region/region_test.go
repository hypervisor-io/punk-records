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

func TestRemoveMember(t *testing.T) {
	s := newTest(t)
	ctx := context.Background()
	if err := s.Register(ctx, "ns", "agent1", "worker"); err != nil {
		t.Fatal(err)
	}
	removed, err := s.RemoveMember(ctx, "ns", "agent1")
	if err != nil || !removed {
		t.Fatalf("remove = %v %v, want true", removed, err)
	}
	removed, err = s.RemoveMember(ctx, "ns", "agent1")
	if err != nil || removed {
		t.Fatalf("remove again = %v %v, want false", removed, err)
	}
}

// TestExpireMembers covers the sweep candidates: aged out, recently seen,
// aged out but currently listening (protected), and a row whose
// last_seen_at is NULL so joined_at is the fallback age.
func TestExpireMembers(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "expire.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	s := New(db, func() time.Time { return now })
	ctx := context.Background()

	for _, a := range []string{"old", "fresh", "never-seen", "listening"} {
		if err := s.Register(ctx, "ns", a, ""); err != nil {
			t.Fatal(err)
		}
	}
	setSeen := func(agent string, seen *time.Time, joined time.Time) {
		t.Helper()
		var seenVal any
		if seen != nil {
			seenVal = store.TimeToDB(*seen)
		}
		if _, err := db.ExecContext(ctx, db.Rebind(
			`UPDATE region_members SET last_seen_at=$1, joined_at=$2 WHERE namespace=$3 AND agent=$4`),
			seenVal, store.TimeToDB(joined), "ns", agent); err != nil {
			t.Fatal(err)
		}
	}
	oldSeen := now.Add(-10 * 24 * time.Hour)
	freshSeen := now.Add(-time.Hour)
	setSeen("old", &oldSeen, oldSeen)
	setSeen("fresh", &freshSeen, freshSeen)
	setSeen("never-seen", nil, oldSeen)
	setSeen("listening", &oldSeen, oldSeen)

	release := s.Attach("ns", "listening")
	defer release()

	n, err := s.ExpireMembers(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expired = %d, want 2", n)
	}
	members, err := s.Members(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, m := range members {
		remaining[m.Agent] = true
	}
	if !remaining["fresh"] || !remaining["listening"] {
		t.Fatalf("must keep fresh and listening members: %v", remaining)
	}
	if remaining["old"] || remaining["never-seen"] {
		t.Fatalf("must expire old and never-seen members: %v", remaining)
	}

	// olderThan <= 0 disables the sweep entirely (config's 0 = disabled).
	if n, err := s.ExpireMembers(ctx, 0); err != nil || n != 0 {
		t.Fatalf("disabled sweep = %d %v", n, err)
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

func TestMembersListeningAndActive(t *testing.T) {
	s := newTest(t)
	ctx := context.Background()
	for _, a := range []string{"claude-code:s1", "opencode:s2", "hand-typed-name"} {
		if err := s.Register(ctx, "ns", a, "satellite"); err != nil {
			t.Fatal(err)
		}
	}
	release := s.Attach("ns", "opencode:s2")
	release2 := s.Attach("ns", "opencode:s2")

	members, err := s.MemberStatuses(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	listening := map[string]bool{}
	for _, m := range members {
		listening[m.Agent] = m.Listening
	}
	if !listening["opencode:s2"] || listening["claude-code:s1"] || listening["hand-typed-name"] {
		t.Fatalf("listening flags = %v", listening)
	}

	// Two streams for one address: still listening after one closes.
	release()
	if !s.Listening("ns", "opencode:s2") {
		t.Fatal("one of two streams closed, want still listening")
	}
	release2()
	release2() // idempotent
	if s.Listening("ns", "opencode:s2") {
		t.Fatal("all streams closed, want not listening")
	}
	if s.Listening("other", "opencode:s2") {
		t.Fatal("presence leaked across namespaces")
	}

	// Active: listening now, or seen within the window. The test clock
	// advances one second per call, so every member registered above was
	// seen a few seconds ago.
	members, _ = s.MemberStatuses(ctx, "ns")
	for _, m := range members {
		if !s.Active(m, time.Minute) {
			t.Fatalf("%s registered seconds ago, want active within a minute", m.Agent)
		}
		if s.Active(m, 0) {
			t.Fatalf("%s not listening, want inactive with a zero window", m.Agent)
		}
	}
	unparseable := MemberStatus{Member: Member{Agent: "x", LastSeenAt: "not a time"}}
	if s.Active(unparseable, time.Hour) {
		t.Fatal("unparseable last_seen_at counted as active")
	}
	if !s.Active(MemberStatus{Member: Member{Agent: "y"}, Listening: true}, 0) {
		t.Fatal("listening member must be active regardless of last_seen_at")
	}

	// Touch moves last_seen_at forward; unregistered agents are ignored.
	before := members[0].LastSeenAt
	if err := s.Touch(ctx, "ns", members[0].Agent); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, "ns", "never-registered"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.MemberStatuses(ctx, "ns")
	var seen string
	for _, m := range after {
		if m.Agent == members[0].Agent {
			seen = m.LastSeenAt
		}
	}
	if seen <= before || len(after) != 3 {
		t.Fatalf("touch: last_seen %q -> %q, members=%d", before, seen, len(after))
	}
}
