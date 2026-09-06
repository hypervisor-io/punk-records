package memory

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestWriteDocumentSourcePreservesUnownedChunks pins the ownership
// boundary of source-aware ingest: prefix membership is not ownership.
// A user-written fact under the chunk prefix, a legacy positional chunk
// left by WriteDocument and another source's chunk all survive an
// ingest at the same prefix - only chunks whose provenance names the
// ingesting source enter its reconcile set.
func TestWriteDocumentSourcePreservesUnownedChunks(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	// a legacy positional chunk from the text-only WriteDocument path
	if _, _, _, _, err := s.WriteDocument(ctx, "ns", "/doc", "legacy positional body", "t"); err != nil {
		t.Fatal(err)
	}
	// a user-maintained fact that happens to sit under the chunk prefix
	// (written after the legacy ingest: WriteDocument owns its whole
	// prefix and would tombstone it, which is its documented behavior)
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/doc/chunk-user-note",
		Body: "manually maintained note", Writer: "user"}); err != nil {
		t.Fatal(err)
	}
	// a chunk owned by a different source at the same prefix
	other := SourceDocument{Source: DocumentSource{ID: "source-B"},
		Sections: []DocumentSection{{Text: "B-owned content"}}}
	if _, _, _, _, err := s.WriteDocumentSource(ctx, "ns", "/doc", other, "ingester"); err != nil {
		t.Fatal(err)
	}
	keysBefore, err := s.ListKeys(ctx, "ns", "/doc")
	if err != nil || len(keysBefore) != 3 {
		t.Fatalf("setup keys = %v (err %v), want 3", keysBefore, err)
	}

	a := SourceDocument{Source: DocumentSource{ID: "source-A"},
		Sections: []DocumentSection{{Text: "new document"}}}
	w, u, r, b, err := s.WriteDocumentSource(ctx, "ns", "/doc", a, "ingester")
	if err != nil || w != 1 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("ingest = %d/%d/%d/%d (err %v), want 1/0/0/0", w, u, r, b, err)
	}
	keysAfter, err := s.ListKeys(ctx, "ns", "/doc")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keysBefore {
		found := false
		for _, k2 := range keysAfter {
			if k == k2 {
				found = true
			}
		}
		if !found {
			t.Fatalf("source ingest tombstoned unowned key %s (keys %v -> %v)", k, keysBefore, keysAfter)
		}
	}
	if len(keysAfter) != 4 {
		t.Fatalf("keys after = %v, want the 3 pre-existing plus source-A's chunk", keysAfter)
	}
}

// TestWriteDocumentSourcePreservesUserReplacementAtGeneratedKey pins the
// write side of the ownership boundary: a user replacement written AT a
// source's generated chunk key carries no source provenance, so the live
// occupant of that destination key is foreign. Reingesting the document
// must neither overwrite nor adopt it - the chunk is counted in blocked,
// nothing is written or tombstoned, and the user's fact revision stays
// live at the key.
func TestWriteDocumentSourcePreservesUserReplacementAtGeneratedKey(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()

	doc := SourceDocument{Source: DocumentSource{ID: "source-A"},
		Sections: []DocumentSection{{Text: "source content"}}}
	if _, _, _, _, err := s.WriteDocumentSource(ctx, "ns", "/doc", doc, "ingester"); err != nil {
		t.Fatal(err)
	}
	facts, err := s.Recall(ctx, "ns", "/doc/", 100)
	if err != nil || len(facts) != 1 {
		t.Fatalf("setup recall = %v (err %v), want the single source chunk", facts, err)
	}
	key := facts[0].Key

	// the user replaces the chunk in place at the generated key
	clk.Set(s.now().Add(time.Second))
	user, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key,
		Body: "user replacement", Writer: "user"})
	if err != nil {
		t.Fatal(err)
	}

	// reingest of the unchanged document: the destination key is
	// occupied by a fact the source does not own, so the write is
	// refused and the replacement preserved
	clk.Set(s.now().Add(time.Second))
	w, u, r, b, err := s.WriteDocumentSource(ctx, "ns", "/doc", doc, "ingester")
	if err != nil || w != 0 || u != 0 || r != 0 || b != 1 {
		t.Fatalf("reingest = %d/%d/%d/%d (err %v), want 0/0/0/1", w, u, r, b, err)
	}
	live, err := s.liveByKeys(ctx, "ns", []string{key})
	if err != nil || len(live) != 1 {
		t.Fatalf("live facts at %s = %v (err %v), want the user's revision", key, live, err)
	}
	if live[0].ID != user.ID {
		t.Fatalf("reingest overwrote the user-owned replacement: %s != %s", live[0].ID, user.ID)
	}
	if _, adopted := live[0].Attributes["source"]; adopted {
		t.Fatalf("reingest adopted the user's replacement into the source: %v", live[0].Attributes)
	}
}

