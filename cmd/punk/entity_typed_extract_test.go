package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// fakeLLM returns a fixed chat response, recording nothing.
type fakeLLM struct {
	content string
	err     error
}

func (f *fakeLLM) Chat(_ context.Context, _ []llm.Turn, _ []llm.Tool) (*llm.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Result{Content: f.content}, nil
}

func (f *fakeLLM) Model() string { return "fake" }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestExtractStructuredParsesAndMapsSources(t *testing.T) {
	ex := &typedEntityExtractor{entityExtractor: &entityExtractor{client: &fakeLLM{content: "```json\n" +
		`[{"name":"Mercury","type":"service","aliases":["mercury-api"],"sources":[1,2,99]},` +
		`{"name":"Alice","type":"person","sources":[2]}]` + "\n```"}, log: testLogger()}}
	sources := []memory.EntitySource{{ID: "rev-a", Key: "/a", Body: "Mercury deploys"}, {ID: "rev-b", Key: "/b", Body: "Alice owns Mercury"}}
	ents, err := ex.ExtractStructured(t.Context(), sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("entities = %v, want 2", ents)
	}
	if ents[0].Name != "Mercury" || ents[0].Type != "service" {
		t.Fatalf("entity[0] = %+v", ents[0])
	}
	// source numbers map 1-based to the exact fact revision IDs (the
	// provenance anchor the store retains); out-of-range 99 is dropped.
	if len(ents[0].SourceFacts) != 2 || ents[0].SourceFacts[0] != "rev-a" || ents[0].SourceFacts[1] != "rev-b" {
		t.Fatalf("source facts = %v, want [rev-a rev-b]", ents[0].SourceFacts)
	}
	if len(ents[0].Aliases) != 1 || ents[0].Aliases[0] != "mercury-api" {
		t.Fatalf("aliases = %v", ents[0].Aliases)
	}
	if len(ents[1].SourceFacts) != 1 || ents[1].SourceFacts[0] != "rev-b" {
		t.Fatalf("source facts = %v, want [rev-b]", ents[1].SourceFacts)
	}
}

// TestExtractStructuredMapsRevisionIDsNotKeys pins that repeated source
// numbers dedupe by revision ID and a source without a revision
// identity is skipped rather than cited as an empty string - the store
// validates citations against the call's real revision IDs, so the
// adapter must never emit keys or blanks.
func TestExtractStructuredMapsRevisionIDsNotKeys(t *testing.T) {
	ex := &typedEntityExtractor{entityExtractor: &entityExtractor{client: &fakeLLM{content: `[{"name":"Mercury","type":"service","sources":[1,1,2]}]`}, log: testLogger()}}
	sources := []memory.EntitySource{{ID: "rev-a", Key: "/a", Body: "Mercury deploys"}, {Key: "/b", Body: "no revision identity"}}
	ents, err := ex.ExtractStructured(t.Context(), sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("entities = %v, want 1", ents)
	}
	if len(ents[0].SourceFacts) != 1 || ents[0].SourceFacts[0] != "rev-a" {
		t.Fatalf("source facts = %v, want [rev-a] (duplicate deduped, identity-less source skipped)", ents[0].SourceFacts)
	}
}

func TestExtractStructuredUnparseable(t *testing.T) {
	ex := &typedEntityExtractor{entityExtractor: &entityExtractor{client: &fakeLLM{content: "not json at all"}, log: testLogger()}}
	if _, err := ex.ExtractStructured(t.Context(), []memory.EntitySource{{Key: "/a", Body: "x"}}); err == nil {
		t.Fatal("want error on unparseable response (store falls back to legacy)")
	}
	ex = &typedEntityExtractor{entityExtractor: &entityExtractor{client: &fakeLLM{err: errors.New("down")}, log: testLogger()}}
	if _, err := ex.ExtractStructured(t.Context(), []memory.EntitySource{{Key: "/a", Body: "x"}}); err == nil {
		t.Fatal("want error propagated when the client fails")
	}
}
