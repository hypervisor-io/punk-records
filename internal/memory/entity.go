package memory

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// EntityExtractor pulls canonical entity names (people, orgs, places,
// concepts) from a fact body. Deterministic-first: a nil extractor
// disables entity enrichment entirely.
type EntityExtractor interface {
	Extract(ctx context.Context, body string) ([]string, error)
}

// BatchEntityExtractor is an optional upgrade an EntityExtractor may
// implement: one model call over several fact bodies, answering
// one entity list per body, in order. EnrichEntitiesBatch uses it when
// present and falls back to per-fact Extract when not.
type BatchEntityExtractor interface {
	ExtractBatch(ctx context.Context, bodies []string) ([][]string, error)
}

var entitySlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// EntitySlug normalizes a name to a stable key segment: lowercased,
// non-alphanumeric runs collapsed to single hyphens, trimmed.
// "Alice Chen" and "alice  chen" both -> "alice-chen".
func EntitySlug(name string) string {
	s := entitySlugRe.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(s, "-")
}

// entityAliasThreshold is the minimum nameSimilarity score for an extracted
// name to be merged into an existing entity instead of creating a new one.
// Deliberately conservative: 0.85 plus the containment rule merges
// "Alice"->"Alice Chen" (containment 0.9) but keeps "Alice"/"Alicia"
// separate (edit ratio ~0.7, no containment).
const entityAliasThreshold = 0.85

// resolveEntitySlug canonicalizes name against existing /entities/ facts in
// ns by fuzzy match (nameSimilarity, cutoff entityAliasThreshold) before
// falling back to the exact EntitySlug. This is what lets aliases ("Alice"
// / "Alice Chen") merge into one entity instead of the old slug-exact
// behavior creating a duplicate node.
// ponytail: O(n) linear scan of existing entities with string similarity
// only (no embeddings) — false-merge risk on names that are lexically close
// but semantically distinct (kept conservative via entityAliasThreshold to
// bound that risk). Upgrade path: embedding-similarity resolution, same
// pattern as ReconcileObservations.
func (s *Store) resolveEntitySlug(ctx context.Context, ns, name string) (string, error) {
	existing, err := s.Recall(ctx, ns, "/entities", 1000)
	if err != nil {
		return "", err
	}
	best, bestScore := "", 0.0
	for _, e := range existing {
		if score := nameSimilarity(name, e.Body); score >= entityAliasThreshold && score > bestScore {
			bestScore = score
			best = strings.TrimPrefix(e.Key, "/entities/")
		}
	}
	if best != "" {
		return best, nil
	}
	return EntitySlug(name), nil
}

// EnrichEntities extracts entities from the live fact at ns/key, upserts
// each as an /entities/<slug> fact (incrementing a mention_count attr),
// links the fact to each entity with a "mentions" edge, and adds a
// "co_occurs" edge between every pair of entities that appear together in
// this fact. Idempotent per (fact, entity): a re-run does not double-count
// mentions it already recorded. No-op without an extractor. Only direct
// writes (facts + links), never memory_outbox — so it cannot re-trigger
// the enricher loop (same guarantee as EnrichKey).
func (s *Store) EnrichEntities(ctx context.Context, ns, key string) (int, error) {
	if s.entityExtractor == nil {
		return 0, nil
	}
	if hasPrefix(key, "/entities/") {
		return 0, nil // never re-extract entities from an entity fact itself
	}
	facts, err := s.liveByKeys(ctx, ns, []string{key})
	if err != nil {
		return 0, err
	}
	if len(facts) == 0 {
		return 0, nil // tombstoned or gone between event and processing
	}
	names, err := s.entityExtractor.Extract(ctx, facts[0].Body)
	if err != nil {
		return 0, err
	}
	return s.applyEntities(ctx, ns, key, names)
}

