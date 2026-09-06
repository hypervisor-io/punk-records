package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// TestHTMLTextOrder is the HTML half of the I02 red proof: text comes
// out in document order, h1/h2 become named sections, script/style/head
// content is dropped and entities are decoded.
func TestHTMLTextOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/page.html", SourceID: "postmortem-alpha"}

	doc, err := Load(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source.MediaType != "text/html" {
		t.Fatalf("media type = %q, want text/html", doc.Source.MediaType)
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("sections = %d, want 2: %+v", len(doc.Sections), doc.Sections)
	}
	if doc.Sections[0].Name != "Postmortem Alpha" {
		t.Fatalf("section 0 name = %q", doc.Sections[0].Name)
	}
	if doc.Sections[1].Name != "Timeline" {
		t.Fatalf("section 1 name = %q", doc.Sections[1].Name)
	}
	want0 := "Postmortem Alpha\n\nFirst paragraph alpha."
	if doc.Sections[0].Text != want0 {
		t.Fatalf("section 0 text = %q, want %q", doc.Sections[0].Text, want0)
	}
	want1 := "Timeline\n\n14:02 pool exhausted\n\n14:10 failover started\n\nClosing note & summary."
	if doc.Sections[1].Text != want1 {
		t.Fatalf("section 1 text = %q, want %q", doc.Sections[1].Text, want1)
	}
	for _, sec := range doc.Sections {
		for _, banned := range []string{"leak", "color: red", "Ignored Title"} {
			if strings.Contains(sec.Text, banned) {
				t.Fatalf("section %q contains %q: script/style/head content must be dropped", sec.Name, banned)
			}
		}
	}

	w, u, r, b, err := Ingest(ctx, s, "ns", "/docs/alpha", "t", spec)
	if err != nil || w != 6 || u+r+b != 0 {
		t.Fatalf("ingest = %d/%d/%d/%d (err %v), want 6/0/0/0", w, u, r, b, err)
	}
	facts := liveChunks(t, s, "ns", "/docs/alpha")
	byBody := map[string]string{}
	for _, f := range facts {
		byBody[f.Body] = f.Key
	}
	// document order in the stored chunks: the Timeline items come after
	// the first paragraph and before the closing note.
	order := []string{"First paragraph alpha.", "14:02 pool exhausted", "14:10 failover started", "Closing note & summary."}
	for _, body := range order {
		if _, ok := byBody[body]; !ok {
			t.Fatalf("chunk %q missing: %v", body, byBody)
		}
	}
	tl := chunkSource(t, mustFact(t, facts, "14:10 failover started"))
	if tl["section"] != "Timeline" {
		t.Fatalf("timeline chunk section = %v", tl["section"])
	}
	assertOffsetsSliceAssembled(t, doc, facts)
}

func mustFact(t *testing.T, facts []memory.Fact, body string) memory.Fact {
	t.Helper()
	for _, f := range facts {
		if f.Body == body {
			return f
		}
	}
	t.Fatalf("fact with body %q missing", body)
	return memory.Fact{}
}