// TestWriteDocumentSourceOwnershipCoexistence: two sources ingesting at
// one prefix never touch each other's chunks, even when their bodies
// are byte-identical - chunk identity incorporates the owning source,
// so same-body chunks get disjoint keys and reconcile stays per-source.
func TestWriteDocumentSourceOwnershipCoexistence(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	a := SourceDocument{Source: DocumentSource{ID: "source-A"},
		Sections: []DocumentSection{{Text: "A-owned content"}}}
	if _, _, _, _, err := s.WriteDocumentSource(ctx, "ns", "/doc", a, "ingester"); err != nil {
		t.Fatal(err)
	}
	aFacts, err := s.Recall(ctx, "ns", "/doc/", 1000)
	if err != nil || len(aFacts) != 1 {
		t.Fatalf("source-A recall = %v (err %v), want 1 chunk", aFacts, err)
	}
	aKey := aFacts[0].Key

	// source B at the same prefix: distinct body first
	b := SourceDocument{Source: DocumentSource{ID: "source-B"},
		Sections: []DocumentSection{{Text: "B-owned content"}}}
	w, u, r, bl, err := s.WriteDocumentSource(ctx, "ns", "/doc", b, "ingester")
	if err != nil || w != 1 || u != 0 || r != 0 || bl != 0 {
		t.Fatalf("source-B ingest = %d/%d/%d/%d (err %v), want 1/0/0/0", w, u, r, bl, err)
	}
	live, err := s.liveByKeys(ctx, "ns", []string{aKey})
	if err != nil || len(live) != 1 {
		t.Fatalf("source B deleted source A's chunk: live=%v err=%v", live, err)
	}

	// then the same-body collision: identical text under both sources
	// must produce two coexisting chunks on disjoint keys
	same := SourceDocument{Source: DocumentSource{ID: "source-A"},
		Sections: []DocumentSection{{Text: "shared body"}}}
	if _, _, _, _, err := s.WriteDocumentSource(ctx, "ns", "/doc2", same, "ingester"); err != nil {
		t.Fatal(err)
	}
	same.Source.ID = "source-B"
	w, u, r, bl, err = s.WriteDocumentSource(ctx, "ns", "/doc2", same, "ingester")
	if err != nil || w != 1 || u != 0 || r != 0 || bl != 0 {
		t.Fatalf("same-body source-B ingest = %d/%d/%d/%d (err %v), want 1/0/0/0", w, u, r, bl, err)
	}
	facts, err := s.Recall(ctx, "ns", "/doc2/chunk-", 10)
	if err != nil || len(facts) != 2 {
		t.Fatalf("same-body recall = %v (err %v), want 2 coexisting chunks", facts, err)
	}
	if facts[0].Key == facts[1].Key {
		t.Fatalf("identical bodies from two sources collided on %s", facts[0].Key)
	}
	owners := map[string]bool{}
	for _, f := range facts {
		if f.Body != "shared body" {
			t.Fatalf("chunk body = %q, want the shared body", f.Body)
		}
		owners[attrStr(t, chunkSource(t, f), "id")] = true
	}
	if !owners["source-A"] || !owners["source-B"] {
		t.Fatalf("coexisting chunks must name both sources: %v", owners)
	}

	// source A retracting its document tombstones only its own chunk
	empty := SourceDocument{Source: DocumentSource{ID: "source-A"}}
	w, u, r, bl, err = s.WriteDocumentSource(ctx, "ns", "/doc2", empty, "ingester")
	if err != nil || w != 0 || u != 0 || r != 1 || bl != 0 {
		t.Fatalf("source-A retract = %d/%d/%d/%d (err %v), want 0/0/1/0", w, u, r, bl, err)
	}
	facts, err = s.Recall(ctx, "ns", "/doc2/chunk-", 10)
	if err != nil || len(facts) != 1 {
		t.Fatalf("post-retract recall = %v (err %v), want source-B's chunk only", facts, err)
	}
	if got := attrStr(t, chunkSource(t, facts[0]), "id"); got != "source-B" {
		t.Fatalf("surviving chunk owner = %q, want source-B", got)
	}
}

// TestWriteDocumentSourceReconcileBeyondRecallCap: reconcile enumerates
// owned chunks completely, past Recall's 1000-row cap. A 1001-chunk
// document shrinks to nothing with zero stale survivors, and reingest
// above the cap stays idempotent.
func TestWriteDocumentSourceReconcileBeyondRecallCap(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	parts := make([]string, 1001)
	for i := range parts {
		parts[i] = fmt.Sprintf("Paragraph number %04d", i)
	}
	doc := SourceDocument{Source: DocumentSource{ID: "source-A"},
		Sections: []DocumentSection{{Text: strings.Join(parts, "\n\n")}}}

	w, u, r, b, err := s.WriteDocumentSource(ctx, "ns", "/doc", doc, "ingester")
	if err != nil || w != 1001 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("1001-chunk ingest = %d/%d/%d/%d (err %v), want 1001/0/0/0", w, u, r, b, err)
	}

	// idempotent reingest above the cap: every chunk compares equal
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/doc", doc, "ingester")
	if err != nil || w != 0 || u != 1001 || r != 0 || b != 0 {
		t.Fatalf("reingest above cap = %d/%d/%d/%d (err %v), want 0/1001/0/0", w, u, r, b, err)
	}

	// deleting every section tombstones all 1001 owned chunks, not just
	// the first 1000
	doc.Sections = nil
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/doc", doc, "ingester")
	if err != nil || w != 0 || u != 0 || r != 1001 || b != 0 {
		t.Fatalf("delete-all = %d/%d/%d/%d (err %v), want 0/0/1001/0", w, u, r, b, err)
	}
	keys, err := s.ListKeys(ctx, "ns", "/doc")
	if err != nil || len(keys) != 0 {
		t.Fatalf("stale chunks survive deleting a 1001-chunk document: %v (err %v)", keys, err)
	}

	// and a full reingest from empty writes all 1001 again
	w, u, r, b, err = s.WriteDocumentSource(ctx, "ns", "/doc", SourceDocument{
		Source:   DocumentSource{ID: "source-A"},
		Sections: []DocumentSection{{Text: strings.Join(parts, "\n\n")}}},
		"ingester")
	if err != nil || w != 1001 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("reingest from empty = %d/%d/%d/%d (err %v), want 1001/0/0/0", w, u, r, b, err)
	}
}
