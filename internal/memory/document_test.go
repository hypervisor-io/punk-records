package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestSplitOversizePrefersSentenceBoundary(t *testing.T) {
	sentence := "This is a sentence about pools. "
	p := strings.Repeat(sentence, 20) // 640 chars
	got := splitOversize(p, 300)
	if len(got) < 3 {
		t.Fatalf("expected >= 3 pieces, got %d", len(got))
	}
	for i, c := range got {
		if len(c) > 300 {
			t.Fatalf("piece %d has %d chars > 300", i, len(c))
		}
		if !strings.HasSuffix(c, ".") {
			t.Fatalf("piece %d does not end at a sentence: %q", i, c)
		}
	}
	joined := strings.Join(got, " ")
	if strings.ReplaceAll(joined, " ", "") != strings.ReplaceAll(p, " ", "") {
		t.Fatalf("content lost across split")
	}
}

func TestSplitOversizeHardCutsWithoutSeparators(t *testing.T) {
	p := strings.Repeat("é", 1000) // multibyte, no spaces
	got := splitOversize(p, 400)
	total := 0
	for _, c := range got {
		if len(c) > 400 {
			t.Fatalf("piece has %d bytes > 400", len(c))
		}
		if !utf8.ValidString(c) {
			t.Fatalf("piece is not valid UTF-8")
		}
		total += utf8.RuneCountInString(c)
	}
	if total != 1000 {
		t.Fatalf("rune count = %d, want 1000", total)
	}
}

func TestChunkTextSplitsOnlyOversizeParagraphs(t *testing.T) {
	small := "short paragraph"
	big := strings.Repeat("word ", 1000) // 5000 chars
	got := chunkText(small+"\n\n"+big+"\n\n"+small, DefaultChunkMaxChars)
	if got[0] != small || got[len(got)-1] != small {
		t.Fatalf("small paragraphs must be untouched: %q ... %q", got[0], got[len(got)-1])
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (small, big/2, big/2, small)", len(got))
	}
	again := chunkText(small+"\n\n"+big+"\n\n"+small, DefaultChunkMaxChars)
	if strings.Join(again, "|") != strings.Join(got, "|") {
		t.Fatalf("chunking must be deterministic")
	}
}

func TestWriteDocumentOversizeParagraphYieldsMultipleChunks(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	big := strings.Repeat("alpha beta. ", 500) // 6000 chars
	w, _, _, _, err := s.WriteDocument(ctx, "ns", "/docs/big", big, "t")
	if err != nil {
		t.Fatal(err)
	}
	if w != 2 {
		t.Fatalf("written = %d, want 2", w)
	}
	w, u, _, _, err := s.WriteDocument(ctx, "ns", "/docs/big", big, "t")
	if err != nil || w != 0 || u != 2 {
		t.Fatalf("re-ingest: w=%d u=%d err=%v, want 0/2", w, u, err)
	}
}
