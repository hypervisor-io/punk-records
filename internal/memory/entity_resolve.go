package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Entity alias resolution and reversible, type-aware merge proposals
// (task G02). Mechanism adapted from Cognee's
// tasks/memify/consolidate_entities.py (pinned 78ff576), reimplemented in
// Go with two deliberate divergences: merges are NEVER destructive (the
// absorbed entity keeps its key, its history and its incoming references;
// lineage makes the merge reversible) and there is no broad fuzzy-name
// auto-merge (a proposal always requires identity evidence, and applying
// it is a separate explicit act).
//
// Resolution order, per the contract: exact aliases first, then
// type-compatible contextual (name similarity) and embedding candidates.
// Candidate discovery is paginated, so entities beyond the old
// first-1000-facts Recall ceiling are no longer invisible.

// ErrProtectedEntityMerge is returned when a proposal touching a
// protected entity type is applied without
// MergeApplyOptions.AllowProtected. Protection is derived from the
// STORED entity types at apply time, never trusted from the
// caller-supplied proposal fields: clearing a proposal's Protected flag
// does not disarm the gate.
var ErrProtectedEntityMerge = errors.New("memory: protected entity type requires explicit AllowProtected")

// ErrEntityMergeConflict is returned when one side of a proposal is
// already absorbed elsewhere (or the canonical target is itself
// absorbed): re-propose so the pair resolves through current lineage.
var ErrEntityMergeConflict = errors.New("memory: entity merge conflicts with existing lineage")

// ErrEntityNotMerged is returned when reverting an entity that carries no
// merged_into lineage.
var ErrEntityNotMerged = errors.New("memory: entity is not merged")

// ErrEntityMergeLineageMissing is returned when reverting a merge whose
// canonical carries no matching lineage record (the canonical fact is
// tombstoned, or its merges attribute was lost): undo cannot subtract the
// merge's contributions safely, so it refuses and leaves the merge -
// including the alias's merged_into/merge_id lineage - fully in place
// rather than reporting a partial undo as success.
var ErrEntityMergeLineageMissing = errors.New("memory: canonical merge lineage record missing")

// ErrEntityMergeStalePlan is returned when an apply or revert planned
// against a canonical/alias revision that a concurrent writer superseded
// before the mutation transaction ran: the planned attributes are stale,
// so the whole mutation is rejected (nothing is written) rather than
// overwriting the other writer's contributions and losing its lineage.
// Re-read and re-propose to retry.
var ErrEntityMergeStalePlan = errors.New("memory: entity merge plan superseded by a concurrent write")

// Evidence kinds on a merge proposal. declared_alias is IDENTITY evidence
// and suffices on its own. shared_source is CORROBORATION: co-occurrence
// in one source revision is contextual, never identity - two distinct
// entities evidenced by the same fact are not aliases just because their
// source_facts overlap - so it only ever combines with name_similarity at
// the extractor's own canonicalization threshold (entityAliasThreshold),
// and never proposes alone. name_similarity and embedding_similarity are
// contextual and can never produce a proposal on their own.
const (
	MergeEvidenceDeclaredAlias = "declared_alias"
	MergeEvidenceSharedSource  = "shared_source"
	MergeEvidenceName          = "name_similarity"
	MergeEvidenceEmbedding     = "embedding_similarity"
)

// Evidence weights; a proposal's score is their capped sum.
const (
	weightDeclaredAlias = 0.7
	weightSharedSource  = 0.5
	weightName          = 0.2
	weightEmbedding     = 0.15
)

// entityMergeEmbeddingCosine is the minimum stored-embedding cosine that
// corroborates a name-similar pair. Reading stored embeddings costs no
// model call; proposals never embed.
const entityMergeEmbeddingCosine = 0.9

// protectedEntityTypes are instance-identity types whose merges are
// higher-stakes: an incident or host is one specific event/machine, and a
// shared name fragment ("inc-2026-0..", "host7") is common and
// meaningless there. They are proposed only on an extractor-declared
// alias, excluded from default discovery, flagged Protected, and refused
// by Apply without explicit AllowProtected - a refusal derived from the
// stored entity types (storedProtectedEntityType), not from the
// caller-supplied proposal.
var protectedEntityTypes = map[string]bool{
	EntityTypeIncident: true,
	EntityTypeHost:     true,
}

// entityScanPageSize bounds one page of the paginated entity scan.
const entityScanPageSize = 500

// resolveMaxHops bounds merged_into chain traversal (cycle guard).
const resolveMaxHops = 8

// latestLivePageSQL is latestLiveSQL plus a keyset cursor ($4): one page
// of newest live revisions under a prefix, ordered by key. $1 ns,
// $2 like-prefix, $3 now, $4 key cursor (exclusive), $5 limit.
const latestLivePageSQL = `
SELECT ` + factCols + `
FROM memories m
JOIN (
    SELECT key, MAX(created_at) AS mc
    FROM memories
    WHERE namespace_id = $1 AND key LIKE $2 ESCAPE '\' AND key > $4
    GROUP BY key
) latest ON m.key = latest.key AND m.created_at = latest.mc
WHERE m.namespace_id = $1 AND m.action <> 'tombstone'
  AND (m.expiration_date IS NULL OR m.expiration_date > $3)
ORDER BY m.key
LIMIT $5`

