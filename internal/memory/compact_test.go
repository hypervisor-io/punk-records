package memory

import (
	"strings"
	"testing"
)

func TestCompactScoredClipsAndFlags(t *testing.T) {
	long := strings.Repeat("x", 700)
	in := []ScoredFact{
		{Fact: Fact{Key: "/a", Body: long}, Score: 0.5,
			Components: map[string]float64{"stale": 1, "invalidated": 1, "model": 1.15}},
		{Fact: Fact{Key: "/b", Body: "short"}, Score: 0.25},
	}
	out := CompactScored(in, CompactBodyMaxRunes)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Key != "/a" || out[0].Score != 0.5 {
		t.Fatalf("hit0 = %+v", out[0])
	}
	if got := len([]rune(out[0].Body)); got != CompactBodyMaxRunes+3 {
		t.Fatalf("clipped body runes = %d, want %d", got, CompactBodyMaxRunes+3)
	}
	if !strings.HasSuffix(out[0].Body, "...") {
		t.Fatalf("clipped body should end with ..., got %q", out[0].Body[len(out[0].Body)-5:])
	}
	if strings.Join(out[0].Flags, ",") != "invalidated,model,stale" {
		t.Fatalf("flags = %v, want [invalidated model stale]", out[0].Flags)
	}
	if out[1].Body != "short" || out[1].Flags != nil {
		t.Fatalf("hit1 = %+v", out[1])
	}
}

func TestCompactMaxRunesZeroMeansDefault(t *testing.T) {
	long := strings.Repeat("y", 1000)
	out := CompactFacts([]Fact{{Key: "/k", Body: long}}, 0)
	if got := len([]rune(out[0].Body)); got != CompactBodyMaxRunes+3 {
		t.Fatalf("runes = %d, want default clip", got)
	}
	if out[0].Score != 0 {
		t.Fatalf("plain facts carry no score, got %v", out[0].Score)
	}
}

func TestCompactUnifiedRendersTriplet(t *testing.T) {
	sf := &ScoredFact{Fact: Fact{Key: "/f", Body: "fact body"}, Score: 0.9}
	tr := &Triplet{From: Fact{Key: "/x"}, To: Fact{Key: "/y"}, LinkType: "leads_to", Description: "x causes y"}
	out := CompactUnified([]UnifiedHit{
		{Kind: "fact", Fact: sf, Score: 0.02},
		{Kind: "relation", Triplet: tr, Score: 0.01},
	}, 0)
	if len(out) != 2 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].Key != "/f" || out[0].Body != "fact body" || out[0].Score != 0.02 {
		t.Fatalf("fact hit = %+v", out[0])
	}
	if out[1].Key != "/x -> leads_to -> /y" || out[1].Body != "x causes y" {
		t.Fatalf("relation hit = %+v", out[1])
	}
	if strings.Join(out[1].Flags, ",") != "relation" {
		t.Fatalf("relation flags = %v", out[1].Flags)
	}
}

func TestTokenBudgetCompactStopsAtFirstNonFit(t *testing.T) {
	hits := []CompactHit{
		{Key: "/1", Body: strings.Repeat("a", 40)}, // 10 tokens
		{Key: "/2", Body: strings.Repeat("b", 40)}, // 10 tokens
		{Key: "/3", Body: "c"},
	}
	got := TokenBudgetCompact(hits, 15)
	if len(got) != 1 || got[0].Key != "/1" {
		t.Fatalf("got %v, want only /1", got)
	}
	if all := TokenBudgetCompact(hits, 0); len(all) != 3 {
		t.Fatalf("maxTokens 0 must return all, got %d", len(all))
	}
}
