package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
)

// ErrEmbedding marks a failure of the embedder call itself so stage
// runners can classify it into the model failure domain ("model")
// instead of the store domain. The wrapped upstream message flows to
// logs only; run rows record the sanitized class, never raw text.
var ErrEmbedding = errors.New("embedding failed")

// EnrichKey embeds the live fact at ns/key if it lacks a vector, then
// links it (similar_to) to its topN nearest live neighbors with cosine
// >= threshold. Returns links added. Embedder failures are returned
// (wrapped in ErrEmbedding) so stage runners record them truthfully
// instead of silently succeeding and blocking recovery retries. Every
// derived write is fenced on the source revision read at entry
// (revisionStillLive): once a newer revision owns the key, this pass
// stops writing and the newer revision's run derives instead. Only
// direct writes (embedding column, memory_links) happen here, never
// memory_outbox, so this can never re-trigger itself via the outbox
// tailer -> bus -> RunEnricher loop.
func (s *Store) EnrichKey(ctx context.Context, ns, key string, threshold float64, topN int) (int, error) {
	return s.enrichKey(ctx, ns, key, threshold, topN, nil)
}

// enrichKey is EnrichKey with an optional pipeline run fence (P01): when
// fence is non-nil, every derived write commits inside fencedStageTx,
// which re-verifies the active claim AND the source revision in the same
// transaction as the write - a same-revision reclaim (lease expired,
// replacement attempt finished) fences the stale attempt's writes off,
// where revisionStillLive alone still sees the same live revision. A
// fence trip stops the pass quietly: the replacement attempt owns the
// key's derived state now.
func (s *Store) enrichKey(ctx context.Context, ns, key string, threshold float64, topN int, fence *runFence) (int, error) {
	if s.embedder == nil {
		return 0, nil
	}
	if threshold <= 0 {
		threshold = 0.75
	}
	if topN <= 0 {
		topN = 2
	}
	facts, vecs, err := s.liveWithVectors(ctx, ns)
	if err != nil {
		return 0, err
	}
	self := -1
	for i, f := range facts {
		if f.Key == key {
			self = i
			break
		}
	}
	if self == -1 {
		return 0, nil // tombstoned or gone between event and processing
	}
	rev := facts[self].ID
	if vecs[self] == nil {
		// write-time embedding failed or embedder arrived later: backfill.
		// Fence first - a newer revision already owning the key makes this
		// revision's backfill pointless model work.
		ok, err := s.revisionStillLive(ctx, ns, key, rev)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, nil
		}
		vv, err := s.embedder.Embed(ctx, []string{facts[self].Body})
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrEmbedding, err)
		}
		if len(vv) != 1 {
			return 0, fmt.Errorf("%w: embedder returned %d vectors for 1 input", ErrEmbedding, len(vv))
		}
		if fence == nil {
			if _, err := s.db.ExecContext(ctx, s.db.Rebind(
				`UPDATE memories SET embedding = $1 WHERE id = $2`),
				encodeVector(vv[0], s.quantize), facts[self].ID); err != nil {
				return 0, err
			}
		} else {
			applied, err := s.fencedStageTx(ctx, ns, key, *fence, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, s.db.Rebind(
					`UPDATE memories SET embedding = $1 WHERE id = $2`),
					encodeVector(vv[0], s.quantize), facts[self].ID)
				return err
			})
			if err != nil {
				return 0, err
			}
			if !applied {
				return 0, nil
			}
		}
		vecs[self] = vv[0]
	}
	// existing outgoing similar_to targets, live or closed - linkTargetsAll,
	// not Neighbors, because a closed target must stay excluded from the
	// candidate pool too. Otherwise every enrich call re-proposes a
	// user-closed edge (a no-op insert, ON CONFLICT DO NOTHING) and it
	// still consumes a topN slot, starving a genuinely new neighbor of
	// budget. This is what keeps EnrichKey idempotent even across a close.
	existing := map[string]bool{key: true}
	targets, err := s.linkTargetsAll(ctx, ns, key, "similar_to")
	if err != nil {
		return 0, err
	}
	for _, tk := range targets {
		existing[tk] = true
	}
	type cand struct {
		key   string
		score float64
	}
	var cands []cand
	for i := range facts {
		if i == self || vecs[i] == nil || existing[facts[i].Key] {
			continue
		}
		if sc := cosine(vecs[self], vecs[i]); sc >= threshold {
			cands = append(cands, cand{facts[i].Key, sc})
		}
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].score > cands[b].score })
	added := 0
	for _, c := range cands {
		if added >= topN {
			break
		}
		// fence at the write boundary: the embedder call above can take
		// seconds, and a newer source revision written during it owns the
		// key's derived links from here on. Under a pipeline run the fence
		// is transactional (fencedStageTx) and also pins the active claim,
		// closing the same-revision reclaim window revisionStillLive
		// cannot see.
		if fence == nil {
			ok, err := s.revisionStillLive(ctx, ns, key, rev)
			if err != nil {
				return added, err
			}
			if !ok {
				return added, nil
			}
			if err := s.AddLinkWeighted(ctx, ns, key, c.key, "similar_to", c.score); err != nil {
				return added, err
			}
		} else {
			applied, err := s.fencedStageTx(ctx, ns, key, *fence, func(tx *sql.Tx) error {
				return s.addLinkTx(ctx, tx, ns, key, c.key, "similar_to", c.score, "", nil, s.now())
			})
			if err != nil {
				return added, err
			}
			if !applied {
				return added, nil
			}
		}
		added++
	}
	return added, nil
}

