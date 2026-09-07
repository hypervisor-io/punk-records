package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Task S01 red proofs for the store-backed procedural-skill index:
// discovery returns metadata only, exact versioned bodies load on
// demand, inactive skills are excluded, and a store with no embedder
// still answers (lexical fallback).

const skillBodySentinel = "SUSTAINED-RATIO-085 pull the incident timeline first"

func skillMetaFixture() SkillMeta {
	return SkillMeta{
		Name:        "db-connection-triage",
		Version:     "0.1.0",
		Description: "Diagnose database connection saturation and pool exhaustion after deploys",
		Tools:       []string{"incidents__get_incident", "incidents__list_incidents"},
		Scope:       "incident",
		Source:      SkillSourceAuthored,
		Active:      true,
	}
}

func indexFixture(t *testing.T, s *Store, ns string, meta SkillMeta, body string) {
	t.Helper()
	if err := s.IndexSkill(context.Background(), ns, meta, body); err != nil {
		t.Fatalf("IndexSkill %s@%s: %v", meta.Name, meta.Version, err)
	}
}

func TestSkillIndexSearchReturnsMetadataOnly(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	meta := skillMetaFixture()
	indexFixture(t, s, "ns", meta, "# Triage\n\n"+skillBodySentinel)
	other := SkillMeta{
		Name: "db-lock-contention", Version: "0.2.0",
		Description: "Diagnose lock contention and blocked queries",
		Source:      SkillSourceAuthored, Active: true,
	}
	indexFixture(t, s, "ns", other, "# Locks\n\n1. List blocked sessions")

	hits, err := s.SearchSkills(ctx, "ns", "connection saturation", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1: %+v", len(hits), hits)
	}
	got := hits[0]
	if got.Name != meta.Name || got.Version != meta.Version || got.Description != meta.Description ||
		got.Scope != meta.Scope || got.Source != SkillSourceAuthored || !got.Active {
		t.Fatalf("metadata mismatch: %+v", got)
	}
	if len(got.Tools) != 2 || got.Tools[0] != "incidents__get_incident" {
		t.Fatalf("tools mismatch: %+v", got.Tools)
	}
	// The discovery payload must not carry procedure text, even though
	// the body is stored in the same namespace.
	raw, err := json.Marshal(hits)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SUSTAINED-RATIO-085") || strings.Contains(string(raw), "Pull the incident") {
		t.Fatalf("discovery payload leaked procedure text: %s", raw)
	}

	// Terms that appear only in the procedure body are not part of the
	// discovery index at all: metadata search cannot surface them.
	bodyOnly, err := s.SearchSkills(ctx, "ns", "SUSTAINED-RATIO-085", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodyOnly) != 0 {
		t.Fatalf("body-only term matched discovery index: %+v", bodyOnly)
	}
}

func TestLoadSkillExactVersionedBody(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	meta := skillMetaFixture()
	body := "# Triage\n\n" + skillBodySentinel
	indexFixture(t, s, "ns", meta, body)

	gotMeta, gotBody, err := s.LoadSkill(ctx, "ns", meta.Name, meta.Version)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body {
		t.Fatalf("body mismatch:\n got %q\nwant %q", gotBody, body)
	}
	if gotMeta.Name != meta.Name || gotMeta.Version != meta.Version {
		t.Fatalf("meta mismatch: %+v", gotMeta)
	}

	// Unversioned load resolves when exactly one version is live.
	if _, b, err := s.LoadSkill(ctx, "ns", meta.Name, ""); err != nil || b != body {
		t.Fatalf("unversioned load: err=%v", err)
	}

	// A second version makes unversioned loads ambiguous, and each exact
	// version keeps its own body.
	v2 := meta
	v2.Version = "0.2.0"
	body2 := "# Triage v2\n\nnew procedure"
	indexFixture(t, s, "ns", v2, body2)
	if _, _, err := s.LoadSkill(ctx, "ns", meta.Name, ""); err == nil {
		t.Fatal("ambiguous unversioned load: want error")
	}
	if _, b, err := s.LoadSkill(ctx, "ns", meta.Name, "0.1.0"); err != nil || b != body {
		t.Fatalf("v0.1.0 body = %q err=%v, want original", b, err)
	}
	if _, b, err := s.LoadSkill(ctx, "ns", meta.Name, "0.2.0"); err != nil || b != body2 {
		t.Fatalf("v0.2.0 body = %q err=%v, want v2", b, err)
	}

	// Re-indexing the same version with identical content is a no-op;
	// changed content under a published version is rejected (published
	// versions are immutable - bump the version to revise).
	indexFixture(t, s, "ns", meta, body)
	if _, b, err := s.LoadSkill(ctx, "ns", meta.Name, "0.1.0"); err != nil || b != body {
		t.Fatalf("idempotent re-index: body = %q err=%v, want original", b, err)
	}
	if err := s.IndexSkill(ctx, "ns", meta, "# Triage v1 revised"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("changed body under published version: err = %v, want ErrSkillVersionConflict", err)
	}
	if _, b, err := s.LoadSkill(ctx, "ns", meta.Name, "0.1.0"); err != nil || b != body {
		t.Fatalf("after conflict: body = %q err=%v, want original pinned body", b, err)
	}
}

