package nativewake

import (
	"strings"
	"testing"
)

// TestSenderAllowedExactByDefault pins the P3-C allowlist rule at the
// pure-matcher level: entries match exactly by default, an entry ending
// in '*' opts into prefix matching, and an empty allowlist passes every
// sender - today's unset behavior, deliberately unchanged.
func TestSenderAllowedExactByDefault(t *testing.T) {
	cases := []struct {
		allow  []string
		sender string
		want   bool
		note   string
	}{
		{nil, "anyone", true, "nil allowlist passes all (unchanged unset behavior)"},
		{[]string{}, "anyone", true, "empty allowlist passes all (unchanged unset behavior)"},
		{[]string{"planner"}, "planner", true, "exact match is the default"},
		{[]string{"planner"}, "planner-2", false, "prefix matching is no longer implicit"},
		{[]string{"planner*"}, "planner-2", true, "a trailing '*' opts into prefix matching"},
		{[]string{"planner*"}, "planner", true, "the prefix also matches its bare form"},
		{[]string{"planner*"}, "x-planner", false, "prefix matches at the start only"},
		{[]string{"*"}, "anyone", true, "a bare '*' allows every sender"},
		{[]string{"lead", "other*"}, "lead", true, "exact entry beside a prefix entry"},
		{[]string{"lead", "other*"}, "lead-1", false, "the exact entry stays exact"},
		{[]string{"lead", "other*"}, "other-9", true, "the prefix entry still prefixes"},
		{[]string{"a*b"}, "a*b", true, "an interior '*' is literal: exact match only"},
		{[]string{"a*b"}, "a-x", false, "an interior '*' grants no prefix"},
	}
	for _, c := range cases {
		if got := SenderAllowed(c.allow, c.sender); got != c.want {
			t.Errorf("SenderAllowed(%q, %q) = %v, want %v (%s)",
				strings.Join(c.allow, ","), c.sender, got, c.want, c.note)
		}
	}
}

// TestRunAllowlistPrefixStarOptIn drives the real runner with a
// trailing-'*' entry: an uncovered sender never wakes (the proven
// denied-only sequence from TestRunAllowlistFilters), then a sender the
// prefix covers wakes despite not being an exact allowlist entry. The
// exact-entry default is already pinned by TestRunAllowlistFilters.
func TestRunAllowlistPrefixStarOptIn(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "stranger", "held back")
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.SenderPrefixes = []string{"planner*"}
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	// Uncovered sender: filtered, never a wake - same sequence
	// TestRunAllowlistFilters proves with an exact entry.
	waitFor(t, "sender_filtered diagnostic", func() bool {
		return contains(f.diagStates(), "sender_filtered")
	})
	if n := tr.wakeCount(); n != 0 {
		t.Fatalf("woke for an uncovered sender (%d wakes)", n)
	}

	// A sender the trailing-'*' entry covers wakes the session.
	f.put("m2", "planner-2", "wake for this")
	waitFor(t, "wake after prefix-covered sender arrives", func() bool { return tr.wakeCount() >= 1 })
	f.forbiddenHit(t)
}
