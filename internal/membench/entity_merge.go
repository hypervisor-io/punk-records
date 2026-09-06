package membench

import (
	"context"
	"fmt"
	"sort"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// EntityMergeEntity is one entity in a labeled merge fixture: the key and
// display name the enricher would have written, its declared type (""
// exercises the legacy untyped bucket), extractor-declared aliases, and
// the keys of the fixture facts that evidenced it (recorded as
// revision-precise source_facts provenance, plus a mentions edge per
// source, mirroring the enricher's own writes).
type EntityMergeEntity struct {
	Key     string
	Name    string
	Type    string
	Aliases []string
	Sources []string
}

// EntityMergeFixture is a labeled entity-resolution corpus: facts and
// entities to stand up, plus AliasPairs, the unordered entity key pairs
// that SHOULD be proposed for merge. Every other pair is implicitly
// labeled distinct, which is what makes false merges measurable.
type EntityMergeFixture struct {
	Facts      []Record // type=fact records (Key/Body)
	Entities   []EntityMergeEntity
	AliasPairs [][2]string
}

// EntityMergeProposalOut is the report form of one proposal.
type EntityMergeProposalOut struct {
	ID           string  `json:"id"`
	AliasKey     string  `json:"alias_key"`
	CanonicalKey string  `json:"canonical_key"`
	EntityType   string  `json:"entity_type"`
	Score        float64 `json:"score"`
	Protected    bool    `json:"protected"`
}

// EntityMergeEval is the G02 measurement: false merges (proposals for
// pairs labeled distinct) and missed aliases (labeled alias pairs with no
// proposal) over a fixture, with the full proposal list for inspection.
type EntityMergeEval struct {
	Entities       int                      `json:"entities"`
	LabeledAliases int                      `json:"labeled_aliases"`
	Proposals      int                      `json:"proposals"`
	FalseMerges    int                      `json:"false_merges"`
	MissedAliases  int                      `json:"missed_aliases"`
	Proposed       []EntityMergeProposalOut `json:"proposed"`
	Missed         [][2]string              `json:"missed,omitempty"`
	FalsePairs     [][2]string              `json:"false_pairs,omitempty"`
}

// EvalEntityMerges stands a labeled fixture up in a fresh namespace
// (facts, then entities with enricher-shaped attributes and mentions
// edges) and scores Store.ProposeEntityMerges against the labels.
// Discovery runs with IncludeProtected so protected-type proposals are
// measured rather than hidden; the fixture decides whether any pair
// qualifies. No model calls: proposals read stored state only.
func EvalEntityMerges(ctx context.Context, s *memory.Store, ns string, fx EntityMergeFixture) (EntityMergeEval, error) {
	var eval EntityMergeEval
	revByKey := map[string]string{}
	for _, r := range fx.Facts {
		if r.Type != "fact" {
			continue
		}
		f, err := s.Write(ctx, memory.WriteInput{Namespace: ns, Key: r.Key, Body: r.Body, Writer: "membench"})
		if err != nil {
			return eval, fmt.Errorf("ingest %s: %w", r.Key, err)
		}
		revByKey[r.Key] = f.ID
	}
	for _, e := range fx.Entities {
		attrs := map[string]any{"mention_count": float64(len(e.Sources))}
		if e.Type != "" {
			attrs["entity_type"] = e.Type
		}
		if len(e.Aliases) > 0 {
			attrs["aliases"] = e.Aliases
		}
		var srcIDs []string
		for _, src := range e.Sources {
			id, ok := revByKey[src]
			if !ok {
				return eval, fmt.Errorf("entity %s cites unknown fact %s", e.Key, src)
			}
			srcIDs = append(srcIDs, id)
		}
		if len(srcIDs) > 0 {
			attrs["source_facts"] = srcIDs
		}
		if _, err := s.Write(ctx, memory.WriteInput{
			Namespace: ns, Key: e.Key, Body: e.Name, Attributes: attrs,
			Writer: "membench", Importance: 0.3,
		}); err != nil {
			return eval, fmt.Errorf("entity %s: %w", e.Key, err)
		}
		for _, src := range e.Sources {
			if err := s.AddLink(ctx, ns, src, e.Key, "mentions"); err != nil {
				return eval, fmt.Errorf("mention %s -> %s: %w", src, e.Key, err)
			}
		}
	}
	eval.Entities = len(fx.Entities)
	eval.LabeledAliases = len(fx.AliasPairs)

	proposals, err := s.ProposeEntityMerges(ctx, ns, memory.MergeProposalOptions{IncludeProtected: true})
	if err != nil {
		return eval, err
	}
	labeled := map[[2]string]bool{}
	normPair := func(a, b string) [2]string {
		if a > b {
			a, b = b, a
		}
		return [2]string{a, b}
	}
	for _, pair := range fx.AliasPairs {
		labeled[normPair(pair[0], pair[1])] = true
	}
	proposed := map[[2]string]bool{}
	for _, p := range proposals {
		eval.Proposed = append(eval.Proposed, EntityMergeProposalOut{
			ID: p.ID, AliasKey: p.AliasKey, CanonicalKey: p.CanonicalKey,
			EntityType: p.EntityType, Score: p.Score, Protected: p.Protected,
		})
		pair := normPair(p.AliasKey, p.CanonicalKey)
		proposed[pair] = true
		if !labeled[pair] {
			eval.FalseMerges++
			eval.FalsePairs = append(eval.FalsePairs, [2]string{p.AliasKey, p.CanonicalKey})
		}
	}
	for _, pair := range fx.AliasPairs {
		if !proposed[normPair(pair[0], pair[1])] {
			eval.MissedAliases++
			eval.Missed = append(eval.Missed, pair)
		}
	}
	sort.Slice(eval.FalsePairs, func(i, j int) bool { return eval.FalsePairs[i][0] < eval.FalsePairs[j][0] })
	sort.Slice(eval.Missed, func(i, j int) bool { return eval.Missed[i][0] < eval.Missed[j][0] })
	eval.Proposals = len(proposals)
	return eval, nil
}
