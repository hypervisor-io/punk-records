package skillmine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/spec"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func writeSkill(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Task S01: authored spec skills and mined draft skills both become
// discoverable through the memory-plane skill index without copying
// their procedures into the discovery payload.

func newMemStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := store.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return memory.New(db, nil)
}

const authoredSkill = `---
name: db-connection-triage
description: Diagnose database connection saturation and pool exhaustion
license: LGPL-2.1-only OR LGPL-3.0-only
metadata:
  version: 0.1.0
  action_class: read
  approval_required: "false"
  scope: incident
allowed-tools: incidents__get_incident incidents__list_incidents
---

# Connection saturation triage

1. Pull the incident timeline; note first alert time.
2. Compare active connection count against the configured ceiling.
`

func TestMetaFromSpec(t *testing.T) {
	sk, err := spec.ParseSkill("skills/db-connection-triage/SKILL.md", "db-connection-triage", []byte(authoredSkill))
	if err != nil {
		t.Fatal(err)
	}
	m := MetaFromSpec(sk)
	if m.Name != "db-connection-triage" || m.Version != "0.1.0" {
		t.Fatalf("meta: %+v", m)
	}
	if m.Description != "Diagnose database connection saturation and pool exhaustion" {
		t.Fatalf("description: %q", m.Description)
	}
	if len(m.Tools) != 2 || m.Tools[0] != "incidents__get_incident" {
		t.Fatalf("tools: %+v", m.Tools)
	}
	if m.Scope != "incident" || m.Source != memory.SkillSourceAuthored || !m.Active {
		t.Fatalf("scope/source/active: %+v", m)
	}

	// No metadata block: version stays empty for the store default,
	// scope empty, tools empty.
	minimal, err := spec.ParseSkill("skills/mini/SKILL.md", "mini",
		[]byte("---\nname: mini\ndescription: smallest legal skill\n---\n\ndoit\n"))
	if err != nil {
		t.Fatal(err)
	}
	mm := MetaFromSpec(minimal)
	if mm.Version != "" || mm.Scope != "" || len(mm.Tools) != 0 {
		t.Fatalf("minimal meta: %+v", mm)
	}
}

func TestIndexBundleMakesAuthoredSkillsDiscoverable(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root+"/skills/db-connection-triage/SKILL.md", authoredSkill)
	writeSkill(t, root+"/skills/db-lock-contention/SKILL.md", `---
name: db-lock-contention
description: Diagnose lock contention and blocked queries
metadata:
  version: 0.2.0
---

# Lock contention

1. List blocked sessions.
`)
	bundle, errs := spec.LoadDir(root)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	mem := newMemStore(t)
	ctx := context.Background()

	n, err := IndexBundle(ctx, mem, "ns", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("indexed %d, want 2", n)
	}

	hits, err := mem.SearchSkills(ctx, "ns", "connection saturation", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "db-connection-triage" {
		t.Fatalf("hits: %+v", hits)
	}
	if hits[0].Source != memory.SkillSourceAuthored || hits[0].Version != "0.1.0" {
		t.Fatalf("hit meta: %+v", hits[0])
	}
	// The on-demand body is the parsed SKILL.md markdown body.
	_, body, err := mem.LoadSkill(ctx, "ns", "db-lock-contention", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "List blocked sessions.") {
		t.Fatalf("body: %q", body)
	}
	// Idempotent: re-indexing the same bundle changes nothing.
	n, err = IndexBundle(ctx, mem, "ns", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reindexed %d, want 2", n)
	}
	if listed, err := mem.ListSkills(ctx, "ns"); err != nil || len(listed) != 2 {
		t.Fatalf("list after reindex: %+v err=%v", listed, err)
	}
}

func TestIndexBundleEmpty(t *testing.T) {
	mem := newMemStore(t)
	n, err := IndexBundle(context.Background(), mem, "ns", &spec.Bundle{Skills: map[string]*spec.SkillSpec{}})
	if err != nil || n != 0 {
		t.Fatalf("empty bundle: n=%d err=%v", n, err)
	}
}

func TestIndexDraftsMakesMinedSkillsDiscoverable(t *testing.T) {
	dir := t.TempDir()
	groups := []Group{{Agent: "db", Trajectory: []string{"get_logs"}, TaskIDs: []string{"t1"}, Summaries: []string{"s"}}}
	if _, err := WriteDrafts(context.Background(), groups, fakeDrafter{}, dir); err != nil {
		t.Fatal(err)
	}
	mem := newMemStore(t)
	ctx := context.Background()

	n, err := IndexDrafts(ctx, mem, "ns", dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("indexed %d, want 1", n)
	}
	hits, err := mem.SearchSkills(ctx, "ns", "triage db incidents", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "db-triage" {
		t.Fatalf("hits: %+v", hits)
	}
	if hits[0].Source != memory.SkillSourceMined {
		t.Fatalf("source: %+v", hits[0])
	}
	// Drafts carry no version; the store default applies.
	if hits[0].Version == "" {
		t.Fatalf("draft version not defaulted: %+v", hits[0])
	}
	_, body, err := mem.LoadSkill(ctx, "ns", hits[0].Name, hits[0].Version)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "look at logs") {
		t.Fatalf("draft body: %q", body)
	}
}

