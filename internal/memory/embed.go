package memory

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Embedder turns text into vectors. Nil on the store disables vector
// search; FTS remains the deterministic default.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dims() int
}

// embedText is the document-side embedding input: the key is prepended
// so the vector carries the fact's place in the hierarchy (/incident/...,
// /runbook/..., /code-map/...) as well as its body. Queries are embedded
// as plain text; the asymmetry is intentional and mirrors how the key is
// also FTS-invisible today.
func embedText(key, body string) string { return "key: " + key + "\n" + body }

// OllamaEmbedder speaks POST {base}/api/embed (Ollama-compatible).
type OllamaEmbedder struct {
	BaseURL string
	Model   string
	D       int
	HTTP    *http.Client
}

func (o *OllamaEmbedder) Dims() int { return o.D }

func (o *OllamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": o.Model, "input": texts})
	if err != nil {
		return nil, err
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(o.BaseURL, "/")+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embed: status %d", resp.StatusCode)
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}

// Storage tags. Every vector written from here on is tag-prefixed: 0x00 is
// float32 (tag + 4*dims little-endian bytes, byte-identical to the
// pre-tag format otherwise), 0x01 is int8 (tag + 4-byte LE float32 scale +
// dims int8 codes). Rows written before this change predate the tag and
// are exactly 4*dims bytes with no prefix at all -- decodeVector's
// disambiguation rule below handles those untouched.
const (
	tagFloat32 byte = 0x00
	tagInt8    byte = 0x01
)

// encodeVector packs a vector for storage, tag-prefixed. quantize=true
// stores int8 (tag 0x01, ~4x smaller, ~2% recall cost); quantize=false
// stores float32 (tag 0x00, unchanged bit layout after the tag).
func encodeVector(v []float32, quantize bool) []byte {
	if quantize {
		codes, scale := QuantizeInt8(v)
		buf := make([]byte, 1+4+len(codes))
		buf[0] = tagInt8
		binary.LittleEndian.PutUint32(buf[1:5], math.Float32bits(scale))
		for i, c := range codes {
			buf[5+i] = byte(c)
		}
		return buf
	}
	buf := make([]byte, 1+4*len(v))
	buf[0] = tagFloat32
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[1+i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeVector reconstructs a stored embedding, tagged or legacy.
//
// Disambiguation rule: legacy (pre-tag) blobs are always exactly 4*dims
// bytes -- a multiple of 4. A tag-0x00 blob is 1+4*dims bytes, which is
// never a multiple of 4, so it can never collide with a legacy blob no
// matter what its first byte happens to be. A tag-0x01 blob is 5+dims
// bytes; for every dims value actually used here (multiples of 4: 384,
// 768, 1024, ...) that's also never a multiple of 4. So: only decode a
// leading 0x00/0x01 as a tag when the remaining length matches what that
// tag implies (which also rules out a legacy blob that coincidentally
// starts with 0x00 or 0x01); otherwise, if the length is a multiple of 4,
// it's legacy float32. (The one gap: dims %4==3 would make an int8 blob
// 5+dims bytes land on a multiple of 4 too, colliding with legacy -- no
// embedding model in use here has such dims.)
func decodeVector(b []byte) []float32 {
	if len(b) == 0 {
		return nil
	}
	if b[0] == tagFloat32 && (len(b)-1)%4 == 0 {
		return decodeFloat32(b[1:])
	}
	if b[0] == tagInt8 && len(b) >= 5 && len(b)%4 != 0 {
		scale := math.Float32frombits(binary.LittleEndian.Uint32(b[1:5]))
		return DequantizeInt8(decodeInt8Codes(b[5:]), scale)
	}
	if len(b)%4 == 0 {
		return decodeFloat32(b)
	}
	return nil // malformed: neither a valid tag nor a legacy length
}

func decodeFloat32(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

func decodeInt8Codes(b []byte) []int8 {
	codes := make([]int8, len(b))
	for i, x := range b {
		codes[i] = int8(x)
	}
	return codes
}

// scoreStored scores a query against one raw stored embedding blob,
// dispatching on its tag: int8 vectors score via cosineQ (integer dot
// product, no dequantize needed); float32 (tagged or legacy) via cosine.
// Centralized here so VectorSearch and the future IVF task share one
// scorer instead of each re-deriving the tag/format branch.
func scoreStored(query []float32, raw []byte) float64 {
	if len(raw) == 0 {
		return -1
	}
	if raw[0] == tagInt8 && len(raw) >= 5 && len(raw)%4 != 0 {
		scale := math.Float32frombits(binary.LittleEndian.Uint32(raw[1:5]))
		return cosineQ(query, decodeInt8Codes(raw[5:]), scale)
	}
	return cosine(query, decodeVector(raw))
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// VectorSearch ranks live facts by cosine similarity to the query. Once a
// namespace has a built IVF index (see BuildIVF) and crosses IVFMinFacts
// vectors, it probes the nprobe nearest centroids' postings instead of
// scoring every vector -- always unioned with a brute-force pass over the
// "tail" (live vectors the index has no *current* entry for: written or
// updated since the last build), so no live vector is ever unreachable
// regardless of nprobe. Below the threshold, or with no index built, it's
// plain brute force over the namespace: correct and portable; fine to
// ~100k facts per namespace (GAPS2 C1 sizing).
func (s *Store) VectorSearch(ctx context.Context, ns, query string, limit int) ([]Fact, error) {
	if s.embedder == nil {
		return nil, fmt.Errorf("memory: vector search requires an embedder")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	qv, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	facts, raws, err := s.liveVectorRows(ctx, ns)
	if err != nil {
		return nil, err
	}

	s.ivfMu.Lock()
	idx := s.ivf[ns]
	s.ivfMu.Unlock()

	type scored struct {
		f     Fact
		score float64
	}
	ranked := []scored{}

	if idx != nil && idx.builtCount >= s.ivfMinFactsOrDefault() {
		// currentID reflects the live snapshot just loaded above (this call),
		// not the build snapshot idx.builtKeys came from -- so it also
		// catches tombstones, which builtKeys can never see (Finding 1).
		currentID := make(map[string]string, len(facts))
		for _, f := range facts {
			currentID[f.Key] = f.ID
		}
		for _, c := range nearestCentroids(qv[0], idx.centroids, s.ivfNprobeOrDefault()) {
			for _, ref := range idx.postings[c] {
				if currentID[ref.fact.Key] != ref.fact.ID {
					continue // tombstoned, or superseded since build; the tail below covers any current revision
				}
				if sc := scoreStored(qv[0], ref.raw); sc >= 0 {
					ranked = append(ranked, scored{ref.fact, sc})
				}
			}
		}
		for i, raw := range raws {
			if len(raw) == 0 || idx.builtKeys[facts[i].Key] == facts[i].ID {
				continue // empty, or current revision already covered by the index above
			}
			if sc := scoreStored(qv[0], raw); sc >= 0 {
				ranked = append(ranked, scored{facts[i], sc})
			}
		}
	} else {
		for i, raw := range raws {
			if len(raw) == 0 {
				continue
			}
			if sc := scoreStored(qv[0], raw); sc >= 0 {
				ranked = append(ranked, scored{facts[i], sc})
			}
		}
	}

	sort.Slice(ranked, func(a, b int) bool { return ranked[a].score > ranked[b].score })
	out := []Fact{}
	for _, r := range ranked {
		out = append(out, r.f)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// HybridSearch fuses FTS and vector ranks with Reciprocal Rank Fusion
// (k=60, the canonical constant), optionally recency-boosted (P12.6,
// P12.8). Without an embedder it degrades to FTS + recency. It wraps
// HybridSearchScored and strips the score explanation.
func (s *Store) HybridSearch(ctx context.Context, ns, query string, limit int, recencyHalfLife time.Duration) ([]Fact, error) {
	scored, err := s.HybridSearchScored(ctx, ns, query, limit, recencyHalfLife)
	if err != nil {
		return nil, err
	}
	out := make([]Fact, len(scored))
	for i, sf := range scored {
		out[i] = sf.Fact
	}
	return out, nil
}

// BackfillEmbeddings embeds live facts that lack vectors, in batches.
func (s *Store) BackfillEmbeddings(ctx context.Context, ns string, batch int) (int, error) {
	if s.embedder == nil {
		return 0, fmt.Errorf("memory: no embedder configured")
	}
	if batch <= 0 {
		batch = 64
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return 0, err
	}
	total := 0
	for {
		rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
			SELECT id, key, body FROM memories
			WHERE namespace_id = $1 AND action <> 'tombstone' AND embedding IS NULL AND invalid_at IS NULL
			LIMIT $2`), nsID, batch)
		if err != nil {
			return total, err
		}
		var ids []string
		var bodies []string
		for rows.Next() {
			var id, key, body string
			if err := rows.Scan(&id, &key, &body); err != nil {
				rows.Close()
				return total, err
			}
			ids = append(ids, id)
			bodies = append(bodies, embedText(key, body))
		}
		rows.Close()
		if len(ids) == 0 {
			return total, nil
		}
		vecs, err := s.embedder.Embed(ctx, bodies)
		if err != nil {
			return total, err
		}
		for i, id := range ids {
			if _, err := s.db.ExecContext(ctx, s.db.Rebind(
				`UPDATE memories SET embedding = $1 WHERE id = $2`),
				encodeVector(vecs[i], s.quantize), id); err != nil {
				return total, err
			}
			total++
		}
	}
}

// ReembedAll recomputes the vector of every latest live fact in ns,
// including those that already have one. Use after changing the
// embedding model or the embedding input format; batch <= 0 means 64.
func (s *Store) ReembedAll(ctx context.Context, ns string, batch int) (int, error) {
	if s.embedder == nil {
		return 0, fmt.Errorf("memory: no embedder configured")
	}
	if batch <= 0 {
		batch = 64
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT m.id, m.key, m.body FROM memories m
		WHERE m.namespace_id = $1 AND m.action <> 'tombstone' AND m.invalid_at IS NULL
		  AND m.created_at = (SELECT MAX(created_at) FROM memories
		                      WHERE namespace_id = m.namespace_id AND key = m.key)
		ORDER BY m.key`), nsID)
	if err != nil {
		return 0, err
	}
	type row struct{ id, key, body string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key, &r.body); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	total := 0
	for start := 0; start < len(all); start += batch {
		end := start + batch
		if end > len(all) {
			end = len(all)
		}
		inputs := make([]string, 0, end-start)
		for _, r := range all[start:end] {
			inputs = append(inputs, embedText(r.key, r.body))
		}
		vecs, err := s.embedder.Embed(ctx, inputs)
		if err != nil {
			return total, err
		}
		for i, r := range all[start:end] {
			if _, err := s.db.ExecContext(ctx, s.db.Rebind(
				`UPDATE memories SET embedding = $1 WHERE id = $2`),
				encodeVector(vecs[i], s.quantize), r.id); err != nil {
				return total, err
			}
			total++
		}
	}
	return total, nil
}

// liveWithVectors loads the latest live facts of a namespace with their
// stored vectors decoded to float32 (nil when not yet embedded). Used by
// callers that need the actual vector (consolidation, entity/dup linking),
// not just a similarity score.
func (s *Store) liveWithVectors(ctx context.Context, ns string) ([]Fact, [][]float32, error) {
	facts, raws, err := s.liveVectorRows(ctx, ns)
	if err != nil {
		return nil, nil, err
	}
	vecs := make([][]float32, len(raws))
	for i, b := range raws {
		if len(b) > 0 {
			vecs[i] = decodeVector(b)
		}
	}
	return facts, vecs, nil
}

// liveVectorRows loads the latest live facts of a namespace with their raw
// (still tag/legacy-encoded) embedding blobs, nil when not yet embedded.
// VectorSearch scores these directly via scoreStored, without decoding
// int8-quantized vectors back to float32 first. Mirrors the
// expiration_date predicate every other live read path applies
// (latestLiveSQL and friends): an expired fact must not surface here
// either, since this is the read path behind VectorSearch,
// HybridSearchScored, and unified_search's vector arm.
func (s *Store) liveVectorRows(ctx context.Context, ns string) ([]Fact, [][]byte, error) {
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil || !ok {
		return nil, nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT `+factCols+`, m.embedding
		FROM memories m
		JOIN (
		    SELECT key, MAX(created_at) AS mc FROM memories
		    WHERE namespace_id = $1 GROUP BY key
		) latest ON m.key = latest.key AND m.created_at = latest.mc
		WHERE m.namespace_id = $1 AND m.action <> 'tombstone'
		  AND (m.expiration_date IS NULL OR m.expiration_date > $2)
		ORDER BY m.key`), nsID, store.TimeToDB(s.now()))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var facts []Fact
	var raws [][]byte
	for rows.Next() {
		f, extra, err := scanFactRow(rows, 1)
		if err != nil {
			return nil, nil, err
		}
		f.Namespace = ns
		facts = append(facts, *f)
		if b, ok := extra[0].([]byte); ok && len(b) > 0 {
			raws = append(raws, b)
		} else {
			raws = append(raws, nil)
		}
	}
	return facts, raws, rows.Err()
}
