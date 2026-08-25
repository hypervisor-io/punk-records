package memory

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
)

// EnrichKey embeds the live fact at ns/key if it lacks a vector, then
// links it (similar_to) to its topN nearest live neighbors with cosine
// >= threshold. Returns links added. Only direct writes (embedding
// column, memory_links) happen here, never memory_outbox, so this can
// never re-trigger itself via the outbox tailer -> bus -> RunEnricher
// loop.
func (s *Store) EnrichKey(ctx context.Context, ns, key string, threshold float64, topN int) (int, error) {
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
	if vecs[self] == nil {
		// write-time embedding failed or embedder arrived later: backfill
		vv, err := s.embedder.Embed(ctx, []string{facts[self].Body})
		if err != nil || len(vv) != 1 {
			return 0, nil // embedding failure never blocks; next event retries
		}
		if _, err := s.db.ExecContext(ctx, s.db.Rebind(
			`UPDATE memories SET embedding = $1 WHERE id = $2`),
			encodeVector(vv[0], s.quantize), facts[self].ID); err != nil {
			return 0, err
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
		if err := s.AddLinkWeighted(ctx, ns, key, c.key, "similar_to", c.score); err != nil {
			return added, err
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
func (s *Store) RunEnricher(ctx context.Context, b *bus.Bus, log *slog.Logger) {
	if s.embedder == nil && s.entityExtractor == nil {
		return
	}
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
		if _, err := s.EnrichEntitiesBatch(ctx, ns, keys); err != nil && ctx.Err() == nil {
			log.Error("entity enrich failed", "ns", ns, "keys", len(keys), "err", err)
		}
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

	for {
		select {
		case <-ctx.Done():
			return
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
			if _, err := s.EnrichKey(ctx, ns, key, 0, 0); err != nil && ctx.Err() == nil {
				log.Error("enrich failed", "ns", ns, "key", key, "err", err)
			}
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
