package region

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// TestSetInboxBindingRequiresMembership pins the spec contract that a
// bind happens only for an agent that is a registered member of the
// namespace at bind time: registering first (anywhere) is not enough,
// the membership must be in the bound namespace itself.
func TestSetInboxBindingRequiresMembership(t *testing.T) {
	s := newTest(t)
	ctx := context.Background()

	err := s.SetInboxBinding(ctx, "punk-pbs", "claude-code:s1")
	if err == nil {
		t.Fatal("bind without membership allowed")
	}
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("bind without membership = %v, want ErrNotMember", err)
	}
	if ns, ok, err := s.InboxBinding(ctx, "claude-code:s1"); err != nil || ok {
		t.Fatalf("lookup after refused bind = (%q, %v, %v), want no binding", ns, ok, err)
	}

	// Registered elsewhere is still not a member of the bound namespace.
	if err := s.Register(ctx, "repo-main", "claude-code:s1", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxBinding(ctx, "punk-pbs", "claude-code:s1"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("bind to a namespace the agent is not a member of = %v, want ErrNotMember", err)
	}
	if err := s.Register(ctx, "punk-pbs", "claude-code:s1", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxBinding(ctx, "punk-pbs", "claude-code:s1"); err != nil {
		t.Fatalf("bind after membership: %v", err)
	}
	ns, ok, err := s.InboxBinding(ctx, "claude-code:s1")
	if err != nil || !ok || ns != "punk-pbs" {
		t.Fatalf("lookup = (%q, %v, %v), want punk-pbs", ns, ok, err)
	}
}

// TestInboxBindingUpsertReplaces pins the one-binding-per-agent rule:
// a new explicit bind replaces the previous namespace (latest wins),
// leaving exactly one row per agent address.
func TestInboxBindingUpsertReplaces(t *testing.T) {
	s := newTest(t)
	ctx := context.Background()
	for _, ns := range []string{"ns-one", "ns-two"} {
		if err := s.Register(ctx, ns, "opencode:s9", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetInboxBinding(ctx, "ns-one", "opencode:s9"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxBinding(ctx, "ns-two", "opencode:s9"); err != nil {
		t.Fatal(err)
	}
	ns, ok, err := s.InboxBinding(ctx, "opencode:s9")
	if err != nil || !ok || ns != "ns-two" {
		t.Fatalf("rebind lookup = (%q, %v, %v), want ns-two", ns, ok, err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_inbox_bindings WHERE agent = 'opencode:s9'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("binding rows = %d err=%v, want 1", rows, err)
	}
}

// TestInboxBindingLookupIsolation: bindings are per agent address; one
// agent's binding never answers for another, and an unbound address
// reports no binding.
func TestInboxBindingLookupIsolation(t *testing.T) {
	s := newTest(t)
	ctx := context.Background()
	for _, a := range []string{"claude-code:s1", "opencode:s2"} {
		if err := s.Register(ctx, "ns-a", a, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Register(ctx, "ns-b", "claude-code:s1", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxBinding(ctx, "ns-a", "opencode:s2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxBinding(ctx, "ns-b", "claude-code:s1"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ agent, want string }{
		{"claude-code:s1", "ns-b"}, {"opencode:s2", "ns-a"},
	} {
		ns, ok, err := s.InboxBinding(ctx, tc.agent)
		if err != nil || !ok || ns != tc.want {
			t.Fatalf("%s lookup = (%q, %v, %v), want %s", tc.agent, ns, ok, err, tc.want)
		}
	}
	if _, ok, err := s.InboxBinding(ctx, "codex:s3"); err != nil || ok {
		t.Fatalf("unbound lookup ok = %v err = %v, want false nil", ok, err)
	}
}

// TestInboxBindingSurvivesMemberExpiry pins the independence contract:
// the binding table is not tied to region_members, so expiring (or
// deregistering) the member never silently deletes the binding.
func TestInboxBindingSurvivesMemberExpiry(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "binding-expire.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	s := New(db, func() time.Time { return now })
	ctx := context.Background()

	if err := s.Register(ctx, "punk-pbs", "claude-code:s1", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxBinding(ctx, "punk-pbs", "claude-code:s1"); err != nil {
		t.Fatal(err)
	}
	// Age the member past the sweep window, then expire it.
	if _, err := db.ExecContext(ctx, db.Rebind(
		`UPDATE region_members SET last_seen_at = $1, joined_at = $1 WHERE namespace = $2 AND agent = $3`),
		store.TimeToDB(now.Add(-10*24*time.Hour)), "punk-pbs", "claude-code:s1"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireMembers(ctx, 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("expire = %d %v, want 1", n, err)
	}
	ns, ok, err := s.InboxBinding(ctx, "claude-code:s1")
	if err != nil || !ok || ns != "punk-pbs" {
		t.Fatalf("binding after member expiry = (%q, %v, %v), want punk-pbs", ns, ok, err)
	}

	// Deregistration is equally independent of the binding.
	if err := s.Register(ctx, "ns-c", "codex:s4", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxBinding(ctx, "ns-c", "codex:s4"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveMember(ctx, "ns-c", "codex:s4"); err != nil {
		t.Fatal(err)
	}
	ns, ok, err = s.InboxBinding(ctx, "codex:s4")
	if err != nil || !ok || ns != "ns-c" {
		t.Fatalf("binding after remove = (%q, %v, %v), want ns-c", ns, ok, err)
	}
}