func TestIndexDraftsEmptyDir(t *testing.T) {
	mem := newMemStore(t)
	n, err := IndexDrafts(context.Background(), mem, "ns", t.TempDir())
	if err != nil || n != 0 {
		t.Fatalf("empty dir: n=%d err=%v", n, err)
	}
	n, err = IndexDrafts(context.Background(), mem, "ns", t.TempDir()+"/never-written")
	if err != nil || n != 0 {
		t.Fatalf("missing dir: n=%d err=%v", n, err)
	}
}

// SyncBundle is the startup/hot-reload lifecycle: it publishes the
// bundle idempotently, keeps a same-version source edit pinned as a
// recorded conflict, sweeps superseded versions on a bump, and
// unpublishes skills whose source vanished from the tree.
func TestSyncBundleLifecycle(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root+"/skills/db-connection-triage/SKILL.md", authoredSkill)
	writeSkill(t, root+"/skills/db-lock-contention/SKILL.md", `---
name: db-lock-contention
description: Diagnose lock contention and blocked queries
metadata:
  version: 0.2.0
---

# Lock contention

1. List blocked sessions.
`)
	load := func() *spec.Bundle {
		bundle, errs := spec.LoadDir(root)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		return bundle
	}
	mem := newMemStore(t)
	ctx := context.Background()

	rep, err := SyncBundle(ctx, mem, "ns", load())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 2 || rep.Tombstoned != 0 || len(rep.Conflicts) != 0 {
		t.Fatalf("initial sync: %+v", rep)
	}
	hits, err := mem.SearchSkills(ctx, "ns", "connection saturation", 0)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits: %+v err=%v", hits, err)
	}

	// Idempotent: syncing the unchanged bundle publishes nothing new and
	// conflicts with nothing.
	rep, err = SyncBundle(ctx, mem, "ns", load())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tombstoned != 0 || len(rep.Conflicts) != 0 {
		t.Fatalf("unchanged re-sync: %+v", rep)
	}

	// Same-version content edit: kept pinned, recorded as a conflict,
	// and the rest of the catalog still syncs.
	writeSkill(t, root+"/skills/db-connection-triage/SKILL.md",
		strings.Replace(authoredSkill, "Pull the incident timeline", "Rewritten step without a version bump", 1))
	rep, err = SyncBundle(ctx, mem, "ns", load())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Conflicts) != 1 || rep.Conflicts[0] != "db-connection-triage@0.1.0" {
		t.Fatalf("conflicts: %+v", rep)
	}
	_, body, err := mem.LoadSkill(ctx, "ns", "db-connection-triage", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Pull the incident timeline") {
		t.Fatalf("pinned body changed under a same-version edit: %q", body)
	}

	// Version bump: the revision publishes, the superseded version is
	// swept, and the conflict clears.
	writeSkill(t, root+"/skills/db-connection-triage/SKILL.md",
		strings.Replace(authoredSkill, "version: 0.1.0", "version: 0.2.0", 1))
	rep, err = SyncBundle(ctx, mem, "ns", load())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Conflicts) != 0 || rep.Tombstoned != 1 {
		t.Fatalf("bump sync: %+v", rep)
	}
	if _, _, err := mem.LoadSkill(ctx, "ns", "db-connection-triage", "0.1.0"); !errors.Is(err, memory.ErrSkillNotFound) {
		t.Fatalf("superseded version: err = %v, want ErrSkillNotFound", err)
	}

	// Deleting the skill from the tree unpublishes it; the untouched
	// sibling survives.
	if err := os.RemoveAll(root + "/skills/db-connection-triage"); err != nil {
		t.Fatal(err)
	}
	rep, err = SyncBundle(ctx, mem, "ns", load())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tombstoned != 1 {
		t.Fatalf("delete sync: %+v", rep)
	}
	if hits, err := mem.SearchSkills(ctx, "ns", "connection saturation", 0); err != nil || len(hits) != 0 {
		t.Fatalf("deleted source still discoverable: %+v err=%v", hits, err)
	}
	if listed, err := mem.ListSkills(ctx, "ns"); err != nil || len(listed) != 1 || listed[0].Name != "db-lock-contention" {
		t.Fatalf("survivor: %+v err=%v", listed, err)
	}
}

