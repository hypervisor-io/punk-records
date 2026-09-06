package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
)

// DocumentSource identifies where a document body came from. ID is the
// stable identifier of the source itself (not of one fetch), URI its
// canonical location, Revision the caller's revision label (git SHA,
// etag) and MediaType the body format. Every field is optional; whatever
// is set is recorded on every chunk as provenance, secret-scrubbed under
// the namespace's defense policy.
type DocumentSource struct {
	ID        string
	URI       string
	Revision  string
	MediaType string
}

// DocumentSection is one labeled part of a source document: a heading's
// body, one page of a paged format, one entry of a structured feed.
// Name and Page are optional provenance labels.
type DocumentSection struct {
	Name string
	Page int
	Text string
}

// SourceDocument is the source-aware document input: the ordered
// sections plus the source they came from. The assembled source text
// (section texts joined by "\n\n", in order) is the coordinate space
// chunk offsets refer to.
type SourceDocument struct {
	Source   DocumentSource
	Sections []DocumentSection
}

// sourceChunk is one chunk body with its byte range [start,end) inside
// the assembled source text and the index of the section it came from.
type sourceChunk struct {
	body       string
	start, end int
	section    int
}

// chunkTextOffsets chunks text exactly like chunkText (paragraphs are
// the delta-locality unit; only oversize paragraphs are split, by the
// same splitOversize) while tracking each chunk's byte offsets inside
// text. base is the offset of text within a larger assembled document.
func chunkTextOffsets(text string, max, base int) []sourceChunk {
	var out []sourceChunk
	pos := 0 // raw offset of the current "\n\n"-separated piece
	for _, raw := range strings.Split(text, "\n\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			start := pos + leadingSpaceLen(raw)
			if len(trimmed) <= max {
				out = append(out, sourceChunk{body: trimmed,
					start: base + start, end: base + start + len(trimmed)})
			} else {
				// splitOversize pieces are trimmed in-order substrings of
				// the paragraph; between consecutive pieces only
				// whitespace was dropped, and a piece starts with a
				// non-space rune, so its first occurrence at or after the
				// cursor is its true position.
				rest := start
				for _, piece := range splitOversize(trimmed, max) {
					idx := strings.Index(text[rest:], piece)
					if idx < 0 {
						break // unreachable: pieces come from this text
					}
					pStart := rest + idx
					out = append(out, sourceChunk{body: piece,
						start: base + pStart, end: base + pStart + len(piece)})
					rest = pStart + len(piece)
				}
			}
		}
		pos += len(raw) + 2
	}
	return out
}

