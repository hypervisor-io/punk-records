package skillmine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/spec"
)

// Task S01 bridges: turn existing SKILL.md material into memory-plane
// skill metadata so SearchSkills/LoadSkill can discover it. Authored
// skills come from a validated spec.Bundle (spec.LoadDir output, the
// same snapshot the registry serves); mined skills are WriteDrafts
// output parsed back through spec.ParseSkill, so one frontmatter format
// serves both. Procedures are indexed once into the store and loaded on
// demand - nothing here copies bodies into prompts.

// MetaFromSpec maps one parsed SKILL.md onto discovery metadata.
// Version and scope ride the punk frontmatter extensions
// (metadata.version, metadata.scope); declared tools are the
// space-separated allowed-tools list. The skill starts active.
func MetaFromSpec(sk *spec.SkillSpec) memory.SkillMeta {
	return memory.SkillMeta{
		Name:        sk.Name,
		Version:     sk.Metadata["version"],
		Description: sk.Description,
		Tools:       strings.Fields(sk.AllowedTools),
		Scope:       sk.Metadata["scope"],
		Source:      memory.SkillSourceAuthored,
		Active:      true,
	}
}

// IndexSkillSpec indexes one parsed skill into ns. Exported so callers
// with their own spec source (for example a hot-reload hook) do not
// need to round-trip through a Bundle.
func IndexSkillSpec(ctx context.Context, mem *memory.Store, ns string, sk *spec.SkillSpec) error {
	if err := mem.IndexSkill(ctx, ns, MetaFromSpec(sk), sk.Body); err != nil {
		return fmt.Errorf("index skill %s: %w", sk.Name, err)
	}
	return nil
}

// IndexBundle indexes every skill in a validated bundle into ns and
// returns how many were indexed. Names are sorted so write order (and
// therefore revision order on re-index) is deterministic. An empty
// bundle is a normal zero result.
func IndexBundle(ctx context.Context, mem *memory.Store, ns string, b *spec.Bundle) (int, error) {
	if b == nil {
		return 0, nil
	}
	names := make([]string, 0, len(b.Skills))
	for name := range b.Skills {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := IndexSkillSpec(ctx, mem, ns, b.Skills[name]); err != nil {
			return 0, err
		}
	}
	return len(names), nil
}

// IndexDrafts indexes every mined draft under dir (WriteDrafts output:
// dir/<slug>/SKILL.md) into ns and returns how many were indexed.
// A missing or empty directory is a normal zero result - nothing was
// mined yet. Drafts without a frontmatter version get a content-derived
// revision (see draftVersion) so a regenerated draft never trips the
// store's published-version immutability.
func IndexDrafts(ctx context.Context, mem *memory.Store, ns, dir string) (int, error) {
	drafts, err := readDrafts(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range drafts {
		if err := mem.IndexSkill(ctx, ns, d.meta, d.body); err != nil {
			return n, fmt.Errorf("index draft %s: %w", d.meta.Name, err)
		}
		n++
	}
	return n, nil
}

// draft is one parsed proposed SKILL.md plus its discovery metadata.
type draft struct {
	meta memory.SkillMeta
	body string
}

// draftVersion pins a draft's discovery version: the frontmatter
// metadata.version when the author declared one, else a content revision
// derived from the exact file bytes ("r" + 12 hex of SHA-256). Mined
// drafts are regenerated in place (WriteDrafts overwrites same-slug
// files), and the store refuses to republish changed content under a
// version that already shipped; the derived revision keeps every
// regeneration a distinct, immutable published version.
func draftVersion(sk *spec.SkillSpec, raw []byte) string {
	if v := sk.Metadata["version"]; v != "" {
		return v
	}
	sum := sha256.Sum256(raw)
	return "r" + hex.EncodeToString(sum[:])[:12]
}

// readDrafts parses every draft under dir (sorted by slug for
// deterministic write order). A missing directory or a folder without a
// SKILL.md is a normal empty result.
func readDrafts(dir string) ([]draft, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	drafts := []draft{}
	for _, name := range names {
		path := filepath.Join(dir, name, "SKILL.md")
		raw, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // a sibling folder, not a draft
			}
			return drafts, err
		}
		sk, err := spec.ParseSkill(path, name, raw)
		if err != nil {
			return drafts, err
		}
		m := MetaFromSpec(sk)
		m.Source = memory.SkillSourceMined
		m.Version = draftVersion(sk, raw)
		drafts = append(drafts, draft{meta: m, body: sk.Body})
	}
	return drafts, nil
}