// entityBatchKeys is how many pending facts accumulate per namespace
// before entity extraction flushes as one batched model call
// instead of one call per message. entityBatchMaxWait bounds how long
// a straggler waits when the batch never fills.
const (
	entityBatchKeys    = 8
	entityBatchMaxWait = 30 * time.Second
)

// runEmbedLinkStage executes the embed-link stage for one key under a
// claimed pipeline run (P01). claimed=false means the delivery was a
// duplicate or another worker owns the run: the stage is not
// re-executed. A crash between EnrichKey's writes and finishPipelineRun
// leaves the run running; recovery requeues it and the idempotent stage
// re-runs without duplicating links.
func (s *Store) runEmbedLinkStage(ctx context.Context, ns, key string, log *slog.Logger) {
	if s.embedder == nil {
		return
	}
	run, attempt, claimed, err := s.claimPipelineWork(ctx, ns, StageEmbedLink, embedLinkStageVersion, key)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("embed-link claim failed", "ns", ns, "key", key, "err", err)
		}
		return
	}
	if !claimed {
		return
	}
	added, err := s.enrichKey(ctx, ns, key, 0, 0, &runFence{runID: run.ID, attempt: attempt, rev: run.SourceRevision})
	if err != nil {
		if ctx.Err() != nil {
			return // shutdown mid-stage: leave running for restart recovery
		}
		// truthful outcome: an embedder failure fails the run (sanitized
		// class, model domain) so recovery retries it within budget,
		// instead of a silent success that blocks retry forever.
		fallback := "store"
		if errors.Is(err, ErrEmbedding) {
			fallback = "model"
		}
		if _, ferr := s.finishPipelineRun(ctx, run.ID, attempt, RunFailed, nil, classifyStageError(err, fallback)); ferr != nil {
			log.Error("embed-link finish failed", "ns", ns, "key", key, "err", ferr)
		}
		log.Error("enrich failed", "ns", ns, "key", key, "err", err)
		return
	}
	// truthful outcome on success too: when the source revision moved on
	// mid-stage, the fenced writes were skipped and a newer revision's
	// run owns the work - this run is superseded, not succeeded.
	live, err := s.revisionStillLive(ctx, ns, key, run.SourceRevision)
	if err != nil {
		if ctx.Err() == nil {
			if _, ferr := s.finishPipelineRun(ctx, run.ID, attempt, RunFailed, nil, "store"); ferr != nil {
				log.Error("embed-link finish failed", "ns", ns, "key", key, "err", ferr)
			}
			log.Error("embed-link fence check failed", "ns", ns, "key", key, "err", err)
		}
		return
	}
	if !live {
		if _, err := s.finishPipelineRun(ctx, run.ID, attempt, RunSuperseded, nil, ""); err != nil && ctx.Err() == nil {
			log.Error("embed-link finish failed", "ns", ns, "key", key, "err", err)
		}
		return
	}
	items := int64(added)
	if _, err := s.finishPipelineRun(ctx, run.ID, attempt, RunSucceeded, &items, ""); err != nil && ctx.Err() == nil {
		log.Error("embed-link finish failed", "ns", ns, "key", key, "err", err)
	}
}