// A source deleted then restored across syncs must not launder the
// version's identity: the sweep unpublishes the vanished source, the
// restored source with changed content under the same version stays a
// recorded conflict with the version unpublished, and only the original
// bytes re-publish it. (Round-3 review finding: unpublish/re-publish
// across the startup/reload lifecycle.)
func TestSyncBundleReaddedSourceKeepsVersionIdentity(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root+"/skills/db-connection-triage/SKILL.md", authoredSkill)
	load := func() *spec.Bundle {
		bundle, errs := spec.LoadDir(root)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		return bundle
	}
	mem := newMemStore(t)
	ctx := context.Background()

	if _, err := SyncBundle(ctx, mem, "ns", load()); err != nil {
		t.Fatal(err)
	}

	// Source vanishes: the version is swept out of discovery and load.
	if err := os.RemoveAll(root + "/skills/db-connection-triage"); err != nil {
		t.Fatal(err)
	}
	rep, err := SyncBundle(ctx, mem, "ns", load())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tombstoned != 1 {
		t.Fatalf("delete sync: %+v", rep)
	}

	// Source returns with changed content under the same version: the
	// tombstoned version still pins what shipped, so this is a recorded
	// conflict and the version stays unpublished.
	writeSkill(t, root+"/skills/db-connection-triage/SKILL.md",
		strings.Replace(authoredSkill, "Pull the incident timeline", "Rewritten step without a version bump", 1))
	rep, err = SyncBundle(ctx, mem, "ns", load())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 0 || len(rep.Conflicts) != 1 || rep.Conflicts[0] != "db-connection-triage@0.1.0" {
		t.Fatalf("changed re-add sync: %+v", rep)
	}
	if _, _, err := mem.LoadSkill(ctx, "ns", "db-connection-triage", "0.1.0"); !errors.Is(err, memory.ErrSkillNotFound) {
		t.Fatalf("changed re-add republished the swept version: err = %v, want ErrSkillNotFound", err)
	}

	// The original bytes return: the identical content re-publishes.
	writeSkill(t, root+"/skills/db-connection-triage/SKILL.md", authoredSkill)
	rep, err = SyncBundle(ctx, mem, "ns", load())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 1 || rep.Tombstoned != 0 || len(rep.Conflicts) != 0 {
		t.Fatalf("restore sync: %+v", rep)
	}
	_, body, err := mem.LoadSkill(ctx, "ns", "db-connection-triage", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Pull the incident timeline") {
		t.Fatalf("restored body: %q", body)
	}
}

// altDrafter regenerates a draft with changed procedure text, the way a
// WriteDrafts rerun overwrites a same-slug file.
type altDrafter struct{}

func (altDrafter) Draft(_ context.Context, g Group) (string, string, string, error) {
	return "Db Triage!", "triage db incidents", "1. read the dashboard first\n2. then check metrics", nil
}

// SyncDrafts is the mined-ingest lifecycle: drafts without a declared
// version get a content-derived revision, so a regenerated draft
// publishes as a new immutable version and its superseded generation is
// swept; a deleted draft is unpublished. Authored skills in the same
// namespace are never touched.
func TestSyncDraftsLifecycle(t *testing.T) {
	dir := t.TempDir()
	groups := []Group{{Agent: "db", Trajectory: []string{"get_logs"}, TaskIDs: []string{"t1"}, Summaries: []string{"s"}}}
	ctx := context.Background()
	if _, err := WriteDrafts(ctx, groups, fakeDrafter{}, dir); err != nil {
		t.Fatal(err)
	}
	mem := newMemStore(t)

	// An authored skill shares the namespace; mined syncs must not sweep it.
	if err := mem.IndexSkill(ctx, "ns", memory.SkillMeta{
		Name: "authored-keeper", Version: "1.0.0", Description: "stays across mined syncs",
		Source: memory.SkillSourceAuthored, Active: true,
	}, "body"); err != nil {
		t.Fatal(err)
	}

	rep, err := SyncDrafts(ctx, mem, "ns", dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 1 || rep.Tombstoned != 0 {
		t.Fatalf("initial draft sync: %+v", rep)
	}
	hits, err := mem.SearchSkills(ctx, "ns", "triage db incidents", 0)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits: %+v err=%v", hits, err)
	}
	first := hits[0]
	if first.Source != memory.SkillSourceMined || !strings.HasPrefix(first.Version, "r") {
		t.Fatalf("mined meta: %+v", first)
	}

	// Regenerate with changed procedure text: a new content revision is
	// published and the superseded generation is swept, so unversioned
	// load stays unambiguous.
	if _, err := WriteDrafts(ctx, groups, altDrafter{}, dir); err != nil {
		t.Fatal(err)
	}
	rep, err = SyncDrafts(ctx, mem, "ns", dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 1 || rep.Tombstoned != 1 || len(rep.Conflicts) != 0 {
		t.Fatalf("regeneration sync: %+v", rep)
	}
	_, body, err := mem.LoadSkill(ctx, "ns", first.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "read the dashboard first") {
		t.Fatalf("regenerated draft body: %q", body)
	}

	// Deleting the drafts unpublishes the mined entries only.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err = SyncDrafts(ctx, mem, "ns", dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tombstoned != 1 {
		t.Fatalf("delete sync: %+v", rep)
	}
	if hits, err := mem.SearchSkills(ctx, "ns", "triage db incidents", 0); err != nil || len(hits) != 0 {
		t.Fatalf("deleted drafts still discoverable: %+v err=%v", hits, err)
	}
	if _, _, err := mem.LoadSkill(ctx, "ns", "authored-keeper", "1.0.0"); err != nil {
		t.Fatalf("mined sync swept an authored skill: %v", err)
	}
}
