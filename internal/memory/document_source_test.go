package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// chunkSource pulls the provenance map off a chunk fact.
func chunkSource(t *testing.T, f Fact) map[string]any {
	t.Helper()
	src, ok := f.Attributes["source"].(map[string]any)
	if !ok {
		t.Fatalf("fact %s has no source attributes: %v", f.Key, f.Attributes)
	}
	return src
}

func attrStr(t *testing.T, m map[string]any, k string) string {
	t.Helper()
	s, _ := m[k].(string)
	if s == "" {
		t.Fatalf("source attrs missing %q: %v", k, m)
	}
	return s
}

func attrNum(t *testing.T, m map[string]any, k string) float64 {
	t.Helper()
	n, ok := m[k].(float64)
	if !ok {
		t.Fatalf("source attrs missing numeric %q: %v", k, m)
	}
	return n
}

func sourceTestDoc(rev, body string) SourceDocument {
	return SourceDocument{
		Source: DocumentSource{ID: "runbook", URI: "file:///docs/runbook.md",
			Revision: rev, MediaType: "text/markdown"},
		Sections: []DocumentSection{{Name: "body", Page: 2, Text: body}},
	}
}

// factsByBody indexes live chunks by body for documents whose paragraph
// bodies are unique (the repeated-paragraph test reads Recall directly).
func factsByBody(t *testing.T, s *Store, ctx context.Context, prefix string) map[string]Fact {
	t.Helper()
	facts, err := s.Recall(ctx, "ns", prefix+"/chunk-", 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Fact{}
	for _, f := range facts {
		out[f.Body] = f
	}
	return out
}

// TestWriteDocumentSourceEarlyInsertPreservesChunkIdentity is the I01 red
// proof: inserting a paragraph at the beginning of a document must not
// rewrite unaffected later chunks. Their keys AND live fact IDs survive,
// and their provenance keeps pointing at the exact revision they were
// ingested from, while the new paragraph cites the new revision.
func TestWriteDocumentSourceEarlyInsertPreservesChunkIdentity(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()

	w, u, r, b, err := s.WriteDocumentSource(ctx, "ns", "/docs/rb",
		sourceTestDoc("r1", "para B about postgres\n\npara C about redis"), "t")
	if err != nil || w != 2 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("rev1 ingest = %d/%d/%d/%d (err %v), want 2/0/0/0", w, u, r, b, err)
	}
	before := factsByBody(t, s, ctx, "/docs/rb")
	bFact := before["para B about postgres"]
	cFact := before["para C about redis"]
	bSrc := chunkSource(t, bFact)
	if got := attrStr(t, bSrc, "revision"); got != "r1" {
		t.Fatalf("revision = %q, want r1", got)
	}
	if got := attrStr(t, bSrc, "id"); got != "runbook" {
		t.Fatalf("id = %q, want runbook", got)
	}
	if got := attrStr(t, bSrc, "uri"); got != "file:///docs/runbook.md" {
		t.Fatalf("uri = %q", got)
	}
	if got := attrStr(t, bSrc, "media_type"); got != "text/markdown" {
		t.Fatalf("media_type = %q", got)
	}
	if got := attrStr(t, bSrc, "section"); got != "body" {
		t.Fatalf("section = %q", got)
	}
	if got := attrNum(t, bSrc, "page"); got != 2 {
		t.Fatalf("page = %v, want 2", got)
	}
	rev1Hash := attrStr(t, bSrc, "content_hash")
	if len(rev1Hash) != 64 {
		t.Fatalf("content_hash = %q, want 64 hex chars", rev1Hash)
	}
	// offsets slice the assembled source text exactly
	if got := attrNum(t, bSrc, "start"); got != 0 {
		t.Fatalf("B start = %v, want 0", got)
	}
	if got := attrNum(t, bSrc, "end"); got != float64(len("para B about postgres")) {
		t.Fatalf("B end = %v, want %d", got, len("para B about postgres"))
	}
	cSrc := chunkSource(t, cFact)
	if got := attrNum(t, cSrc, "start"); got != float64(len("para B about postgres")+2) {
		t.Fatalf("C start = %v, want %d", got, len("para B about postgres")+2)
	}
	if !strings.HasPrefix(bFact.SourceRef, "document:runbook@") {
		t.Fatalf("SourceRef = %q, want document:runbook@<hash12>", bFact.SourceRef)
	}
	// the instant right after the rev1 ingest (the clock ticks per
	// write, so the later rev1 chunk's CreatedAt covers both)
	asOf := cFact.CreatedAt

	// rev2: a paragraph inserted at the BEGINNING
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/docs/rb",
		sourceTestDoc("r2", "para A about ceph\n\npara B about postgres\n\npara C about redis"), "t")
	if err != nil || w != 1 || u != 2 || r != 0 || b != 0 {
		t.Fatalf("early-insert ingest = %d/%d/%d/%d (err %v), want 1/2/0/0", w, u, r, b, err)
	}
	after := factsByBody(t, s, ctx, "/docs/rb")
	b2 := after["para B about postgres"]
	c2 := after["para C about redis"]
	if b2.Key != bFact.Key || b2.ID != bFact.ID {
		t.Fatalf("B chunk identity changed: key %s->%s id %s->%s", bFact.Key, b2.Key, bFact.ID, b2.ID)
	}
	if c2.Key != cFact.Key || c2.ID != cFact.ID {
		t.Fatalf("C chunk identity changed: key %s->%s id %s->%s", cFact.Key, c2.Key, cFact.ID, c2.ID)
	}
	// unaffected chunks keep the provenance of the revision they cite:
	// still r1, still the rev1 content hash, still the rev1 offsets
	b2Src := chunkSource(t, b2)
	if got := attrStr(t, b2Src, "revision"); got != "r1" {
		t.Fatalf("unchanged B revision = %q, want r1 (provenance must not be rewritten)", got)
	}
	if got := attrStr(t, b2Src, "content_hash"); got != rev1Hash {
		t.Fatalf("unchanged B content_hash changed: %q != %q", got, rev1Hash)
	}
	if got := attrNum(t, b2Src, "start"); got != 0 {
		t.Fatalf("unchanged B start = %v, want the frozen rev1 offset 0", got)
	}
	a2 := after["para A about ceph"]
	if a2.ID == "" || a2.Key == bFact.Key || a2.Key == cFact.Key {
		t.Fatalf("new paragraph did not get its own chunk: %+v", a2)
	}
	a2Src := chunkSource(t, a2)
	if got := attrStr(t, a2Src, "revision"); got != "r2" {
		t.Fatalf("new chunk revision = %q, want r2", got)
	}
	if got := attrStr(t, a2Src, "content_hash"); got == rev1Hash {
		t.Fatalf("new chunk cites the old content hash")
	}
	if got := attrNum(t, a2Src, "end"); got != float64(len("para A about ceph")) {
		t.Fatalf("A end = %v, want %d", got, len("para A about ceph"))
	}

	// old fact citations remain resolvable within retention: at the
	// instant after rev1 the store still shows exactly the rev1 chunks
	got, err := s.RecallAsOf(ctx, "ns", "/docs/rb/chunk-", asOf, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("recall as-of rev1 = %d facts, want 2", len(got))
	}
	for _, f := range got {
		if f.ID != bFact.ID && f.ID != cFact.ID {
			t.Fatalf("as-of fact %s (%s) is not a rev1 citation", f.Key, f.ID)
		}
	}
}

