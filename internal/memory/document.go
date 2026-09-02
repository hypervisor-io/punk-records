package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// DefaultChunkMaxChars bounds one chunk. Paragraphs are still the unit of
// delta-locality: a paragraph under the bound is one chunk, untouched.
// Only an oversize paragraph is split, at the best-scoring break point
// inside the last 30 percent of each window, so an edit in one part of a
// long paragraph rewrites one or two chunks, not the whole document, and
// no chunk exceeds what an embedding model can see.
const DefaultChunkMaxChars = 4000

// SetChunkMaxChars overrides DefaultChunkMaxChars; n <= 0 restores it.
func (s *Store) SetChunkMaxChars(n int) {
	if n <= 0 {
		n = DefaultChunkMaxChars
	}
	s.chunkMaxChars = n
}

func (s *Store) chunkMax() int {
	if s.chunkMaxChars <= 0 {
		return DefaultChunkMaxChars
	}
	return s.chunkMaxChars
}

// chunkText splits text on blank lines into paragraph chunks; paragraphs
// above max bytes are further split by splitOversize.
func chunkText(text string, max int) []string {
	paras := strings.Split(text, "\n\n")
	chunks := []string{}
	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) <= max {
			chunks = append(chunks, p)
			continue
		}
		chunks = append(chunks, splitOversize(p, max)...)
	}
	return chunks
}

// breakScore ranks a cut position just AFTER byte i: sentence end 4,
// newline 3, clause punctuation 2, space 1, else 0. The cut keeps the
// separator with the left piece.
func breakScore(p string, i int) int {
	c := p[i]
	next := byte(' ')
	if i+1 < len(p) {
		next = p[i+1]
	}
	switch {
	case c == '\n':
		return 3
	case (c == '.' || c == '!' || c == '?') && (next == ' ' || next == '\n'):
		return 4
	case (c == ';' || c == ':') && next == ' ':
		return 2
	case c == ' ':
		return 1
	}
	return 0
}

// splitOversize cuts p into pieces of at most max bytes. Within each
// window it scans the last 30 percent for the highest breakScore (latest
// position wins ties) and cuts after it; with no separator it hard-cuts
// at the last rune boundary at or before max. Pieces are TrimSpace'd
// and never empty.
func splitOversize(p string, max int) []string {
	var out []string
	for len(p) > max {
		start := max * 7 / 10
		best, bestScore := -1, 0
		for i := start; i < max; i++ {
			if sc := breakScore(p, i); sc >= bestScore && sc > 0 {
				best, bestScore = i, sc
			}
		}
		cut := best + 1
		if best < 0 {
			cut = max
			for cut > 0 && !utf8.RuneStart(p[cut]) {
				cut--
			}
			if cut == 0 {
				cut = max // degenerate: single rune wider than max cannot happen for max >= 4
			}
		}
		if piece := strings.TrimSpace(p[:cut]); piece != "" {
			out = append(out, piece)
		}
		p = strings.TrimSpace(p[cut:])
	}
	if p != "" {
		out = append(out, p)
	}
	return out
}

// WriteDocument chunks text under prefix (/prefix/chunk-0001, ...) and
// writes ONLY changed chunks (delta-retain): unchanged chunk
// bodies are skipped without touching the DB row, chunks past the new
// end are tombstoned. The store's write-time defense mode applies per
// chunk before the comparison, mirroring ImportJSONL: "redact" scrubs
// the body first so a re-ingest of the same secret compares equal
// against the already-redacted stored body; "block" skips a sensitive
// chunk entirely (counted in blocked, old version if any left alone)
// instead of erroring out the whole document forever. Returns
// written / unchanged / removed / blocked counts.
func (s *Store) WriteDocument(ctx context.Context, ns, prefix, text, writer string) (written, unchanged, removed, blocked int, err error) {
	if err := ValidateKey(prefix); err != nil {
		return 0, 0, 0, 0, err
	}
	chunks := chunkText(text, s.chunkMax())
	existing, err := s.Recall(ctx, ns, prefix+"/chunk-", 1000)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	byKey := map[string]string{} // key -> live body
	for _, f := range existing {
		byKey[f.Key] = f.Body
	}
	for i, body := range chunks {
		key := fmt.Sprintf("%s/chunk-%04d", prefix, i+1)
		switch s.defense {
		case "redact":
			body, _ = Scrub(body)
		case "block":
			if _, labels := Scrub(body); len(labels) > 0 {
				blocked++
				delete(byKey, key)
				continue
			}
		}
		if byKey[key] == body {
			unchanged++
			delete(byKey, key)
			continue
		}
		if _, err := s.Write(ctx, WriteInput{Namespace: ns, Key: key, Body: body,
			Writer: writer, Author: writer, SourceRef: "document:" + prefix}); err != nil {
			return written, unchanged, removed, blocked, err
		}
		written++
		delete(byKey, key)
	}
	// leftover keys are past the new document end: tombstone them
	for key := range byKey {
		if err := s.Forget(ctx, ns, key, writer); err != nil {
			return written, unchanged, removed, blocked, err
		}
		removed++
	}
	return written, unchanged, removed, blocked, nil
}
