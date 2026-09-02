package memory

import "sort"

// CompactHit is the token-lean projection of a search hit for agent
// context: key, clipped body, score, and a few advisory flags. Timestamps,
// attributes, provenance and score components are deliberately absent;
// an agent that needs them calls recall on the key.
type CompactHit struct {
	Key   string   `json:"key"`
	Body  string   `json:"body"`
	Score float64  `json:"score,omitempty"`
	Flags []string `json:"flags,omitempty"` // sorted: invalidated | model | relation | stale
}

// CompactBodyMaxRunes is the default body clip. Long enough to judge
// relevance, short enough that a 10-hit page costs well under 2k tokens.
const CompactBodyMaxRunes = 600

func clipRunes(s string, max int) string {
	if max <= 0 {
		max = CompactBodyMaxRunes
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func flagsFromComponents(c map[string]float64) []string {
	var flags []string
	if c["stale"] == 1 {
		flags = append(flags, "stale")
	}
	if c["invalidated"] == 1 {
		flags = append(flags, "invalidated")
	}
	if c["model"] > 0 {
		flags = append(flags, "model")
	}
	sort.Strings(flags)
	return flags
}

// CompactScored projects scored hits. maxRunes <= 0 uses CompactBodyMaxRunes.
func CompactScored(hits []ScoredFact, maxRunes int) []CompactHit {
	out := make([]CompactHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, CompactHit{
			Key:   h.Key,
			Body:  clipRunes(h.Body, maxRunes),
			Score: h.Score,
			Flags: flagsFromComponents(h.Components),
		})
	}
	return out
}

// CompactFacts projects plain (unscored) facts.
func CompactFacts(facts []Fact, maxRunes int) []CompactHit {
	out := make([]CompactHit, 0, len(facts))
	for _, f := range facts {
		out = append(out, CompactHit{Key: f.Key, Body: clipRunes(f.Body, maxRunes)})
	}
	return out
}

// CompactUnified projects unified hits. A relation renders its endpoints
// and edge type as the key ("/x -> leads_to -> /y") and the edge
// description as the body, so the agent sees the relation itself.
func CompactUnified(hits []UnifiedHit, maxRunes int) []CompactHit {
	out := make([]CompactHit, 0, len(hits))
	for _, h := range hits {
		switch {
		case h.Fact != nil:
			out = append(out, CompactHit{
				Key:   h.Fact.Key,
				Body:  clipRunes(h.Fact.Body, maxRunes),
				Score: h.Score,
				Flags: flagsFromComponents(h.Fact.Components),
			})
		case h.Triplet != nil:
			out = append(out, CompactHit{
				Key:   h.Triplet.From.Key + " -> " + h.Triplet.LinkType + " -> " + h.Triplet.To.Key,
				Body:  clipRunes(h.Triplet.Description, maxRunes),
				Score: h.Score,
				Flags: []string{"relation"},
			})
		}
	}
	return out
}

// TokenBudgetCompact is TokenBudget over compact hits: ranked order kept,
// stop at the first hit that does not fit (no gap-skipping).
func TokenBudgetCompact(hits []CompactHit, maxTokens int) []CompactHit {
	if maxTokens <= 0 {
		return hits
	}
	used := 0
	for i, h := range hits {
		used += EstimateTokens(h.Body)
		if used > maxTokens {
			return hits[:i]
		}
	}
	return hits
}