// SyncReport is what one lifecycle sync did. Conflicts and Rejected are
// per-skill skips, not failures: a conflict means the source republished
// changed content under a version that already shipped (the pinned
// version stays; bump metadata.version to publish the revision), a
// rejection means the skill's metadata failed store validation. Neither
// blocks the rest of the catalog.
type SyncReport struct {
	Indexed    int      // desired versions confirmed in the index (new or already present; identical re-syncs count here)
	Tombstoned int      // versions swept because their source vanished
	Conflicts  []string // "name@version" entries kept pinned
	Rejected   []string // "name@version (reason)" entries refused by validation
}

// declaredVersion normalizes a spec-declared version the way the store's
// validation does (empty becomes "0"), so the sync's desired set compares
// equal to what IndexSkill actually publishes.
func declaredVersion(v string) string {
	if v == "" {
		return "0"
	}
	return v
}

// syncSkills is the shared lifecycle engine: index every desired skill
// (idempotently - unchanged content is a no-op, conflicts and invalid
// metadata are recorded and skipped), then unpublish every live indexed
// skill of the same source whose name@version is no longer desired.
// Only entries of the given source are swept, so the authored sync never
// touches mined drafts and vice versa.
func syncSkills(ctx context.Context, mem *memory.Store, ns, source string, desired []draft) (SyncReport, error) {
	var rep SyncReport
	want := map[string]bool{}
	for _, d := range desired {
		d.meta.Source = source
		d.meta.Version = declaredVersion(d.meta.Version)
		want[d.meta.Name+"\x00"+d.meta.Version] = true
		if err := mem.IndexSkill(ctx, ns, d.meta, d.body); err != nil {
			if errors.Is(err, memory.ErrSkillVersionConflict) {
				rep.Conflicts = append(rep.Conflicts, d.meta.Name+"@"+d.meta.Version)
				continue
			}
			rep.Rejected = append(rep.Rejected, fmt.Sprintf("%s@%s (%v)", d.meta.Name, d.meta.Version, err))
			continue
		}
		rep.Indexed++
	}
	live, err := mem.ListSkillsAll(ctx, ns)
	if err != nil {
		return rep, err
	}
	for _, m := range live {
		if m.Source != source || want[m.Name+"\x00"+m.Version] {
			continue
		}
		if err := mem.UnpublishSkill(ctx, ns, m.Name, m.Version); err != nil {
			return rep, err
		}
		rep.Tombstoned++
	}
	return rep, nil
}

// SyncBundle reconciles the authored skill index in ns with a validated
// spec bundle (the registry's active snapshot): new or bumped versions
// are published, unchanged ones are skipped, versions the tree no longer
// declares are unpublished, and same-version content edits are kept
// pinned and reported as conflicts. This is the startup and hot-reload
// lifecycle entry point; a nil or empty bundle unpublishes every
// authored skill, matching "the spec tree is the source of truth".
func SyncBundle(ctx context.Context, mem *memory.Store, ns string, b *spec.Bundle) (SyncReport, error) {
	desired := []draft{}
	if b != nil {
		names := make([]string, 0, len(b.Skills))
		for name := range b.Skills {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sk := b.Skills[name]
			desired = append(desired, draft{meta: MetaFromSpec(sk), body: sk.Body})
		}
	}
	return syncSkills(ctx, mem, ns, memory.SkillSourceAuthored, desired)
}

// SyncDrafts reconciles the mined skill index in ns with the drafts
// currently under dir (WriteDrafts output). This is the mined-ingest
// lifecycle entry point (punk skills propose). Content-derived draft
// versions mean a regenerated draft publishes as a new revision and its
// superseded generation is swept; a deleted draft is unpublished.
func SyncDrafts(ctx context.Context, mem *memory.Store, ns, dir string) (SyncReport, error) {
	desired, err := readDrafts(dir)
	if err != nil {
		return SyncReport{}, err
	}
	return syncSkills(ctx, mem, ns, memory.SkillSourceMined, desired)
}
