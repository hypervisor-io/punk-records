package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Triplet is a source→edge→target fact relation scored for a query.
type Triplet struct {
	From, To    Fact
	LinkType    string
	Description string
	Score       float64 // higher = more relevant
}

// TripletSearch embeds the query and ranks triplets (links with an
// embedded description) by combined query relevance over the edge and
// its two endpoint facts: score = edgeCos + fromCos + toCos, each a
// cosine of the query against that element's embedding (0 when absent).
// The relation must itself be relevant, not just the endpoints.
// Returns the top-k triplets. No embedder → empty (nil,nil).
// ponytail: O(links) scan over the namespace, fine to ~10k links/ns; ANN when it outgrows that.
func (s *Store) TripletSearch(ctx context.Context, ns, query string, k int) ([]Triplet, error) {
	if s.embedder == nil {
		return nil, nil
	}
	if k <= 0 {
		k = 10
	}
	qvs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	qv := qvs[0]

	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT from_key, to_key, link_type, COALESCE(description,''), embedding
		FROM memory_links WHERE namespace = $1 AND embedding IS NOT NULL AND invalid_at IS NULL`), ns)
	if err != nil {
		return nil, err
	}
	type rawLink struct {
		from, to, linkType, description string
		embedding                       []byte
	}
	var links []rawLink
	keySet := map[string]struct{}{}
	for rows.Next() {
		var l rawLink
		if err := rows.Scan(&l.from, &l.to, &l.linkType, &l.description, &l.embedding); err != nil {
			rows.Close()
			return nil, err
		}
		links = append(links, l)
		keySet[l.from] = struct{}{}
		keySet[l.to] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	facts, err := s.liveByKeys(ctx, ns, keys)
	if err != nil {
		return nil, err
	}
	factByKey := make(map[string]Fact, len(facts))
	for _, f := range facts {
		factByKey[f.Key] = f
	}
	rawByKey, err := s.embeddingsByKeys(ctx, ns, keys)
	if err != nil {
		return nil, err
	}

	out := []Triplet{}
	for _, l := range links {
		from, ok1 := factByKey[l.from]
		to, ok2 := factByKey[l.to]
		if !ok1 || !ok2 {
			continue // endpoint not live (gone or tombstoned)
		}
		var fromCos, toCos float64
		if raw, ok := rawByKey[l.from]; ok {
			fromCos = scoreStored(qv, raw)
		}
		if raw, ok := rawByKey[l.to]; ok {
			toCos = scoreStored(qv, raw)
		}
		out = append(out, Triplet{
			From: from, To: to, LinkType: l.linkType, Description: l.description,
			Score: scoreStored(qv, l.embedding) + fromCos + toCos,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// embeddingsByKeys loads the raw (tag/legacy-encoded) embedding blob for
// each live fact among keys, omitting keys with no embedding.
func (s *Store) embeddingsByKeys(ctx context.Context, ns string, keys []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(keys) == 0 {
		return out, nil
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return out, err
	}
	ph := make([]string, len(keys))
	args := []any{nsID}
	for i, key := range keys {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, key)
	}
	args = append(args, store.TimeToDB(s.now()))
	q := `
SELECT m.key, m.embedding
FROM memories m
JOIN (
    SELECT key, MAX(created_at) AS mc
    FROM memories
    WHERE namespace_id = $1 AND key IN (` + strings.Join(ph, ",") + `)
    GROUP BY key
) latest ON m.key = latest.key AND m.created_at = latest.mc
WHERE m.namespace_id = $1 AND m.action <> 'tombstone'
  AND (m.expiration_date IS NULL OR m.expiration_date > $` + fmt.Sprintf("%d", len(keys)+2) + `)`
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			out[key] = raw
		}
	}
	return out, rows.Err()
}