// scanLivePrefix returns every live fact under prefix via keyset
// pagination - the unbounded replacement for Recall(prefix, 1000), whose
// silent ceiling hid late-sorting keys from entity resolution and merge
// discovery. Malformed rows are quarantined exactly like scanFacts
// (quarantine deletes the row, so the next page from the last good key
// still makes progress).
func (s *Store) scanLivePrefix(ctx context.Context, ns, prefix string) ([]Fact, error) {
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Fact{}, nil
	}
	out := []Fact{}
	cursor := ""
	for {
		rows, err := s.db.QueryContext(ctx, s.db.Rebind(latestLivePageSQL),
			nsID, likePrefix(prefix), store.TimeToDB(s.now()), cursor, entityScanPageSize)
		if err != nil {
			return nil, fmt.Errorf("live prefix page: %w", err)
		}
		n := 0
		var quarantine []*rowError
		for rows.Next() {
			n++
			f, _, serr := scanFactRow(rows, 0)
			if serr != nil {
				var re *rowError
				if errors.As(serr, &re) {
					quarantine = append(quarantine, re)
					continue
				}
				rows.Close()
				return nil, serr
			}
			f.Namespace = ns
			out = append(out, *f)
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			return nil, fmt.Errorf("live prefix rows: %w", rerr)
		}
		for _, b := range quarantine {
			if err := s.quarantineRow(ctx, nsID, b.id, b.raw, b.reason); err != nil {
				return nil, err
			}
		}
		if n < entityScanPageSize {
			break
		}
		if len(out) > 0 {
			cursor = out[len(out)-1].Key // strict key order: progress
		}
	}
	return out, nil
}

// entityKeyType is the type bucket of an entity key: /entities/<type>/
// <slug> carries its type; a legacy untyped /entities/<slug> buckets as
// "". Buckets constrain matching: only same-bucket pairs are ever
// proposed, so an untyped legacy name (no type constraint) is never
// paired with a typed entity.
func entityKeyType(key string) string {
	rest := strings.TrimPrefix(key, "/entities/")
	typ, _, ok := strings.Cut(rest, "/")
	if !ok || !ValidEntityType(typ) {
		return ""
	}
	return typ
}

// EntityMergeEvidence is one piece of evidence for a proposed merge.
type EntityMergeEvidence struct {
	Kind   string  `json:"kind"` // MergeEvidence* constants
	Detail string  `json:"detail"`
	Weight float64 `json:"weight"`
}

// EntityMergeProposal is a dry-run proposal to absorb AliasKey into
// CanonicalKey. It is pure data: proposing writes nothing, and applying
// is the separate, reversible ApplyEntityMerge act. ID is deterministic
// (namespace + alias + canonical), so re-running discovery replays the
// same identity and Apply is idempotent per proposal.
type EntityMergeProposal struct {
	ID            string                `json:"id"`
	Namespace     string                `json:"namespace"`
	CanonicalKey  string                `json:"canonical_key"`
	AliasKey      string                `json:"alias_key"`
	CanonicalName string                `json:"canonical_name"`
	AliasName     string                `json:"alias_name"`
	EntityType    string                `json:"entity_type"` // "" = legacy untyped bucket
	Score         float64               `json:"score"`
	Evidence      []EntityMergeEvidence `json:"evidence"`
	Protected     bool                  `json:"protected"` // protected type: apply needs AllowProtected
}

// MergeProposalOptions controls discovery. Limit caps returned proposals
// (<=0 = 100). IncludeProtected admits protected-type proposals (only
// ever backed by a declared alias); default discovery excludes them.
type MergeProposalOptions struct {
	Limit            int
	IncludeProtected bool
}

// MergeApplyOptions controls ApplyEntityMerge. AllowProtected is the
// explicit reconfirmation a Protected proposal requires.
type MergeApplyOptions struct {
	AllowProtected bool
}

// EntityMergeResult reports an apply or revert. Applied=false means the
// apply was an idempotent replay (no new writes); Reverted marks a
// successful RevertEntityMerge.
type EntityMergeResult struct {
	ProposalID   string `json:"proposal_id"`
	CanonicalKey string `json:"canonical_key"`
	AliasKey     string `json:"alias_key"`
	Applied      bool   `json:"applied"`
	Reverted     bool   `json:"reverted,omitempty"`
}

// entityMergeID is the deterministic proposal identity: replaying
// discovery for the same pair reproduces it, which is what makes Apply
// idempotent per proposal.
func entityMergeID(ns, aliasKey, canonicalKey string) string {
	sum := sha256.Sum256([]byte(ns + "\x00" + aliasKey + "\x00" + canonicalKey))
	return "em-" + hex.EncodeToString(sum[:])[:16]
}

// mentionCountOf reads the enricher's mention_count attribute.
func mentionCountOf(f Fact) float64 {
	mc, _ := f.Attributes["mention_count"].(float64)
	return mc
}

// mergedIntoOf reads the merged_into lineage marker ("" when unmerged).
func mergedIntoOf(f Fact) string {
	m, _ := f.Attributes["merged_into"].(string)
	return m
}

// normSet lowercases/normalizes names for exact-match comparison.
func normSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		if nn := normalizeName(n); nn != "" {
			out[nn] = true
		}
	}
	return out
}

