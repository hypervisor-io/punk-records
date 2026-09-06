package memory

import (
	"context"
	"testing"
	"time"
)

// Reviewer regression probes (G02 revision 2), kept as permanent tests
// with the exact review scenarios. reviewerG02Seed writes one fact
// directly; reviewerG02Proposal discovers the real proposal for a pair.
func reviewerG02Seed(t *testing.T, s *Store, key, body string, attrs map[string]any) {
	t.Helper()
	if _, err := s.Write(t.Context(), WriteInput{Namespace: "ns", Key: key, Body: body, Attributes: attrs}); err != nil {
		t.Fatal(err)
	}
}

func reviewerG02Proposal(t *testing.T, s *Store, alias, canonical string) EntityMergeProposal {
	t.Helper()
	ps, err := s.ProposeEntityMerges(t.Context(), "ns", MergeProposalOptions{IncludeProtected: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.AliasKey == alias && p.CanonicalKey == canonical {
			return p
		}
	}
	t.Fatalf("missing real proposal %s -> %s: %+v", alias, canonical, ps)
	return EntityMergeProposal{}
}

// TestTypedEnrichmentRetainsMergeLineageForUndo: an ordinary typed
// enrichment landing on a merge canonical must carry the merges /
// merge_base lineage forward, so a later undo still subtracts exactly
// what the merge contributed (alias-only source provenance, merge-created
// mention edges) while preserving legitimate provenance the enrichment
// added after the merge.
func TestTypedEnrichmentRetainsMergeLineageForUndo(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	src, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/old", Body: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	reviewerG02Seed(t, s, "/entities/person/a", "Alice", map[string]any{"entity_type": "person", "source_facts": []string{src.ID}})
	reviewerG02Seed(t, s, "/entities/person/c", "Alice Chen", map[string]any{"entity_type": "person", "mention_count": 100, "aliases": []string{"Alice"}})
	if err := s.AddLinkWeighted(ctx, "ns", "/old", "/entities/person/a", "mentions", 1); err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(time.Second))
	if _, err := s.ApplyEntityMerge(ctx, "ns", reviewerG02Proposal(t, s, "/entities/person/a", "/entities/person/c"), MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(time.Second))
	fresh, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fresh", Body: "Alice Chen joined a new team"})
	if err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(time.Second))
	if _, err := s.applyTypedEntities(ctx, "ns", []typedEntity{{name: "Alice Chen", typ: "person", sources: []string{"/fresh"}, sourceIDs: []string{fresh.ID}}}); err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(time.Second))
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); err != nil {
		t.Fatal(err)
	}
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("canonical %v %v", fs, err)
	}
	hasFresh := false
	for _, id := range attrStringList(fs[0], "source_facts") {
		if id == src.ID {
			t.Error("undo left alias-only provenance after enrichment discarded merge lineage")
		}
		if id == fresh.ID {
			hasFresh = true
		}
	}
	if !hasFresh {
		t.Error("undo erased legitimate later enrichment provenance")
	}
	links, err := s.Neighbors(ctx, "ns", "/old", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.ToKey == "/entities/person/c" {
			t.Error("undo left merge-created mention edge after enrichment")
		}
	}
}

// TestMergeProtectionDerivedFromStoredTypesNotCallerFlag: clearing the
// caller-supplied Protected flag on a proposal must not disarm the
// protected-type gate - protection derives from stored entity types.
func TestMergeProtectionDerivedFromStoredTypesNotCallerFlag(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	reviewerG02Seed(t, s, "/entities/host/a", "Service A", map[string]any{"entity_type": "host"})
	reviewerG02Seed(t, s, "/entities/host/b", "Service B", map[string]any{"entity_type": "host", "mention_count": 100, "aliases": []string{"Service A"}})
	p := reviewerG02Proposal(t, s, "/entities/host/a", "/entities/host/b")
	if !p.Protected {
		t.Fatal("expected protected proposal")
	}
	p.Protected = false
	r, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{})
	if err == nil && r.Applied {
		t.Fatal("protected host merge accepted by clearing caller-supplied Protected flag")
	}
}

// TestFailedMergeApplyLeavesNoAddedMentions: a canonical revision write
// failing mid-apply rolls the whole apply back - no canonical mention
// edge may survive an uncommitted merge.
func TestFailedMergeApplyLeavesNoAddedMentions(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	reviewerG02Seed(t, s, "/f", "source", nil)
	reviewerG02Seed(t, s, "/entities/person/a", "Alice", map[string]any{"entity_type": "person"})
	reviewerG02Seed(t, s, "/entities/person/b", "Alice Chen", map[string]any{"entity_type": "person", "mention_count": 100, "aliases": []string{"Alice"}})
	p := reviewerG02Proposal(t, s, "/entities/person/a", "/entities/person/b")
	if err := s.AddLinkWeighted(ctx, "ns", "/f", "/entities/person/a", "mentions", 1); err != nil {
		t.Fatal(err)
	}
	trigger := `CREATE TRIGGER reviewer_g02_fail BEFORE INSERT ON memories WHEN NEW.key = '/entities/person/b' BEGIN SELECT RAISE(ABORT,'canonical write failure'); END`
	if s.db.Driver == "postgres" {
		if _, err := s.db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION reviewer_g02_fail_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'canonical write failure'; END $$`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := s.db.ExecContext(context.Background(), `DROP FUNCTION reviewer_g02_fail_insert() CASCADE`); err != nil {
				t.Error(err)
			}
		})
		trigger = `CREATE TRIGGER reviewer_g02_fail BEFORE INSERT ON memories FOR EACH ROW WHEN (NEW.key = '/entities/person/b') EXECUTE FUNCTION reviewer_g02_fail_insert()`
	}
	if _, err := s.db.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	_, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{})
	if err == nil {
		t.Fatal("failure injection did not fire")
	}
	links, err := s.Neighbors(ctx, "ns", "/f", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.ToKey == "/entities/person/b" {
			t.Fatal("failed apply left a canonical mention edge without a committed merge")
		}
	}
}

// TestMergeUndoKeepsSharedProvenanceForActiveMerge: undoing the first of
// two merges that share one source revision must keep the shared
// provenance the second ACTIVE merge still requires.
func TestMergeUndoKeepsSharedProvenanceForActiveMerge(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	source, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Alice identity evidence"})
	if err != nil {
		t.Fatal(err)
	}
	reviewerG02Seed(t, s, "/entities/person/c", "Alice Chen", map[string]any{"entity_type": "person", "mention_count": 100, "aliases": []string{"/entities/person/a", "/entities/person/b"}})
	for _, key := range []string{"/entities/person/a", "/entities/person/b"} {
		reviewerG02Seed(t, s, key, key, map[string]any{"entity_type": "person", "source_facts": []string{source.ID}})
	}
	for _, key := range []string{"/entities/person/a", "/entities/person/b"} {
		clk.Set(s.now().Add(time.Second))
		if _, err := s.ApplyEntityMerge(ctx, "ns", reviewerG02Proposal(t, s, key, "/entities/person/c"), MergeApplyOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	clk.Set(s.now().Add(time.Second))
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); err != nil {
		t.Fatal(err)
	}
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("canonical %v %v", fs, err)
	}
	for _, id := range attrStringList(fs[0], "source_facts") {
		if id == source.ID {
			return
		}
	}
	t.Fatal("undoing first merge removed shared provenance still required by the second active merge")
}