// flushEntityStage executes the batched entity stage for keys under one
// claimed run per key. The model call stays batched: one
// ExtractStructured call for the whole batch when the extractor
// implements StructuredEntityExtractor (the G01 typed path - canonical
// /entities/<type>/<slug> keys with revision-precise source_facts
// provenance, degraded to legacy names of type unknown on a structured
// failure), one ExtractBatch call when it only supports legacy batching,
// and a per-key Extract loop as the last resort. Each run's items is the
// mentions edges its key gained during the run, so an idempotent retry
// after a crash records 0 instead of double-counting the interrupted
// attempt's output.
//
// Every key's derived writes land inside fencedStageTx: the same
// transaction re-verifies the active claim (run still running at this
// attempt) and the claimed revision's liveness before any write commits,
// so a stale worker can never emit derived state a replacement attempt
// or a newer revision's run owns - finishPipelineRun's attempt guard
// alone protects only the status row, and revisionStillLive alone cannot
// see a same-revision reclaim. A batch-level extraction error fails
// every claimed run in the batch with the same sanitized class; keys
// already applied are retried idempotently by recovery.
func (s *Store) flushEntityStage(ctx context.Context, ns string, keys []string, log *slog.Logger) {
	if s.entityExtractor == nil || len(keys) == 0 {
		return
	}
	type claimedRun struct {
		key     string
		rev     string
		runID   int64
		attempt int
		before  int
	}
	var claims []claimedRun
	for _, key := range keys {
		run, attempt, claimed, err := s.claimPipelineWork(ctx, ns, StageEntities, entitiesStageVersion, key)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("entity claim failed", "ns", ns, "key", key, "err", err)
			}
			continue
		}
		if !claimed {
			continue
		}
		before, err := s.countMentions(ctx, ns, key)
		if err != nil {
			if ctx.Err() == nil {
				if _, ferr := s.finishPipelineRun(ctx, run.ID, attempt, RunFailed, nil, "store"); ferr != nil {
					log.Error("entity finish failed", "ns", ns, "key", key, "err", ferr)
				}
				log.Error("entity pre-count failed", "ns", ns, "key", key, "err", err)
			}
			continue
		}
		claims = append(claims, claimedRun{key, run.SourceRevision, run.ID, attempt, before})
	}
	if len(claims) == 0 {
		return
	}
	finish := func(c claimedRun, status string, items *int64, errClass string) {
		if _, err := s.finishPipelineRun(ctx, c.runID, c.attempt, status, items, errClass); err != nil && ctx.Err() == nil {
			log.Error("entity finish failed", "ns", ns, "key", c.key, "err", err)
		}
	}
	claimKeys := make([]string, len(claims))
	for i, c := range claims {
		claimKeys[i] = c.key
	}
	live, err := s.liveByKeys(ctx, ns, claimKeys)
	if err != nil {
		if ctx.Err() == nil {
			for _, c := range claims {
				finish(c, RunFailed, nil, "store")
			}
			log.Error("entity live read failed", "ns", ns, "keys", len(claims), "err", err)
		}
		return
	}
	byKey := make(map[string]Fact, len(live))
	for _, f := range live {
		byKey[f.Key] = f
	}
	// Narrow to claims whose revision is still live: entity facts are
	// never re-extracted (same guard as EnrichEntitiesBatch), and a
	// revision that moved on between claim and read is the newer
	// revision's run's work already.
	var batch []claimedRun
	var batchFacts []Fact
	for _, c := range claims {
		if hasPrefix(c.key, "/entities/") {
			zero := int64(0)
			finish(c, RunSucceeded, &zero, "")
			continue
		}
		f, ok := byKey[c.key]
		if !ok || f.ID != c.rev {
			finish(c, RunSuperseded, nil, "")
			continue
		}
		batch = append(batch, c)
		batchFacts = append(batchFacts, f)
	}
	if len(batch) == 0 {
		return
	}

	// Extraction. batchNames is the legacy name list per batch key;
	// batchTyped is the validated typed entities partitioned per cited
	// source key: each key's run applies - under its own fence - exactly
	// the slice its revision evidenced, so an entity citing several keys
	// earns each mention when that key's fenced apply lands.
	var batchNames [][]string
	var batchTyped map[string][]typedEntity
	if se, ok := s.entityExtractor.(StructuredEntityExtractor); ok {
		sources := make([]EntitySource, len(batchFacts))
		for i, f := range batchFacts {
			sources[i] = EntitySource{ID: f.ID, Key: f.Key, Body: f.Body}
		}
		raw, err := se.ExtractStructured(ctx, sources)
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown mid-stage: leave runs running for restart recovery
			}
			if raw, err = s.legacyExtractionAsStructured(ctx, batchFacts); err != nil {
				if ctx.Err() != nil {
					return // shutdown mid-stage: leave runs running for restart recovery
				}
				class := classifyStageError(err, "model")
				for _, bc := range batch {
					finish(bc, RunFailed, nil, class)
				}
				log.Error("entity enrich failed", "ns", ns, "keys", len(batch), "err", err)
				return
			}
		}
		batchTyped = map[string][]typedEntity{}
		for _, e := range s.validateExtracted(raw, batchFacts) {
			for i, src := range e.sources {
				sub := e
				sub.sources = []string{src}
				sub.sourceIDs = []string{e.sourceIDs[i]}
				batchTyped[src] = append(batchTyped[src], sub)
			}
		}
	} else {
		bodies := make([]string, len(batchFacts))
		for i, f := range batchFacts {
			bodies[i] = f.Body
		}
		if batcher, ok := s.entityExtractor.(BatchEntityExtractor); ok && len(batch) > 1 {
			if l, err := batcher.ExtractBatch(ctx, bodies); err == nil && len(l) == len(batch) {
				batchNames = l
			}
			// a batch failure or a miscounted answer degrades to the per-key
			// loop below rather than dropping the batch: correctness over the
			// batching saving (same rule as EnrichEntitiesBatch).
		}
		if batchNames == nil {
			batchNames = make([][]string, len(batch))
			for i := range batch {
				names, err := s.entityExtractor.Extract(ctx, bodies[i])
				if err != nil {
					if ctx.Err() != nil {
						return // shutdown mid-stage: leave runs running for restart recovery
					}
					class := classifyStageError(err, "model")
					for _, bc := range batch {
						finish(bc, RunFailed, nil, class)
					}
					log.Error("entity enrich failed", "ns", ns, "keys", len(batch), "err", err)
					return
				}
				batchNames[i] = names
			}
		}
	}

	for i, c := range batch {
		var plan *entityApplyPlan
		var err error
		if batchTyped != nil {
			plan, err = s.planTypedSourceApply(ctx, ns, c.key, batchTyped[c.key])
		} else {
			plan, err = s.planEntityApply(ctx, ns, c.key, batchNames[i])
		}
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown mid-stage: leave the rest running for restart recovery
			}
			finish(c, RunFailed, nil, classifyStageError(err, "store"))
			log.Error("entity apply plan failed", "ns", ns, "key", c.key, "err", err)
			continue
		}
		applied, err := s.fencedStageTx(ctx, ns, c.key, runFence{c.runID, c.attempt, c.rev}, func(tx *sql.Tx) error {
			return s.execEntityApplyTx(ctx, tx, ns, c.key, plan)
		})
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown mid-stage: leave the rest running for restart recovery
			}
			finish(c, RunFailed, nil, classifyStageError(err, "store"))
			log.Error("entity apply failed", "ns", ns, "key", c.key, "err", err)
			continue
		}
		if !applied {
			finish(c, RunSuperseded, nil, "")
			continue
		}
		after, err := s.countMentions(ctx, ns, c.key)
		if err != nil {
			if ctx.Err() == nil {
				finish(c, RunFailed, nil, "store")
				log.Error("entity post-count failed", "ns", ns, "key", c.key, "err", err)
			}
			continue
		}
		items := int64(after - c.before)
		finish(c, RunSucceeded, &items, "")
	}
}

