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

// The entity type vocabulary: a small fixed set, not a general ontology
// engine. Anything an extractor declares outside this list is coerced to
// EntityTypeUnknown (with the declared value retained as declared_type).
const (
	EntityTypeService    = "service"
	EntityTypeRepository = "repository"
	EntityTypeDatabase   = "database"
	EntityTypeHost       = "host"
	EntityTypeIncident   = "incident"
	EntityTypePerson     = "person"
	EntityTypeUnknown    = "unknown"
)

// ValidEntityType reports whether t is in the fixed vocabulary.
func ValidEntityType(t string) bool {
	switch t {
	case EntityTypeService, EntityTypeRepository, EntityTypeDatabase,
		EntityTypeHost, EntityTypeIncident, EntityTypePerson, EntityTypeUnknown:
		return true
	}
	return false
}

// EntitySource is one fact handed to a StructuredEntityExtractor. ID is
// the exact revision identity of the fact (the provenance anchor an
// extractor cites and the store retains); Key is its mutable address
// (mentions links stay key-addressed), Body the text extracted from.
type EntitySource struct {
	ID   string
	Key  string
	Body string
}

// ExtractedEntity is one structured entity mention: a display name, a
// declared type from the fixed vocabulary, optional aliases, and the
// source facts that evidenced it. SourceFacts entries should be exact
// fact revision IDs (EntitySource.ID); source keys are also accepted
// and resolve to the revision of that key actually present in the call.
// Citations matching nothing in the call are invalid: they are dropped
// and never expand to the whole batch.
type ExtractedEntity struct {
	Name        string
	Type        string
	Aliases     []string
	SourceFacts []string
}

