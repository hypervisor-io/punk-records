package hookcli

import (
	"strings"
	"testing"
)

// TestInboxAllowlistExactMatchByDefault pins the P3-C rule at the hook
// inbox gate: PUNK_MESSAGING_FROM entries match exactly by default -
// "lead" passes sender "lead" but no longer "lead-1" - and an
// uncovered sender is held back unread.
func TestInboxAllowlistExactMatchByDefault(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING_FROM", "messaging-glm, lead")
	f, srv := newFakeInbox(t)
	fakeReplyClient(t, "fake", false, "")
	f.add(msg("m1", "messaging-glm", "keep"))
	f.add(msg("m2", "lead", "keep too"))
	f.add(msg("m3", "lead-1", "drop: no longer covered by an exact entry"))
	f.add(msg("m4", "stranger", "drop"))
	out, errs := runInbox(t, InboxOpts{Client: "fake", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"}, fakeStdin)
	if strings.Contains(out, "drop") || !strings.Contains(out, "keep") {
		t.Fatalf("allowlist not applied: %s", out)
	}
	if ids := strings.Join(f.ackedIDs(), ","); ids != "m1,m2" {
		t.Fatalf("acked %s, want m1,m2 only (exact match by default)", ids)
	}
	if ids := strings.Join(f.releasedIDs(), ","); ids != "m3,m4" {
		t.Fatalf("held-back leases not released: %s", ids)
	}
	if !strings.Contains(errs, "held back 2 message(s)") {
		t.Fatalf("no stderr note: %q", errs)
	}
}

// TestInboxAllowlistPrefixStarOptIn pins the opt-in: an entry ending in
// '*' keeps the old prefix behavior for that entry alone - "lead*" passes
// both "lead" and "lead-1" - while "stranger" stays held.
func TestInboxAllowlistPrefixStarOptIn(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING_FROM", "messaging-glm, lead*")
	f, srv := newFakeInbox(t)
	fakeReplyClient(t, "fake", false, "")
	f.add(msg("m1", "messaging-glm", "keep"))
	f.add(msg("m2", "lead", "keep"))
	f.add(msg("m3", "lead-1", "keep too"))
	f.add(msg("m4", "stranger", "drop"))
	out, errs := runInbox(t, InboxOpts{Client: "fake", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"}, fakeStdin)
	if strings.Contains(out, "drop") || !strings.Contains(out, "keep too") {
		t.Fatalf("allowlist not applied: %s", out)
	}
	if ids := strings.Join(f.ackedIDs(), ","); ids != "m1,m2,m3" {
		t.Fatalf("acked %s, want m1,m2,m3 (lead* covers lead and lead-1)", ids)
	}
	if ids := strings.Join(f.releasedIDs(), ","); ids != "m4" {
		t.Fatalf("held-back lease not released: %s", ids)
	}
	if !strings.Contains(errs, "held back 1 message(s)") {
		t.Fatalf("no stderr note: %q", errs)
	}
}

// TestInboxAllowlistUnsetPassesAll pins the unchanged unset case: with
// PUNK_MESSAGING_FROM empty every sender is delivered and acked,
// exactly as before P3-C.
func TestInboxAllowlistUnsetPassesAll(t *testing.T) {
	inboxTestEnv(t) // blanks PUNK_MESSAGING_FROM
	f, srv := newFakeInbox(t)
	fakeReplyClient(t, "fake", false, "")
	f.add(msg("m1", "lead", "one"))
	f.add(msg("m2", "lead-1", "two"))
	f.add(msg("m3", "stranger", "three"))
	out, errs := runInbox(t, InboxOpts{Client: "fake", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"}, fakeStdin)
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(out, want) {
			t.Fatalf("unset allowlist must pass every sender: %s", out)
		}
	}
	if ids := strings.Join(f.ackedIDs(), ","); ids != "m1,m2,m3" {
		t.Fatalf("acked %s, want all", ids)
	}
	if len(f.releasedIDs()) != 0 {
		t.Fatalf("nothing should be held back: %s", f.releasedIDs())
	}
	if strings.Contains(errs, "held back") {
		t.Fatalf("unset allowlist must not hold anything: %q", errs)
	}
}