// TestWriteDocumentSourceSectionDeleteTombstonesOwnedChunks: dropping a
// section from the input tombstones exactly the chunks no remaining
// section produces; other sections' chunks keep identity and provenance.
func TestWriteDocumentSourceSectionDeleteTombstonesOwnedChunks(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	src := DocumentSource{ID: "manual", Revision: "r1", MediaType: "text/markdown"}
	full := SourceDocument{Source: src, Sections: []DocumentSection{
		{Name: "install", Text: "step one about ceph\n\nstep two about pools"},
		{Name: "ops", Text: "ops note about alerts"},
	}}
	w, u, r, b, err := s.WriteDocumentSource(ctx, "ns", "/docs/manual", full, "t")
	if err != nil || w != 3 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("initial = %d/%d/%d/%d (err %v), want 3/0/0/0", w, u, r, b, err)
	}
	before := factsByBody(t, s, ctx, "/docs/manual")
	ops := before["ops note about alerts"]

	shrunk := SourceDocument{Source: src, Sections: []DocumentSection{
		{Name: "ops", Text: "ops note about alerts"},
	}}
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/docs/manual", shrunk, "t")
	if err != nil || w != 0 || u != 1 || r != 2 || b != 0 {
		t.Fatalf("section delete = %d/%d/%d/%d (err %v), want 0/1/2/0", w, u, r, b, err)
	}
	keys, err := s.ListKeys(ctx, "ns", "/docs/manual")
	if err != nil || len(keys) != 1 || keys[0] != ops.Key {
		t.Fatalf("live keys = %v (err %v), want exactly the ops chunk %s", keys, err, ops.Key)
	}
	after := factsByBody(t, s, ctx, "/docs/manual")
	if got := after["ops note about alerts"]; got.ID != ops.ID {
		t.Fatalf("surviving chunk was rewritten: %s != %s", got.ID, ops.ID)
	}
	// idempotent re-ingest of the shrunk document
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/docs/manual", shrunk, "t")
	if err != nil || w != 0 || u != 1 || r != 0 || b != 0 {
		t.Fatalf("re-ingest = %d/%d/%d/%d (err %v), want 0/1/0/0", w, u, r, b, err)
	}
}

