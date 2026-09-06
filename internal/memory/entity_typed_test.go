package memory

import (
	"context"
	"errors"
	"testing"
)

// structuredFake implements both the legacy EntityExtractor and the new
// StructuredEntityExtractor, counting calls so tests can pin which path
// ran.
type structuredFake struct {
	names        []string // legacy Extract answer
	structured   []ExtractedEntity
	structErr    error
	structCalls  int
	lastSources  []EntitySource
	extractCalls int
}

func (f *structuredFake) Extract(_ context.Context, _ string) ([]string, error) {
	f.extractCalls++
	return f.names, nil
}

func (f *structuredFake) ExtractStructured(_ context.Context, sources []EntitySource) ([]ExtractedEntity, error) {
	f.structCalls++
	f.lastSources = sources
	if f.structErr != nil {
		return nil, f.structErr
	}
	return f.structured, nil
}

func entityAttr(t *testing.T, s *Store, ctx context.Context, ns, key string) Fact {
	t.Helper()
	facts, err := s.liveByKeys(ctx, ns, []string{key})
	if err != nil || len(facts) != 1 {
		t.Fatalf("liveByKeys(%q) = %v, %v; want exactly 1", key, facts, err)
	}
	return facts[0]
}

func attrStrings(f Fact, name string) []string {
	raw, ok := f.Attributes[name].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestTypedEntitiesSameNameDistinctTypes is the red proof: one display
// name ("Mercury") used for a person and a service must produce TWO
// distinct canonical entities, each retaining its declared type and the
// provenance of the fact that evidenced it.
func TestTypedEntitiesSameNameDistinctTypes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structured: []ExtractedEntity{
		{Name: "Mercury", Type: "person", SourceFacts: []string{"/f1"}},
		{Name: "Mercury", Type: "service", SourceFacts: []string{"/f2"}},
	}}
	s.SetEntityExtractor(ex)

	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Mercury joined the oncall rota"})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f2", Body: "Mercury deploys the billing API"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/f1", "/f2"}); err != nil {
		t.Fatal(err)
	}
	if ex.structCalls != 1 {
		t.Fatalf("structured calls = %d, want 1 (one batched call)", ex.structCalls)
	}

	person := entityAttr(t, s, ctx, "ns", "/entities/person/mercury")
	if person.Body != "Mercury" {
		t.Fatalf("person body = %q, want display name %q", person.Body, "Mercury")
	}
	if person.Attributes["entity_type"] != "person" {
		t.Fatalf("person entity_type = %v, want person", person.Attributes["entity_type"])
	}
	service := entityAttr(t, s, ctx, "ns", "/entities/service/mercury")
	if service.Attributes["entity_type"] != "service" {
		t.Fatalf("service entity_type = %v, want service", service.Attributes["entity_type"])
	}

	// provenance: each entity cites only its own source fact, by exact
	// contributing revision ID (not the mutable key).
	if got := attrStrings(person, "source_facts"); len(got) != 1 || got[0] != f1.ID {
		t.Fatalf("person source_facts = %v, want [%s]", got, f1.ID)
	}
	if got := attrStrings(service, "source_facts"); len(got) != 1 || got[0] != f2.ID {
		t.Fatalf("service source_facts = %v, want [%s]", got, f2.ID)
	}

	// the legacy untyped key must NOT appear in typed mode.
	if dup, err := s.liveByKeys(ctx, "ns", []string{"/entities/mercury"}); err != nil || len(dup) != 0 {
		t.Fatalf("/entities/mercury should not exist in typed mode: %v %v", dup, err)
	}

	// mentions edges land on the type-correct canonical entity.
	f1Links, err := s.Neighbors(ctx, "ns", "/f1", "out")
	if err != nil {
		t.Fatal(err)
	}
	sawPerson := false
	for _, l := range f1Links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/person/mercury" {
			sawPerson = true
		}
		if l.LinkType == "mentions" && l.ToKey == "/entities/service/mercury" {
			t.Fatalf("/f1 must not mention the service entity: %v", f1Links)
		}
	}
	if !sawPerson {
		t.Fatalf("/f1 must mention /entities/person/mercury: %v", f1Links)
	}
}

// TestTypedEntitiesDisabledUsesLegacy pins that an extractor implementing
// only the legacy name-only interface keeps producing untyped legacy
// /entities/<slug> keys - typed mode is opt-in via the
// StructuredEntityExtractor interface, so its absence is disabled mode.
func TestTypedEntitiesDisabledUsesLegacy(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := fakeExtractor{names: []string{"Alice"}}
	s.SetEntityExtractor(ex)

	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Alice joined"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/f"); err != nil {
		t.Fatal(err)
	}
	e := entityAttr(t, s, ctx, "ns", "/entities/alice")
	if _, ok := e.Attributes["entity_type"]; ok {
		t.Fatalf("legacy entity must not carry entity_type: %v", e.Attributes)
	}
}