func TestInactiveSkillExcluded(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	meta := skillMetaFixture()
	indexFixture(t, s, "ns", meta, "# Triage\n\nbody")

	if err := s.SetSkillActive(ctx, "ns", meta.Name, meta.Version, false); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchSkills(ctx, "ns", "connection saturation", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("inactive skill in search hits: %+v", hits)
	}
	listed, err := s.ListSkills(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("inactive skill in list: %+v", listed)
	}
	if _, _, err := s.LoadSkill(ctx, "ns", meta.Name, meta.Version); !errors.Is(err, ErrSkillInactive) {
		t.Fatalf("load inactive: err = %v, want ErrSkillInactive", err)
	}

	if err := s.SetSkillActive(ctx, "ns", meta.Name, meta.Version, true); err != nil {
		t.Fatal(err)
	}
	hits, err = s.SearchSkills(ctx, "ns", "connection saturation", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("reactivated skill not back in hits: %+v", hits)
	}
	if _, _, err := s.LoadSkill(ctx, "ns", meta.Name, meta.Version); err != nil {
		t.Fatalf("load reactivated: %v", err)
	}

	// Toggling a skill that was never indexed is a not-found error, not
	// a silent no-op.
	if err := s.SetSkillActive(ctx, "ns", "ghost", "0.1.0", false); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("toggle ghost: err = %v, want ErrSkillNotFound", err)
	}
}

// A discovered exact version pins its procedure: re-indexing changed
// body text under the same published version is a conflict, and the
// exact load keeps returning the originally published procedure after
// the source edit. (Round-2 review reproducer, folded in.)
func TestSkillVersionConflictKeepsPinnedBody(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	m := skillMetaFixture()
	indexFixture(t, s, "ns", m, "original procedure")

	hits, err := s.SearchSkills(ctx, "ns", "connection", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: hits=%+v err=%v", hits, err)
	}
	if err := s.IndexSkill(ctx, "ns", m, "changed procedure"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-index changed body under published version: err = %v, want ErrSkillVersionConflict", err)
	}
	_, body, err := s.LoadSkill(ctx, "ns", hits[0].Name, hits[0].Version)
	if err != nil {
		t.Fatal(err)
	}
	if body != "original procedure" {
		t.Fatalf("exact version %s@%s now loads %q, want the pinned original", hits[0].Name, hits[0].Version, body)
	}

	// Changed metadata under a published version is a conflict too; the
	// digest is part of the published identity.
	edited := m
	edited.Description = "Different description after the fact"
	if err := s.IndexSkill(ctx, "ns", edited, "original procedure"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-index changed metadata under published version: err = %v, want ErrSkillVersionConflict", err)
	}
}