// EnrichEntitiesBatch extracts entities for several keys at once: one
// model call for the whole batch when the extractor implements
// BatchEntityExtractor (with a per-fact fallback when the batch answer
// doesn't line up), a plain per-key loop otherwise. Keys that are
// tombstoned, gone, or /entities/ facts are skipped. Returns total
// entities applied.
func (s *Store) EnrichEntitiesBatch(ctx context.Context, ns string, keys []string) (int, error) {
	if s.entityExtractor == nil || len(keys) == 0 {
		return 0, nil
	}
	var want []string
	for _, k := range keys {
		if !hasPrefix(k, "/entities/") {
			want = append(want, k)
		}
	}
	if len(want) == 0 {
		return 0, nil
	}
	live, err := s.liveByKeys(ctx, ns, want)
	if err != nil {
		return 0, err
	}
	if len(live) == 0 {
		return 0, nil
	}

	perFact := func() (int, error) {
		total := 0
		for _, f := range live {
			names, err := s.entityExtractor.Extract(ctx, f.Body)
			if err != nil {
				return total, err
			}
			n, err := s.applyEntities(ctx, ns, f.Key, names)
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}

	batcher, ok := s.entityExtractor.(BatchEntityExtractor)
	if !ok || len(live) == 1 {
		return perFact()
	}
	bodies := make([]string, len(live))
	for i, f := range live {
		bodies[i] = f.Body
	}
	lists, err := batcher.ExtractBatch(ctx, bodies)
	if err != nil || len(lists) != len(live) {
		// A batch failure or a miscounted answer degrades to per-fact
		// calls rather than dropping the batch: correctness over the
		// batching saving.
		return perFact()
	}
	total := 0
	for i, f := range live {
		n, err := s.applyEntities(ctx, ns, f.Key, lists[i])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// applyEntities upserts the extracted entity names for the live fact at
// ns/key: /entities/<slug> facts with mention counts, mentions edges,
// and co_occurs bumps - the write half of EnrichEntities, shared with
// the batch path.
func (s *Store) applyEntities(ctx context.Context, ns, key string, names []string) (int, error) {
	type entity struct{ key, name string }
	seen := map[string]bool{}
	var ents []entity
	for _, n := range names {
		slug, err := s.resolveEntitySlug(ctx, ns, n)
		if err != nil {
			return 0, err
		}
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		ents = append(ents, entity{"/entities/" + slug, n})
	}
	if len(ents) == 0 {
		return 0, nil
	}

	// existing outgoing mentions targets, live or closed - linkTargetsAll,
	// not Neighbors, because a closed target must stay excluded from
	// re-attempt too (same guard shape as EnrichKey's similar_to check).
	// Otherwise every re-enrich of an unchanged fact treats the closed
	// mentions edge as new: a fresh mention_count bump and a fresh
	// co_occurs weight bump each time, since ON CONFLICT DO NOTHING on the
	// closed link row never reports back that nothing changed.
	existing := map[string]bool{}
	targets, err := s.linkTargetsAll(ctx, ns, key, "mentions")
	if err != nil {
		return 0, err
	}
	for _, tk := range targets {
		existing[tk] = true
	}

	newKeys := map[string]bool{}
	for _, e := range ents {
		if existing[e.key] {
			continue
		}
		count := 1.0
		name := e.name
		cur, err := s.liveByKeys(ctx, ns, []string{e.key})
		if err != nil {
			return 0, err
		}
		if len(cur) == 1 {
			if mc, ok := cur[0].Attributes["mention_count"].(float64); ok {
				count = mc + 1
			}
			name = cur[0].Body // keep first-seen casing
		}
		if _, err := s.writeNoOutbox(ctx, WriteInput{
			Namespace: ns, Key: e.key, Body: name,
			Attributes: map[string]any{"mention_count": count},
			Writer:     "enricher", Importance: 0.3,
		}); err != nil {
			return 0, err
		}
		if err := s.AddLinkWeighted(ctx, ns, key, e.key, "mentions", 1.0); err != nil {
			return 0, err
		}
		newKeys[e.key] = true
	}

	// co_occurs only where this fact contributed a genuinely new mention on
	// at least one side of the pair — otherwise a re-run of an unchanged
	// fact would double-count a pair it already linked.
	for i := 0; i < len(ents); i++ {
		for j := i + 1; j < len(ents); j++ {
			a, b := ents[i].key, ents[j].key
			if !newKeys[a] && !newKeys[b] {
				continue
			}
			if err := s.bumpCoOccurs(ctx, ns, a, b); err != nil {
				return 0, err
			}
		}
	}

	return len(ents), nil
}

// bumpCoOccurs increments (creating at weight 1 if absent) the co_occurs
// edge between two entity keys in BOTH directions, so the symmetric
// relation is discoverable via Neighbors(key, "out") from either entity
// (matching similar_to). Both rows carry the same running count.
func (s *Store) bumpCoOccurs(ctx context.Context, ns, a, b string) error {
	// now is captured once and reused for created_at and valid_at (same
	// pattern as AddLinkDescribed), so a freshly-created co_occurs edge's
	// validity window starts at its creation instant. The DO UPDATE weight
	// bump is gated on invalid_at IS NULL (upsert-WHERE, standard on both
	// sqlite and postgres) so a user-closed co_occurs edge stops
	// accumulating weight instead of drifting while dead; when the guard
	// fails the row is left untouched, same as ON CONFLICT DO NOTHING.
	q := s.db.Rebind(`
		INSERT INTO memory_links (namespace, from_key, to_key, link_type, weight, created_at, valid_at)
		VALUES ($1,$2,$3,'co_occurs',1,$4,$5)
		ON CONFLICT (namespace, from_key, to_key, link_type)
		DO UPDATE SET weight = memory_links.weight + 1 WHERE memory_links.invalid_at IS NULL`)
	now := store.TimeToDB(s.now())
	if _, err := s.db.ExecContext(ctx, q, ns, a, b, now, now); err != nil {
		return fmt.Errorf("bump co_occurs: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, q, ns, b, a, now, now); err != nil {
		return fmt.Errorf("bump co_occurs: %w", err)
	}
	return nil
}

// ListEntities returns live entity facts under ns, newest revision per key.
func (s *Store) ListEntities(ctx context.Context, ns string) ([]Fact, error) {
	return s.Recall(ctx, ns, "/entities", 1000)
}