// TestTypedEntitiesMalformedSkipped is the red proof that malformed
// structured results never write partial invalid entities: an empty-name
// entry writes nothing, and provenance referencing facts outside the call
// is dropped rather than linked.
func TestTypedEntitiesMalformedSkipped(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structured: []ExtractedEntity{
		{Name: "   ", Type: "person", SourceFacts: []string{"/f1"}},              // malformed: no name
		{Name: "Mercury", Type: "person", SourceFacts: []string{"/not-in-call"}}, // bogus provenance
		{Name: "Freddie", Type: "person", SourceFacts: []string{"/f1"}},
	}}
	s.SetEntityExtractor(ex)

	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Mercury and Freddie"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.EnrichEntities(ctx, "ns", "/f1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("applied = %d, want 2 (empty-name entry dropped)", n)
	}

	ents, err := s.Recall(ctx, "ns", "/entities", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("entities = %v, want exactly 2 (no partial invalid writes)", ents)
	}

	// bogus source fact is dropped; with nothing valid left a
	// single-fact call attributes to that one fact (bounded per-fact
	// fallback), never to an invented key - and provenance cites the
	// exact contributing revision, not the mutable key.
	mercury := entityAttr(t, s, ctx, "ns", "/entities/person/mercury")
	if got := attrStrings(mercury, "source_facts"); len(got) != 1 || got[0] != f1.ID {
		t.Fatalf("mercury source_facts = %v, want [%s] (bogus provenance dropped)", got, f1.ID)
	}
	links, err := s.Neighbors(ctx, "ns", "/entities/person/mercury", "in")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.FromKey == "/not-in-call" {
			t.Fatalf("mentions edge to a fact outside the call must not exist: %v", links)
		}
	}
}

// TestTypedEntitiesUnknownTypePolicy pins the explicit policy for a
// declared type outside the fixed vocabulary: the entity is stored under
// the unknown type, with the declared type retained for inspection.
func TestTypedEntitiesUnknownTypePolicy(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structured: []ExtractedEntity{
		{Name: "Voyager", Type: "vessel", SourceFacts: []string{"/f1"}},
		{Name: "NoType", Type: "", SourceFacts: []string{"/f1"}},
	}}
	s.SetEntityExtractor(ex)

	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Voyager met NoType"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/f1"); err != nil {
		t.Fatal(err)
	}

	v := entityAttr(t, s, ctx, "ns", "/entities/unknown/voyager")
	if v.Attributes["entity_type"] != "unknown" {
		t.Fatalf("entity_type = %v, want unknown", v.Attributes["entity_type"])
	}
	if v.Attributes["declared_type"] != "vessel" {
		t.Fatalf("declared_type = %v, want vessel (retained for inspection)", v.Attributes["declared_type"])
	}
	nt := entityAttr(t, s, ctx, "ns", "/entities/unknown/notype")
	if nt.Attributes["entity_type"] != "unknown" {
		t.Fatalf("entity_type = %v, want unknown", nt.Attributes["entity_type"])
	}
	if _, ok := nt.Attributes["declared_type"]; ok {
		t.Fatalf("empty declared type must not be recorded: %v", nt.Attributes)
	}
}

// TestTypedEntitiesStructuredErrorFallsBack: a failing structured call
// degrades to legacy per-fact extraction with results adapted to type
// unknown, never drops the batch. This also pins the legacy name-only
// interface adaptation: fallback results arrive as plain names and are
// adapted into structured form.
func TestTypedEntitiesStructuredErrorFallsBack(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structErr: errors.New("model exploded"), names: []string{"Alice"}}
	s.SetEntityExtractor(ex)

	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Alice joined"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/f"); err != nil {
		t.Fatal(err)
	}
	if ex.structCalls != 1 || ex.extractCalls != 1 {
		t.Fatalf("structured=%d legacy=%d, want 1/1 (fallback)", ex.structCalls, ex.extractCalls)
	}
	entityAttr(t, s, ctx, "ns", "/entities/unknown/alice")
}

// TestTypedEntitiesAliasMergesSameType: an extracted alias resolves a
// later same-type mention to the same canonical entity, while the alias
// list is retained on the entity fact.
func TestTypedEntitiesAliasMergesSameType(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{}
	s.SetEntityExtractor(ex)

	ex.structured = []ExtractedEntity{{Name: "Alice Chen", Type: "person", Aliases: []string{"Al"}, SourceFacts: []string{"/f1"}}}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f1", Body: "Alice Chen filed the report"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/f1"); err != nil {
		t.Fatal(err)
	}

	ex.structured = []ExtractedEntity{{Name: "Al", Type: "person", SourceFacts: []string{"/f2"}}}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f2", Body: "Al joined the call"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/f2"); err != nil {
		t.Fatal(err)
	}

	ents, err := s.Recall(ctx, "ns", "/entities", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("entities = %v, want 1 (alias mention must merge same-type)", ents)
	}
	if ents[0].Key != "/entities/person/alice-chen" {
		t.Fatalf("entity key = %q, want /entities/person/alice-chen", ents[0].Key)
	}
	if ents[0].Attributes["mention_count"].(float64) != 2 {
		t.Fatalf("mention_count = %v, want 2", ents[0].Attributes["mention_count"])
	}
	got := attrStrings(ents[0], "aliases")
	if len(got) != 1 || got[0] != "Al" {
		t.Fatalf("aliases = %v, want [Al]", got)
	}
}

// TestTypedEntityArmRetrieval: typed entities still participate in the
// entity arm of hybrid search - the E01 retrieval baseline behavior is
// preserved under the new key shape.
func TestTypedEntityArmRetrieval(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ex := &structuredFake{structured: []ExtractedEntity{
		{Name: "Mercury", Type: "service", SourceFacts: []string{"/notes/n1"}},
	}}
	s.SetEntityExtractor(ex)

	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/n1", Body: "decided to defer the migration", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/notes/n1"); err != nil {
		t.Fatal(err)
	}

	hits, err := s.HybridSearchScored(ctx, "ns", "mercury", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Key == "/notes/n1" {
			found = true
			if h.Components["entity"] <= 0 {
				t.Fatalf("entity component missing: %+v", h.Components)
			}
		}
	}
	if !found {
		t.Fatalf("fact mentioning typed entity Mercury must surface for query 'mercury': %+v", hits)
	}
}
