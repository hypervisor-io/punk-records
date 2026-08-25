package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Consolidate is the Vegapunk "sync once a day": fold a region's append
// log into consolidated knowledge (B6). Two layers:
//
//	Deterministic (always): drop superseded/tombstoned revisions older
//	than horizon, keeping the latest live revision of every key. Pure
//	compaction; the live view is unchanged, the log shrinks.
//
//	Summarized (optional): when a Summarizer is set, the day's live facts
//	under a prefix are rolled into a /consolidated/<prefix> fact so
//	satellites can recall the gist without re-reading the whole region.
type Summarizer interface {
	Summarize(ctx context.Context, facts []Fact) (string, error)
}

// ConsolidateResult reports one run.
type ConsolidateResult struct {
	Compacted  int64 `json:"compacted"`
	Summarized int   `json:"summarized"`
}

// Consolidate compacts a region and, if sum != nil, writes rollups.
func (s *Store) Consolidate(ctx context.Context, ns string, horizon time.Duration, sum Summarizer) (*ConsolidateResult, error) {
	res := &ConsolidateResult{}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return res, err
	}
	cutoff := store.TimeToDB(s.now().Add(-horizon))

	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		// drop superseded revisions older than the horizon (a newer
		// revision of the same key exists)
		r, err := tx.ExecContext(ctx, s.db.Rebind(`
			DELETE FROM memories WHERE namespace_id = $1 AND created_at < $2 AND EXISTS (
			    SELECT 1 FROM memories m2 WHERE m2.namespace_id = memories.namespace_id
			      AND m2.key = memories.key AND m2.created_at > memories.created_at)`),
			nsID, cutoff)
		if err != nil {
			return fmt.Errorf("compact superseded: %w", err)
		}
		n, _ := r.RowsAffected()
		res.Compacted += n
		// drop dead (tombstoned) chains older than the horizon
		r, err = tx.ExecContext(ctx, s.db.Rebind(`
			DELETE FROM memories WHERE namespace_id = $1 AND action = 'tombstone' AND created_at < $2
			  AND NOT EXISTS (SELECT 1 FROM memories m2 WHERE m2.namespace_id = memories.namespace_id
			      AND m2.key = memories.key AND m2.created_at > memories.created_at)`),
			nsID, cutoff)
		if err != nil {
			return fmt.Errorf("compact tombstones: %w", err)
		}
		n, _ = r.RowsAffected()
		res.Compacted += n
		return nil
	})
	if err != nil {
		return res, err
	}

	if sum != nil {
		facts, err := s.Recall(ctx, ns, "/", 1000)
		if err != nil {
			return res, err
		}
		// never summarize the summaries
		live := facts[:0]
		for _, f := range facts {
			if !hasPrefix(f.Key, "/consolidated/") {
				live = append(live, f)
			}
		}
		if len(live) > 0 {
			// staleness gate: never re-summarize identical
			// content. Skip when the existing rollup is newer than every
			// live fact.
			prev, err := s.Recall(ctx, ns, "/consolidated/region", 1)
			if err != nil {
				return res, err
			}
			if len(prev) == 1 {
				stale := false
				for _, f := range live {
					if f.CreatedAt.After(prev[0].CreatedAt) {
						stale = true
						break
					}
				}
				if !stale {
					return res, nil
				}
			}
			text, err := sum.Summarize(ctx, live)
			if err != nil {
				return res, fmt.Errorf("summarize: %w", err)
			}
			if _, err := s.Write(ctx, WriteInput{
				Namespace: ns, Key: "/consolidated/region",
				Body: text, Writer: "consolidation",
			}); err != nil {
				return res, err
			}
			res.Summarized = 1
		}
	}
	return res, nil
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// CreativePass is the REM-sleep analogue: scan live
// facts' vectors and link non-obvious neighbors with similar_to edges,
// so bridge discovery has a graph to traverse without manual linking.
// Returns the number of links added.
// ponytail: O(n^2) cosine scan, fine to ~10k facts/namespace; move to
// an ANN index when a region outgrows that.
func (s *Store) CreativePass(ctx context.Context, ns string, threshold float64, maxLinks int) (int, error) {
	if s.embedder == nil {
		return 0, nil
	}
	if threshold <= 0 {
		threshold = 0.80
	}
	if maxLinks <= 0 {
		maxLinks = 20
	}
	facts, vecs, err := s.liveWithVectors(ctx, ns)
	if err != nil {
		return 0, err
	}
	// existing similar_to pairs, both orientations. Deliberately NOT
	// scoped to live edges, unlike every other default link read - this is
	// a dedup guard, not a display read, and a closed edge must stay in it.
	// Otherwise CreativePass re-proposes a pair the user closed on every
	// tick: AddLinkWeighted's insert is a no-op (ON CONFLICT DO NOTHING
	// against the still-present closed row), but it still counts toward
	// maxLinks and starves a genuinely new pair of budget. A user
	// invalidation is final: no resurrection, no budget burn.
	seen := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT from_key, to_key FROM memory_links
		WHERE namespace = $1 AND link_type = 'similar_to'`), ns)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return 0, err
		}
		seen[a+"\x00"+b] = true
		seen[b+"\x00"+a] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	added := 0
	for i := range facts {
		if vecs[i] == nil {
			continue
		}
		for j := i + 1; j < len(facts); j++ {
			if vecs[j] == nil || seen[facts[i].Key+"\x00"+facts[j].Key] {
				continue
			}
			if sc := cosine(vecs[i], vecs[j]); sc >= threshold {
				if err := s.AddLinkWeighted(ctx, ns, facts[i].Key, facts[j].Key, "similar_to", sc); err != nil {
					return added, err
				}
				seen[facts[i].Key+"\x00"+facts[j].Key] = true
				added++
				if added >= maxLinks {
					return added, nil
				}
			}
		}
	}
	return added, nil
}
