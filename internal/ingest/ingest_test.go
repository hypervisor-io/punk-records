package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// newTestStore opens a migrated SQLite-backed memory store in a temp
// dir, mirroring internal/memory's newTest without the fake clock.
func newTestStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return memory.New(db, nil)
}

func liveChunks(t *testing.T, s *memory.Store, ns, prefix string) []memory.Fact {
	t.Helper()
	facts, err := s.Recall(context.Background(), ns, prefix+"/chunk-", 500)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func chunkSource(t *testing.T, f memory.Fact) map[string]any {
	t.Helper()
	src, ok := f.Attributes["source"].(map[string]any)
	if !ok {
		t.Fatalf("fact %s has no source attributes: %v", f.Key, f.Attributes)
	}
	return src
}

// assertOffsetsSliceAssembled proves every chunk's recorded source
// location: start/end must slice the chunk's exact body out of the
// assembled source text (sections joined by "\n\n", I01's coordinate
// space).
func assertOffsetsSliceAssembled(t *testing.T, doc *memory.SourceDocument, facts []memory.Fact) {
	t.Helper()
	texts := make([]string, len(doc.Sections))
	for i, sec := range doc.Sections {
		texts[i] = sec.Text
	}
	assembled := strings.Join(texts, "\n\n")
	if len(facts) == 0 {
		t.Fatal("no chunk facts written")
	}
	for _, f := range facts {
		src := chunkSource(t, f)
		start, ok1 := src["start"].(float64)
		end, ok2 := src["end"].(float64)
		if !ok1 || !ok2 {
			t.Fatalf("fact %s missing start/end offsets: %v", f.Key, src)
		}
		if int(end) > len(assembled) || assembled[int(start):int(end)] != f.Body {
			t.Fatalf("fact %s offsets [%v,%v) do not slice its body out of the assembled source", f.Key, start, end)
		}
	}
}

// TestMarkdownPreservesHeadingsAndProvenance is the Markdown half of the
// I02 red proof: a fixture's ATX headings survive both as section names
// in the provenance and verbatim in the chunk text, and every chunk
// carries source offsets that slice its body out of the assembled
// document.
func TestMarkdownPreservesHeadingsAndProvenance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/runbook.md", SourceID: "runbook"}

	doc, err := Load(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Source.MediaType; got != "text/markdown" {
		t.Fatalf("media type = %q, want text/markdown", got)
	}
	if !strings.HasPrefix(doc.Source.URI, "file://") {
		t.Fatalf("source URI = %q, want file:// provenance", doc.Source.URI)
	}
	wantNames := []string{"Runbook: Database Failover", "Detection", "Failover Steps"}
	if len(doc.Sections) != len(wantNames) {
		t.Fatalf("sections = %d, want %d: %+v", len(doc.Sections), len(wantNames), doc.Sections)
	}
	wantHeading := []string{"# Runbook: Database Failover", "## Detection", "## Failover Steps"}
	for i, sec := range doc.Sections {
		if sec.Name != wantNames[i] {
			t.Fatalf("section %d name = %q, want %q", i, sec.Name, wantNames[i])
		}
		if !strings.Contains(sec.Text, wantHeading[i]) {
			t.Fatalf("section %q text lost its heading line %q: %q", sec.Name, wantHeading[i], sec.Text)
		}
	}

	w, u, r, b, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec)
	if err != nil || w != 6 || u != 0 || r != 0 || b != 0 {
		t.Fatalf("first ingest = %d/%d/%d/%d (err %v), want 6/0/0/0", w, u, r, b, err)
	}
	facts := liveChunks(t, s, "ns", "/docs/rb")
	byBody := map[string]memory.Fact{}
	for _, f := range facts {
		byBody[f.Body] = f
		src := chunkSource(t, f)
		if src["id"] != "runbook" {
			t.Fatalf("fact %s source id = %v, want runbook", f.Key, src["id"])
		}
		if src["media_type"] != "text/markdown" {
			t.Fatalf("fact %s media_type = %v", f.Key, src["media_type"])
		}
	}
	if _, ok := byBody["# Runbook: Database Failover"]; !ok {
		t.Fatal("heading line chunk missing")
	}
	det := chunkSource(t, byBody["## Detection"])
	if det["section"] != "Detection" {
		t.Fatalf("Detection chunk section = %v, want Detection", det["section"])
	}
	assertOffsetsSliceAssembled(t, doc, facts)
}