// A failed body write must never leave active searchable metadata with
// no loadable procedure behind it: the publish is staged body-first, so
// the failure leaves nothing discoverable and any prior version stays
// coherent. (Round-2 review reproducer, folded in; the trigger simulates
// the store refusing the body insert.)
func TestFailedBodyWriteLeavesNoDiscoverableSkill(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	installSkillInsertFailure(t, s, "fail_skill_body", "/skill-bodies/%", "simulated body write failure")
	if err := s.IndexSkill(ctx, "ns", skillMetaFixture(), "procedure"); err == nil {
		t.Fatal("failure injection did not fire")
	}
	hits, err := s.SearchSkills(ctx, "ns", "connection", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("failed body write left an active discoverable skill that cannot load its procedure: %+v", hits)
	}
	if listed, err := s.ListSkillsAll(ctx, "ns"); err != nil || len(listed) != 0 {
		t.Fatalf("failed body write left a listed skill: %+v err=%v", listed, err)
	}
	if _, _, err := s.LoadSkill(ctx, "ns", "db-connection-triage", "0.1.0"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("load after failed publish: err = %v, want ErrSkillNotFound", err)
	}
}

// Re-indexing identical content under a published version is a no-op
// that preserves an operator's deactivation: SetSkillActive(false) is
// never silently reverted by a lifecycle re-sync.
func TestSkillReindexPreservesDeactivation(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	meta := skillMetaFixture()
	indexFixture(t, s, "ns", meta, "body")
	if err := s.SetSkillActive(ctx, "ns", meta.Name, meta.Version, false); err != nil {
		t.Fatal(err)
	}
	indexFixture(t, s, "ns", meta, "body") // identical content: must not reactivate
	if _, _, err := s.LoadSkill(ctx, "ns", meta.Name, meta.Version); !errors.Is(err, ErrSkillInactive) {
		t.Fatalf("load after identical re-index of deactivated skill: err = %v, want ErrSkillInactive", err)
	}
	all, err := s.ListSkillsAll(ctx, "ns")
	if err != nil || len(all) != 1 || all[0].Active {
		t.Fatalf("ListSkillsAll = %+v err=%v, want the one inactive version", all, err)
	}
	if listed, err := s.ListSkills(ctx, "ns"); err != nil || len(listed) != 0 {
		t.Fatalf("ListSkills = %+v err=%v, want empty (active only)", listed, err)
	}
}

// UnpublishSkill tombstones both the discovery document and the body:
// the version vanishes from search, list and load, and unpublishing it
// again (or a version that never existed) is a no-op so lifecycle sweeps
// can call it unconditionally.
func TestUnpublishSkillRemovesVersion(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	meta := skillMetaFixture()
	indexFixture(t, s, "ns", meta, "body")

	if err := s.UnpublishSkill(ctx, "ns", meta.Name, meta.Version); err != nil {
		t.Fatal(err)
	}
	if hits, err := s.SearchSkills(ctx, "ns", "connection", 10); err != nil || len(hits) != 0 {
		t.Fatalf("search after unpublish: %+v err=%v", hits, err)
	}
	if _, _, err := s.LoadSkill(ctx, "ns", meta.Name, meta.Version); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("load after unpublish: err = %v, want ErrSkillNotFound", err)
	}
	if all, err := s.ListSkillsAll(ctx, "ns"); err != nil || len(all) != 0 {
		t.Fatalf("ListSkillsAll after unpublish: %+v err=%v", all, err)
	}
	if err := s.UnpublishSkill(ctx, "ns", meta.Name, meta.Version); err != nil {
		t.Fatalf("double unpublish: %v", err)
	}
	if err := s.UnpublishSkill(ctx, "ns", "never-existed", "9.9.9"); err != nil {
		t.Fatalf("unpublish unknown: %v", err)
	}
	// The version can be re-published after a tombstone when the
	// identical content returns (deleted then restored source).
	indexFixture(t, s, "ns", meta, "body")
	if hits, err := s.SearchSkills(ctx, "ns", "connection", 10); err != nil || len(hits) != 1 {
		t.Fatalf("search after republish: %+v err=%v", hits, err)
	}
}