// TestWriteDocumentSourceRepeatedParagraphs pins the deterministic
// handling of repeated identical paragraphs: identity is (body content,
// document-order occurrence ordinal), so two identical paragraphs are two
// stable distinct chunks, and inserting a third copy at the beginning
// writes exactly one new chunk instead of rewriting the existing ones.
func TestWriteDocumentSourceRepeatedParagraphs(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	dup := "dup para about etcd"
	v1 := SourceDocument{Source: DocumentSource{ID: "doc", Revision: "r1"},
		Sections: []DocumentSection{{Text: dup + "\n\n" + dup + "\n\nunique tail about quorum"}}}
	w, u, r, b, err := s.WriteDocumentSource(ctx, "ns", "/docs/dup", v1, "t")
	if err != nil || w != 3 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("initial = %d/%d/%d/%d (err %v), want 3/0/0/0", w, u, r, b, err)
	}
	facts, err := s.Recall(ctx, "ns", "/docs/dup/chunk-", 10)
	if err != nil {
		t.Fatal(err)
	}
	var dups []Fact
	var tail Fact
	for _, f := range facts {
		if f.Body == dup {
			dups = append(dups, f)
		} else {
			tail = f
		}
	}
	if len(dups) != 2 || dups[0].Key == dups[1].Key {
		t.Fatalf("repeated paragraphs must be two distinct chunks: %+v", dups)
	}
	occs := map[float64]bool{}
	for _, d := range dups {
		occs[attrNum(t, chunkSource(t, d), "occurrence")] = true
	}
	if !occs[1] || !occs[2] {
		t.Fatalf("occurrence ordinals = %v, want 1 and 2", occs)
	}

	// idempotent re-ingest
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/docs/dup", v1, "t")
	if err != nil || w != 0 || u != 3 || r != 0 || b != 0 {
		t.Fatalf("re-ingest = %d/%d/%d/%d (err %v), want 0/3/0/0", w, u, r, b, err)
	}

	// a third identical copy at the beginning: exactly one new chunk,
	// the two existing occurrences and the tail keep their identities
	v2 := SourceDocument{Source: DocumentSource{ID: "doc", Revision: "r2"},
		Sections: []DocumentSection{{Text: dup + "\n\n" + dup + "\n\n" + dup + "\n\nunique tail about quorum"}}}
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/docs/dup", v2, "t")
	if err != nil || w != 1 || u != 3 || r != 0 || b != 0 {
		t.Fatalf("dup insert = %d/%d/%d/%d (err %v), want 1/3/0/0", w, u, r, b, err)
	}
	after, err := s.Recall(ctx, "ns", "/docs/dup/chunk-", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 4 {
		t.Fatalf("chunks = %d, want 4", len(after))
	}
	live := map[string]bool{}
	for _, f := range after {
		live[f.Key] = true
		if f.Body == tail.Body && f.ID != tail.ID {
			t.Fatalf("tail chunk was rewritten: %s != %s", f.ID, tail.ID)
		}
	}
	for _, d := range dups {
		if !live[d.Key] {
			t.Fatalf("existing duplicate chunk %s disappeared", d.Key)
		}
	}
}