// TestRepeatedIngestWritesNoUnchangedChunks is the acceptance proof that
// loader output rides I01's delta ingest: a byte-identical reload writes
// nothing and keeps every fact ID, and an early insertion writes exactly
// one new chunk while leaving the survivors' keys and IDs untouched.
func TestRepeatedIngestWritesNoUnchangedChunks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/runbook.md", SourceID: "runbook"}

	if _, _, _, _, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{} // body -> fact ID
	for _, f := range liveChunks(t, s, "ns", "/docs/rb") {
		before[f.Body] = f.ID
	}

	w, u, r, b, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec)
	if err != nil || w != 0 || u != 6 || r != 0 || b != 0 {
		t.Fatalf("repeat ingest = %d/%d/%d/%d (err %v), want 0/6/0/0: unchanged chunks must not be rewritten", w, u, r, b, err)
	}
	for _, f := range liveChunks(t, s, "ns", "/docs/rb") {
		if before[f.Body] != f.ID {
			t.Fatalf("fact for %q got a new ID across an unchanged re-ingest", f.Body)
		}
	}

	// An early insertion rewrites only the inserted paragraph.
	raw, err := os.ReadFile("testdata/runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw),
		"This runbook covers postgres failover.",
		"New intro paragraph.\n\nThis runbook covers postgres failover.", 1)
	if edited == string(raw) {
		t.Fatal("fixture edit did not apply")
	}
	tmp := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(tmp, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.Path = tmp
	w, u, r, b, err = Ingest(ctx, s, "ns", "/docs/rb", "t", spec)
	if err != nil || w != 1 || u != 6 || r != 0 || b != 0 {
		t.Fatalf("insert ingest = %d/%d/%d/%d (err %v), want 1/6/0/0", w, u, r, b, err)
	}
	for _, f := range liveChunks(t, s, "ns", "/docs/rb") {
		if f.Body == "New intro paragraph." {
			continue
		}
		if before[f.Body] != f.ID {
			t.Fatalf("surviving chunk %q lost its identity after an early insertion", f.Body)
		}
	}
}