// Unpublishing a version hides it but must not reset its immutable
// identity: re-publishing changed content under the tombstoned
// name@version is a conflict that leaves the version unpublished, while
// re-publishing the identical content restores it. Visibility and
// content are separate concerns. (Round-3 review reproducer, folded in.)
func TestUnpublishedVersionKeepsPinnedIdentity(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	m := skillMetaFixture()
	indexFixture(t, s, "ns", m, "original procedure")

	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", m.Name, m.Version); err != nil {
		t.Fatal(err)
	}

	// Changed bytes under the tombstoned exact version: refused, and the
	// version stays unpublished (the refusal is not a sneaky restore).
	clk.Set(s.now().Add(time.Second))
	if err := s.IndexSkill(ctx, "ns", m, "changed procedure"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-publish changed body under unpublished version: err = %v, want ErrSkillVersionConflict", err)
	}
	if _, _, err := s.LoadSkill(ctx, "ns", m.Name, m.Version); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("load after refused re-publish: err = %v, want ErrSkillNotFound", err)
	}
	if hits, err := s.SearchSkills(ctx, "ns", "connection", 10); err != nil || len(hits) != 0 {
		t.Fatalf("refused re-publish made the version discoverable: %+v err=%v", hits, err)
	}

	// Changed metadata under the tombstoned version is a conflict too:
	// the digest is part of the published identity.
	edited := m
	edited.Description = "Different description after unpublish"
	if err := s.IndexSkill(ctx, "ns", edited, "original procedure"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-publish changed metadata under unpublished version: err = %v, want ErrSkillVersionConflict", err)
	}

	// Identical content re-publishes: the deleted-then-restored source
	// comes back discoverable with its originally published bytes.
	clk.Set(s.now().Add(time.Second))
	indexFixture(t, s, "ns", m, "original procedure")
	_, body, err := s.LoadSkill(ctx, "ns", m.Name, m.Version)
	if err != nil {
		t.Fatal(err)
	}
	if body != "original procedure" {
		t.Fatalf("restored version loads %q, want the originally published procedure", body)
	}
	if hits, err := s.SearchSkills(ctx, "ns", "connection", 10); err != nil || len(hits) != 1 {
		t.Fatalf("restored version not discoverable: %+v err=%v", hits, err)
	}
}

// A failed discovery tombstone must stop the unpublish before the body
// is touched: search keeps advertising only loadable procedures, the
// coherent prior state survives, and a retry after the failure clears
// completes the removal. (Round-3 review reproducer, folded in; the
// trigger simulates the store refusing the digest tombstone.)
func TestFailedUnpublishKeepsSkillCoherent(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	m := skillMetaFixture()
	indexFixture(t, s, "ns", m, "original procedure")

	clearFailure := installSkillInsertFailure(t, s, "fail_skill_digest", "/skills/%", "simulated digest failure")
	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", m.Name, m.Version); err == nil {
		t.Fatal("failure injection did not fire")
	}
	hits, err := s.SearchSkills(ctx, "ns", "connection", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("failed unpublish changed discovery: %+v", hits)
	}
	for _, h := range hits {
		if _, body, err := s.LoadSkill(ctx, "ns", h.Name, h.Version); err != nil || body != "original procedure" {
			t.Fatalf("failed unpublish left advertised skill unloadable: body=%q err=%v", body, err)
		}
	}

	// The failure cleared: the retry completes the removal.
	clearFailure()
	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", m.Name, m.Version); err != nil {
		t.Fatalf("retry after cleared failure: %v", err)
	}
	if hits, err := s.SearchSkills(ctx, "ns", "connection", 10); err != nil || len(hits) != 0 {
		t.Fatalf("search after retry: %+v err=%v", hits, err)
	}
	if _, _, err := s.LoadSkill(ctx, "ns", m.Name, m.Version); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("load after retry: err = %v, want ErrSkillNotFound", err)
	}
}