// entityApplyPlan is the precomputed derived write set for one source
// key's extraction output: entity fact upserts (the embedding already
// computed per write, so the fence transaction never holds its
// connection across a model call), the mentions edges to add from the
// source key, and the co_occurs pairs to bump. Planning reads current
// state outside the fence transaction; execEntityApplyTx writes the plan
// inside it, after the fence re-verifies ownership of the source
// revision.
type entityApplyPlan struct {
	writes  []*writePlan
	links   []string
	coPairs [][2]string
}

// execEntityApplyTx writes one source key's plan inside the fence
// transaction: entity upserts (never via the outbox - derived facts must
// not re-trigger the enricher loop), then the mentions edges, then the
// co_occurs bumps. Links and bumps share one instant, matching
// AddLinkDescribed's one-now-per-call rule.
func (s *Store) execEntityApplyTx(ctx context.Context, tx *sql.Tx, ns, srcKey string, plan *entityApplyPlan) error {
	for _, wp := range plan.writes {
		if _, err := s.writeTx(ctx, tx, wp, false); err != nil {
			return err
		}
	}
	now := s.now()
	for _, to := range plan.links {
		if err := s.addLinkTx(ctx, tx, ns, srcKey, to, "mentions", 1.0, "", nil, now); err != nil {
			return err
		}
	}
	for _, p := range plan.coPairs {
		if err := s.bumpCoOccursTx(ctx, tx, ns, p[0], p[1], now); err != nil {
			return err
		}
	}
	return nil
}

