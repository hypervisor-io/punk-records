package memory

import (
	"strings"
	"testing"
)

func TestTokenBudget(t *testing.T) {
	mk := func(n int) Fact { return Fact{Body: strings.Repeat("a", n)} }
	facts := []Fact{mk(400), mk(400), mk(400)} // ~100 tokens each
	if got := TokenBudget(facts, 250); len(got) != 2 {
		t.Fatalf("budget 250 kept %d, want 2", len(got))
	}
	if got := TokenBudget(facts, 50); len(got) != 0 {
		t.Fatalf("budget 50 kept %d, want 0", len(got))
	}
	if got := TokenBudget(facts, 0); len(got) != 3 {
		t.Fatalf("budget 0 (disabled) kept %d, want 3", len(got))
	}
	if EstimateTokens("abcdefgh") != 2 {
		t.Fatalf("EstimateTokens(8 chars) = %d, want 2", EstimateTokens("abcdefgh"))
	}
}