// A failure on the body tombstone (the second step) leaves an invisible
// orphan body, never an advertised digest without a loadable procedure:
// the discovery document is removed first, the retry cleans the orphan
// up, and the identical content still re-publishes afterwards.
func TestFailedBodyTombstoneLeavesInvisibleOrphan(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	m := skillMetaFixture()
	indexFixture(t, s, "ns", m, "original procedure")

	clearFailure := installSkillInsertFailure(t, s, "fail_skill_body_tombstone", "/skill-bodies/%", "simulated body failure")
	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", m.Name, m.Version); err == nil {
		t.Fatal("failure injection did not fire")
	}
	if hits, err := s.SearchSkills(ctx, "ns", "connection", 10); err != nil || len(hits) != 0 {
		t.Fatalf("partially failed unpublish still advertises: %+v err=%v", hits, err)
	}
	if _, _, err := s.LoadSkill(ctx, "ns", m.Name, m.Version); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("load after partial unpublish: err = %v, want ErrSkillNotFound", err)
	}

	clearFailure()
	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", m.Name, m.Version); err != nil {
		t.Fatalf("retry after cleared failure: %v", err)
	}
	clk.Set(s.now().Add(time.Second))
	indexFixture(t, s, "ns", m, "original procedure")
	if _, body, err := s.LoadSkill(ctx, "ns", m.Name, m.Version); err != nil || body != "original procedure" {
		t.Fatalf("re-publish after retried unpublish: body=%q err=%v", body, err)
	}
}

// Immutable version identity must not ride rows the retention sweeper
// purges: after unpublish plus a full sweep the discovery document and
// the procedure bytes are gone for good - only the content-addressed
// identity record (a digest, never the body) survives - so changed
// content under the swept name@version stays a conflict while identical
// content still restores the version. (Round-4 review reproducer,
// folded in.)
func TestSkillIdentitySurvivesRetentionSweep(t *testing.T) {
	s, db, clk := newTest(t)
	ctx := context.Background()
	m := skillMetaFixture()
	indexFixture(t, s, "ns", m, "original procedure")

	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", m.Name, m.Version); err != nil {
		t.Fatal(err)
	}

	// The sweep must actually purge the unpublished chains, or the test
	// would not prove identity survives retention.
	clk.Set(s.now().Add(48 * time.Hour))
	n, err := s.SweepRetention(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("sweep removed nothing: identity survival would be vacuous")
	}
	var purged int
	if err := db.QueryRowContext(ctx, db.Rebind(`SELECT count(*) FROM memories WHERE key IN ($1, $2)`),
		skillDocKey(m.Name, m.Version), skillBodyKey(m.Name, m.Version)).Scan(&purged); err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Fatalf("unpublished discovery/body rows survived the sweep: %d row(s)", purged)
	}
	// The deleted procedure bytes stay deleted: the store remembers the
	// digest, never the body.
	var bodies int
	if err := db.QueryRowContext(ctx, db.Rebind(`SELECT count(*) FROM memories WHERE body = $1`),
		"original procedure").Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if bodies != 0 {
		t.Fatalf("retention kept the deleted procedure body in %d row(s)", bodies)
	}

	// Changed bytes under the swept exact version: still refused, and
	// the refusal leaves the version unpublished.
	if err := s.IndexSkill(ctx, "ns", m, "changed procedure"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-publish changed body after the sweep purged the shipped rows: err = %v, want ErrSkillVersionConflict", err)
	}
	// Changed metadata under the swept version is a conflict too: the
	// digest is part of the published identity.
	edited := m
	edited.Description = "Different description after the sweep"
	if err := s.IndexSkill(ctx, "ns", edited, "original procedure"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-publish changed metadata after the sweep: err = %v, want ErrSkillVersionConflict", err)
	}
	if _, _, err := s.LoadSkill(ctx, "ns", m.Name, m.Version); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("load after refused re-publish: err = %v, want ErrSkillNotFound", err)
	}

	// Identical content still restores the version: the identity record
	// is a pin, not a blockade (deleted-then-restored source).
	clk.Set(s.now().Add(time.Second))
	indexFixture(t, s, "ns", m, "original procedure")
	if _, body, err := s.LoadSkill(ctx, "ns", m.Name, m.Version); err != nil || body != "original procedure" {
		t.Fatalf("restored version: body=%q err=%v, want the originally published procedure", body, err)
	}

	// The consolidation pass purges with the same rules; the identity
	// survives it too.
	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", m.Name, m.Version); err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(48 * time.Hour))
	if _, err := s.Consolidate(ctx, "ns", 24*time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.IndexSkill(ctx, "ns", m, "changed procedure"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-publish changed body after consolidation purged the shipped rows: err = %v, want ErrSkillVersionConflict", err)
	}

	// Namespace scoping: the identity pin is per-namespace - the same
	// name@version elsewhere is a separate version with its own
	// identity, free to carry different bytes.
	indexFixture(t, s, "other-ns", m, "different procedure")
	if _, body, err := s.LoadSkill(ctx, "other-ns", m.Name, m.Version); err != nil || body != "different procedure" {
		t.Fatalf("other-ns load: body=%q err=%v, want its own procedure", body, err)
	}
}

