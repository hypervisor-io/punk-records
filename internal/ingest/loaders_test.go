package ingest

import (
	"context"
	"testing"
)

// TestMarkdownFenceTracksOpeningMarker: a ``` fence containing a ~~~
// line (a common pattern when a code sample itself demonstrates a
// tilde-fenced block) must not be closed by the mismatched marker. Only
// a line starting with the SAME marker that opened the fence closes it,
// so a "# Not a heading" line inside the still-open ``` fence stays
// fenced text, not a new section.
func TestMarkdownFenceTracksOpeningMarker(t *testing.T) {
	body := "# Doc\n\n" +
		"```\n" +
		"~~~\n" +
		"# Not a heading\n" +
		"```\n" +
		"\n" +
		"## Real Section\n" +
		"text\n"
	res, err := markdownLoader{}.Load(context.Background(), LoadInput{Name: "x.md", Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sections) != 2 {
		t.Fatalf("sections = %d, want 2 (Doc, Real Section): %+v", len(res.Sections), res.Sections)
	}
	if res.Sections[0].Name != "Doc" {
		t.Fatalf("section 0 name = %q, want Doc", res.Sections[0].Name)
	}
	for _, sec := range res.Sections {
		if sec.Name == "Not a heading" {
			t.Fatalf("the fenced \"# Not a heading\" line must not start its own section: %+v", res.Sections)
		}
	}
	if res.Sections[1].Name != "Real Section" {
		t.Fatalf("section 1 name = %q, want Real Section", res.Sections[1].Name)
	}
}