// planEntityApply computes the legacy (untyped) derived write set for
// one source key: the same canonicalization, mention-count, idempotency
// and co_occurs rules as applyEntities, staged for the fence transaction
// instead of written directly.
func (s *Store) planEntityApply(ctx context.Context, ns, key string, names []string) (*entityApplyPlan, error) {
	plan := &entityApplyPlan{}
	type entity struct{ key, name string }
	seen := map[string]bool{}
	var ents []entity
	for _, n := range names {
		slug, err := s.resolveEntitySlug(ctx, ns, n)
		if err != nil {
			return nil, err
		}
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		ents = append(ents, entity{"/entities/" + slug, n})
	}
	if len(ents) == 0 {
		return plan, nil
	}
	// existing outgoing mentions targets, live or closed - linkTargetsAll,
	// not Neighbors, because a closed target must stay excluded from
	// re-attempt too (same guard shape as applyEntities).
	existing := map[string]bool{}
	targets, err := s.linkTargetsAll(ctx, ns, key, "mentions")
	if err != nil {
		return nil, err
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
			return nil, err
		}
		if len(cur) == 1 {
			if mc, ok := cur[0].Attributes["mention_count"].(float64); ok {
				count = mc + 1
			}
			name = cur[0].Body // keep first-seen casing
		}
		wp, err := s.writePrepare(ctx, WriteInput{
			Namespace: ns, Key: e.key, Body: name,
			Attributes: map[string]any{"mention_count": count},
			Writer:     "enricher", Importance: 0.3,
		})
		if err != nil {
			return nil, err
		}
		plan.writes = append(plan.writes, wp)
		plan.links = append(plan.links, e.key)
		newKeys[e.key] = true
	}
	// co_occurs only where this fact contributed a genuinely new mention
	// on at least one side of the pair (applyEntities' rule).
	for i := 0; i < len(ents); i++ {
		for j := i + 1; j < len(ents); j++ {
			a, b := ents[i].key, ents[j].key
			if !newKeys[a] && !newKeys[b] {
				continue
			}
			plan.coPairs = append(plan.coPairs, [2]string{a, b})
		}
	}
	return plan, nil
}

