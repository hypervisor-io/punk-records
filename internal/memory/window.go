package memory

import (
	"context"
	"errors"
	"time"
)

const windowBuckets = 8

// WindowedSearch runs FTS over live facts, filters to CreatedAt within
// [from, to), then spreads selection across 8 time buckets (best match
// per populated bucket, round-robin) so a whole-window query returns
// representative coverage, not just the densest stretch.
func (s *Store) WindowedSearch(ctx context.Context, ns, query string, from, to time.Time, limit int) ([]Fact, error) {
	if !to.After(from) {
		return nil, errors.New("memory: window to must be after from")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	all, err := s.Search(ctx, ns, query, 200) // FTS rank order
	if err != nil {
		return nil, err
	}
	in := []Fact{}
	for _, f := range all {
		if !f.CreatedAt.Before(from) && f.CreatedAt.Before(to) {
			in = append(in, f)
		}
	}
	if len(in) <= limit {
		return in, nil
	}
	// bucket by time, preserve FTS rank inside each bucket
	span := to.Sub(from) / windowBuckets
	if span <= 0 {
		span = 1
	}
	buckets := make([][]Fact, windowBuckets)
	for _, f := range in {
		b := int(f.CreatedAt.Sub(from) / span)
		if b >= windowBuckets {
			b = windowBuckets - 1
		}
		buckets[b] = append(buckets[b], f)
	}
	// round-robin the best remaining match from each populated bucket
	out := []Fact{}
	for round := 0; len(out) < limit; round++ {
		took := false
		for b := range buckets {
			if round < len(buckets[b]) {
				out = append(out, buckets[b][round])
				took = true
				if len(out) >= limit {
					break
				}
			}
		}
		if !took {
			break
		}
	}
	return out, nil
}