// TestPlainTextLoader: plain text ingests as one unnamed section.
func TestPlainTextLoader(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile("testdata/notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Load(ctx, Spec{Path: "testdata/notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source.MediaType != "text/plain" {
		t.Fatalf("media type = %q, want text/plain", doc.Source.MediaType)
	}
	if len(doc.Sections) != 1 || doc.Sections[0].Name != "" {
		t.Fatalf("sections = %+v, want one unnamed section", doc.Sections)
	}
	if doc.Sections[0].Text != string(raw) {
		t.Fatalf("text loader must pass the body through verbatim")
	}
	s := newTestStore(t)
	w, u, r, b, err := Ingest(ctx, s, "ns", "/notes/a", "t", Spec{Path: "testdata/notes.txt"})
	if err != nil || w != 2 || u+r+b != 0 {
		t.Fatalf("ingest = %d/%d/%d/%d (err %v), want 2/0/0/0", w, u, r, b, err)
	}
}

// TestDetectMediaType pins detection priority: explicit flag, then
// extension, then content type, then byte sniffing.
func TestDetectMediaType(t *testing.T) {
	cases := []struct {
		name, contentType, explicit string
		body                        []byte
		want                        string
		wantErr                     string
	}{
		{explicit: "md", want: "text/markdown"},
		{explicit: "incident-json", want: IncidentMediaType},
		{explicit: "bogus", wantErr: "unknown format"},
		{name: "a.md", want: "text/markdown"},
		{name: "a.markdown", want: "text/markdown"},
		{name: "a.html", want: "text/html"},
		{name: "a.json", want: IncidentMediaType},
		{name: "a.pdf", want: PDFMediaType},
		{name: "a.txt", want: "text/plain"},
		// extension beats the served content type
		{name: "a.md", contentType: "text/html", want: "text/markdown"},
		{name: "x", contentType: "text/html; charset=utf-8", want: "text/html"},
		{name: "x", contentType: "application/json", want: IncidentMediaType},
		{name: "x", body: []byte("%PDF-1.7 binary"), want: PDFMediaType},
		{name: "x", body: []byte(`{"title":"t","summary":"s"}`), want: IncidentMediaType},
		{name: "x", body: []byte("plain utf8 words"), want: "text/plain"},
		{name: "x", body: []byte{0xff, 0x00, 0x01}, wantErr: "cannot detect"},
	}
	for _, c := range cases {
		got, err := DetectMediaType(c.name, c.contentType, c.body, c.explicit)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("%+v: err = %v, want substring %q", c, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%+v: got %q, err %v; want %q", c, got, err, c.want)
		}
	}
}

func TestLoaderForUnsupportedMediaType(t *testing.T) {
	_, err := LoaderForMediaType("application/msword")
	if err == nil || !strings.Contains(err.Error(), "unsupported") ||
		!strings.Contains(err.Error(), "text/markdown") {
		t.Fatalf("err = %v, want an actionable unsupported-format message naming the supported loaders", err)
	}
}

// TestEmptyInputRefused: a whitespace-only input must fail loudly rather
// than reconcile the prefix's chunks down to zero.
func TestEmptyInputRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.md")
	if err := os.WriteFile(p, []byte("  \n\n "), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t)
	_, err := Load(context.Background(), Spec{Path: p})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want an empty-input error", err)
	}
	if _, _, _, _, err := Ingest(context.Background(), s, "ns", "/docs/e", "t", Spec{Path: p}); err == nil {
		t.Fatal("empty ingest must fail before any write")
	}
	if got := liveChunks(t, s, "ns", "/docs/e"); len(got) != 0 {
		t.Fatalf("empty ingest wrote %d chunks", len(got))
	}
}

// TestSizeLimit: inputs above MaxBytes fail before any write, with the
// cap named in the error.
func TestSizeLimit(t *testing.T) {
	s := newTestStore(t)
	_, err := Load(context.Background(), Spec{Path: "testdata/runbook.md", MaxBytes: 8})
	if err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("err = %v, want a max_bytes error", err)
	}
	if _, _, _, _, err := Ingest(context.Background(), s, "ns", "/docs/big", "t",
		Spec{Path: "testdata/runbook.md", MaxBytes: 8}); err == nil {
		t.Fatal("oversize ingest must fail before any write")
	}
	if got := liveChunks(t, s, "ns", "/docs/big"); len(got) != 0 {
		t.Fatalf("oversize ingest wrote %d chunks", len(got))
	}
}

// TestLoaderOutputPassesDefense: loader output rides the store's
// write-time defense path (acceptance: all writes pass
// defense/provenance handling).
func TestLoaderOutputPassesDefense(t *testing.T) {
	s := newTestStore(t)
	s.SetDefense("redact")
	p := filepath.Join(t.TempDir(), "secrets.md")
	body := "# Secrets\n\nThe db password=hunter2x is rotated monthly.\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := Ingest(context.Background(), s, "ns", "/docs/sec", "t", Spec{Path: p}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range liveChunks(t, s, "ns", "/docs/sec") {
		if strings.Contains(f.Body, "rotated monthly") {
			found = true
			if strings.Contains(f.Body, "hunter2x") || !strings.Contains(f.Body, "[REDACTED:password]") {
				t.Fatalf("secret survived loader ingest: %q", f.Body)
			}
		}
	}
	if !found {
		t.Fatal("secret paragraph chunk missing")
	}
}
