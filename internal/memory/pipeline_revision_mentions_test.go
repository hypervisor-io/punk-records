package memory

import (
	"testing"
	"time"
)

// TestReviewerP01NewRevisionDoesNotDoubleCountMention is the reviewer's
// r4 repro: mention counts are per source KEY. Writing a newer revision
// of an already-enriched key and enriching again appends that revision's
// ID to source_facts provenance, but the key still contributes exactly
// one mentions edge and one mention_count - the provenance-only update
// must not inflate the typed entity's counts.
func TestReviewerP01NewRevisionDoesNotDoubleCountMention(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	var ids []string
	for _, body := range []string{"Mercury service original", "Mercury service updated"} {
		clk.Set(s.now().Add(time.Second))
		f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: body})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.ID)
		clk.Set(s.now().Add(time.Second))
		s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())
	}
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("entity %v %v", fs, err)
	}
	if got := fs[0].Attributes["mention_count"]; got != float64(1) {
		t.Errorf("same source key counted twice across revisions: mention_count=%v want1", got)
	}
	gotIDs := attrStringList(fs[0], "source_facts")
	for _, want := range ids {
		found := false
		for _, got := range gotIDs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Error("missing source revision", want)
		}
	}
}

// TestPipelineEnrichmentPreservesEntityLineageAttrs is the P01 half of
// the G02 coordination requirement: entity facts can carry attributes
// the enricher does not own - G02's merges/merge_base undo lineage - and
// planTypedSourceApply rebuilds the entity's attributes from the
// extraction output. An ordinary durable enrichment of a newer source
// revision must preserve those foreign attributes while adding the new
// revision's provenance, and must keep per-key counts intact doing it.
func TestPipelineEnrichmentPreservesEntityLineageAttrs(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	s.SetEntityExtractor(reviewerP01TypedExtractor{})

	clk.Set(s.now().Add(time.Second))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service original"}); err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(time.Second))
	s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())

	// a merge (G02) records its undo lineage on the canonical entity fact
	cur, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(cur) != 1 {
		t.Fatalf("entity %v %v", cur, err)
	}
	attrs := map[string]any{}
	for k, v := range cur[0].Attributes {
		attrs[k] = v
	}
	attrs["merges"] = []any{map[string]any{"alias": "/entities/service/merc", "added_alias": "Merc"}}
	attrs["merge_base"] = "/entities/service/mercury"
	clk.Set(s.now().Add(time.Second))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/service/mercury",
		Body: cur[0].Body, Attributes: attrs, Writer: "merger"}); err != nil {
		t.Fatal(err)
	}

	// ordinary enrichment of a newer revision of the source key
	clk.Set(s.now().Add(time.Second))
	f2, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service updated"})
	if err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(time.Second))
	s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())

	got, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(got) != 1 {
		t.Fatalf("entity %v %v", got, err)
	}
	if m, ok := got[0].Attributes["merges"].([]any); !ok || len(m) != 1 {
		t.Errorf("ordinary enrichment dropped the merges undo lineage: %v", got[0].Attributes["merges"])
	}
	if mb, _ := got[0].Attributes["merge_base"].(string); mb != "/entities/service/mercury" {
		t.Errorf("merge_base = %v, want preserved", got[0].Attributes["merge_base"])
	}
	if mc := got[0].Attributes["mention_count"]; mc != float64(1) {
		t.Errorf("mention_count = %v, want 1 (one mentioning source key)", mc)
	}
	ids := attrStringList(got[0], "source_facts")
	if len(ids) != 2 || ids[1] != f2.ID {
		t.Errorf("source_facts = %v, want the new revision %s appended", ids, f2.ID)
	}
	links, err := s.Neighbors(ctx, "ns", "/f", "out")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/service/mercury" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("mentions edges = %d, want 1", n)
	}
}
