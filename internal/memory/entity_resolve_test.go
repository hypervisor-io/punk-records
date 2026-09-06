package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// writeEntityFact writes an entity fact directly (no extractor), the way
// the enricher would leave it: entity_type, aliases, source_facts (exact
// contributing revision IDs) and mention_count. Direct writes let merge
// fixtures stand up entities that extraction-time canonicalization would
// otherwise have collapsed into one key.
func writeEntityFact(t *testing.T, s *Store, ctx context.Context, ns, key, name, typ string, aliases, sourceIDs []string, mentions float64) {
	t.Helper()
	attrs := map[string]any{"mention_count": mentions}
	if typ != "" {
		attrs["entity_type"] = typ
	}
	if len(aliases) > 0 {
		attrs["aliases"] = aliases
	}
	if len(sourceIDs) > 0 {
		attrs["source_facts"] = sourceIDs
	}
	if _, err := s.writeNoOutbox(ctx, WriteInput{
		Namespace: ns, Key: key, Body: name, Attributes: attrs,
		Writer: "test", Importance: 0.3,
	}); err != nil {
		t.Fatal(err)
	}
}

// aliceFixture writes the shared Alice/Alice Chen duplicate-entity
// fixture: /notes/n3 evidences BOTH names (shared source revision), so
// the pair carries supporting identity evidence.
func aliceFixture(t *testing.T, s *Store, ctx context.Context) (n1, n2, n3 Fact) {
	t.Helper()
	write := func(key, body string) Fact {
		f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return *f
	}
	n1 = write("/notes/n1", "Alice Chen filed the incident report")
	n2 = write("/notes/n2", "Alice joined the oncall rota")
	n3 = write("/notes/n3", "Alice Chen, aka Alice, owns the pager")
	writeEntityFact(t, s, ctx, "ns", "/entities/person/alice-chen", "Alice Chen", "person", nil, []string{n1.ID, n3.ID}, 2)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/alice", "Alice", "person", nil, []string{n2.ID, n3.ID}, 2)
	for _, link := range [][2]string{
		{"/notes/n1", "/entities/person/alice-chen"},
		{"/notes/n3", "/entities/person/alice-chen"},
		{"/notes/n2", "/entities/person/alice"},
		{"/notes/n3", "/entities/person/alice"},
	} {
		if err := s.AddLink(ctx, "ns", link[0], link[1], "mentions"); err != nil {
			t.Fatal(err)
		}
	}
	return n1, n2, n3
}

func evidenceKinds(p EntityMergeProposal) map[string]bool {
	out := map[string]bool{}
	for _, e := range p.Evidence {
		out[e.Kind] = true
	}
	return out
}

// TestProposeEntityMergesAliasWithSharedSourceEvidence is red proof 1:
// Alice and Alice Chen MAY resolve - they share a source revision (one
// fact evidencing both names), which is supporting identity evidence. The
// proposal is dry-run: evidence-bearing, deterministic, and it writes
// nothing.
func TestProposeEntityMergesAliasWithSharedSourceEvidence(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	aliceFixture(t, s, ctx)

	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 {
		t.Fatalf("proposals = %v, want exactly 1 (the evidenced Alice pair)", props)
	}
	p := props[0]
	if p.CanonicalKey != "/entities/person/alice-chen" || p.AliasKey != "/entities/person/alice" {
		t.Fatalf("proposal = %s -> %s, want alias /entities/person/alice into canonical /entities/person/alice-chen", p.AliasKey, p.CanonicalKey)
	}
	if p.EntityType != "person" {
		t.Fatalf("EntityType = %q, want person", p.EntityType)
	}
	kinds := evidenceKinds(p)
	if !kinds["shared_source"] {
		t.Fatalf("evidence kinds = %v, want shared_source (the identity evidence)", p.Evidence)
	}
	if !kinds["name_similarity"] {
		t.Fatalf("evidence kinds = %v, want name_similarity recorded as context", p.Evidence)
	}
	if p.Protected {
		t.Fatal("person merges are not a protected type")
	}
	if p.ID == "" {
		t.Fatal("proposal ID must be set (idempotency identity)")
	}

	// deterministic: a second dry run yields the identical proposal ID.
	again, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != p.ID {
		t.Fatalf("re-proposal = %v, want identical ID %q", again, p.ID)
	}
}

