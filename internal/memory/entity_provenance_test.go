package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTypedEntityProvenanceRetainsExactRevisions pins that typed entity
// provenance cites the EXACT contributing fact revisions, not just the
// mutable source key: after the source key is rewritten, the entity's
// source_facts still identifies the original revision, and re-enriching
// the newer revision APPENDS its ID without losing the prior one and
// without another mention_count bump - key-level mentions dedupe must
// not discard revision provenance. Regression (G01 review round 2,
// finding 1): the typed store kept only the mutable key /f1, so a
// rewrite of /f1 destroyed the attribution to the original evidence.
func TestTypedEntityProvenanceRetainsExactRevisions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structured: []ExtractedEntity{{Name: "Mercury", Type: "service", SourceFacts: []string{"/f1"}}}}
	s.SetEntityExtractor(ex)

	old, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Mercury runs on host7"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/f1"); err != nil {
		t.Fatal(err)
	}

	latest, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Mercury now runs on host9"})
	if err != nil {
		t.Fatal(err)
	}
	entity := entityAttr(t, s, ctx, "ns", "/entities/service/mercury")
	if got := attrStrings(entity, "source_facts"); len(got) != 1 || got[0] != old.ID {
		t.Fatalf("source_facts = %v, want [%s]: provenance must cite the exact contributing revision, not the mutable key", got, old.ID)
	}

	if _, err := s.EnrichEntities(ctx, "ns", "/f1"); err != nil {
		t.Fatal(err)
	}
	entity = entityAttr(t, s, ctx, "ns", "/entities/service/mercury")
	if got := attrStrings(entity, "source_facts"); len(got) != 2 || got[0] != old.ID || got[1] != latest.ID {
		t.Fatalf("source_facts = %v, want [%s %s]: re-enrichment must append the new contributing revision and retain the prior one", got, old.ID, latest.ID)
	}
	if mc := entity.Attributes["mention_count"].(float64); mc != 1 {
		t.Fatalf("mention_count = %v, want 1: a newer revision of an already-linked key is provenance, not a new mention", mc)
	}
	raw, _ := json.Marshal(entity)
	if !strings.Contains(string(raw), old.ID) || !strings.Contains(string(raw), latest.ID) {
		t.Fatalf("entity serialization lost a contributing revision: %s", raw)
	}
}

// TestTypedEntityInvalidSourcesNeverFabricateAttribution pins that a
// structured result citing only a fact OUTSIDE the call never expands
// to every input key: an unrelated fact in the same batch must not
// receive a mentions edge. The invalid citation is dropped and the
// bounded per-fact fallback (literal body evidence) decides
// attribution, citing that fact's exact revision. Regression (G01
// review round 2, finding 2): an entity citing only /not-in-call fell
// back to ALL input keys, so an unrelated weather bulletin was
// recorded as mentioning Mercury.
func TestTypedEntityInvalidSourcesNeverFabricateAttribution(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structured: []ExtractedEntity{{Name: "Mercury", Type: "service", SourceFacts: []string{"/not-in-call"}}}}
	s.SetEntityExtractor(ex)

	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Mercury service"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f2", Body: "Unrelated weather bulletin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/f1", "/f2"}); err != nil {
		t.Fatal(err)
	}

	f2Links, err := s.Neighbors(ctx, "ns", "/f2", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range f2Links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/service/mercury" {
			t.Fatalf("invalid attribution expanded to the whole batch: unrelated /f2 mentions Mercury: %v", f2Links)
		}
	}
	// the bounded fallback still keeps the genuinely evidenced entity,
	// attributed only to the fact whose body literally supports it.
	mercury := entityAttr(t, s, ctx, "ns", "/entities/service/mercury")
	if got := attrStrings(mercury, "source_facts"); len(got) != 1 || got[0] != f1.ID {
		t.Fatalf("source_facts = %v, want [%s]: only the literally-evidencing fact, by exact revision", got, f1.ID)
	}
	f1Links, err := s.Neighbors(ctx, "ns", "/f1", "out")
	if err != nil {
		t.Fatal(err)
	}
	saw := false
	for _, l := range f1Links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/service/mercury" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("/f1 must mention /entities/service/mercury: %v", f1Links)
	}
}

