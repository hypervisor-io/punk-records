package memory

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClipBodyMultibyte proves clipBody cuts on a rune boundary: a body
// made entirely of 3-byte runes (well past n bytes but not past n runes)
// must come back whole, and one past n runes must come back valid UTF-8
// with no U+FFFD replacement character from a split rune.
func TestClipBodyMultibyte(t *testing.T) {
	// "日" is 3 bytes in UTF-8; 200 of them is 600 bytes but only 200 runes.
	exact := strings.Repeat("日", 200)
	if got := clipBody(exact, 200); got != exact {
		t.Fatalf("clipBody at exact rune count truncated: got %d runes, want %d", utf8.RuneCountInString(got), 200)
	}
	if !utf8.ValidString(exact) {
		t.Fatal("test fixture itself invalid, bug in test")
	}

	over := strings.Repeat("日", 201)
	got := clipBody(over, 200)
	if !utf8.ValidString(got) {
		t.Fatalf("clipBody produced invalid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("clipBody split a multibyte rune: %q", got)
	}
	wantPrefix := strings.Repeat("日", 200) + "..."
	if got != wantPrefix {
		t.Fatalf("clipBody = %q, want %q", got, wantPrefix)
	}
}

func TestProfile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// raw facts
	for _, k := range []string{"/svc/db", "/svc/api", "/agent-sessions/s1/tool-1"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "body " + k, Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	// an entity with mention_count
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/alice-chen", Body: "Alice Chen",
		Attributes: map[string]any{"mention_count": 4.0}, Writer: "enricher"}); err != nil {
		t.Fatal(err)
	}
	// an observation and a model
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/o1", Body: "belief", Writer: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RememberModel(ctx, "ns", "m1", "synthesis", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "ns", "/svc/db", "/svc/api", "relates_to"); err != nil {
		t.Fatal(err)
	}
	_ = s.TouchAccess(ctx, []string{mustID(t, s, "ns", "/svc/db")})

	p, err := s.Profile(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if p.Facts != 6 || p.Observations != 1 || p.Models != 1 || p.EntityCount != 1 || p.Links != 1 {
		t.Fatalf("counts wrong: %+v", p)
	}
	if len(p.TopEntities) != 1 || p.TopEntities[0].Name != "Alice Chen" || p.TopEntities[0].Mentions != 4 {
		t.Fatalf("entities: %+v", p.TopEntities)
	}
	// hot keys: only /svc/db was touched (access_count=1); every other
	// live key has access_count=0, and the loop stops at the first zero
	// once it has at least one entry - so /svc/api never gets a chance
	// even though it too is neither an entity nor an agent-session key.
	// Also proves entities and agent-sessions are excluded from hot keys.
	wantHotKeys := map[string]bool{"/svc/db": true}
	if len(p.HotKeys) != len(wantHotKeys) {
		t.Fatalf("hot keys = %+v, want exactly %v", p.HotKeys, wantHotKeys)
	}
	for _, hk := range p.HotKeys {
		if !wantHotKeys[hk.Key] {
			t.Fatalf("unexpected hot key %q (want only %v): %+v", hk.Key, wantHotKeys, p.HotKeys)
		}
	}

	// recent: every live key except /agent-sessions/* and /entities/*.
	wantRecent := map[string]bool{
		"/svc/db": true, "/svc/api": true, "/observations/o1": true, "/mental-models/m1": true,
	}
	if len(p.Recent) != len(wantRecent) {
		t.Fatalf("recent = %+v, want exactly %v", p.Recent, wantRecent)
	}
	for _, rf := range p.Recent {
		if !wantRecent[rf.Key] {
			t.Fatalf("unexpected recent key %q (entities/agent-sessions must be excluded): %+v", rf.Key, p.Recent)
		}
	}
	// unknown namespace: empty profile, no error
	p2, err := s.Profile(ctx, "nope")
	if err != nil || p2.Facts != 0 {
		t.Fatalf("unknown ns: %+v %v", p2, err)
	}
}

// mustID fetches the live fact id for a key.
func mustID(t *testing.T, s *Store, ns, key string) string {
	t.Helper()
	facts, err := s.Recall(context.Background(), ns, key, 1)
	if err != nil || len(facts) != 1 {
		t.Fatal(err, facts)
	}
	return facts[0].ID
}
