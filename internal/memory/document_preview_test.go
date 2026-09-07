package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// previewDoc is a small two-chunk document the preview tests diff.
func previewDoc(id string) SourceDocument {
	return SourceDocument{
		Source: DocumentSource{ID: id},
		Sections: []DocumentSection{
			{Name: "intro", Text: "first section of the previewed document"},
			{Name: "detail", Text: "second section of the previewed document"},
		},
	}
}

// storageCounts snapshots the row count of every table a document write or
// its enrichment touches. A preview is proved side-effect-free by
// comparing this before and after, not by spying on Write/Forget:
// quarantine is an internal transaction no spy model ever sees.
func storageCounts(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"memories", "memories_quarantine", "memory_outbox", "memory_pipeline_runs"} {
		var n int
		if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

// corruptChunkAttributes makes every live chunk under prefix undecodable
// the way a partial write or a bad migration would.
func corruptChunkAttributes(t *testing.T, db *store.DB, prefix string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE memories SET attributes = '{bad' WHERE key LIKE '`+prefix+`/chunk-%'`); err != nil {
		t.Fatal(err)
	}
}

// TestPreviewDocumentSourceDoesNotQuarantine: a preview is advertised as
// strictly read-only, so it must leave a malformed stored row exactly
// where it is. An ordinary read quarantines such a row (one poisoned row
// may never brick a namespace); a preview has no business repairing the
// store it is describing. Folded from the reviewer's reproduced
// /tmp/punk-P02-review-readonly_test.go.
func TestPreviewDocumentSourceDoesNotQuarantine(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()
	doc := previewDoc("reviewer-doc")
	if _, _, _, _, err := s.WriteDocumentSource(ctx, "ns", "/doc", doc, "reviewer"); err != nil {
		t.Fatal(err)
	}
	corruptChunkAttributes(t, db, "/doc")

	before := storageCounts(t, db)
	_, previewErr := s.PreviewDocumentSource(ctx, "ns", "/doc", doc)
	after := storageCounts(t, db)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("preview wrote storage: %s before=%d after=%d, preview error=%v",
				table, n, after[table], previewErr)
		}
	}
}

// TestPreviewDocumentSourceSurfacesMalformedRow: the corrupted chunk is
// reported as uncertainty, never silently treated as absent. The preview
// names the row it could not decode and predicts the write a real ingest
// would make once that row is quarantined out of its way.
func TestPreviewDocumentSourceSurfacesMalformedRow(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()
	doc := previewDoc("reviewer-doc")
	if _, _, _, _, err := s.WriteDocumentSource(ctx, "ns", "/doc", doc, "reviewer"); err != nil {
		t.Fatal(err)
	}
	clean, err := s.PreviewDocumentSource(ctx, "ns", "/doc", doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Malformed) != 0 || len(clean.Changed) != 0 || clean.Unchanged != 2 {
		t.Fatalf("clean preview = %+v, want 2 unchanged and nothing malformed", clean)
	}

	corruptChunkAttributes(t, db, "/doc")
	got, err := s.PreviewDocumentSource(ctx, "ns", "/doc", doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Malformed) != 2 {
		t.Fatalf("malformed = %+v, want both corrupted chunks reported", got.Malformed)
	}
	for _, m := range got.Malformed {
		if m.Key == "" || m.Reason == "" {
			t.Fatalf("malformed entry %+v must name the key and why it could not be decoded", m)
		}
		if strings.Contains(m.Reason, "{bad") {
			t.Fatalf("malformed reason %q must not carry the stored payload", m.Reason)
		}
	}
	if len(got.Changed) != 2 || got.Unchanged != 0 {
		t.Fatalf("preview = %d changed, %d unchanged; want the 2 chunks a real ingest would rewrite after quarantining",
			len(got.Changed), got.Unchanged)
	}
	for _, m := range got.Malformed {
		found := false
		for _, c := range got.Changed {
			if c.Key == m.Key {
				found = true
			}
		}
		if !found {
			t.Fatalf("malformed chunk %s is not predicted as changed: corrupted state must not be read as absence", m.Key)
		}
	}
}

// TestLiveChunkFactsStillQuarantines: the read-only preview path must not
// weaken the ordinary one. A real read still repairs a poisoned row so one
// bad record cannot brick a namespace.
func TestLiveChunkFactsStillQuarantines(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()
	doc := previewDoc("reviewer-doc")
	if _, _, _, _, err := s.WriteDocumentSource(ctx, "ns", "/doc", doc, "reviewer"); err != nil {
		t.Fatal(err)
	}
	corruptChunkAttributes(t, db, "/doc")

	before := storageCounts(t, db)
	facts, err := s.liveChunkFacts(ctx, "ns", "/doc")
	if err != nil {
		t.Fatal(err)
	}
	after := storageCounts(t, db)
	if len(facts) != 0 {
		t.Fatalf("live chunks = %d, want the poisoned rows quarantined out", len(facts))
	}
	if after["memories_quarantine"] != before["memories_quarantine"]+2 {
		t.Fatalf("quarantine rows = %d, want %d: an ordinary read must still repair",
			after["memories_quarantine"], before["memories_quarantine"]+2)
	}
	if after["memories"] != before["memories"]-2 {
		t.Fatalf("memories rows = %d, want %d: quarantine moves the row out",
			after["memories"], before["memories"]-2)
	}
}

// TestPreviewDocumentSourceReadOnlyOnCleanStore: the ordinary case stays
// read-only too - no row count moves when nothing is corrupted.
func TestPreviewDocumentSourceReadOnlyOnCleanStore(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()
	doc := previewDoc("clean-doc")
	if _, _, _, _, err := s.WriteDocumentSource(ctx, "ns", "/doc", doc, "writer"); err != nil {
		t.Fatal(err)
	}
	before := storageCounts(t, db)
	if _, err := s.PreviewDocumentSource(ctx, "ns", "/doc", doc); err != nil {
		t.Fatal(err)
	}
	after := storageCounts(t, db)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("preview wrote storage on a clean store: %s before=%d after=%d", table, n, after[table])
		}
	}
}