// TestTypedEntityMixedValidInvalidSourcesKeepValidSubset pins the mixed
// batch case: an entry citing both a valid fact and a bogus one keeps
// exactly the valid subset. The bogus citation is dropped, never
// replaced by the whole batch, and a valid citation set is never
// widened by the body-evidence fallback - an uncited fact containing
// the name stays unlinked.
func TestTypedEntityMixedValidInvalidSourcesKeepValidSubset(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structured: []ExtractedEntity{{
		Name: "Mercury", Type: "service", SourceFacts: []string{"/f1", "/bogus"},
	}}}
	s.SetEntityExtractor(ex)

	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Mercury service online"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f2", Body: "Mercury telemetry nominal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/f1", "/f2"}); err != nil {
		t.Fatal(err)
	}

	mercury := entityAttr(t, s, ctx, "ns", "/entities/service/mercury")
	if got := attrStrings(mercury, "source_facts"); len(got) != 1 || got[0] != f1.ID {
		t.Fatalf("source_facts = %v, want [%s]: valid subset only, bogus citation dropped", got, f1.ID)
	}
	f2Links, err := s.Neighbors(ctx, "ns", "/f2", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range f2Links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/service/mercury" {
			t.Fatalf("uncited /f2 must not be linked even though its body contains the name: %v", f2Links)
		}
	}
	inLinks, err := s.Neighbors(ctx, "ns", "/entities/service/mercury", "in")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range inLinks {
		if l.FromKey == "/bogus" {
			t.Fatalf("mentions edge from a fact outside the call must not exist: %v", inLinks)
		}
	}
}

// TestTypedEntityMissingSourcesUseBoundedPerFactFallback pins the
// missing-sources batch case: an entry with NO citations at all is
// attributed per-fact by literal body evidence only - an entity named
// in exactly one body links only that fact (by exact revision), and an
// entity named in none is skipped entirely instead of being attributed
// to every fact in the call.
func TestTypedEntityMissingSourcesUseBoundedPerFactFallback(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structured: []ExtractedEntity{
		{Name: "Mercury", Type: "service"}, // no sources; body evidence in /f1 only
		{Name: "Ghost", Type: "incident"},  // no sources; no body evidence anywhere
	}}
	s.SetEntityExtractor(ex)

	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Mercury service"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f2", Body: "Unrelated weather bulletin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/f1", "/f2"}); err != nil {
		t.Fatal(err)
	}

	mercury := entityAttr(t, s, ctx, "ns", "/entities/service/mercury")
	if got := attrStrings(mercury, "source_facts"); len(got) != 1 || got[0] != f1.ID {
		t.Fatalf("source_facts = %v, want [%s]: only the literally-evidencing fact", got, f1.ID)
	}
	if ghost, err := s.liveByKeys(ctx, "ns", []string{"/entities/incident/ghost"}); err != nil || len(ghost) != 0 {
		t.Fatalf("evidence-free entity must be skipped, not written: %v %v", ghost, err)
	}
	f2Links, err := s.Neighbors(ctx, "ns", "/f2", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range f2Links {
		if l.LinkType == "mentions" {
			t.Fatalf("/f2 evidences nothing and must carry no mentions edge: %v", f2Links)
		}
	}
}

// TestTypedEntityAcceptsRevisionIDCitations pins the extractor-side
// contract the cmd/punk adapter emits: citations that are exact fact
// revision IDs (EntitySource.ID) validate against the call and are
// retained as provenance, same as key citations.
func TestTypedEntityAcceptsRevisionIDCitations(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Mercury deploys the billing API"})
	if err != nil {
		t.Fatal(err)
	}
	ex := &structuredFake{structured: []ExtractedEntity{{Name: "Mercury", Type: "service", SourceFacts: []string{f1.ID}}}}
	s.SetEntityExtractor(ex)
	if _, err := s.EnrichEntities(ctx, "ns", "/f1"); err != nil {
		t.Fatal(err)
	}

	mercury := entityAttr(t, s, ctx, "ns", "/entities/service/mercury")
	if got := attrStrings(mercury, "source_facts"); len(got) != 1 || got[0] != f1.ID {
		t.Fatalf("source_facts = %v, want [%s] (revision-ID citation retained)", got, f1.ID)
	}
	links, err := s.Neighbors(ctx, "ns", "/f1", "out")
	if err != nil {
		t.Fatal(err)
	}
	saw := false
	for _, l := range links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/service/mercury" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("/f1 must mention /entities/service/mercury: %v", links)
	}
}
