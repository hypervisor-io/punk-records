package memory

import (
	"strings"
	"testing"
)

func TestWriteDocumentDelta(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	doc := "para one about ceph\n\npara two about postgres\n\npara three about redis"
	w, u, r, b, err := s.WriteDocument(ctx, "ns", "/docs/runbook", doc, "test")
	if err != nil || w != 3 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("initial ingest = %d/%d/%d/%d (err %v), want 3/0/0/0", w, u, r, b, err)
	}
	// change only the middle paragraph
	doc2 := "para one about ceph\n\npara two REVISED\n\npara three about redis"
	w, u, r, b, err = s.WriteDocument(ctx, "ns", "/docs/runbook", doc2, "test")
	if err != nil || w != 1 || u != 2 || r != 0 || b != 0 {
		t.Fatalf("delta ingest = %d/%d/%d/%d (err %v), want 1/2/0/0", w, u, r, b, err)
	}
	// shrink to one paragraph: chunks 2 and 3 tombstoned
	w, u, r, b, err = s.WriteDocument(ctx, "ns", "/docs/runbook", "para one about ceph", "test")
	if err != nil || w != 0 || u != 1 || r != 2 || b != 0 {
		t.Fatalf("shrink ingest = %d/%d/%d/%d (err %v), want 0/1/2/0", w, u, r, b, err)
	}
	keys, err := s.ListKeys(ctx, "ns", "/docs/runbook")
	if err != nil || len(keys) != 1 || !strings.HasSuffix(keys[0], "/chunk-0001") {
		t.Fatalf("live keys = %v (err %v)", keys, err)
	}
}

// TestWriteDocumentRedactMode covers the review finding: Write() scrubs
// bodies under "redact" mode, so WriteDocument must compare against the
// scrubbed body too — otherwise a secret-bearing paragraph never
// compares equal to its stored (redacted) form and gets rewritten (and
// re-embedded, re-hashed) on every single re-ingest.
func TestWriteDocumentRedactMode(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	s.SetDefense("redact")
	doc := "para one about ceph\n\nconn postgres://admin:hunter22secret@db.internal:5432/prod"
	w, u, r, b, err := s.WriteDocument(ctx, "ns", "/docs/secret", doc, "test")
	if err != nil || w != 2 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("initial ingest = %d/%d/%d/%d (err %v), want 2/0/0/0", w, u, r, b, err)
	}
	facts, err := s.Recall(ctx, "ns", "/docs/secret/chunk-", 10)
	if err != nil {
		t.Fatal(err)
	}
	var secretBody string
	for _, f := range facts {
		if strings.HasSuffix(f.Key, "chunk-0002") {
			secretBody = f.Body
		}
	}
	if !strings.Contains(secretBody, "[REDACTED:dsn_credentials]") || strings.Contains(secretBody, "hunter22secret") {
		t.Fatalf("stored chunk not scrubbed: %q", secretBody)
	}
	// re-ingest the SAME raw document: the secret chunk must compare
	// equal against its scrubbed stored body and be reported unchanged,
	// not rewritten every time.
	w, u, r, b, err = s.WriteDocument(ctx, "ns", "/docs/secret", doc, "test")
	if err != nil || w != 0 || u != 2 || r != 0 || b != 0 {
		t.Fatalf("redact re-ingest = %d/%d/%d/%d (err %v), want 0/2/0/0", w, u, r, b, err)
	}
}

// TestWriteDocumentBlockMode covers the review finding: under "block"
// mode Write() errors on any raw secret-bearing chunk, which would
// break the whole document ingest forever. WriteDocument must instead
// skip (not write, not tombstone) the offending chunk and count it in
// blocked, letting clean chunks in the same document still land.
func TestWriteDocumentBlockMode(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	s.SetDefense("block")
	doc := "para one about ceph\n\nconn postgres://admin:hunter22secret@db.internal:5432/prod"
	w, u, r, b, err := s.WriteDocument(ctx, "ns", "/docs/blocked", doc, "test")
	if err != nil || w != 1 || u != 0 || r != 0 || b != 1 {
		t.Fatalf("initial ingest = %d/%d/%d/%d (err %v), want 1/0/0/1", w, u, r, b, err)
	}
	keys, err := s.ListKeys(ctx, "ns", "/docs/blocked")
	if err != nil || len(keys) != 1 || !strings.HasSuffix(keys[0], "/chunk-0001") {
		t.Fatalf("live keys = %v (err %v), want only chunk-0001", keys, err)
	}
	// re-ingest: clean chunk is now unchanged, secret chunk blocked again
	// (idempotent — no permanent break, no infinite rewrite either).
	w, u, r, b, err = s.WriteDocument(ctx, "ns", "/docs/blocked", doc, "test")
	if err != nil || w != 0 || u != 1 || r != 0 || b != 1 {
		t.Fatalf("block re-ingest = %d/%d/%d/%d (err %v), want 0/1/0/1", w, u, r, b, err)
	}
}