// collectMergeEvidence gathers the evidence for one same-type pair, in a
// fixed kind order so proposals are deterministic. Identity evidence
// (declared alias) comes first, then corroboration (shared source
// revisions) and contextual signals (name similarity, then stored-embedding
// cosine). Embedding similarity is only ever computed for a pair that
// already has name evidence, bounding pair work, and uses stored embeddings
// only - a proposal never makes an embed call.
func (s *Store) collectMergeEvidence(ctx context.Context, ns string, a, b Fact) []EntityMergeEvidence {
	var ev []EntityMergeEvidence
	na, nb := normalizeName(a.Body), normalizeName(b.Body)
	if normSet(entityAliases(a))[nb] {
		ev = append(ev, EntityMergeEvidence{MergeEvidenceDeclaredAlias,
			fmt.Sprintf("name %q declared as alias on %s", b.Body, a.Key), weightDeclaredAlias})
	}
	if normSet(entityAliases(b))[na] {
		ev = append(ev, EntityMergeEvidence{MergeEvidenceDeclaredAlias,
			fmt.Sprintf("name %q declared as alias on %s", a.Body, b.Key), weightDeclaredAlias})
	}
	sa := map[string]bool{}
	for _, id := range attrStringList(a, "source_facts") {
		sa[id] = true
	}
	var shared []string
	for _, id := range attrStringList(b, "source_facts") {
		if sa[id] {
			shared = append(shared, id)
		}
	}
	if len(shared) > 0 {
		show := shared
		if len(show) > 3 {
			show = show[:3]
		}
		ev = append(ev, EntityMergeEvidence{MergeEvidenceSharedSource,
			fmt.Sprintf("%d shared source revision(s): %s", len(shared), strings.Join(show, ", ")), weightSharedSource})
	}
	nameSc := nameSimilarity(a.Body, b.Body)
	if nameSc >= entityAliasThreshold {
		ev = append(ev, EntityMergeEvidence{MergeEvidenceName,
			fmt.Sprintf("name similarity %.2f (%q ~ %q)", nameSc, a.Body, b.Body), weightName})
		raw, err := s.embeddingsByKeys(ctx, ns, []string{a.Key, b.Key})
		if err == nil && len(raw[a.Key]) > 0 && len(raw[b.Key]) > 0 {
			if cos := cosine(decodeVector(raw[a.Key]), decodeVector(raw[b.Key])); cos >= entityMergeEmbeddingCosine {
				ev = append(ev, EntityMergeEvidence{MergeEvidenceEmbedding,
					fmt.Sprintf("stored embedding cosine %.2f", cos), weightEmbedding})
			}
		}
	}
	return ev
}

// pickCanonical deterministically chooses the merge survivor: more
// mentions, then more source provenance, then the fuller display name,
// then the lower key.
func pickCanonical(a, b Fact) (canonical, alias Fact) {
	if ma, mb := mentionCountOf(a), mentionCountOf(b); ma != mb {
		if ma > mb {
			return a, b
		}
		return b, a
	}
	if sa, sb := len(attrStringList(a, "source_facts")), len(attrStringList(b, "source_facts")); sa != sb {
		if sa > sb {
			return a, b
		}
		return b, a
	}
	if len(a.Body) != len(b.Body) {
		if len(a.Body) > len(b.Body) {
			return a, b
		}
		return b, a
	}
	if a.Key <= b.Key {
		return a, b
	}
	return b, a
}

// ProposeEntityMerges is the dry-run candidate pass: it scans every live
// entity (paginated, no first-page ceiling), pairs entities inside their
// type bucket, and emits a proposal for each pair carrying identity
// evidence: an extractor-declared alias, or name equivalence at the
// extractor's own canonicalization threshold corroborated by a shared
// source revision. Context never produces a proposal on its own -
// neither similarity alone (name or embedding) nor co-occurrence alone
// (a shared source revision between distinctly named entities is
// contextual, not identity). Protected types additionally require a
// declared alias and are excluded unless IncludeProtected. Entities
// already absorbed (merged_into set) leave the candidate set - their old
// references resolve through ResolveEntityKey. Pure read: no writes.
func (s *Store) ProposeEntityMerges(ctx context.Context, ns string, opts MergeProposalOptions) ([]EntityMergeProposal, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	ents, err := s.scanLivePrefix(ctx, ns, "/entities")
	if err != nil {
		return nil, err
	}
	buckets := map[string][]Fact{}
	for _, e := range ents {
		if mergedIntoOf(e) != "" {
			continue // absorbed already; not a candidate on either side
		}
		typ := entityKeyType(e.Key)
		buckets[typ] = append(buckets[typ], e)
	}
	types := make([]string, 0, len(buckets))
	for typ := range buckets {
		types = append(types, typ)
	}
	sort.Strings(types)

	var proposals []EntityMergeProposal
	for _, typ := range types {
		bucket := buckets[typ] // scanLivePrefix order: sorted by key
		protected := protectedEntityTypes[typ]
		for i := 0; i < len(bucket); i++ {
			for j := i + 1; j < len(bucket); j++ {
				a, b := bucket[i], bucket[j]
				ev := s.collectMergeEvidence(ctx, ns, a, b)
				declared, shared, nameEq := false, false, false
				score := 0.0
				for _, e := range ev {
					score += e.Weight
					switch e.Kind {
					case MergeEvidenceDeclaredAlias:
						declared = true
					case MergeEvidenceSharedSource:
						shared = true
					case MergeEvidenceName:
						nameEq = true
					}
				}
				// identity gate: a declared alias, or name equivalence at
				// the extractor's canonicalization threshold corroborated
				// by a shared source revision. Shared sources alone are
				// co-occurrence - context, never identity - and similarity
				// alone is context too: no broad fuzzy-name auto-merge.
				if !declared && !(nameEq && shared) {
					continue
				}
				if protected && (!declared || !opts.IncludeProtected) {
					continue
				}
				canonical, alias := pickCanonical(a, b)
				if score > 1 {
					score = 1
				}
				proposals = append(proposals, EntityMergeProposal{
					ID:            entityMergeID(ns, alias.Key, canonical.Key),
					Namespace:     ns,
					CanonicalKey:  canonical.Key,
					AliasKey:      alias.Key,
					CanonicalName: canonical.Body,
					AliasName:     alias.Body,
					EntityType:    typ,
					Score:         score,
					Evidence:      ev,
					Protected:     protected,
				})
			}
		}
	}
	sort.Slice(proposals, func(i, j int) bool {
		if proposals[i].Score != proposals[j].Score {
			return proposals[i].Score > proposals[j].Score
		}
		if proposals[i].AliasKey != proposals[j].AliasKey {
			return proposals[i].AliasKey < proposals[j].AliasKey
		}
		return proposals[i].CanonicalKey < proposals[j].CanonicalKey
	})
	if len(proposals) > limit {
		proposals = proposals[:limit]
	}
	return proposals, nil
}