// A publish that fails on the identity write still advertised the
// version (the digest ships before the record is minted), so the
// missing record is healed from the live rows - by the next identical
// re-index or by the unpublish itself, before the removal - and the
// healed pin then outlives retention like any minted-at-publish one.
func TestSkillIdentityHealsFromLiveRows(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	a := skillMetaFixture()
	b := skillMetaFixture()
	b.Name = "pool-audit"
	b.Description = "Audit pooler settings after connection saturation incidents"

	clearFailure := installSkillInsertFailure(t, s, "fail_skill_identity", "/skill-identities/%", "simulated identity write failure")
	if err := s.IndexSkill(ctx, "ns", a, "procedure a"); err == nil {
		t.Fatal("failure injection did not fire")
	}
	if err := s.IndexSkill(ctx, "ns", b, "procedure b"); err == nil {
		t.Fatal("failure injection did not fire")
	}
	// Both digests shipped before the identity step: advertised and
	// loadable despite the failed publish.
	if _, body, err := s.LoadSkill(ctx, "ns", a.Name, a.Version); err != nil || body != "procedure a" {
		t.Fatalf("load after failed identity write: body=%q err=%v", body, err)
	}
	if _, body, err := s.LoadSkill(ctx, "ns", b.Name, b.Version); err != nil || body != "procedure b" {
		t.Fatalf("load after failed identity write: body=%q err=%v", body, err)
	}

	clearFailure()
	// a heals through the identical re-index.
	indexFixture(t, s, "ns", a, "procedure a")
	// b heals through its unpublish: the removal pins the identity from
	// the live rows before tombstoning them.
	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", b.Name, b.Version); err != nil {
		t.Fatal(err)
	}

	// Retention purges b's tombstoned chains; both healed pins stay.
	clk.Set(s.now().Add(48 * time.Hour))
	if n, err := s.SweepRetention(ctx, 24*time.Hour); err != nil || n == 0 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", a.Name, a.Version); err != nil {
		t.Fatal(err)
	}
	if err := s.IndexSkill(ctx, "ns", a, "changed a"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-publish changed body under re-index-healed version: err = %v, want ErrSkillVersionConflict", err)
	}
	if err := s.IndexSkill(ctx, "ns", b, "changed b"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-publish changed body under unpublish-healed version after the sweep: err = %v, want ErrSkillVersionConflict", err)
	}
	// Identical content still restores the swept version.
	indexFixture(t, s, "ns", b, "procedure b")
	if _, body, err := s.LoadSkill(ctx, "ns", b.Name, b.Version); err != nil || body != "procedure b" {
		t.Fatalf("restored version: body=%q err=%v, want the originally published procedure", body, err)
	}
}