// planTypedSourceApply computes the typed-mode derived write set one
// source key evidenced: the same canonical-key resolution, merge,
// provenance-union and co_occurs rules as applyTypedEntities, scoped to
// the sub-entities citing this source (subs always carry exactly one
// source) and staged for the fence transaction. An entity citing several
// keys composes across their applies: each key's plan is computed after
// the previous key's fence transaction committed, so the later plan
// reads the earlier apply's entity revision and unions into it. Counts
// stay per source KEY: re-enriching a NEWER revision of an already-
// linked key appends that revision's ID to source_facts without another
// mention_count or co_occurs bump, and attributes the enricher does not
// own (merge lineage like G02's merges/merge_base) survive the derived
// revision via preserveEntityAttrs.
func (s *Store) planTypedSourceApply(ctx context.Context, ns, src string, subs []typedEntity) (*entityApplyPlan, error) {
	plan := &entityApplyPlan{}
	byKey := map[string]int{}
	var merged []typedEntity
	for _, e := range subs {
		key, err := s.resolveTypedEntityKey(ctx, ns, e)
		if err != nil {
			return nil, err
		}
		e.key = key
		if i, ok := byKey[key]; ok {
			m := &merged[i]
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
		return plan, nil
	}
	targets, err := s.linkTargetsAll(ctx, ns, src, "mentions")
	if err != nil {
		return nil, err
	}
	linked := map[string]bool{}
	for _, tk := range targets {
		linked[tk] = true
	}
	newOnSrc := map[string]bool{}
	var entKeys []string
	for i := range merged {
		e := &merged[i]
		entKeys = append(entKeys, e.key)
		newSource := !linked[e.key]
		cur, err := s.liveByKeys(ctx, ns, []string{e.key})
		if err != nil {
			return nil, err
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
		if !newSource && !freshID {
			continue // idempotent re-run: same revision, already linked
		}
		if len(cur) != 1 && !newSource {
			continue // entity dead with the key already linked: do not resurrect
		}
		name := e.name
		// mention counts are per source KEY (applyTypedEntities' rule):
		// this plan contributes one only when src is a genuinely new
		// mentioning key. A newer revision of an already-linked key is a
		// provenance-only update - it appends its ID to source_facts and
		// leaves mention_count (and co_occurs, gated on newOnSrc below)
		// untouched, so one key can never count twice across revisions.
		bump := 0.0
		if newSource {
			bump = 1.0
		}
		attrs := map[string]any{
			"entity_type":   e.typ,
			"mention_count": bump,
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
				attrs["mention_count"] = mc + bump
			}
			name = cur[0].Body // keep first-seen casing
			if al := unionStrings(entityAliases(cur[0]), e.aliases); len(al) > 0 {
				attrs["aliases"] = al
			} else {
				delete(attrs, "aliases")
			}
			attrs["source_facts"] = unionStrings(attrStringList(cur[0], "source_facts"), e.sourceIDs)
			// derived attrs win; everything else the entity carried
			// (declared_type when not redeclared, merge lineage, any
			// other foreign attribute) is preserved, not rebuilt away.
			attrs = preserveEntityAttrs(cur[0].Attributes, attrs)
		}
		wp, err := s.writePrepare(ctx, WriteInput{
			Namespace: ns, Key: e.key, Body: name,
			Attributes: attrs,
			Writer:     "enricher", Importance: 0.3,
		})
		if err != nil {
			return nil, err
		}
		plan.writes = append(plan.writes, wp)
		if newSource {
			plan.links = append(plan.links, e.key)
			newOnSrc[e.key] = true
		}
	}
	for i := 0; i < len(entKeys); i++ {
		for j := i + 1; j < len(entKeys); j++ {
			a, b := entKeys[i], entKeys[j]
			if a == b || (!newOnSrc[a] && !newOnSrc[b]) {
				continue
			}
			plan.coPairs = append(plan.coPairs, [2]string{a, b})
		}
	}
	return plan, nil
}

// countMentions reports how many mentions edges (live or closed) leave
// key; flushEntityStage diffs it around the batch to count what this
// run added.
func (s *Store) countMentions(ctx context.Context, ns, key string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.db.Rebind(`
		SELECT count(*) FROM memory_links
		WHERE namespace = $1 AND from_key = $2 AND link_type = 'mentions'`), ns, key).Scan(&n)
	return n, err
}