// ApplyEntityMerge performs one proposed merge, reversibly and atomically:
// the canonical entity gains the alias's names, revision-precise source
// provenance and a lineage record of what this merge contributes; the
// alias entity stays live (old IDs and history preserved) carrying
// merged_into/merge_id lineage; the alias's live mention sources gain
// edges to the canonical while the old edges stay untouched. All
// mutations (mention edges, both fact revisions, lineage) commit in ONE
// transaction: a failure midway rolls the whole apply back, so a failed
// apply never leaves a mention edge or marker behind. The transaction
// also validates the planned predecessor revisions (planning, including
// embedding, happens outside it): a concurrent merge/undo committed in
// between fails the whole apply with ErrEntityMergeStalePlan instead of
// overwriting the other writer's lineage. Idempotent per
// proposal: replaying it is a no-op (Applied=false). Protection is
// derived from the STORED entity types at apply time (a caller-cleared
// Protected flag does not disarm the gate); a protected merge requires
// AllowProtected. Never touches memory_outbox, same as the enricher, so
// a merge never re-triggers the enrich loop.
func (s *Store) ApplyEntityMerge(ctx context.Context, ns string, p EntityMergeProposal, opts MergeApplyOptions) (EntityMergeResult, error) {
	id := p.ID
	if id == "" {
		id = entityMergeID(ns, p.AliasKey, p.CanonicalKey)
	}
	res := EntityMergeResult{ProposalID: id, CanonicalKey: p.CanonicalKey, AliasKey: p.AliasKey}
	// the caller-side Protected flag is an additional gate, never the
	// only one: stored-type derivation follows the live-fact load below.
	if p.Protected && !opts.AllowProtected {
		return res, fmt.Errorf("%w: %s", ErrProtectedEntityMerge, p.EntityType)
	}
	if err := ValidateKey(p.CanonicalKey); err != nil {
		return res, err
	}
	if err := ValidateKey(p.AliasKey); err != nil {
		return res, err
	}
	if p.CanonicalKey == p.AliasKey {
		return res, fmt.Errorf("memory: cannot merge an entity into itself: %s", p.AliasKey)
	}
	facts, err := s.liveByKeys(ctx, ns, []string{p.CanonicalKey, p.AliasKey})
	if err != nil {
		return res, err
	}
	byKey := map[string]Fact{}
	for _, f := range facts {
		byKey[f.Key] = f
	}
	canonical, ok := byKey[p.CanonicalKey]
	if !ok {
		return res, fmt.Errorf("%w: canonical %s", ErrNotFound, p.CanonicalKey)
	}
	alias, ok := byKey[p.AliasKey]
	if !ok {
		return res, fmt.Errorf("%w: alias %s", ErrNotFound, p.AliasKey)
	}
	if entityKeyType(alias.Key) != entityKeyType(canonical.Key) {
		return res, fmt.Errorf("%w: type mismatch %s vs %s", ErrEntityMergeConflict, alias.Key, canonical.Key)
	}
	if !opts.AllowProtected {
		if typ := storedProtectedEntityType(canonical, alias); typ != "" {
			return res, fmt.Errorf("%w: %s", ErrProtectedEntityMerge, typ)
		}
	}
	switch mi := mergedIntoOf(alias); {
	case mi == canonical.Key:
		return res, nil // idempotent replay: this pair is already merged
	case mi != "":
		return res, fmt.Errorf("%w: %s already merged into %s", ErrEntityMergeConflict, alias.Key, mi)
	}
	if mi := mergedIntoOf(canonical); mi != "" {
		return res, fmt.Errorf("%w: canonical %s is itself absorbed by %s", ErrEntityMergeConflict, canonical.Key, mi)
	}

	// classify the alias's LIVE mention sources against the canonical's
	// existing ones (linkSourcesAll includes closed edges, so a
	// user-closed mention is never re-added): contributed is the full
	// required set undo needs (not only the newly-added difference, so a
	// mention shared with another merge survives that merge's revert);
	// added is what this merge actually creates; protected is prelinked
	// edges no recorded merge created - user/extraction-owned edges undo
	// must never close.
	incoming, err := s.Neighbors(ctx, ns, alias.Key, "in")
	if err != nil {
		return res, err
	}
	canonSources, err := s.linkSourcesAll(ctx, ns, canonical.Key, "mentions")
	if err != nil {
		return res, err
	}
	linked := map[string]bool{}
	for _, fk := range canonSources {
		linked[fk] = true
	}
	records := mergeRecords(canonical)
	claimedByMerge := map[string]bool{}
	for _, r := range records {
		for _, m := range recordStringList(r, "added_mentions") {
			claimedByMerge[m] = true
		}
	}
	var contribMentions, addedMentions, protectedMentions []string
	for _, l := range incoming {
		if l.LinkType != "mentions" {
			continue
		}
		contribMentions = append(contribMentions, l.FromKey)
		switch {
		case !linked[l.FromKey]:
			addedMentions = append(addedMentions, l.FromKey)
		case !claimedByMerge[l.FromKey]:
			protectedMentions = append(protectedMentions, l.FromKey)
		}
	}

	// canonical revision: union of names and revision-precise provenance,
	// plus the lineage record of what this merge contributes (the undo
	// data RevertEntityMerge subtracts, keeping whatever another ACTIVE
	// merge also contributed or the canonical already had pre-merge).
	canonAttrs := copyAttrs(canonical.Attributes)
	curAliases := entityAliases(canonical)
	canonName := normalizeName(canonical.Body)
	var addedAliases []string
	seenAlias := normSet(append(curAliases, canonical.Body))
	for _, cand := range append([]string{alias.Body}, entityAliases(alias)...) {
		nc := normalizeName(cand)
		if nc == "" || nc == canonName || seenAlias[nc] {
			continue
		}
		seenAlias[nc] = true
		addedAliases = append(addedAliases, cand)
	}
	curSources := attrStringList(canonical, "source_facts")
	seenSrc := map[string]bool{}
	for _, id := range curSources {
		seenSrc[id] = true
	}
	var addedSources []string
	for _, id := range attrStringList(alias, "source_facts") {
		if !seenSrc[id] {
			seenSrc[id] = true
			addedSources = append(addedSources, id)
		}
	}
	if al := unionStrings(curAliases, addedAliases); len(al) > 0 {
		canonAttrs["aliases"] = al
	}
	if sf := unionStrings(curSources, addedSources); len(sf) > 0 {
		canonAttrs["source_facts"] = sf
	}
	canonAttrs["mention_count"] = mentionCountOf(canonical) + float64(len(addedMentions))
	if !hasMergeRecord(canonical, id) {
		record := map[string]any{
			"merge_id":                 id,
			"alias_key":                alias.Key,
			"contributed_aliases":      append([]string{alias.Body}, entityAliases(alias)...),
			"contributed_source_facts": attrStringList(alias, "source_facts"),
			"contributed_mentions":     contribMentions,
			"added_mentions":           addedMentions,
			"protected_mentions":       protectedMentions,
		}
		if len(records) == 0 {
			// first merge on this canonical: snapshot the canonical's own
			// pre-merge state so no later undo ever subtracts what the
			// canonical had before any merge contributed.
			canonAttrs["merge_base"] = map[string]any{
				"aliases":      entityAliases(canonical),
				"source_facts": attrStringList(canonical, "source_facts"),
				"mentions":     canonSources,
			}
		}
		merges, _ := canonAttrs["merges"].([]any)
		canonAttrs["merges"] = append(merges, record)
	}

	// alias revision: lineage markers only - same key, same body, same
	// history. Nothing is tombstoned or deleted.
	aliasAttrs := copyAttrs(alias.Attributes)
	aliasAttrs["merged_into"] = canonical.Key
	aliasAttrs["merge_id"] = id

	// embeddings are computed BEFORE the transaction opens (sqlite holds
	// its single connection inside a tx; an embedder call must never
	// stall it - same rule as AddLinkDescribed).
	canonEmb := s.mergeRevisionEmbedding(ctx, canonical.Key, canonical.Body)
	aliasEmb := s.mergeRevisionEmbedding(ctx, alias.Key, alias.Body)
	now := s.now()
	if err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, src := range addedMentions {
			if err := s.addLinkTx(ctx, tx, ns, src, canonical.Key, "mentions", 1.0, "", nil, now); err != nil {
				return err
			}
		}
		if err := s.writeMergeRevisionTx(ctx, tx, ns, canonical, canonAttrs, canonEmb, now); err != nil {
			return err
		}
		return s.writeMergeRevisionTx(ctx, tx, ns, alias, aliasAttrs, aliasEmb, now)
	}); err != nil {
		return res, err
	}
	res.Applied = true
	return res, nil
}