// leadingSpaceLen is the byte length of s's leading whitespace run (the
// same definition strings.TrimSpace uses).
func leadingSpaceLen(s string) int {
	for i, r := range s {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return len(s)
}

// chunkIDKey derives a chunk's stable key from the owning source, the
// body that will be stored and the document-order occurrence ordinal of
// that body: identity is (owner, content, occurrence), so an insertion
// or deletion elsewhere never rewrites a chunk whose body survived
// regardless of position shifts, repeated identical paragraphs get
// deterministic distinct keys (…-1, …-2, …) instead of colliding on one
// key, and two sources with identical bodies coexist at one prefix
// instead of colliding on the content hash. An anonymous source (no ID,
// no URI) keeps the round-1 shape chunk-<sha256(body)[:8]>-<occurrence>;
// an identified source adds its own owner hash segment.
func chunkIDKey(prefix, owner, body string, occurrence int) string {
	sum := sha256.Sum256([]byte(body))
	if owner == "" {
		return fmt.Sprintf("%s/chunk-%s-%d", prefix, hex.EncodeToString(sum[:8]), occurrence)
	}
	own := sha256.Sum256([]byte(owner))
	return fmt.Sprintf("%s/chunk-%s-%s-%d", prefix,
		hex.EncodeToString(sum[:8]), hex.EncodeToString(own[:8]), occurrence)
}

// sourceOwner is the identity a source manages chunks under: its ID,
// else its URI. Unlike sourceRef there is no prefix fallback - a source
// that names nothing owns the anonymous-owner chunks only.
func sourceOwner(src DocumentSource) string {
	if src.ID != "" {
		return src.ID
	}
	return src.URI
}

// chunkOwner reports the owning source of a live fact under a chunk
// prefix: the effective owner recorded in its provenance (id, else uri)
// and whether the fact carries source provenance at all. Facts without
// it - user-written keys, legacy positional chunks - are owned by
// nobody; a source ingest never adopts, rewrites or tombstones them.
func chunkOwner(f Fact) (owner string, owned bool) {
	src, ok := f.Attributes["source"].(map[string]any)
	if !ok {
		return "", false
	}
	if id, _ := src["id"].(string); id != "" {
		return id, true
	}
	uri, _ := src["uri"].(string)
	return uri, true
}

// liveChunkFacts enumerates every live chunk under prefix with no
// 1000-row cap: ListKeys is uncapped and liveByKeys is paged in bounded
// batches, so reconcile stays complete above a thousand chunks.
func (s *Store) liveChunkFacts(ctx context.Context, ns, prefix string) ([]Fact, error) {
	keys, err := s.ListKeys(ctx, ns, prefix+"/chunk-")
	if err != nil {
		return nil, err
	}
	out := []Fact{}
	for i := 0; i < len(keys); i += 500 {
		facts, err := s.liveByKeys(ctx, ns, keys[i:min(i+500, len(keys))])
		if err != nil {
			return nil, err
		}
		out = append(out, facts...)
	}
	return out, nil
}

// sourceRef renders the fact-level citation: the document identity
// pinned to the exact ingested revision through the first 12 hex chars
// of the document content hash.
func sourceRef(src DocumentSource, prefix, contentHash string) string {
	id := src.ID
	if id == "" {
		id = src.URI
	}
	if id == "" {
		id = prefix
	}
	return "document:" + id + "@" + contentHash[:12]
}

// sourceAttrs is the per-chunk provenance record, stored under
// attributes["source"] so it rides the existing export/import shape. It
// describes the exact source revision the chunk was written from:
// unchanged chunks are never rewritten, so their provenance keeps citing
// that retained revision (and its offsets) even after later revisions
// move the paragraph elsewhere in the document.
func sourceAttrs(src DocumentSource, contentHash string, sec DocumentSection, ordinal int, c sourceChunk, occurrence int) map[string]any {
	m := map[string]any{
		"content_hash": contentHash,
		"chunk":        ordinal,
		"occurrence":   occurrence,
		"start":        c.start,
		"end":          c.end,
	}
	if src.ID != "" {
		m["id"] = src.ID
	}
	if src.URI != "" {
		m["uri"] = src.URI
	}
	if src.Revision != "" {
		m["revision"] = src.Revision
	}
	if src.MediaType != "" {
		m["media_type"] = src.MediaType
	}
	if sec.Name != "" {
		m["section"] = sec.Name
	}
	if sec.Page > 0 {
		m["page"] = sec.Page
	}
	return map[string]any{"source": m}
}

// WriteDocumentSource ingests a source-aware document under prefix with
// stable chunk identity and full source provenance. It is the
// source-aware sibling of WriteDocument and shares its chunker, its
// delta-retain contract (only changed chunks are rewritten; chunks no
// remaining section produces are tombstoned) and the write-time defense
// path:
//
//   - Chunk keys are content-addressed (chunkIDKey): identity is
//     (owning source, stored body, document-order occurrence ordinal).
//     Inserting an early paragraph writes exactly one new chunk; every
//     unaffected later paragraph keeps its key, its live fact revision
//     and its provenance. Because the owner is part of the identity,
//     two sources ingesting at one prefix - even identical bodies -
//     get disjoint chunk keys and never collide.
//   - A source manages ONLY its own chunks, on both sides of reconcile.
//     Deletion side: reconcile reads the live chunks under the prefix
//     (uncapped enumeration, liveChunkFacts) and considers just those
//     whose provenance names this source's effective owner
//     (sourceOwner). Destination side: a chunk whose generated key is
//     occupied by a live fact this source does not own - a user
//     replacement written at the chunk key, a foreign chunk reusing
//     that content address - is preserved untouched and counted in
//     blocked, never overwritten or adopted. Foreign facts -
//     user-written keys under the chunk prefix, legacy positional
//     chunks, other sources' chunks - are thus never adopted,
//     rewritten or tombstoned, and another source's ingest leaves this
//     source's chunks untouched.
//   - Repeated identical paragraphs are distinct chunks keyed by their
//     occurrence ordinal; inserting another copy writes one new chunk
//     with the next ordinal and rewrites nothing.
//   - Every chunk fact carries attributes["source"]: the source ID/URI/
//     revision/media type, section name/page, document content hash,
//     chunk ordinal, occurrence ordinal and exact byte offsets into the
//     assembled source text. The Fact.SourceRef citation pins the same
//     revision (document:<id>@<hash12>).
//   - Source metadata is secret-scrubbed under the same namespace
//     defense policy as bodies: "redact" scrubs every metadata string,
//     "block" blocks the whole ingest when any metadata string is
//     sensitive (counted in blocked, nothing written) - metadata is
//     document-wide, so no chunk could carry it anyway.
//   - Reingestion of an unchanged document is idempotent: nothing is
//     rewritten even when the caller's revision label changed, and old
//     fact citations stay resolvable within retention (bi-temporal
//     history; see RecallAsOf and SweepRetention).
//
// A prefix previously ingested through the legacy positional
// WriteDocument keeps its positional chunks (chunk-0001, …): they carry
// no source provenance, so a source-aware write leaves them untouched
// alongside the new content-addressed chunks. The text-only
// WriteDocument API itself is unchanged.
func (s *Store) WriteDocumentSource(ctx context.Context, ns, prefix string, doc SourceDocument, writer string) (written, unchanged, removed, blocked int, err error) {
	if err := ValidateKey(prefix); err != nil {
		return 0, 0, 0, 0, err
	}
	mode := s.defenseMode(ns)
	src := doc.Source
	sections := make([]DocumentSection, len(doc.Sections))
	copy(sections, doc.Sections)
	metaSensitive := false
	scrubMeta := func(v string) string {
		switch mode {
		case "redact":
			v, _ = Scrub(v)
		case "block":
			if _, labels := Scrub(v); len(labels) > 0 {
				metaSensitive = true
			}
		}
		return v
	}
	src.ID = scrubMeta(src.ID)
	src.URI = scrubMeta(src.URI)
	src.Revision = scrubMeta(src.Revision)
	src.MediaType = scrubMeta(src.MediaType)
	for i := range sections {
		sections[i].Name = scrubMeta(sections[i].Name)
	}

	texts := make([]string, len(sections))
	for i, sec := range sections {
		texts[i] = sec.Text
	}
	full := strings.Join(texts, "\n\n")
	sum := sha256.Sum256([]byte(full))
	contentHash := hex.EncodeToString(sum[:])

	var chunks []sourceChunk
	base := 0
	for i, sec := range sections {
		for _, c := range chunkTextOffsets(sec.Text, s.chunkMax(), base) {
			c.section = i
			chunks = append(chunks, c)
		}
		base += len(sec.Text) + 2
	}
	if metaSensitive {
		return 0, 0, 0, len(chunks), nil
	}

	existing, err := s.liveChunkFacts(ctx, ns, prefix)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	// prefix membership is not ownership: only chunks whose provenance
	// names this source's effective owner enter the reconcile set, so
	// leftover tombstoning can never hit a foreign or user chunk. The
	// same boundary guards the write side: occupied is every live key
	// under the prefix, and a chunk whose destination key is occupied
	// by a fact this source does not own is preserved, never written.
	owner := sourceOwner(src)
	byKey := map[string]string{}  // key -> live body owned by this source
	occupied := map[string]bool{} // every live key under the chunk prefix
	for _, f := range existing {
		occupied[f.Key] = true
		if fo, owned := chunkOwner(f); owned && fo == owner {
			byKey[f.Key] = f.Body
		}
	}
	occurrences := map[string]int{}
	for i, c := range chunks {
		body := c.body
		switch mode {
		case "redact":
			body, _ = Scrub(body)
		case "block":
			if _, labels := Scrub(body); len(labels) > 0 {
				// counted, not written, and the key's old version (if
				// any) is left alone - the per-chunk mirror of
				// WriteDocument's block behavior.
				blocked++
				occurrences[body]++
				delete(byKey, chunkIDKey(prefix, owner, body, occurrences[body]))
				continue
			}
		}
		occurrences[body]++
		key := chunkIDKey(prefix, owner, body, occurrences[body])
		if byKey[key] == body {
			unchanged++
			delete(byKey, key)
			continue
		}
		if _, mine := byKey[key]; !mine && occupied[key] {
			// destination ownership: the generated key is held by a
			// live fact this source does not own - a user replacement
			// written at the chunk key, another owner's chunk reusing
			// the content address. Counted, not written, and the
			// occupant is left alone: an ingest never reclaims or
			// overwrites a key whose live occupant is foreign.
			blocked++
			continue
		}
		if _, err := s.Write(ctx, WriteInput{Namespace: ns, Key: key, Body: body,
			Attributes: sourceAttrs(src, contentHash, sections[c.section], i+1, c, occurrences[body]),
			Writer:     writer, Author: writer,
			SourceRef: sourceRef(src, prefix, contentHash)}); err != nil {
			return written, unchanged, removed, blocked, err
		}
		written++
		delete(byKey, key)
	}
	// leftover owned keys are produced by no remaining section (deleted
	// section, shrunk document): tombstone them
	for key := range byKey {
		if err := s.Forget(ctx, ns, key, writer); err != nil {
			return written, unchanged, removed, blocked, err
		}
		removed++
	}
	return written, unchanged, removed, blocked, nil
}
