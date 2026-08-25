package memory

import (
	"context"
	"strings"
)

// MergeJudge decides whether two near-duplicate observations are the same
// belief. Returns merge=true plus the merged body, or merge=false to keep
// both.
type MergeJudge interface {
	ShouldMerge(ctx context.Context, a, b Observation) (merge bool, mergedBody string, err error)
}

// factSourceIDs reads the source_ids attribute (stored as []any of
// strings after the JSON round-trip) off a consolidated observation fact.
func factSourceIDs(f Fact) []string {
	raw, _ := f.Attributes["source_ids"].([]any)
	ids := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			ids = append(ids, s)
		}
	}
	return ids
}

func toObservation(f Fact) Observation {
	kind, _ := f.Attributes["kind"].(string)
	return Observation{Slug: f.Key[len("/observations/"):], Body: f.Body, SourceIDs: factSourceIDs(f), Kind: kind}
}

// ReconcileObservations finds observation pairs with embedding cosine >=
// threshold and, for each, asks the judge whether to merge. On merge it
// writes a merged observation (union of source_ids, proof_count = union
// size, judge's body) under a fresh slug and tombstones the two
// originals. Deterministic-first: nil judge or threshold >= 1 is a
// no-op. Returns merges performed.
// ponytail: O(n^2) cosine over observations only (few per namespace);
// ANN if a namespace grows thousands of beliefs.
func (s *Store) ReconcileObservations(ctx context.Context, ns string, threshold float64, judge MergeJudge) (int, error) {
	if judge == nil || threshold >= 1 {
		return 0, nil
	}
	facts, vecs, err := s.liveWithVectors(ctx, ns)
	if err != nil {
		return 0, err
	}
	var obsFacts []Fact
	var obsVecs [][]float32
	for i, f := range facts {
		if hasPrefix(f.Key, "/observations/") {
			obsFacts = append(obsFacts, f)
			obsVecs = append(obsVecs, vecs[i])
		}
	}
	consumed := make([]bool, len(obsFacts))
	merged := 0
	for i := range obsFacts {
		if consumed[i] || obsVecs[i] == nil {
			continue
		}
		for j := i + 1; j < len(obsFacts); j++ {
			if consumed[j] || obsVecs[j] == nil || cosine(obsVecs[i], obsVecs[j]) < threshold {
				continue
			}
			a, b := toObservation(obsFacts[i]), toObservation(obsFacts[j])
			doMerge, body, err := judge.ShouldMerge(ctx, a, b)
			if err != nil {
				return merged, err
			}
			if !doMerge {
				continue
			}
			seen := make(map[string]bool, len(a.SourceIDs)+len(b.SourceIDs))
			union := make([]any, 0, len(a.SourceIDs)+len(b.SourceIDs))
			for _, id := range append(append([]string{}, a.SourceIDs...), b.SourceIDs...) {
				if !seen[id] {
					seen[id] = true
					union = append(union, id)
				}
			}
			survivingKey := obsFacts[i].Key
			// ponytail: stable-key convention — a key already ending in
			// "-merged" keeps that same key on re-merge (new revision,
			// source_ids just grow) instead of restacking a "-merged" suffix
			// each tick it re-enters the candidate set.
			alreadyMerged := strings.HasSuffix(survivingKey, "-merged")
			mergedKey := survivingKey
			if !alreadyMerged {
				mergedKey = survivingKey + "-merged"
			}
			mergedAttrs := map[string]any{"source_ids": union, "proof_count": float64(len(union))}
			// The reasoning kind survives a merge only when both sides
			// agree on it; a merge across kinds is a new synthesis whose
			// derivation is no longer any single one of them.
			if a.Kind != "" && a.Kind == b.Kind {
				mergedAttrs["kind"] = a.Kind
			}
			if _, err := s.Write(ctx, WriteInput{
				Namespace: ns, Key: mergedKey, Body: body,
				Attributes: mergedAttrs,
				Writer:     "reconciliation", Importance: 0.5,
			}); err != nil {
				return merged, err
			}
			// Forget the surviving original only when the merge wrote under a
			// new key; when reusing survivingKey, forgetting it would
			// tombstone the write we just made.
			if !alreadyMerged {
				if err := s.Forget(ctx, ns, survivingKey, "reconciliation"); err != nil {
					return merged, err
				}
			}
			if err := s.Forget(ctx, ns, obsFacts[j].Key, "reconciliation"); err != nil {
				return merged, err
			}
			consumed[i], consumed[j] = true, true
			merged++
			break // i is spent; move on to the next surviving candidate
		}
	}
	return merged, nil
}