// RevertEntityMerge undoes the merge that absorbed aliasKey, atomically:
// superseding revisions remove what that merge contributed (aliases,
// source provenance, mention edges, the lineage record) from the
// canonical and the merged_into/merge_id markers from the alias, all in
// ONE transaction - a failure midway rolls the whole undo back, leaving
// the merge fully in place. The same predecessor validation inside the
// transaction rejects the undo with ErrEntityMergeStalePlan when a
// concurrent writer superseded the planned canonical/alias revision.
// Shared contributions survive: anything
// another ACTIVE merge also contributed, or the canonical already had
// before any merge (its merge_base snapshot), is preserved. History keeps
// the merge: old revisions and the closed merge-added edges remain
// visible to as-of reads. Reverting an unmerged entity is
// ErrEntityNotMerged; reverting one whose canonical lineage record is
// gone is ErrEntityMergeLineageMissing, and refuses before touching the
// alias's lineage.
func (s *Store) RevertEntityMerge(ctx context.Context, ns, aliasKey string) (EntityMergeResult, error) {
	res := EntityMergeResult{AliasKey: aliasKey}
	if err := ValidateKey(aliasKey); err != nil {
		return res, err
	}
	facts, err := s.liveByKeys(ctx, ns, []string{aliasKey})
	if err != nil {
		return res, err
	}
	if len(facts) != 1 {
		return res, fmt.Errorf("%w: %s", ErrNotFound, aliasKey)
	}
	alias := facts[0]
	canonicalKey := mergedIntoOf(alias)
	if canonicalKey == "" {
		return res, fmt.Errorf("%w: %s", ErrEntityNotMerged, aliasKey)
	}
	mergeID, _ := alias.Attributes["merge_id"].(string)
	res.ProposalID = mergeID
	res.CanonicalKey = canonicalKey

	aliasAttrs := copyAttrs(alias.Attributes)
	delete(aliasAttrs, "merged_into")
	delete(aliasAttrs, "merge_id")

	// plan the canonical side (pure reads): subtract this merge's
	// contributed aliases/sources/mentions, keeping anything another
	// recorded ACTIVE merge also contributed or the canonical already
	// had before merges started (merge_base).
	var canonical *Fact
	var canonAttrs map[string]any
	var canonEmb []byte
	var closeMentions []string
	cur, err := s.liveByKeys(ctx, ns, []string{canonicalKey})
	if err != nil {
		return res, err
	}
	if len(cur) != 1 {
		// canonical fact gone: undo cannot subtract this merge's
		// contributions, so it refuses instead of clearing the alias's
		// lineage alone and reporting a partial undo as success.
		return res, fmt.Errorf("%w: canonical %s", ErrEntityMergeLineageMissing, canonicalKey)
	}
	canonical = &cur[0]
	record, rest := splitMergeRecord(*canonical, mergeID)
	if record == nil {
		// the canonical's lineage record is gone (e.g. a revision that
		// dropped merges/merge_base): same refusal - the merge stays
		// fully in place, alias lineage included.
		return res, fmt.Errorf("%w: canonical %s merge %s", ErrEntityMergeLineageMissing, canonicalKey, mergeID)
	}
	{
		otherAliases := map[string]bool{}
		otherSources := map[string]bool{}
		otherMentions := map[string]bool{}
		for _, r := range rest {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			for _, a := range recordStringList(rm, "contributed_aliases") {
				otherAliases[normalizeName(a)] = true
			}
			for _, id := range recordStringList(rm, "contributed_source_facts") {
				otherSources[id] = true
			}
			for _, m := range recordStringList(rm, "contributed_mentions") {
				otherMentions[m] = true
			}
		}
		baseAliases, baseSources, baseMentions := mergeBaseOf(*canonical)
		dropAlias := normSet(recordStringList(record, "contributed_aliases"))
		for n := range otherAliases {
			delete(dropAlias, n)
		}
		for n := range baseAliases {
			delete(dropAlias, n)
		}
		dropSrc := map[string]bool{}
		for _, id := range recordStringList(record, "contributed_source_facts") {
			if !otherSources[id] && !baseSources[id] {
				dropSrc[id] = true
			}
		}
		closeSet := map[string]bool{}
		for _, m := range recordStringList(record, "contributed_mentions") {
			closeSet[m] = true
		}
		for m := range otherMentions {
			delete(closeSet, m)
		}
		for m := range baseMentions {
			delete(closeSet, m)
		}
		for _, m := range recordStringList(record, "protected_mentions") {
			delete(closeSet, m)
		}
		for m := range closeSet {
			closeMentions = append(closeMentions, m)
		}
		sort.Strings(closeMentions) // deterministic close order
		var keepAliases []string
		for _, a := range entityAliases(*canonical) {
			if !dropAlias[normalizeName(a)] {
				keepAliases = append(keepAliases, a)
			}
		}
		var keepSources []string
		for _, id := range attrStringList(*canonical, "source_facts") {
			if !dropSrc[id] {
				keepSources = append(keepSources, id)
			}
		}
		canonAttrs = copyAttrs(canonical.Attributes)
		if len(keepAliases) > 0 {
			canonAttrs["aliases"] = keepAliases
		} else {
			delete(canonAttrs, "aliases")
		}
		if len(keepSources) > 0 {
			canonAttrs["source_facts"] = keepSources
		} else {
			delete(canonAttrs, "source_facts")
		}
		if mc := mentionCountOf(*canonical) - float64(len(closeMentions)); mc > 0 {
			canonAttrs["mention_count"] = mc
		} else {
			canonAttrs["mention_count"] = 0.0
		}
		if len(rest) > 0 {
			canonAttrs["merges"] = rest
		} else {
			delete(canonAttrs, "merges")
			delete(canonAttrs, "merge_base") // last merge undone: snapshot retired
		}
		canonEmb = s.mergeRevisionEmbedding(ctx, canonical.Key, canonical.Body)
	}
	aliasEmb := s.mergeRevisionEmbedding(ctx, alias.Key, alias.Body)
	now := s.now()
	if err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.writeMergeRevisionTx(ctx, tx, ns, *canonical, canonAttrs, canonEmb, now); err != nil {
			return err
		}
		// close exactly the mention edges this merge contributed and
		// no active merge still needs (still-live ones; an edge a
		// user closed meanwhile stays closed).
		for _, src := range closeMentions {
			if err := s.invalidateLinkTx(ctx, tx, ns, src, canonicalKey, "mentions", now); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		return s.writeMergeRevisionTx(ctx, tx, ns, alias, aliasAttrs, aliasEmb, now)
	}); err != nil {
		return res, err
	}
	res.Reverted = true
	return res, nil
}

