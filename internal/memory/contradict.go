package memory

import (
	"context"
	"database/sql"
	"strings"
)

// ContradictionJudge decides whether two similar facts state opposing
// claims.
type ContradictionJudge interface {
	Contradicts(ctx context.Context, a, b Fact) (bool, error)
}

// contradictSkip lists key prefixes never judged: derived layers and
// high-volume capture keys, not primary claims.
var contradictSkip = []string{"/observations/", "/consolidated/", "/mental-models/", "/entities/", "/agent-sessions/"}

func contradictEligible(key string) bool {
	for _, p := range contradictSkip {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return true
}

// DetectContradictions finds live raw-fact pairs (distinct keys) with
// embedding cosine >= threshold, asks the judge whether they contradict, and
// on yes links them: contradicts both ways plus invalidated_by from the
// older to the newer (scored search halves the older fact's score).
// Detect-and-link, never delete (punk's append-only rule).
// Deterministic-first: nil judge or no embedder is a no-op. threshold <= 0
// defaults to 0.85; threshold >= 1 is a no-op, mirroring
// ReconcileObservations' ">= 1 means never fires" convention. maxPairs caps
// the number of JUDGE INVOCATIONS per pass (not links written); <= 0
// defaults to 20, mirroring CreativePass's maxLinks convention. The scan
// stops as soon as the budget is exhausted, so one pass over a large
// namespace costs at most maxPairs judge calls. A judge error skips the
// pair (the call still counts against the budget) and never fails the
// pass. All three edges of a linked pair are written in a single
// transaction (addLinkTx), so a mid-triple failure leaves no half-linked
// pair. Ties in CreatedAt resolve to scan order (keys ascending), so the
// lexicographically smaller key counts as older. Negative judgements are
// not memoized: a pair the judge said no to is re-judged on every later
// pass - deferred to the phase gate - and the maxPairs budget is what
// bounds how expensive that repeated re-judging can get. Returns pairs
// linked.
func (s *Store) DetectContradictions(ctx context.Context, ns string, threshold float64, maxPairs int, judge ContradictionJudge) (int, error) {
	if judge == nil || s.embedder == nil {
		return 0, nil
	}
	if threshold <= 0 {
		threshold = 0.85
	} else if threshold >= 1 {
		return 0, nil
	}
	if maxPairs <= 0 {
		maxPairs = 20
	}
	facts, vecs, err := s.liveWithVectors(ctx, ns)
	if err != nil {
		return 0, err
	}
	var elig []int
	for i, f := range facts {
		if contradictEligible(f.Key) && vecs[i] != nil {
			elig = append(elig, i)
		}
	}
	linkedPairs, err := s.contradictLinkedPairs(ctx, ns)
	if err != nil {
		return 0, err
	}
	linked := 0
	judged := 0
	for a := 0; a < len(elig) && judged < maxPairs; a++ {
		for b := a + 1; b < len(elig) && judged < maxPairs; b++ {
			fa, fb := facts[elig[a]], facts[elig[b]]
			if cosine(vecs[elig[a]], vecs[elig[b]]) < threshold {
				continue
			}
			if linkedPairs[fa.Key+"\x00"+fb.Key] {
				continue
			}
			judged++
			yes, err := judge.Contradicts(ctx, fa, fb)
			if err != nil || !yes {
				continue
			}
			older, newer := fa, fb
			if fb.CreatedAt.Before(fa.CreatedAt) {
				older, newer = fb, fa
			}
			now := s.now()
			// All three edges in one tx - do not split into separate WithTx
			// calls (see TestAddLinkTxAtomicRollback, which proves a
			// mid-triple failure here rolls back everything already Exec'd,
			// not just the failing statement). description is always "" at
			// this call site, so the precomputed embedding is always nil -
			// addLinkTx never touches s.embedder itself (finding 1, round 2).
			err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
				if err := s.addLinkTx(ctx, tx, ns, fa.Key, fb.Key, "contradicts", 1.0, "", nil, now); err != nil {
					return err
				}
				if err := s.addLinkTx(ctx, tx, ns, fb.Key, fa.Key, "contradicts", 1.0, "", nil, now); err != nil {
					return err
				}
				return s.addLinkTx(ctx, tx, ns, older.Key, newer.Key, "invalidated_by", 1.0, "", nil, now)
			})
			if err != nil {
				return linked, err
			}
			linkedPairs[fa.Key+"\x00"+fb.Key] = true
			linkedPairs[fb.Key+"\x00"+fa.Key] = true
			linked++
		}
	}
	return linked, nil
}

// contradictLinkedPairs preloads every existing contradicts/invalidated_by
// edge, once per pass, as a both-orientations set - replacing what used to
// be one SELECT per candidate pair inside the O(n^2) scan (mirrors
// CreativePass's seen-map preload, consolidate.go). Deliberately
// validity-UNFILTERED (T2.2 rule): a user-closed contradiction edge means
// "stop flagging this pair", so a closed row must stay in the set - live-
// filtering it would resurrect the noise every pass.
func (s *Store) contradictLinkedPairs(ctx context.Context, ns string) (map[string]bool, error) {
	seen := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT from_key, to_key FROM memory_links
		WHERE namespace = $1 AND link_type IN ('contradicts','invalidated_by')`), ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		seen[a+"\x00"+b] = true
		seen[b+"\x00"+a] = true
	}
	return seen, rows.Err()
}