func TestSkillCollectionMissingIsEmpty(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()

	// A namespace that was never written at all.
	hits, err := s.SearchSkills(ctx, "ghost-ns", "connection", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("ghost-ns hits: %+v", hits)
	}
	listed, err := s.ListSkills(ctx, "ghost-ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("ghost-ns list: %+v", listed)
	}
	if _, _, err := s.LoadSkill(ctx, "ghost-ns", "db-connection-triage", "0.1.0"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("ghost-ns load: err = %v, want ErrSkillNotFound", err)
	}

	// A namespace with ordinary facts but no skill collection.
	if _, err := s.Remember(ctx, "plain-ns", "/runbook/x", "restart the pooler", nil, "t"); err != nil {
		t.Fatal(err)
	}
	hits, err = s.SearchSkills(ctx, "plain-ns", "pooler", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("plain-ns leaked a non-skill fact as a skill: %+v", hits)
	}
	if listed, err := s.ListSkills(ctx, "plain-ns"); err != nil || len(listed) != 0 {
		t.Fatalf("plain-ns list: %+v err=%v", listed, err)
	}
}

// synonymEmbedder maps "cert"-related text onto one vector and
// everything else onto a second, so a semantic-only query (no lexical
// overlap with the metadata digest) is still retrievable through the
// vector arm while FTS stays blind to it.
type synonymEmbedder struct{ calls int }

func (e *synonymEmbedder) Dims() int { return 2 }

func (e *synonymEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	out := make([][]float32, len(texts))
	for i, txt := range texts {
		if strings.Contains(txt, "cert") {
			out[i] = []float32{1, 0}
		} else {
			out[i] = []float32{0, 1}
		}
	}
	return out, nil
}