// ResolveEntityKey follows merged_into lineage from an entity key to its
// current canonical key, so references written before a merge (mention
// edges, stored entity keys) keep resolving. A key with no lineage
// (including a non-entity or unknown key) returns itself unchanged. A
// lineage cycle is an error, never an infinite loop.
func (s *Store) ResolveEntityKey(ctx context.Context, ns, key string) (string, error) {
	seen := map[string]bool{}
	cur := key
	for hop := 0; hop < resolveMaxHops; hop++ {
		facts, err := s.liveByKeys(ctx, ns, []string{cur})
		if err != nil {
			return "", err
		}
		if len(facts) != 1 {
			return cur, nil // tombstoned or never existed: last known key
		}
		next := mergedIntoOf(facts[0])
		if next == "" {
			return cur, nil
		}
		if seen[next] {
			return "", fmt.Errorf("memory: merged_into cycle at %s", next)
		}
		seen[cur] = true
		cur = next
	}
	return "", fmt.Errorf("memory: merged_into chain from %s exceeds %d hops", key, resolveMaxHops)
}

// storedProtectedEntityType derives the protected entity type from
// STORED state - the /entities/<type>/ key path first, then the
// enricher's entity_type attribute - across the given facts, or "" when
// none is protected. ApplyEntityMerge uses it so protection never
// depends on caller-controlled proposal fields.
func storedProtectedEntityType(facts ...Fact) string {
	for _, f := range facts {
		if typ := entityKeyType(f.Key); protectedEntityTypes[typ] {
			return typ
		}
		if attr, ok := f.Attributes["entity_type"].(string); ok {
			if typ := strings.ToLower(strings.TrimSpace(attr)); protectedEntityTypes[typ] {
				return typ
			}
		}
	}
	return ""
}

