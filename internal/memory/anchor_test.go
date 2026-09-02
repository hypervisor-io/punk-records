package memory

import (
	"context"
	"testing"
)

func TestSearchPhraseRequiresAdjacency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	must := func(k, b string) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	must("/a", "the pool exhausted itself under load")
	must("/b", "exhausted the connection pool by leaking handles")
	got, err := s.SearchPhrase(ctx, "ns", "pool exhausted", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "/a" {
		t.Fatalf("phrase search = %v, want only /a", keysOf(got))
	}
	if empty, err := s.SearchPhrase(ctx, "ns", "   ", 10); err != nil || len(empty) != 0 {
		t.Fatalf("blank phrase: %v %v", empty, err)
	}
}

func TestAnchorArmPullsExactIdentifier(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	must := func(k, b string) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	must("/incident/1", "database outage traced to connection saturation")
	must("/runbook/pool", "when ERR_POOL_EXHAUSTED appears, raise max_connections and restart pgbouncer")

	base, err := s.HybridSearchScored(ctx, "ns", "database outage", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range base {
		if h.Key == "/runbook/pool" {
			t.Fatalf("control: runbook must not match the plain query")
		}
	}
	got, err := s.HybridSearchScoredWith(ctx, "ns", "database outage",
		HybridOpts{Limit: 10, Anchors: []string{"ERR_POOL_EXHAUSTED"}})
	if err != nil {
		t.Fatal(err)
	}
	var found *ScoredFact
	for i := range got {
		if got[i].Key == "/runbook/pool" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("anchored fact missing from %v", keysOfScored(got))
	}
	if found.Components["anchor"] <= 0 {
		t.Fatalf("anchor component not recorded: %v", found.Components)
	}
}

func TestAnchorsAreCappedAndTrimmed(t *testing.T) {
	in := []string{" a ", "", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	got := cleanAnchors(in)
	if len(got) != maxAnchors || got[0] != "a" || got[1] != "b" {
		t.Fatalf("cleanAnchors = %v", got)
	}
}

func keysOf(fs []Fact) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Key
	}
	return out
}

func keysOfScored(fs []ScoredFact) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Key
	}
	return out
}
