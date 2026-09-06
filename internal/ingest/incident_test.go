package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIncidentJSONFieldsWithSourceLocations is the incident half of the
// I02 red proof: every structured field lands in a section named after
// its JSON field, and the written chunks carry those field names plus
// exact offsets as source locations.
func TestIncidentJSONFieldsWithSourceLocations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/incident.json", SourceID: "incident-2041"}

	doc, err := Load(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source.MediaType != IncidentMediaType {
		t.Fatalf("media type = %q, want %q", doc.Source.MediaType, IncidentMediaType)
	}
	wantNames := []string{"incident", "summary", "impact", "root_cause", "timeline", "action_items"}
	if len(doc.Sections) != len(wantNames) {
		t.Fatalf("sections = %d, want %d: %+v", len(doc.Sections), len(wantNames), doc.Sections)
	}
	for i, name := range wantNames {
		if doc.Sections[i].Name != name {
			t.Fatalf("section %d name = %q, want %q", i, doc.Sections[i].Name, name)
		}
	}
	header := doc.Sections[0].Text
	for _, want := range []string{"INC-2041", "Postgres connection pool exhausted", "resolved", "sev2", "2026-08-14T14:02:00Z"} {
		if !strings.Contains(header, want) {
			t.Fatalf("incident header lost field %q: %q", want, header)
		}
	}
	if got := doc.Sections[1].Text; got != "The primary database ran out of client connections during the deploy." {
		t.Fatalf("summary section = %q", got)
	}
	tl := doc.Sections[4].Text
	first := strings.Index(tl, "Alerts fired on connection saturation.")
	second := strings.Index(tl, "Pool recovered after restart.")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("timeline lost event order: %q", tl)
	}
	if !strings.Contains(doc.Sections[5].Text, "dba-team") {
		t.Fatalf("action items lost the owner: %q", doc.Sections[5].Text)
	}

	w, u, r, b, err := Ingest(ctx, s, "ns", "/incidents/2041", "t", spec)
	if err != nil || w != 7 || u+r+b != 0 {
		t.Fatalf("ingest = %d/%d/%d/%d (err %v), want 7/0/0/0", w, u, r, b, err)
	}
	facts := liveChunks(t, s, "ns", "/incidents/2041")
	sectionsSeen := map[string]bool{}
	for _, f := range facts {
		src := chunkSource(t, f)
		name, _ := src["section"].(string)
		sectionsSeen[name] = true
	}
	for _, name := range wantNames {
		if !sectionsSeen[name] {
			t.Fatalf("no chunk carries source location for field %q", name)
		}
	}
	assertOffsetsSliceAssembled(t, doc, facts)
}

// TestIncidentStrictValidation: unknown fields, missing required fields
// and non-incident JSON are visible errors, never silent drops.
func TestIncidentStrictValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"unknown-field", `{"title":"x","summary":"y","bogus":1}`, "bogus"},
		{"not-an-incident", `{"foo":"bar"}`, "unknown field"},
		{"no-content", `{"title":"only a title"}`, "summary"},
		{"bad-json", `{not json`, "invalid"},
		{"trailing", `{"title":"x","summary":"y"} {"title":"z"}`, "trailing"},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), c.name+".json")
		if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(context.Background(), Spec{Path: p})
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("%s: err = %v, want substring %q", c.name, err, c.wantErr)
		}
	}
}