// mergeRevisionEmbedding precomputes a merge revision's embedding BEFORE
// its transaction opens (same rule as AddLinkDescribed: never hold
// sqlite's single-connection tx across an embedder call). Key and body
// are unchanged by a merge, so the revision embeds the same text its
// predecessor did; embed failure degrades to a nil vector exactly like
// write(), and BackfillEmbeddings catches up.
func (s *Store) mergeRevisionEmbedding(ctx context.Context, key, body string) []byte {
	if s.embedder == nil {
		return nil
	}
	if vecs, err := s.embedder.Embed(ctx, []string{embedText(key, body)}); err == nil && len(vecs) == 1 {
		return encodeVector(vecs[0], s.quantize)
	}
	return nil
}

// writeMergeRevisionTx writes one superseding entity revision against a
// caller-owned transaction: the merge/undo-scoped counterpart of
// writeNoOutbox with the same dedup, predecessor-invalidation and insert
// shape, minus outbox, defense and embedder (a merge revision reuses the
// already-stored, already-scrubbed entity body, and its embedding is
// precomputed by the caller so the tx is never held across a model
// call). This is what lets ApplyEntityMerge and RevertEntityMerge commit
// facts, links and lineage in one atomic transaction.
func (s *Store) writeMergeRevisionTx(ctx context.Context, tx *sql.Tx, ns string, prev Fact, attrs map[string]any, embedding []byte, now time.Time) error {
	nsID, err := s.ensureNamespace(ctx, tx, ns)
	if err != nil {
		return err
	}
	attrsJSON := "{}"
	if len(attrs) > 0 {
		raw, err := json.Marshal(attrs)
		if err != nil {
			return fmt.Errorf("marshal attributes: %w", err)
		}
		attrsJSON = string(raw)
	}
	dbNow := store.TimeToDB(now)
	hash := contentHash(prev.Key, prev.Body, attrsJSON)
	// idempotent, same guard as write(): the live revision already
	// carrying this exact content is a duplicate concurrent write -
	// corroborate it instead of inserting a second copy.
	var liveID, liveHash, liveAction string
	herr := tx.QueryRowContext(ctx, s.db.Rebind(`
		SELECT id, content_hash, action FROM memories
		WHERE namespace_id = $1 AND key = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`), nsID, prev.Key).Scan(&liveID, &liveHash, &liveAction)
	if herr == nil && liveHash == hash && liveAction != "tombstone" {
		_, err := tx.ExecContext(ctx, s.db.Rebind(
			`UPDATE memories SET reinforcements = reinforcements + 1 WHERE id = $1`), liveID)
		return err
	}
	if herr != nil && !errors.Is(herr, sql.ErrNoRows) {
		return herr
	}
	// predecessor CAS: Apply/Revert compute their planned attrs from prev
	// OUTSIDE this transaction (model calls included), so a bare tx's
	// atomicity cannot protect the plan - a concurrent merge/undo that
	// commits in between leaves prev superseded, and writing the stale
	// plan would drop the other writer's contributions (lost merge
	// lineage). Validate inside the tx that the latest revision is still
	// the planned predecessor; reject so the caller re-proposes from
	// current state. ErrNoRows = the fact vanished since planning
	// (quarantine deletes malformed rows): same stale-plan refusal.
	if herr != nil || liveID != prev.ID {
		return fmt.Errorf("%w: %s (expected predecessor %s)", ErrEntityMergeStalePlan, prev.Key, prev.ID)
	}
	action, err := s.liveAction(ctx, tx, nsID, prev.Key)
	if err != nil {
		return err
	}
	if s.mergePreInvalidateHook != nil {
		if err := s.mergePreInvalidateHook(ctx, tx, nsID, prev); err != nil {
			return err
		}
	}
	// the mutation predicate ITSELF binds the exact planned predecessor:
	// on read-committed databases a concurrent writer can commit after
	// the CAS read above but before this UPDATE starts, and the UPDATE
	// then evaluates against a fresh snapshot. Without id = prev.ID that
	// snapshot still satisfies namespace/key/invalid_at IS NULL via the
	// OTHER writer's new live row, and the stale plan would invalidate it
	// and commit undetected. Binding id turns that window into zero
	// matched rows: the planned predecessor is no longer live.
	inv, err := tx.ExecContext(ctx, s.db.Rebind(`
		UPDATE memories SET invalid_at = $1
		WHERE namespace_id = $2 AND key = $3 AND id = $4 AND invalid_at IS NULL`),
		dbNow, nsID, prev.Key, prev.ID)
	if err != nil {
		return fmt.Errorf("invalidate predecessor: %w", err)
	}
	// exactly one row must invalidate: the planned predecessor and
	// nothing else. Zero rows = a concurrent writer superseded it in the
	// window above (stale plan; retryable via re-propose). A RowsAffected
	// error is never ignored - the invalidation count is then unknown,
	// so the plan is equally unverifiable and rejected the same way.
	n, err := inv.RowsAffected()
	if err != nil {
		return fmt.Errorf("%w: %s (rows affected: %w)", ErrEntityMergeStalePlan, prev.Key, err)
	}
	if n != 1 {
		return fmt.Errorf("%w: %s (expected predecessor %s)", ErrEntityMergeStalePlan, prev.Key, prev.ID)
	}
	_, err = tx.ExecContext(ctx, s.db.Rebind(
		`INSERT INTO memories (id, namespace_id, key, action, body, attributes, author, created_at,
		                       writer, confidence, valid_at, embedding, content_hash, importance)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`),
		newID(), nsID, prev.Key, action, prev.Body, attrsJSON, "", dbNow,
		"entity-merge", 0.0, dbNow, embedding, hash, prev.Importance)
	if err != nil {
		return fmt.Errorf("insert memory: %w", err)
	}
	return nil
}

