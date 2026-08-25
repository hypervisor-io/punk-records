package memory

// EstimateTokens approximates token count as ceil(len/4) — good enough
// for budget filling; agents pass max_tokens, not top-k.
func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

// TokenBudget keeps ranked facts, in order, while the running Body
// token total stays within maxTokens. Stops at the first fact that
// does not fit (ranked order is the contract; no gap-skipping).
func TokenBudget(facts []Fact, maxTokens int) []Fact {
	if maxTokens <= 0 {
		return facts
	}
	used := 0
	for i, f := range facts {
		used += EstimateTokens(f.Body)
		if used > maxTokens {
			return facts[:i]
		}
	}
	return facts
}

// TokenBudgetScored is TokenBudget for scored results (post-ranking,
// same first-non-fitting-fact cutoff over Body).
func TokenBudgetScored(facts []ScoredFact, maxTokens int) []ScoredFact {
	if maxTokens <= 0 {
		return facts
	}
	used := 0
	for i, f := range facts {
		used += EstimateTokens(f.Body)
		if used > maxTokens {
			return facts[:i]
		}
	}
	return facts
}