// TestWriteDocumentSourceSecretMetadata: source metadata is scrubbed
// under the same namespace policy as bodies - redacted in redact mode,
// and in block mode a sensitive URI blocks the whole ingest (counted,
// not written) instead of leaking the secret into provenance.
func TestWriteDocumentSourceSecretMetadata(t *testing.T) {
	ctx := t.Context()
	secretDoc := func() SourceDocument {
		return SourceDocument{
			Source: DocumentSource{ID: "runbook",
				URI:      "https://admin:hunter22secret@docs.internal/runbook.md",
				Revision: "r1"},
			Sections: []DocumentSection{{Text: "para one about ceph\n\npara two about pools"}},
		}
	}

	s := newTestStore(t)
	s.SetDefense("redact")
	w, u, r, b, err := s.WriteDocumentSource(ctx, "ns", "/docs/secret", secretDoc(), "t")
	if err != nil || w != 2 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("redact ingest = %d/%d/%d/%d (err %v), want 2/0/0/0", w, u, r, b, err)
	}
	facts, err := s.Recall(ctx, "ns", "/docs/secret/chunk-", 10)
	if err != nil || len(facts) != 2 {
		t.Fatalf("recall = %v (err %v)", facts, err)
	}
	for _, f := range facts {
		uri := attrStr(t, chunkSource(t, f), "uri")
		if !strings.Contains(uri, "[REDACTED:dsn_credentials]") || strings.Contains(uri, "hunter22secret") {
			t.Fatalf("source uri not scrubbed: %q", uri)
		}
		if strings.Contains(f.SourceRef, "hunter22secret") {
			t.Fatalf("SourceRef leaks the secret: %q", f.SourceRef)
		}
	}
	// idempotent under redaction
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/docs/secret", secretDoc(), "t")
	if err != nil || w != 0 || u != 2 || r != 0 || b != 0 {
		t.Fatalf("redact re-ingest = %d/%d/%d/%d (err %v), want 0/2/0/0", w, u, r, b, err)
	}

	s2 := newTestStore(t)
	s2.SetDefense("block")
	w, u, r, b, err = s2.WriteDocumentSource(ctx, "ns", "/docs/blocked", secretDoc(), "t")
	if err != nil || w != 0 || u != 0 || r != 0 || b != 2 {
		t.Fatalf("block ingest = %d/%d/%d/%d (err %v), want 0/0/0/2", w, u, r, b, err)
	}
	keys, err := s2.ListKeys(ctx, "ns", "/docs/blocked")
	if err != nil || len(keys) != 0 {
		t.Fatalf("blocked ingest wrote keys: %v (err %v)", keys, err)
	}
	w, u, r, b, err = s2.WriteDocumentSource(ctx, "ns", "/docs/blocked", secretDoc(), "t")
	if err != nil || w != 0 || u != 0 || r != 0 || b != 2 {
		t.Fatalf("block re-ingest = %d/%d/%d/%d (err %v), want 0/0/0/2", w, u, r, b, err)
	}
}