func TestSkillSearchSemanticArmAndLexicalFallback(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	meta := SkillMeta{
		Name: "tls-rotation", Version: "1.0.0",
		Description: "Rotate expiring TLS certificates on the edge proxies",
		Source:      SkillSourceMined, Active: true,
	}
	indexFixture(t, s, "ns", meta, "# Rotate\n\n1. Renew via the CA")

	// No embedder: the lexical fallback still answers metadata queries,
	// and a query with no lexical overlap returns empty rather than an
	// error ("requires an embedder" must never escape discovery).
	lex, err := s.SearchSkills(ctx, "ns", "certificates proxies", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lex) != 1 || lex[0].Name != "tls-rotation" {
		t.Fatalf("lexical fallback hits: %+v", lex)
	}
	blind, err := s.SearchSkills(ctx, "ns", "cert renewal", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(blind) != 0 {
		t.Fatalf("no-embedder store must not semantic-match: %+v", blind)
	}

	// With an embedder the same semantic-only query matches through the
	// vector arm; the lexical-arm hit keeps working too. The skill was
	// indexed before the embedder existed, so backfill its vectors first
	// (the real path for pre-embedder writes).
	s.SetEmbedder(&synonymEmbedder{})
	if _, err := s.BackfillEmbeddings(ctx, "ns", 0); err != nil {
		t.Fatal(err)
	}
	sem, err := s.SearchSkills(ctx, "ns", "cert renewal", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sem) != 1 || sem[0].Name != "tls-rotation" {
		t.Fatalf("semantic hits: %+v", sem)
	}
	both, err := s.SearchSkills(ctx, "ns", "certificates proxies", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 1 || both[0].Name != "tls-rotation" {
		t.Fatalf("hybrid hits: %+v", both)
	}
	// Inactive stays excluded on the vector arm too.
	if err := s.SetSkillActive(ctx, "ns", "tls-rotation", "1.0.0", false); err != nil {
		t.Fatal(err)
	}
	sem, err = s.SearchSkills(ctx, "ns", "cert renewal", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sem) != 0 {
		t.Fatalf("inactive skill leaked through vector arm: %+v", sem)
	}
}

func TestSkillIndexValidation(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	base := skillMetaFixture()

	bad := []struct {
		name string
		mut  func(*SkillMeta)
	}{
		{"empty name", func(m *SkillMeta) { m.Name = "" }},
		{"name with slash", func(m *SkillMeta) { m.Name = "a/b" }},
		{"name with space", func(m *SkillMeta) { m.Name = "a b" }},
		{"oversize name", func(m *SkillMeta) { m.Name = strings.Repeat("a", 65) }},
		{"oversize description", func(m *SkillMeta) { m.Description = strings.Repeat("d", 1025) }},
		{"version with slash", func(m *SkillMeta) { m.Version = "1/2" }},
		{"oversize version", func(m *SkillMeta) { m.Version = strings.Repeat("v", 65) }},
		{"oversize scope", func(m *SkillMeta) { m.Scope = strings.Repeat("s", 129) }},
		{"too many tools", func(m *SkillMeta) { m.Tools = make([]string, 17) }},
		{"oversize tool", func(m *SkillMeta) { m.Tools = []string{strings.Repeat("t", 129)} }},
	}
	for _, tc := range bad {
		m := base
		tc.mut(&m)
		if err := s.IndexSkill(ctx, "ns", m, "body"); err == nil {
			t.Errorf("%s: want validation error", tc.name)
		}
	}

	// Empty version and empty source get defaults; empty tools/scope are fine.
	m := base
	m.Version = ""
	m.Source = ""
	m.Tools = nil
	m.Scope = ""
	if err := s.IndexSkill(ctx, "ns", m, "body"); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.LoadSkill(ctx, "ns", m.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version == "" || got.Source == "" {
		t.Fatalf("defaults not applied: %+v", got)
	}
}

func TestSkillSearchLimitCapped(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	for i := 0; i < SkillDiscoveryMaxHits+5; i++ {
		m := SkillMeta{
			Name:        fmt.Sprintf("skill-%02d", i),
			Version:     "1.0.0",
			Description: "shared benchmark token",
			Source:      SkillSourceAuthored, Active: true,
		}
		indexFixture(t, s, "ns", m, "body")
	}
	hits, err := s.SearchSkills(ctx, "ns", "benchmark", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != SkillDiscoveryMaxHits {
		t.Fatalf("hits = %d, want capped at %d", len(hits), SkillDiscoveryMaxHits)
	}
	listed, err := s.ListSkills(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != SkillDiscoveryMaxHits+5 {
		t.Fatalf("list = %d, want every active skill", len(listed))
	}
}

// TestListSkillsAllPagesPastRecallCap is the 2d red proof: ListSkillsAll
// used Recall(ctx, ns, "/skills/", 0), which caps at 1000 rows, so the
// lifecycle sweep (skillmine's tombstone reconciliation) silently missed
// every skill version past the first 1000. Indexing 1005 versions must
// make every one of them visible.
func TestListSkillsAllPagesPastRecallCap(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	const n = 1005
	for i := 0; i < n; i++ {
		m := SkillMeta{
			Name:        fmt.Sprintf("cap-skill-%04d", i),
			Version:     "1",
			Description: "small body, past the 1000-row recall cap",
			Source:      SkillSourceAuthored, Active: true,
		}
		indexFixture(t, s, "ns", m, "b")
	}
	all, err := s.ListSkillsAll(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Fatalf("ListSkillsAll = %d, want %d (past the 1000-row Recall cap)", len(all), n)
	}
}

// installSkillInsertFailure injects the same store failure on both engines.
// Identifiers are fixed test constants; patterns/messages are SQL-quoted.
// The returned clear function supports the existing retry-after-failure tests.
func installSkillInsertFailure(t *testing.T, s *Store, name, pattern, message string) func() {
	t.Helper()
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	trigger := fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT ON memories WHEN NEW.key LIKE %s BEGIN SELECT RAISE(ABORT,%s); END", name, quote(pattern), quote(message))
	cleanup := fmt.Sprintf("DROP TRIGGER IF EXISTS %s", name)
	if s.db.Driver == "postgres" {
		function := name + "_fn"
		ddl := fmt.Sprintf("CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION %s; END $$", function, quote(message))
		if _, err := s.db.ExecContext(t.Context(), ddl); err != nil {
			t.Fatal(err)
		}
		trigger = fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT ON memories FOR EACH ROW WHEN (NEW.key LIKE %s) EXECUTE FUNCTION %s()", name, quote(pattern), function)
		cleanup = fmt.Sprintf("DROP FUNCTION IF EXISTS %s() CASCADE", function)
	}
	clear := func() {
		t.Helper()
		if _, err := s.db.ExecContext(context.Background(), cleanup); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(clear)
	if _, err := s.db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	return clear
}