// StructuredEntityExtractor is an optional upgrade an EntityExtractor may
// implement: typed extraction over a batch of facts, returning names,
// types, aliases and source fact IDs. Its presence on the configured
// extractor selects typed entity mode (canonical /entities/<type>/<slug>
// keys); a plain EntityExtractor keeps the legacy untyped behavior.
// Adapted from Cognee's consolidate_entities.py mechanism (typed,
// provenance-linked entity consolidation), reimplemented in Go.
type StructuredEntityExtractor interface {
	ExtractStructured(ctx context.Context, sources []EntitySource) ([]ExtractedEntity, error)
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
	if se, ok := s.entityExtractor.(StructuredEntityExtractor); ok {
		return s.enrichEntitiesTyped(ctx, ns, facts, se)
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
	if se, ok := s.entityExtractor.(StructuredEntityExtractor); ok {
		return s.enrichEntitiesTyped(ctx, ns, live, se)
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

	lists, err := s.extractNamesBatched(ctx, live)
	if err != nil {
		return 0, err
	}
	if lists == nil {
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

// extractNamesBatched runs BatchEntityExtractor over live, returning one
// name list per fact in order. A nil slice (with nil error) means the
// batch could not be used (no batcher, single fact, batch error or a
// miscounted answer) and the caller should fall back to per-fact Extract.
func (s *Store) extractNamesBatched(ctx context.Context, live []Fact) ([][]string, error) {
	batcher, ok := s.entityExtractor.(BatchEntityExtractor)
	if !ok || len(live) == 1 {
		return nil, nil
	}
	bodies := make([]string, len(live))
	for i, f := range live {
		bodies[i] = f.Body
	}
	lists, err := batcher.ExtractBatch(ctx, bodies)
	if err != nil || len(lists) != len(live) {
		return nil, nil
	}
	return lists, nil
}

// typedEntity is a validated ExtractedEntity plus its resolved canonical
// key. declared retains an out-of-vocabulary extractor type for
// inspection when typ was coerced to unknown. sources holds the source
// fact KEYS (mentions edges and co_occurs are key-addressed); sourceIDs
// holds the exact contributing revision IDs stored as source_facts
// provenance on the entity fact.
type typedEntity struct {
	key, name, typ, declared string
	aliases, sources         []string
	sourceIDs                []string
}

// enrichEntitiesTyped is the typed-mode write path shared by
// EnrichEntities and EnrichEntitiesBatch: one structured call for the
// whole fact set, validation, then canonical typed upserts. A structured
// failure degrades to legacy name extraction adapted to type unknown, so
// typed mode can only add information, never lose entities.
func (s *Store) enrichEntitiesTyped(ctx context.Context, ns string, live []Fact, se StructuredEntityExtractor) (int, error) {
	sources := make([]EntitySource, len(live))
	for i, f := range live {
		sources[i] = EntitySource{ID: f.ID, Key: f.Key, Body: f.Body}
	}
	raw, err := se.ExtractStructured(ctx, sources)
	if err != nil {
		return s.enrichEntitiesTypedFallback(ctx, ns, live)
	}
	return s.applyTypedEntities(ctx, ns, s.validateExtracted(raw, live))
}

// enrichEntitiesTypedFallback adapts the legacy name-only interface into
// structured results of type unknown when the structured call fails.
// Each name cites the exact revision ID of the fact it came from, so
// fallback provenance is revision-precise like the structured path.
func (s *Store) enrichEntitiesTypedFallback(ctx context.Context, ns string, live []Fact) (int, error) {
	var ents []ExtractedEntity
	lists, err := s.extractNamesBatched(ctx, live)
	if err != nil {
		return 0, err
	}
	if lists != nil {
		for i, f := range live {
			for _, n := range lists[i] {
				ents = append(ents, ExtractedEntity{Name: n, SourceFacts: []string{f.ID}})
			}
		}
	} else {
		for _, f := range live {
			names, err := s.entityExtractor.Extract(ctx, f.Body)
			if err != nil {
				return 0, err
			}
			for _, n := range names {
				ents = append(ents, ExtractedEntity{Name: n, SourceFacts: []string{f.ID}})
			}
		}
	}
	return s.applyTypedEntities(ctx, ns, s.validateExtracted(ents, live))
}

// validateExtracted filters malformed structured results so they never
// write partial invalid entities: entries without a usable name are
// dropped; out-of-vocabulary types are coerced to unknown with the
// declared value kept. Provenance is validated against the facts in
// THIS call: a citation matching a fact's exact revision ID or its key
// resolves to that fact's revision (the key keeps mentions links
// working, the revision ID is retained as the provenance anchor);
// anything else is invalid and dropped - it never expands to the whole
// batch. An entry left with no valid citation gets the bounded,
// explicit per-fact fallback (boundedSourceFallback); with no
// supporting fact at all the entry is skipped.
func (s *Store) validateExtracted(raw []ExtractedEntity, live []Fact) []typedEntity {
	byKey := make(map[string]Fact, len(live))
	byID := make(map[string]Fact, len(live))
	for _, f := range live {
		if _, ok := byKey[f.Key]; !ok {
			byKey[f.Key] = f
		}
		if f.ID != "" {
			byID[f.ID] = f
		}
	}
	var out []typedEntity
	for _, e := range raw {
		name := strings.TrimSpace(e.Name)
		if name == "" || EntitySlug(name) == "" {
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(e.Type))
		declared := ""
		switch {
		case typ == "":
			typ = EntityTypeUnknown
		case !ValidEntityType(typ):
			declared, typ = typ, EntityTypeUnknown
		}
		var aliases []string
		seenAlias := map[string]bool{strings.ToLower(name): true}
		for _, a := range e.Aliases {
			a = strings.TrimSpace(a)
			if a == "" || EntitySlug(a) == "" || seenAlias[strings.ToLower(a)] {
				continue
			}
			seenAlias[strings.ToLower(a)] = true
			aliases = append(aliases, a)
		}
		var srcs, srcIDs []string
		seenSrc := map[string]bool{}
		for _, cited := range e.SourceFacts {
			f, ok := byID[cited]
			if !ok {
				f, ok = byKey[cited]
			}
			if !ok || seenSrc[f.Key] {
				continue // outside this call (or already cited): invalid, dropped
			}
			seenSrc[f.Key] = true
			srcs = append(srcs, f.Key)
			srcIDs = append(srcIDs, f.ID)
		}
		if len(srcs) == 0 {
			srcs, srcIDs = boundedSourceFallback(name, aliases, live)
			if len(srcs) == 0 {
				continue // no supporting fact in the call: skip, never invent
			}
		}
		out = append(out, typedEntity{
			name: name, typ: typ, declared: declared,
			aliases: aliases, sources: srcs, sourceIDs: srcIDs,
		})
	}
	return out
}

// boundedSourceFallback attributes an entity whose citations were all
// missing or invalid to the facts of this call that can actually
// support it, never to the whole batch: a single-fact call attributes
// to that one fact (the extractor saw exactly one text); a multi-fact
// call attributes only to facts whose body literally contains the
// display name or an alias. An empty result means skip the entity.
func boundedSourceFallback(name string, aliases []string, live []Fact) (keys, ids []string) {
	if len(live) == 1 {
		return []string{live[0].Key}, []string{live[0].ID}
	}
	needles := make([]string, 0, len(aliases)+1)
	needles = append(needles, strings.ToLower(name))
	for _, a := range aliases {
		needles = append(needles, strings.ToLower(a))
	}
	for _, f := range live {
		body := strings.ToLower(f.Body)
		for _, n := range needles {
			if n != "" && strings.Contains(body, n) {
				keys = append(keys, f.Key)
				ids = append(ids, f.ID)
				break
			}
		}
	}
	return keys, ids
}

// resolveTypedEntityKey canonicalizes a typed name against existing
// /entities/<typ>/ facts only - same display name under a different type
// is a different entity. Both the name and its aliases fuzzy-match
// against existing bodies and alias lists (cutoff entityAliasThreshold)
// before falling back to the exact /entities/<typ>/<slug> key.
func (s *Store) resolveTypedEntityKey(ctx context.Context, ns string, e typedEntity) (string, error) {
	prefix := "/entities/" + e.typ + "/"
	existing, err := s.Recall(ctx, ns, prefix, 1000)
	if err != nil {
		return "", err
	}
	probes := append([]string{e.name}, e.aliases...)
	best, bestScore := "", 0.0
	for _, ex := range existing {
		cands := append([]string{ex.Body}, entityAliases(ex)...)
		for _, p := range probes {
			for _, c := range cands {
				if score := nameSimilarity(p, c); score >= entityAliasThreshold && score > bestScore {
					bestScore = score
					best = ex.Key
				}
			}
		}
	}
	if best != "" {
		return best, nil
	}
	return prefix + EntitySlug(e.name), nil
}

// entityAliases reads the aliases attribute of an entity fact.
func entityAliases(f Fact) []string {
	return attrStringList(f, "aliases")
}

// attrStringList reads a string-list attribute written by the enricher.
func attrStringList(f Fact, name string) []string {
	raw, ok := f.Attributes[name].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if a, ok := v.(string); ok {
			out = append(out, a)
		}
	}
	return out
}

// applyTypedEntities upserts validated typed entities: /entities/<type>/
// <slug> facts carrying entity_type, aliases and revision-precise
// source_facts provenance (the exact contributing fact revision IDs),
// mentions edges from each source fact KEY, and co_occurs bumps per
// source fact - the typed counterpart of applyEntities, idempotent per
// (fact, entity) the same way. Re-enriching a NEWER revision of an
// already-linked key appends that revision's ID to source_facts without
// another mention_count or co_occurs bump: key-level mentions dedupe
// must not discard revision provenance.
func (s *Store) applyTypedEntities(ctx context.Context, ns string, ents []typedEntity) (int, error) {
	// resolve canonical keys and merge entries that land on the same one
	// (e.g. a name and its alias extracted separately).
	byKey := map[string]int{}
	var merged []typedEntity
	for _, e := range ents {
		key, err := s.resolveTypedEntityKey(ctx, ns, e)
		if err != nil {
			return 0, err
		}
		e.key = key
		if i, ok := byKey[key]; ok {
			m := &merged[i]
			seen := map[string]bool{}
			for _, k := range m.sources {
				seen[k] = true
			}
			for _, k := range e.sources {
				if !seen[k] {
					seen[k] = true
					m.sources = append(m.sources, k)
				}
			}
			seenID := map[string]bool{}
			for _, id := range m.sourceIDs {
				seenID[id] = true
			}
			for _, id := range e.sourceIDs {
				if !seenID[id] {
					seenID[id] = true
					m.sourceIDs = append(m.sourceIDs, id)
				}
			}
			seenAlias := map[string]bool{}
			for _, a := range m.aliases {
				seenAlias[strings.ToLower(a)] = true
			}
			for _, a := range e.aliases {
				if !seenAlias[strings.ToLower(a)] {
					seenAlias[strings.ToLower(a)] = true
					m.aliases = append(m.aliases, a)
				}
			}
			if m.declared == "" {
				m.declared = e.declared
			}
			continue
		}
		byKey[key] = len(merged)
		merged = append(merged, e)
	}
	if len(merged) == 0 {
		return 0, nil
	}

	// mentions edges already recorded per source fact, live or closed
	// (linkTargetsAll, same guard shape as applyEntities), and source
	// revision IDs already recorded on the entity fact. A re-run is a
	// no-op only when every key is already linked AND every contributing
	// revision is already provenance.
	newOnFact := map[string]map[string]bool{} // source key -> entity keys newly mentioned there
	for i := range merged {
		e := &merged[i]
		var newSources []string
		for _, src := range e.sources {
			targets, err := s.linkTargetsAll(ctx, ns, src, "mentions")
			if err != nil {
				return 0, err
			}
			linked := false
			for _, tk := range targets {
				if tk == e.key {
					linked = true
					break
				}
			}
			if !linked {
				newSources = append(newSources, src)
			}
		}
		cur, err := s.liveByKeys(ctx, ns, []string{e.key})
		if err != nil {
			return 0, err
		}
		priorIDs := map[string]bool{}
		if len(cur) == 1 {
			for _, id := range attrStringList(cur[0], "source_facts") {
				priorIDs[id] = true
			}
		}
		freshID := false
		for _, id := range e.sourceIDs {
			if !priorIDs[id] {
				freshID = true
				break
			}
		}
		if len(newSources) == 0 && !freshID {
			continue // idempotent re-run: same revisions, already linked
		}
		if len(cur) != 1 && len(newSources) == 0 {
			continue // entity dead with every key already linked: do not resurrect
		}
		count := float64(len(newSources))
		name := e.name
		attrs := map[string]any{
			"entity_type":   e.typ,
			"mention_count": count,
			"source_facts":  append([]string(nil), e.sourceIDs...),
		}
		if len(e.aliases) > 0 {
			attrs["aliases"] = append([]string(nil), e.aliases...)
		}
		if e.declared != "" {
			attrs["declared_type"] = e.declared
		}
		if len(cur) == 1 {
			if mc, ok := cur[0].Attributes["mention_count"].(float64); ok {
				attrs["mention_count"] = mc + count
			}
			name = cur[0].Body // keep first-seen casing
			if al := unionStrings(entityAliases(cur[0]), e.aliases); len(al) > 0 {
				attrs["aliases"] = al
			} else {
				delete(attrs, "aliases")
			}
			attrs["source_facts"] = unionStrings(attrStringList(cur[0], "source_facts"), e.sourceIDs)
			if e.declared == "" {
				if d, ok := cur[0].Attributes["declared_type"].(string); ok && d != "" {
					attrs["declared_type"] = d
				}
			}
		}
		if _, err := s.writeNoOutbox(ctx, WriteInput{
			Namespace: ns, Key: e.key, Body: name,
			Attributes: attrs,
			Writer:     "enricher", Importance: 0.3,
		}); err != nil {
			return 0, err
		}
		for _, src := range newSources {
			if err := s.AddLinkWeighted(ctx, ns, src, e.key, "mentions", 1.0); err != nil {
				return 0, err
			}
			if newOnFact[src] == nil {
				newOnFact[src] = map[string]bool{}
			}
			newOnFact[src][e.key] = true
		}
	}

	// co_occurs per source fact: bump a pair when this fact contributed a
	// genuinely new mention on at least one side (same rule as
	// applyEntities).
	bySource := map[string][]string{}
	for _, e := range merged {
		if e.key == "" {
			continue
		}
		for _, src := range e.sources {
			bySource[src] = append(bySource[src], e.key)
		}
	}
	for src, keys := range bySource {
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				a, b := keys[i], keys[j]
				if a == b || (!newOnFact[src][a] && !newOnFact[src][b]) {
					continue
				}
				if err := s.bumpCoOccurs(ctx, ns, a, b); err != nil {
					return 0, err
				}
			}
		}
	}

	return len(merged), nil
}

// unionStrings merges two string lists, order-preserving, case-insensitive
// dedupe. An empty result is reported as nil so the attribute stays
// absent rather than serializing as an empty array.
func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if !seen[strings.ToLower(s)] {
			seen[strings.ToLower(s)] = true
			out = append(out, s)
		}
	}
	return out
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