// mergeBaseOf reads the canonical's merge_base snapshot: the canonical's
// own aliases, source_facts and incoming mention sources as they stood
// before its FIRST merge. Undo never subtracts anything the canonical
// already had pre-merge, even when a merge also contributed the same
// item. Absent snapshot (no merge ever recorded) yields empty sets.
func mergeBaseOf(f Fact) (aliases, sources, mentions map[string]bool) {
	aliases, sources, mentions = map[string]bool{}, map[string]bool{}, map[string]bool{}
	base, ok := f.Attributes["merge_base"].(map[string]any)
	if !ok {
		return aliases, sources, mentions
	}
	for _, a := range recordStringList(base, "aliases") {
		aliases[normalizeName(a)] = true
	}
	for _, id := range recordStringList(base, "source_facts") {
		sources[id] = true
	}
	for _, m := range recordStringList(base, "mentions") {
		mentions[m] = true
	}
	return aliases, sources, mentions
}

// copyAttrs shallow-copies a fact attribute map for a superseding
// revision.
func copyAttrs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeRecords reads the canonical's merges lineage attribute: raw
// []any of map[string]any after the JSON round-trip.
func mergeRecords(f Fact) []map[string]any {
	raw, _ := f.Attributes["merges"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// hasMergeRecord reports whether the canonical already carries a lineage
// record for mergeID (partial-replay guard: never append it twice).
func hasMergeRecord(canonical Fact, mergeID string) bool {
	for _, r := range mergeRecords(canonical) {
		if r["merge_id"] == mergeID {
			return true
		}
	}
	return false
}

// splitMergeRecord separates the lineage record for mergeID from the
// rest, in original order. nil record means the canonical has none.
func splitMergeRecord(canonical Fact, mergeID string) (record map[string]any, rest []any) {
	raw, _ := canonical.Attributes["merges"].([]any)
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if ok && m["merge_id"] == mergeID && record == nil {
			record = m
			continue
		}
		rest = append(rest, r)
	}
	return record, rest
}

// recordStringList reads a string list off a lineage record.
func recordStringList(rec map[string]any, name string) []string {
	raw, _ := rec[name].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