// RunEnricher drains memory write events into EnrichKey immediately and
// into batched entity extraction. Embedding stays per-event (one cheap
// local HTTP call, and search freshness wants it now); entity
// extraction is an LLM call, so pending keys buffer per namespace and
// flush as one EnrichEntitiesBatch call when entityBatchKeys accumulate
// or entityBatchMaxWait passes - an order-of-magnitude fewer model
// calls under hook-capture bursts. Slow is fine: the bus drops on
// overflow and the creative pass sweeps up stragglers. Both enrichers
// self-gate on their own nil dependency, so this only needs to skip
// subscribing when neither is configured.
//
// Every stage execution is tracked as a pipeline run (P01): startup and
// the recovery tick requeue runs a crashed process left behind, and the
// work-key claim makes redelivery a no-op.
func (s *Store) RunEnricher(ctx context.Context, b *bus.Bus, log *slog.Logger) {
	if s.embedder == nil && s.entityExtractor == nil {
		return
	}
	s.recoverPipelineRuns(ctx, log) // restart recovery before new work
	ch, cancel := b.Subscribe()
	defer cancel()

	// pending entity work, per namespace, insertion-ordered and deduped.
	pending := map[string][]string{}
	pendingSet := map[string]map[string]bool{}
	flush := func(ns string) {
		keys := pending[ns]
		if len(keys) == 0 {
			return
		}
		delete(pending, ns)
		delete(pendingSet, ns)
		s.flushEntityStage(ctx, ns, keys, log)
	}
	flushAll := func() {
		for ns := range pending {
			flush(ns)
		}
	}
	var tick *time.Ticker
	var tickC <-chan time.Time
	if s.entityExtractor != nil {
		tick = time.NewTicker(entityBatchMaxWait)
		defer tick.Stop()
		tickC = tick.C
	}
	recoverTick := time.NewTicker(pipelineRecoveryInterval)
	defer recoverTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-recoverTick.C:
			s.recoverPipelineRuns(ctx, log)
		case <-tickC:
			flushAll()
		case e, ok := <-ch:
			if !ok {
				flushAll()
				return
			}
			if e.Kind != "memory" || e.Data["action"] == "tombstone" {
				continue
			}
			ns, key := e.Data["namespace"], e.Data["key"]
			s.runEmbedLinkStage(ctx, ns, key, log)
			if s.entityExtractor == nil {
				continue
			}
			if pendingSet[ns] == nil {
				pendingSet[ns] = map[string]bool{}
			}
			if !pendingSet[ns][key] {
				pendingSet[ns][key] = true
				pending[ns] = append(pending[ns], key)
			}
			if len(pending[ns]) >= entityBatchKeys {
				flush(ns)
			}
		}
	}
}