// TestWriteDocumentSourceExportImportRoundTrip: source metadata lives in
// fact attributes, which ExportJSONL/ImportJSONL carry, so an export/
// import round-trip preserves chunk provenance (and the fact IDs old
// citations name). Boundary: the source_ref COLUMN is not part of the
// exportRecord shape (pre-existing export scope: id/key/action/body/
// attributes/author/created_at), which is why the canonical provenance
// is the attributes map.
func TestWriteDocumentSourceExportImportRoundTrip(t *testing.T) {
	ctx := t.Context()
	s1, _, _ := newTest(t)
	doc := SourceDocument{
		Source: DocumentSource{ID: "runbook", URI: "file:///docs/runbook.md",
			Revision: "r7", MediaType: "text/markdown"},
		Sections: []DocumentSection{{Name: "install", Page: 3, Text: "step one about ceph\n\nstep two about pools"}},
	}
	if _, _, _, _, err := s1.WriteDocumentSource(ctx, "ns", "/docs/rb", doc, "t"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s1.ExportJSONL(ctx, "ns", &buf); err != nil {
		t.Fatal(err)
	}
	want, err := s1.Recall(ctx, "ns", "/docs/rb/chunk-", 10)
	if err != nil {
		t.Fatal(err)
	}

	s2, _, _ := newTest(t)
	imported, skipped, blocked, err := s2.ImportJSONL(ctx, "ns", bytes.NewReader(buf.Bytes()))
	if err != nil || imported != 2 || skipped != 0 || blocked != 0 {
		t.Fatalf("import = %d/%d/%d (err %v), want 2/0/0", imported, skipped, blocked, err)
	}
	got, err := s2.Recall(ctx, "ns", "/docs/rb/chunk-", 10)
	if err != nil || len(got) != len(want) {
		t.Fatalf("imported recall = %v (err %v), want %d chunks", got, err, len(want))
	}
	byKey := map[string]Fact{}
	for _, f := range got {
		byKey[f.Key] = f
	}
	for _, wf := range want {
		gf, ok := byKey[wf.Key]
		if !ok {
			t.Fatalf("chunk %s did not round-trip", wf.Key)
		}
		if gf.ID != wf.ID {
			t.Fatalf("chunk %s ID changed: %s != %s", wf.Key, gf.ID, wf.ID)
		}
		wantJSON, err := json.Marshal(wf.Attributes)
		if err != nil {
			t.Fatal(err)
		}
		gotJSON, err := json.Marshal(gf.Attributes)
		if err != nil {
			t.Fatal(err)
		}
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("chunk %s attributes changed:\n%s\n%s", wf.Key, wantJSON, gotJSON)
		}
	}
}

// TestChunkTextOffsetsSlicesSource verifies the offset-aware chunker:
// bodies are identical to chunkText's, and [start,end) slices the source
// text exactly, including oversize splits and section base offsets.
func TestChunkTextOffsetsSlicesSource(t *testing.T) {
	text := "para one\n\n  para two  \n\npara three"
	chunks := chunkTextOffsets(text, DefaultChunkMaxChars, 0)
	want := chunkText(text, DefaultChunkMaxChars)
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %d, want %d", len(chunks), len(want))
	}
	for i, c := range chunks {
		if c.body != want[i] {
			t.Fatalf("chunk %d body = %q, want %q", i, c.body, want[i])
		}
		if text[c.start:c.end] != c.body {
			t.Fatalf("chunk %d offsets [%d,%d) slice %q, want %q", i, c.start, c.end, text[c.start:c.end], c.body)
		}
		if i > 0 && c.start < chunks[i-1].end {
			t.Fatalf("chunk %d overlaps its predecessor", i)
		}
	}
	// base offsets shift every chunk (section inside a larger document)
	shifted := chunkTextOffsets(text, DefaultChunkMaxChars, 100)
	for i, c := range shifted {
		if c.start != chunks[i].start+100 || c.end != chunks[i].end+100 {
			t.Fatalf("shifted chunk %d = [%d,%d), want [%d,%d)", i, c.start, c.end, chunks[i].start+100, chunks[i].end+100)
		}
	}
	// oversize paragraphs: pieces keep exact offsets too
	big := strings.Repeat("alpha beta. ", 500)
	bigChunks := chunkTextOffsets(big, 4000, 0)
	if len(bigChunks) < 2 {
		t.Fatalf("oversize paragraph produced %d chunks, want >= 2", len(bigChunks))
	}
	for i, c := range bigChunks {
		if big[c.start:c.end] != c.body {
			t.Fatalf("oversize chunk %d offsets [%d,%d) slice %q, want %q", i, c.start, c.end, big[c.start:c.end], c.body)
		}
	}
}
