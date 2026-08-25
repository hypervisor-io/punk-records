package memory

import (
	"context"
	"fmt"
	"strings"
)

// ponytail: each paragraph is its own chunk — no merging of separate
// paragraphs into one. Merging would defeat delta-locality, the whole
// point of per-paragraph chunking: editing one paragraph should rewrite
// one chunk, not a bundle of unrelated ones. A paragraph is never split,
// so an oversized (>2000 char) paragraph is simply kept whole as its own
// larger-than-usual chunk — no special-case code needed for that.

// chunkText splits text on blank lines into paragraph chunks, never
// splitting mid-paragraph.
func chunkText(text string) []string {
	paras := strings.Split(text, "\n\n")
	chunks := []string{}
	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		chunks = append(chunks, p)
	}
	return chunks
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
	chunks := chunkText(text)
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