// TestProposeEntityMergesDistinctShortNameServicesNotMerged is red proof
// 2: two different services that share the short name "API" as a declared
// alias must NOT be proposed - a shared short name is context, not
// identity evidence. Same-name entities of different types (person vs
// service Mercury) are type-constrained apart as well.
func TestProposeEntityMergesDistinctShortNameServicesNotMerged(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f6, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/n6", Body: "Auth API issues tokens"})
	if err != nil {
		t.Fatal(err)
	}
	f7, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/n7", Body: "Billing API generates invoices"})
	if err != nil {
		t.Fatal(err)
	}
	f8, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/n8", Body: "Mercury joined the oncall rota"})
	if err != nil {
		t.Fatal(err)
	}
	f9, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/n9", Body: "Mercury deploys the billing API"})
	if err != nil {
		t.Fatal(err)
	}
	writeEntityFact(t, s, ctx, "ns", "/entities/service/auth-api", "Auth API", "service", []string{"API"}, []string{f6.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/service/billing-api", "Billing API", "service", []string{"API"}, []string{f7.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/mercury", "Mercury", "person", nil, []string{f8.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/service/mercury", "Mercury", "service", nil, []string{f9.ID}, 1)

	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{IncludeProtected: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 0 {
		t.Fatalf("proposals = %v, want none: shared short name and cross-type same name are not identity evidence", props)
	}
}

// TestApplyEntityMergeRecordsAliasesLineageAndPreservesHistory pins the
// apply half: canonical gains the alias names, revision-precise source
// provenance and a lineage record; the alias entity stays live (old IDs
// and historical facts preserved) with merged_into lineage; old mentions
// edges stay and the alias's sources gain edges to the canonical; old
// references resolve through the chain.
func TestApplyEntityMergeRecordsAliasesLineageAndPreservesHistory(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	n1, n2, n3 := aliceFixture(t, s, ctx)

	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 {
		t.Fatalf("proposals = %v, want 1", props)
	}
	preMerge := clk.Now()

	res, err := s.ApplyEntityMerge(ctx, "ns", props[0], MergeApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.CanonicalKey != "/entities/person/alice-chen" || res.AliasKey != "/entities/person/alice" {
		t.Fatalf("apply result = %+v, want applied alice -> alice-chen", res)
	}

	canonical := entityAttr(t, s, ctx, "ns", "/entities/person/alice-chen")
	if got := attrStrings(canonical, "aliases"); len(got) != 1 || got[0] != "Alice" {
		t.Fatalf("canonical aliases = %v, want [Alice]", got)
	}
	wantSources := map[string]bool{n1.ID: true, n2.ID: true, n3.ID: true}
	for _, id := range attrStrings(canonical, "source_facts") {
		delete(wantSources, id)
	}
	if len(wantSources) != 0 {
		t.Fatalf("canonical source_facts missing revisions %v: provenance must survive the merge", wantSources)
	}
	if mc := canonical.Attributes["mention_count"].(float64); mc != 3 {
		t.Fatalf("mention_count = %v, want 3 (2 own + 1 newly-linked alias source)", mc)
	}

	// lineage record on the canonical carries the exact applied deltas.
	merges, ok := canonical.Attributes["merges"].([]any)
	if !ok || len(merges) != 1 {
		t.Fatalf("canonical merges = %v, want one lineage record", canonical.Attributes["merges"])
	}
	rec := merges[0].(map[string]any)
	if rec["merge_id"] != props[0].ID || rec["alias_key"] != "/entities/person/alice" {
		t.Fatalf("merge record = %v, want merge_id %q alias_key /entities/person/alice", rec, props[0].ID)
	}

	// the alias entity is NOT deleted: still live, carrying lineage.
	alias := entityAttr(t, s, ctx, "ns", "/entities/person/alice")
	if alias.Body != "Alice" {
		t.Fatalf("alias body = %q, want Alice (no destructive merge)", alias.Body)
	}
	if alias.Attributes["merged_into"] != "/entities/person/alice-chen" {
		t.Fatalf("alias merged_into = %v, want /entities/person/alice-chen", alias.Attributes["merged_into"])
	}
	if alias.Attributes["merge_id"] != props[0].ID {
		t.Fatalf("alias merge_id = %v, want %q", alias.Attributes["merge_id"], props[0].ID)
	}

	// historical revisions preserved: as-of before the merge the alias
	// had no merged_into marker.
	before, err := s.RecallAsOf(ctx, "ns", "/entities/person/alice", preMerge, 1)
	if err != nil || len(before) != 1 {
		t.Fatalf("recall as-of pre-merge: %v %v", before, err)
	}
	if _, merged := before[0].Attributes["merged_into"]; merged {
		t.Fatal("historical revision of the alias was rewritten: merged_into must be a NEW revision, history intact")
	}

	// old mentions edge preserved; alias sources newly mention canonical.
	oldLinks, err := s.Neighbors(ctx, "ns", "/notes/n2", "out")
	if err != nil {
		t.Fatal(err)
	}
	var sawOld, sawNew bool
	for _, l := range oldLinks {
		if l.LinkType == "mentions" && l.ToKey == "/entities/person/alice" {
			sawOld = true
		}
		if l.LinkType == "mentions" && l.ToKey == "/entities/person/alice-chen" {
			sawNew = true
		}
	}
	if !sawOld || !sawNew {
		t.Fatalf("/notes/n2 links = %v, want mentions of BOTH the preserved alias and the canonical", oldLinks)
	}

	// old references resolve.
	got, err := s.ResolveEntityKey(ctx, "ns", "/entities/person/alice")
	if err != nil || got != "/entities/person/alice-chen" {
		t.Fatalf("ResolveEntityKey(alias) = %q, %v; want /entities/person/alice-chen", got, err)
	}
}

// TestApplyEntityMergeIdempotentReplay is red proof 3: replaying the same
// proposal is a no-op - no new revisions, no duplicated aliases or
// lineage records.
func TestApplyEntityMergeIdempotentReplay(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	aliceFixture(t, s, ctx)
	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEntityMerge(ctx, "ns", props[0], MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	before := entityAttr(t, s, ctx, "ns", "/entities/person/alice-chen")

	replay, err := s.ApplyEntityMerge(ctx, "ns", props[0], MergeApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Applied {
		t.Fatalf("replay result = %+v, want Applied=false (idempotent no-op)", replay)
	}
	after := entityAttr(t, s, ctx, "ns", "/entities/person/alice-chen")
	if after.ID != before.ID {
		t.Fatalf("canonical revision changed on replay: %s -> %s", before.ID, after.ID)
	}
	if got := attrStrings(after, "aliases"); len(got) != 1 || got[0] != "Alice" {
		t.Fatalf("aliases = %v after replay, want exactly [Alice]", got)
	}
	if merges := after.Attributes["merges"].([]any); len(merges) != 1 {
		t.Fatalf("merges = %v after replay, want exactly one lineage record", merges)
	}
	if mc := after.Attributes["mention_count"].(float64); mc != 3 {
		t.Fatalf("mention_count = %v after replay, want 3 unchanged", mc)
	}
}

// TestRejectedProposalLeavesDataUntouched is red proof 4: a proposal that
// is never applied (rejected) changes nothing, and an apply refused by
// the protected-type gate also writes nothing.
func TestRejectedProposalLeavesDataUntouched(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	aliceFixture(t, s, ctx)

	snapshot := func() (string, string) {
		ents, err := s.ListEntities(ctx, "ns")
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, e := range ents {
			fmt.Fprintf(&b, "%s=%s:%v;", e.Key, e.ID, e.Attributes)
		}
		links, err := s.linksForKeys(ctx, "ns", []string{"/notes/n1", "/notes/n2", "/notes/n3"})
		if err != nil {
			t.Fatal(err)
		}
		var lb strings.Builder
		for _, l := range links {
			fmt.Fprintf(&lb, "%s-%s->%s;", l.FromKey, l.LinkType, l.ToKey)
		}
		return b.String(), lb.String()
	}
	factsBefore, linksBefore := snapshot()

	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 {
		t.Fatalf("proposals = %v, want 1", props)
	}
	// rejection path 1: never applied (dry-run only).
	// rejection path 2: apply refused at the protected-type gate.
	blocked := props[0]
	blocked.Protected = true
	if _, err := s.ApplyEntityMerge(ctx, "ns", blocked, MergeApplyOptions{}); !errors.Is(err, ErrProtectedEntityMerge) {
		t.Fatalf("protected apply = %v, want ErrProtectedEntityMerge", err)
	}

	factsAfter, linksAfter := snapshot()
	if factsBefore != factsAfter || linksBefore != linksAfter {
		t.Fatalf("rejected/refused proposals mutated data:\nfacts %q -> %q\nlinks %q -> %q", factsBefore, factsAfter, linksBefore, linksAfter)
	}
}

// TestApplyEntityMergeProtectedTypeRequiresOptIn: protected types
// (incident, host) are excluded from proposals by default; even an
// extractor-declared alias only produces a Protected-flagged proposal
// under IncludeProtected, and applying it needs explicit AllowProtected.
// Similarity alone never proposes a protected merge at all.
func TestApplyEntityMergeProtectedTypeRequiresOptIn(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/i1", Body: "inc-2026-042 (INC-42) pool exhaustion"})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/i2", Body: "INC-42 followup"})
	if err != nil {
		t.Fatal(err)
	}
	f3, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/i3", Body: "inc-2026-001 disk full"})
	if err != nil {
		t.Fatal(err)
	}
	f4, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/i4", Body: "inc-2026-002 disk full"})
	if err != nil {
		t.Fatal(err)
	}
	writeEntityFact(t, s, ctx, "ns", "/entities/incident/inc-2026-042", "inc-2026-042", "incident", []string{"INC-42"}, []string{f1.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/incident/inc-42", "INC-42", "incident", nil, []string{f2.ID}, 1)
	// near-identical names, no declared alias, no shared source: never a proposal.
	writeEntityFact(t, s, ctx, "ns", "/entities/incident/inc-2026-001", "inc-2026-001", "incident", nil, []string{f3.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/incident/inc-2026-002", "inc-2026-002", "incident", nil, []string{f4.ID}, 1)

	def, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 0 {
		t.Fatalf("default proposals = %v, want none (protected types excluded)", def)
	}
	inc, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{IncludeProtected: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 1 {
		t.Fatalf("IncludeProtected proposals = %v, want exactly the declared-alias pair", inc)
	}
	if !inc[0].Protected || inc[0].EntityType != "incident" {
		t.Fatalf("proposal = %+v, want Protected incident", inc[0])
	}
	if !evidenceKinds(inc[0])["declared_alias"] {
		t.Fatalf("evidence = %v, want declared_alias (the only evidence strong enough for a protected type)", inc[0].Evidence)
	}
	if _, err := s.ApplyEntityMerge(ctx, "ns", inc[0], MergeApplyOptions{}); !errors.Is(err, ErrProtectedEntityMerge) {
		t.Fatalf("apply without opt-in = %v, want ErrProtectedEntityMerge", err)
	}
	res, err := s.ApplyEntityMerge(ctx, "ns", inc[0], MergeApplyOptions{AllowProtected: true})
	if err != nil || !res.Applied {
		t.Fatalf("apply with AllowProtected = %+v, %v; want applied", res, err)
	}
	got, err := s.ResolveEntityKey(ctx, "ns", inc[0].AliasKey)
	if err != nil || got != inc[0].CanonicalKey {
		t.Fatalf("ResolveEntityKey = %q, %v; want %q", got, err, inc[0].CanonicalKey)
	}
}

// TestRevertEntityMergeRestoresPriorState pins undo: superseding revisions
// remove exactly what the merge added (aliases, source provenance, mention
// edges, lineage records) on both entities, while history keeps the merge.
func TestRevertEntityMergeRestoresPriorState(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	n1, _, n3 := aliceFixture(t, s, ctx)
	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEntityMerge(ctx, "ns", props[0], MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	duringMerge := clk.Now()

	res, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/alice")
	if err != nil || !res.Reverted {
		t.Fatalf("revert = %+v, %v; want reverted", res, err)
	}

	alias := entityAttr(t, s, ctx, "ns", "/entities/person/alice")
	if _, ok := alias.Attributes["merged_into"]; ok {
		t.Fatalf("alias attrs after revert = %v, want merged_into removed", alias.Attributes)
	}
	if _, ok := alias.Attributes["merge_id"]; ok {
		t.Fatalf("alias attrs after revert = %v, want merge_id removed", alias.Attributes)
	}
	canonical := entityAttr(t, s, ctx, "ns", "/entities/person/alice-chen")
	if _, ok := canonical.Attributes["aliases"]; ok {
		t.Fatalf("canonical aliases after revert = %v, want the added alias removed", canonical.Attributes["aliases"])
	}
	if _, ok := canonical.Attributes["merges"]; ok {
		t.Fatalf("canonical merges after revert = %v, want the lineage record removed", canonical.Attributes["merges"])
	}
	wantSources := map[string]bool{n1.ID: true, n3.ID: true}
	for _, id := range attrStrings(canonical, "source_facts") {
		delete(wantSources, id)
	}
	if len(wantSources) != 0 {
		t.Fatalf("canonical source_facts after revert missing %v", wantSources)
	}
	if mc := canonical.Attributes["mention_count"].(float64); mc != 2 {
		t.Fatalf("mention_count = %v after revert, want 2 (merge-added mention removed)", mc)
	}

	// the merge-added mentions edge is closed, not deleted: it was live
	// during the merge window and stays visible to as-of reads.
	live, err := s.Neighbors(ctx, "ns", "/notes/n2", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range live {
		if l.LinkType == "mentions" && l.ToKey == "/entities/person/alice-chen" {
			t.Fatalf("merge-added edge still live after revert: %v", live)
		}
	}
	during, err := s.NeighborsAsOf(ctx, "ns", "/notes/n2", "out", duringMerge)
	if err != nil {
		t.Fatal(err)
	}
	sawMergedEdge := false
	for _, l := range during {
		if l.LinkType == "mentions" && l.ToKey == "/entities/person/alice-chen" {
			sawMergedEdge = true
		}
	}
	if !sawMergedEdge {
		t.Fatalf("as-of during merge = %v, want the (now closed) merge-added edge visible: history keeps the merge", during)
	}

	// double revert is an explicit error, and the pair is eligible for a
	// fresh proposal again.
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/alice"); !errors.Is(err, ErrEntityNotMerged) {
		t.Fatalf("second revert = %v, want ErrEntityNotMerged", err)
	}
	reprops, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reprops) != 1 || reprops[0].ID != props[0].ID {
		t.Fatalf("re-proposal after revert = %v, want the same deterministic proposal %q", reprops, props[0].ID)
	}
}

// TestResolveEntityKeyChainAndCycle: resolution follows multi-hop
// merged_into chains, unknown keys return unchanged, and a hand-crafted
// cycle is an error, never an infinite loop.
func TestResolveEntityKeyChainAndCycle(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/c1", Body: "Alice on rota"})
	if err != nil {
		t.Fatal(err)
	}
	writeEntityFact(t, s, ctx, "ns", "/entities/person/alice", "Alice", "person", nil, []string{f1.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/alice-chen", "Alice Chen", "person", nil, []string{f1.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/alice-chen-li", "Alice Chen-Li", "person", nil, []string{f1.ID}, 1)

	if _, err := s.ApplyEntityMerge(ctx, "ns", EntityMergeProposal{
		ID: "em-test-1", Namespace: "ns", CanonicalKey: "/entities/person/alice-chen", AliasKey: "/entities/person/alice", EntityType: "person",
	}, MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEntityMerge(ctx, "ns", EntityMergeProposal{
		ID: "em-test-2", Namespace: "ns", CanonicalKey: "/entities/person/alice-chen-li", AliasKey: "/entities/person/alice-chen", EntityType: "person",
	}, MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveEntityKey(ctx, "ns", "/entities/person/alice")
	if err != nil || got != "/entities/person/alice-chen-li" {
		t.Fatalf("chain resolve = %q, %v; want /entities/person/alice-chen-li", got, err)
	}
	self, err := s.ResolveEntityKey(ctx, "ns", "/notes/n1")
	if err != nil || self != "/notes/n1" {
		t.Fatalf("unknown key = %q, %v; want returned unchanged", self, err)
	}

	writeEntityFact(t, s, ctx, "ns", "/entities/person/cyc-a", "Cyc A", "person", nil, nil, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/cyc-b", "Cyc B", "person", nil, nil, 1)
	// hand-craft a cycle via direct revision writes (Apply refuses these).
	if _, err := s.writeNoOutbox(ctx, WriteInput{Namespace: "ns", Key: "/entities/person/cyc-a", Body: "Cyc A",
		Attributes: map[string]any{"merged_into": "/entities/person/cyc-b"}, Writer: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeNoOutbox(ctx, WriteInput{Namespace: "ns", Key: "/entities/person/cyc-b", Body: "Cyc B",
		Attributes: map[string]any{"merged_into": "/entities/person/cyc-a"}, Writer: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEntityKey(ctx, "ns", "/entities/person/cyc-a"); err == nil {
		t.Fatal("cycle must surface an error, not loop forever")
	}
}

// TestProposeEntityMergesSkipsMergedAndScopesNamespaces: once merged, the
// alias entity leaves the candidate set (no re-proposal, no merging INTO
// an absorbed entity), and discovery never crosses namespace boundaries.
func TestProposeEntityMergesSkipsMergedAndScopesNamespaces(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	aliceFixture(t, s, ctx)
	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEntityMerge(ctx, "ns", props[0], MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	again, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("proposals after merge = %v, want none (merged alias already absorbed)", again)
	}
	// the absorbed alias must not be a merge TARGET either: applying a
	// stale proposal that names it canonical is a conflict.
	stale := EntityMergeProposal{
		ID: "em-stale", Namespace: "ns",
		CanonicalKey: "/entities/person/alice", AliasKey: "/entities/person/alice-chen", EntityType: "person",
	}
	if _, err := s.ApplyEntityMerge(ctx, "ns", stale, MergeApplyOptions{}); !errors.Is(err, ErrEntityMergeConflict) {
		t.Fatalf("apply into absorbed entity = %v, want ErrEntityMergeConflict", err)
	}
	other, err := s.ProposeEntityMerges(ctx, "other-ns", MergeProposalOptions{})
	if err != nil || len(other) != 0 {
		t.Fatalf("unknown namespace = %v, %v; want empty, nil", other, err)
	}
}

// TestEntityResolutionBeyondFirstPage is the bounded-scan blind-spot
// proof: with more entities than one Recall page (1000), extraction-time
// resolution and merge discovery must still see entities whose keys sort
// past the first page. Pre-pagination this returned a fresh slug and the
// duplicate pair was invisible.
func TestEntityResolutionBeyondFirstPage(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	shared, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/shared", Body: "Zed Zephyr, aka Zed Zephyr II"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1003; i++ {
		writeEntityFact(t, s, ctx, "ns", fmt.Sprintf("/entities/person/e%04d", i),
			fmt.Sprintf("Filler %04d", i), "person", nil, nil, 1)
	}
	// the pair sorts after every filler key: past the old first page.
	writeEntityFact(t, s, ctx, "ns", "/entities/person/zz-target", "Zed Zephyr", "person", nil, []string{shared.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/zz-target-ii", "Zed Zephyr II", "person", nil, []string{shared.ID}, 1)

	key, err := s.resolveTypedEntityKey(ctx, "ns", typedEntity{name: "Zed Zephyr", typ: "person"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "/entities/person/zz-target" {
		t.Fatalf("resolveTypedEntityKey = %q, want /entities/person/zz-target: entities past the first page must resolve", key)
	}
	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range props {
		if (p.AliasKey == "/entities/person/zz-target" && p.CanonicalKey == "/entities/person/zz-target-ii") ||
			(p.AliasKey == "/entities/person/zz-target-ii" && p.CanonicalKey == "/entities/person/zz-target") {
			found = true
		}
	}
	if !found {
		t.Fatalf("proposals = %v, want the shared-source zz-target pair (paginated discovery past page 1)", props)
	}
}

// TestResolveTypedEntityKeyPrefersExactMatch: an exact display-name match
// outranks an exact alias match, which outranks any fuzzy candidate -
// exact aliases resolve first, contextual candidates second.
func TestResolveTypedEntityKeyPrefersExactMatch(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	writeEntityFact(t, s, ctx, "ns", "/entities/person/al", "Al", "person", nil, nil, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/aa-al", "AA Al", "person", []string{"Al"}, nil, 1)

	key, err := s.resolveTypedEntityKey(ctx, "ns", typedEntity{name: "Al", typ: "person"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "/entities/person/al" {
		t.Fatalf("resolve = %q, want /entities/person/al: exact body match beats exact alias match", key)
	}
	// exact alias still wins over no match at all.
	key, err = s.resolveTypedEntityKey(ctx, "ns", typedEntity{name: "AA Al", typ: "person"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "/entities/person/aa-al" {
		t.Fatalf("resolve = %q, want /entities/person/aa-al", key)
	}
}

// TestProposeEntityMergesLegacyUntypedPair: legacy untyped /entities/<slug>
// duplicates resolve among themselves (same-bucket rule), and an untyped
// entity is never proposed against a typed one - an untyped name carries
// no type constraint, so cross-shape pairing is unsafe.
func TestProposeEntityMergesLegacyUntypedPair(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/l1", Body: "Alice Chen wrote the runbook"})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/l2", Body: "Alice reviewed it"})
	if err != nil {
		t.Fatal(err)
	}
	writeEntityFact(t, s, ctx, "ns", "/entities/alice-chen", "Alice Chen", "", []string{"Alice"}, []string{f1.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/alice", "Alice", "", nil, []string{f2.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/alice", "Alice", "person", nil, []string{f2.ID}, 1)

	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 {
		t.Fatalf("proposals = %v, want exactly the legacy untyped pair", props)
	}
	if props[0].EntityType != "" {
		t.Fatalf("EntityType = %q, want empty (legacy untyped bucket)", props[0].EntityType)
	}
	if props[0].CanonicalKey != "/entities/alice-chen" || props[0].AliasKey != "/entities/alice" {
		t.Fatalf("proposal = %s -> %s, want legacy alice into alice-chen", props[0].AliasKey, props[0].CanonicalKey)
	}
}

// findMergeProposal returns the proposal merging alias into canonical, or
// fails the test listing what discovery actually returned.
func findMergeProposal(t *testing.T, s *Store, ctx context.Context, alias, canonical string) EntityMergeProposal {
	t.Helper()
	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{IncludeProtected: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range props {
		if p.AliasKey == alias && p.CanonicalKey == canonical {
			return p
		}
	}
	t.Fatalf("missing proposal %s -> %s in %+v", alias, canonical, props)
	return EntityMergeProposal{}
}

// TestProposeEntityMergesSharedSourceAloneIsNotIdentity: two separately
// named services evidenced by ONE shared fact revision share that source
// revision - co-occurrence is contextual evidence, never identity
// evidence, so the overlap alone must not produce a proposal. The same
// shared revision does corroborate a pair that carries independent
// identity evidence (a declared alias).
func TestProposeEntityMergesSharedSourceAloneIsNotIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	call, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/call",
		Body: "Auth API calls Billing API; these are two separate services owned by different teams."})
	if err != nil {
		t.Fatal(err)
	}
	writeEntityFact(t, s, ctx, "ns", "/entities/service/auth-api", "Auth API", "service", nil, []string{call.ID}, 1)
	writeEntityFact(t, s, ctx, "ns", "/entities/service/billing-api", "Billing API", "service", nil, []string{call.ID}, 1)

	for _, opts := range []MergeProposalOptions{{}, {IncludeProtected: true}} {
		props, err := s.ProposeEntityMerges(ctx, "ns", opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(props) != 0 {
			t.Fatalf("proposals = %v, want none: a shared source revision between distinctly named entities is co-occurrence, not identity", props)
		}
	}

	// positive control: with a declared alias present, the SAME shared
	// revision corroborates (shared_source rides along on the proposal)
	// instead of sufficing alone.
	writeEntityFact(t, s, ctx, "ns", "/entities/service/billing-api", "Billing API", "service", []string{"Auth API"}, []string{call.ID}, 1)
	props, err := s.ProposeEntityMerges(ctx, "ns", MergeProposalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0].AliasKey != "/entities/service/auth-api" || props[0].CanonicalKey != "/entities/service/billing-api" {
		t.Fatalf("proposals = %v, want the declared-alias pair", props)
	}
	kinds := evidenceKinds(props[0])
	if !kinds[MergeEvidenceDeclaredAlias] || !kinds[MergeEvidenceSharedSource] {
		t.Fatalf("evidence = %v, want declared_alias (identity) corroborated by shared_source (context)", props[0].Evidence)
	}
}

// TestApplyEntityMergeProtectionDerivedFromStoredTypes: apply-side
// protection is derived from the STORED entity types, never from
// caller-controlled proposal fields - a protected-type (host) proposal
// with its Protected flag cleared must still be refused without
// AllowProtected, and the refusal must write nothing.
func TestApplyEntityMergeProtectionDerivedFromStoredTypes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	writeEntityFact(t, s, ctx, "ns", "/entities/host/a", "Service A", "host", nil, nil, 0)
	writeEntityFact(t, s, ctx, "ns", "/entities/host/b", "Service B", "host", []string{"Service A"}, nil, 100)

	p := findMergeProposal(t, s, ctx, "/entities/host/a", "/entities/host/b")
	if !p.Protected {
		t.Fatalf("proposal = %+v, want Protected (host is a protected type)", p)
	}
	p.Protected = false // caller clears the flag: stored types must still gate

	if _, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{}); !errors.Is(err, ErrProtectedEntityMerge) {
		t.Fatalf("apply with cleared Protected flag = %v, want ErrProtectedEntityMerge derived from stored types", err)
	}
	alias := entityAttr(t, s, ctx, "ns", "/entities/host/a")
	if _, merged := alias.Attributes["merged_into"]; merged {
		t.Fatalf("refused merge wrote lineage: %v", alias.Attributes)
	}
	canonical := entityAttr(t, s, ctx, "ns", "/entities/host/b")
	if _, ok := canonical.Attributes["merges"]; ok {
		t.Fatalf("refused merge wrote a lineage record: %v", canonical.Attributes)
	}

	// explicit opt-in still applies the same stored-protected pair.
	res, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{AllowProtected: true})
	if err != nil || !res.Applied {
		t.Fatalf("apply with AllowProtected = %+v, %v; want applied", res, err)
	}
}

// TestApplyEntityMergeFailureRollsBackMentionEdges: a canonical revision
// INSERT failing mid-apply must roll the WHOLE apply back - the mention
// edges the apply was adding cannot survive an uncommitted merge. Undo
// follows the same atomic rule: a failed revert leaves the merge fully
// in place. Failure injection uses a sqlite RAISE trigger.
func TestApplyEntityMergeFailureRollsBackMentionEdges(t *testing.T) {
	s, _, _ := newTest(t)
	if s.db.Driver != "sqlite" {
		t.Skip("failure injection uses a sqlite RAISE trigger")
	}
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "source"}); err != nil {
		t.Fatal(err)
	}
	writeEntityFact(t, s, ctx, "ns", "/entities/person/a", "Alice", "person", nil, nil, 0)
	writeEntityFact(t, s, ctx, "ns", "/entities/person/b", "Alice Chen", "person", []string{"Alice"}, nil, 100)
	p := findMergeProposal(t, s, ctx, "/entities/person/a", "/entities/person/b")
	if err := s.AddLinkWeighted(ctx, "ns", "/f", "/entities/person/a", "mentions", 1); err != nil {
		t.Fatal(err)
	}

	arm := func(key string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER g02_fail BEFORE INSERT ON memories
			WHEN NEW.key = '`+key+`' BEGIN SELECT RAISE(ABORT,'merge write failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	disarm := func() {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS g02_fail`); err != nil {
			t.Fatal(err)
		}
	}
	hasCanonicalEdge := func() bool {
		t.Helper()
		links, err := s.Neighbors(ctx, "ns", "/f", "out")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range links {
			if l.LinkType == "mentions" && l.ToKey == "/entities/person/b" {
				return true
			}
		}
		return false
	}

	// failed apply (canonical revision write aborts): rolled back
	// completely, no mention edge survives.
	arm("/entities/person/b")
	if _, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{}); err == nil {
		t.Fatal("failure injection did not fire")
	}
	disarm()
	if hasCanonicalEdge() {
		t.Fatal("failed apply left a canonical mention edge without a committed merge")
	}
	canonical := entityAttr(t, s, ctx, "ns", "/entities/person/b")
	if _, ok := canonical.Attributes["merges"]; ok {
		t.Fatalf("failed apply left a lineage record: %v", canonical.Attributes)
	}
	alias := entityAttr(t, s, ctx, "ns", "/entities/person/a")
	if _, ok := alias.Attributes["merged_into"]; ok {
		t.Fatalf("failed apply left alias lineage: %v", alias.Attributes)
	}

	// clean apply commits edges, revisions and lineage together.
	if _, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if !hasCanonicalEdge() {
		t.Fatal("applied merge missing the canonical mention edge")
	}

	// failed revert (alias revision write - the undo's LAST mutation -
	// aborts): rolled back completely, the merge stays fully in place
	// (canonical lineage record kept, mention edge still live).
	arm("/entities/person/a")
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); err == nil {
		t.Fatal("failure injection did not fire on revert")
	}
	disarm()
	alias = entityAttr(t, s, ctx, "ns", "/entities/person/a")
	if alias.Attributes["merged_into"] != "/entities/person/b" {
		t.Fatalf("failed revert removed alias lineage: %v", alias.Attributes)
	}
	canonical = entityAttr(t, s, ctx, "ns", "/entities/person/b")
	if merges, _ := canonical.Attributes["merges"].([]any); len(merges) != 1 {
		t.Fatalf("failed revert dropped the lineage record: %v", canonical.Attributes["merges"])
	}
	if !hasCanonicalEdge() {
		t.Fatal("failed revert closed the mention edge without a committed undo")
	}

	// clean revert closes exactly the merge-added edge.
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); err != nil {
		t.Fatal(err)
	}
	if hasCanonicalEdge() {
		t.Fatal("reverted merge left the canonical mention edge live")
	}
}

// TestRevertEntityMergePreservesOtherMergeSharedProvenance: two
// independently valid aliases share one source revision (and one mention
// source); merging both into the same canonical and undoing the FIRST
// must preserve everything the second ACTIVE merge still requires -
// shared source provenance, shared aliases and the shared mention edge -
// while undoing the second then removes what no merge needs anymore.
func TestRevertEntityMergePreservesOtherMergeSharedProvenance(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	source, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Alice identity evidence"})
	if err != nil {
		t.Fatal(err)
	}
	writeEntityFact(t, s, ctx, "ns", "/entities/person/c", "Alice Chen", "person",
		[]string{"/entities/person/a", "/entities/person/b"}, nil, 100)
	for _, key := range []string{"/entities/person/a", "/entities/person/b"} {
		writeEntityFact(t, s, ctx, "ns", key, key, "person", nil, []string{source.ID}, 0)
		if err := s.AddLinkWeighted(ctx, "ns", "/f", key, "mentions", 1); err != nil {
			t.Fatal(err)
		}
	}
	first := findMergeProposal(t, s, ctx, "/entities/person/a", "/entities/person/c")
	second := findMergeProposal(t, s, ctx, "/entities/person/b", "/entities/person/c")
	for _, p := range []EntityMergeProposal{first, second} {
		if _, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	canonicalEdgeLive := func() bool {
		t.Helper()
		links, err := s.Neighbors(ctx, "ns", "/f", "out")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range links {
			if l.LinkType == "mentions" && l.ToKey == "/entities/person/c" {
				return true
			}
		}
		return false
	}
	if !canonicalEdgeLive() {
		t.Fatal("merges left no canonical mention edge")
	}

	// undo the FIRST merge: the second active merge still requires the
	// shared source revision, the shared mention edge, and the canonical
	// keeps its own pre-merge aliases plus the second merge's record.
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); err != nil {
		t.Fatal(err)
	}
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("canonical after first revert: %v %v", fs, err)
	}
	shared := false
	for _, id := range attrStringList(fs[0], "source_facts") {
		if id == source.ID {
			shared = true
		}
	}
	if !shared {
		t.Fatalf("source_facts = %v, want %s preserved: undoing the first merge removed shared provenance still required by the second active merge",
			attrStringList(fs[0], "source_facts"), source.ID)
	}
	if got := attrStrings(fs[0], "aliases"); len(got) != 2 {
		t.Fatalf("canonical aliases = %v, want both pre-merge alias entries preserved", got)
	}
	if merges, _ := fs[0].Attributes["merges"].([]any); len(merges) != 1 {
		t.Fatalf("canonical merges = %v, want exactly the second merge's record kept", fs[0].Attributes["merges"])
	}
	if mc := fs[0].Attributes["mention_count"].(float64); mc != 101 {
		t.Fatalf("mention_count = %v, want 101 (second merge still holds the shared mention)", mc)
	}
	if !canonicalEdgeLive() {
		t.Fatal("first revert closed the mention edge the second active merge still requires")
	}
	aliasA := entityAttr(t, s, ctx, "ns", "/entities/person/a")
	if _, ok := aliasA.Attributes["merged_into"]; ok {
		t.Fatalf("first alias still carries lineage: %v", aliasA.Attributes)
	}
	aliasB := entityAttr(t, s, ctx, "ns", "/entities/person/b")
	if aliasB.Attributes["merged_into"] != "/entities/person/c" {
		t.Fatalf("second alias lost its lineage: %v", aliasB.Attributes)
	}

	// undo the SECOND merge too: nothing requires the shared revision or
	// edge anymore, so both go; the canonical's own pre-merge aliases and
	// mention_count remain, and the merge bookkeeping is fully retired.
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/b"); err != nil {
		t.Fatal(err)
	}
	canonical := entityAttr(t, s, ctx, "ns", "/entities/person/c")
	for _, id := range attrStringList(canonical, "source_facts") {
		if id == source.ID {
			t.Fatalf("source_facts = %v, want the shared revision removed once no active merge needs it", attrStringList(canonical, "source_facts"))
		}
	}
	if _, ok := canonical.Attributes["merges"]; ok {
		t.Fatalf("merges = %v, want the lineage retired after the last revert", canonical.Attributes["merges"])
	}
	if _, ok := canonical.Attributes["merge_base"]; ok {
		t.Fatalf("merge_base = %v, want the pre-merge snapshot retired after the last revert", canonical.Attributes["merge_base"])
	}
	if got := attrStrings(canonical, "aliases"); len(got) != 2 {
		t.Fatalf("canonical aliases = %v, want the canonical's own pre-merge aliases preserved", got)
	}
	if mc := canonical.Attributes["mention_count"].(float64); mc != 100 {
		t.Fatalf("mention_count = %v, want the pre-merge 100 restored", mc)
	}
	if canonicalEdgeLive() {
		t.Fatal("mention edge still live after every merge needing it was undone")
	}
}
