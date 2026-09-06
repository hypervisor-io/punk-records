package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Task S01: the procedural-skill index. Skills (agentskills.io SKILL.md
// procedures, authored under specs/skills/ or drafted by skillmine) are
// indexed into the memory plane as THREE keys per version:
//
//   /skills/<name>/<version>        discovery document: the searchable
//                                   metadata digest, with the structured
//                                   metadata in attributes. This is the
//                                   only document discovery reads.
//   /skill-bodies/<name>/<version>  the exact procedure body, loaded on
//                                   demand by LoadSkill and never
//                                   returned by SearchSkills/ListSkills.
//   /skill-identities/<name>/<version>
//                                   the version's content address: the
//                                   SHA-256 hex digest of its published
//                                   metadata and procedure, minted when
//                                   the version first ships. Only the
//                                   digest is stored, never the body.
//
// Discovery (SearchSkills) is metadata-only by construction: the digest
// never contains procedure text, so a hit cannot leak it, and search
// terms that appear only inside a procedure match nothing (Cognee's
// skills_retriever pattern: retrieve on name/description, load the
// procedure separately). With an embedder wired, SearchSkills fuses the
// FTS arm with a vector arm (RRF k=60); without one it is plain scoped
// FTS - the lexical fallback must never error for lack of embeddings.
//
// Lifecycle: Active=false excludes a skill from discovery and load
// (SetSkillActive flips it with a new revision; the stored body is kept
// for audit). A tombstoned discovery document disappears entirely,
// courtesy of latest-wins. A published name@version is IMMUTABLE:
// re-indexing identical content is a no-op, re-indexing changed content
// under a version that already shipped is ErrSkillVersionConflict (bump
// the version to publish a revision), so an exact-version load can never
// return a different procedure after the source is edited. Visibility
// and identity are separate concerns: unpublishing hides a version but
// does NOT reset its identity. Identity rides the content-addressed
// record, NOT the bi-temporal history: history is purgeable audit data
// (SweepRetention and Consolidate delete superseded and dead rows), so
// pinning identity to it would let routine retention reset a published
// version. The identity record is retention-exempt by construction -
// one revision, never superseded, never tombstoned or expired by the
// skill lifecycle, and both purge paths keep every key's latest live
// revision - so the pin outlives the discovery document and body it was
// minted from: after unpublish plus a full retention sweep, identical
// content still restores the version while changed content under the
// same name@version stays a conflict, and the deleted procedure bytes
// stay deleted (only the digest is remembered). Identity records are
// namespace-scoped like every other fact: the same name@version in two
// namespaces are two independent versions.

// SkillMeta is the discovery record for one skill version. It carries
// no Body field on purpose: procedure text moves only through LoadSkill.
type SkillMeta struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Tools       []string `json:"tools,omitempty"`
	Scope       string   `json:"scope,omitempty"`
	Source      string   `json:"source"`
	Active      bool     `json:"active"`
}

// Skill provenance values for SkillMeta.Source.
const (
	SkillSourceAuthored = "authored" // specs/skills/ tree (spec.LoadDir)
	SkillSourceMined    = "mined"    // skillmine draft output
)

// SkillDiscoveryMaxHits is the hard cap on one SearchSkills result. With
// the per-field validation limits below it is what makes the discovery
// payload size a closed-form bound (pinned in internal/mcpserver's
// TestSkillDiscoveryPayloadWithinBound).
const SkillDiscoveryMaxHits = 20

// skillDiscoveryDefaultHits applies when the caller passes no limit.
const skillDiscoveryDefaultHits = 10

// skillSearchCandidates is how many ranked rows each retrieval arm may
// contribute before fusion and the hit cap; arms are ranked, so the cap
// only trims the long tail.
const skillSearchCandidates = 200

// Per-field limits mirror spec.ParseSkill's (name 64, description 1024)
// and extend them to the fields spec leaves free-form, so the MCP
// payload bound holds no matter which writer indexed the skill.
const (
	skillNameMaxRunes        = 64
	skillVersionMaxRunes     = 64
	skillDescriptionMaxRunes = 1024
	skillScopeMaxRunes       = 128
	skillMaxTools            = 16
	skillToolMaxRunes        = 128
	skillSourceMaxRunes      = 32
)

var (
	// ErrSkillNotFound: no live discovery document for name[/version].
	ErrSkillNotFound = errors.New("memory: skill not found")
	// ErrSkillInactive: the skill is indexed but deactivated; discovery
	// excludes it and load refuses it until reactivated.
	ErrSkillInactive = errors.New("memory: skill is inactive")
	// ErrSkillVersionConflict: name@version is already published with
	// different metadata or a different procedure. Published versions are
	// immutable; bump the version to publish a revision.
	ErrSkillVersionConflict = errors.New("memory: skill version already published with different content")
)

var skillNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func skillDocKey(name, version string) string  { return "/skills/" + name + "/" + version }
func skillBodyKey(name, version string) string { return "/skill-bodies/" + name + "/" + version }
func skillIdentityKey(name, version string) string {
	return "/skill-identities/" + name + "/" + version
}

// validateSkillMeta enforces the field limits and applies the defaults
// (empty version -> "0", empty source -> authored).
func validateSkillMeta(m *SkillMeta) error {
	if len(m.Name) > skillNameMaxRunes || !skillNameRE.MatchString(m.Name) {
		return fmt.Errorf("memory: skill name %q: 1-%d chars, lowercase alphanumerics and hyphens", m.Name, skillNameMaxRunes)
	}
	if m.Version == "" {
		m.Version = "0"
	}
	if len(m.Version) > skillVersionMaxRunes || strings.ContainsAny(m.Version, "/ \t\n") {
		return fmt.Errorf("memory: skill version %q: 1-%d chars, no slashes or whitespace", m.Version, skillVersionMaxRunes)
	}
	if m.Description == "" {
		return errors.New("memory: skill description is required")
	}
	if len(m.Description) > skillDescriptionMaxRunes {
		return fmt.Errorf("memory: skill description exceeds %d chars", skillDescriptionMaxRunes)
	}
	if len(m.Scope) > skillScopeMaxRunes {
		return fmt.Errorf("memory: skill scope exceeds %d chars", skillScopeMaxRunes)
	}
	if len(m.Tools) > skillMaxTools {
		return fmt.Errorf("memory: skill tools: want at most %d entries", skillMaxTools)
	}
	for _, t := range m.Tools {
		if len(t) > skillToolMaxRunes || strings.ContainsAny(t, " \t\n") {
			return fmt.Errorf("memory: skill tool %q: 1-%d chars, no whitespace", t, skillToolMaxRunes)
		}
	}
	if m.Source == "" {
		m.Source = SkillSourceAuthored
	}
	if len(m.Source) > skillSourceMaxRunes {
		return fmt.Errorf("memory: skill source exceeds %d chars", skillSourceMaxRunes)
	}
	return nil
}

// skillDigest is the discovery document's body: the metadata text FTS
// and embedding retrieval match on. Procedure text never enters it.
func skillDigest(m SkillMeta) string {
	var b strings.Builder
	b.WriteString(m.Name)
	b.WriteString(": ")
	b.WriteString(m.Description)
	if len(m.Tools) > 0 {
		b.WriteString("\ntools: ")
		b.WriteString(strings.Join(m.Tools, " "))
	}
	if m.Scope != "" {
		b.WriteString("\nscope: ")
		b.WriteString(m.Scope)
	}
	return b.String()
}

func skillAttributes(m SkillMeta) map[string]any {
	attrs := map[string]any{
		"skill_name":        m.Name,
		"skill_version":     m.Version,
		"skill_description": m.Description,
		"skill_scope":       m.Scope,
		"skill_source":      m.Source,
		"skill_active":      m.Active,
	}
	if len(m.Tools) > 0 {
		tools := make([]any, len(m.Tools))
		for i, t := range m.Tools {
			tools[i] = t
		}
		attrs["skill_tools"] = tools
	}
	return attrs
}

// skillMetaFromFact projects a live discovery-document fact back to
// SkillMeta. Facts without the skill attribute set (user-written keys
// that happen to sit under /skills/) are not skills: ok=false.
func skillMetaFromFact(f Fact) (SkillMeta, bool) {
	var m SkillMeta
	if f.Attributes == nil {
		return m, false
	}
	name, ok := f.Attributes["skill_name"].(string)
	if !ok || name == "" {
		return m, false
	}
	version, ok := f.Attributes["skill_version"].(string)
	if !ok {
		return m, false
	}
	m.Name, m.Version = name, version
	m.Description, _ = f.Attributes["skill_description"].(string)
	m.Scope, _ = f.Attributes["skill_scope"].(string)
	m.Source, _ = f.Attributes["skill_source"].(string)
	m.Active, _ = f.Attributes["skill_active"].(bool)
	if raw, ok := f.Attributes["skill_tools"].([]any); ok {
		for _, t := range raw {
			if s, ok := t.(string); ok {
				m.Tools = append(m.Tools, s)
			}
		}
	}
	return m, true
}

// skillMetaContentEqual compares the content fields two versions of a
// discovery record may not differ in once published. Active is excluded
// on purpose: deactivation is an operator act (SetSkillActive) that
// re-indexing identical content must not silently revert.
func skillMetaContentEqual(a, b SkillMeta) bool {
	if a.Name != b.Name || a.Version != b.Version || a.Description != b.Description ||
		a.Scope != b.Scope || a.Source != b.Source || len(a.Tools) != len(b.Tools) {
		return false
	}
	for i := range a.Tools {
		if a.Tools[i] != b.Tools[i] {
			return false
		}
	}
	return true
}

// skillIdentityHash is the content address of one skill version: a
// SHA-256 hex digest over the identity-bearing metadata fields and the
// procedure body, each length-prefixed so no field boundary is
// ambiguous. It covers exactly what skillMetaContentEqual compares plus
// the body, and excludes Active on the same contract: visibility flips
// are operator acts, not content changes. The digest is what the
// identity record stores - a durable pin needs the hash, never the
// procedure bytes.
func skillIdentityHash(m SkillMeta, body string) string {
	var b strings.Builder
	for _, p := range append([]string{m.Name, m.Version, m.Description, m.Scope, m.Source, body}, m.Tools...) {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// IndexSkill registers one skill version for discovery: the metadata
// digest at /skills/<name>/<version>, the procedure body at
// /skill-bodies/<name>/<version> and the content-addressed identity
// record at /skill-identities/<name>/<version>.
//
// Published versions are immutable, and neither unpublishing nor
// retention sweeps reset that: once a version has shipped, its identity
// record pins the published metadata and procedure digest for good.
// Re-indexing identical content is a no-op while the version is live
// (and preserves an operator's SetSkillActive(false)) and re-publishes
// it when it was tombstoned (a deleted-then-restored source);
// re-indexing changed metadata or a changed body under a shipped
// version fails with ErrSkillVersionConflict - live, tombstoned or
// already swept by retention - so a discovered exact version can never
// start loading a different procedure after a source edit. Bump the
// version to publish a revision. A version whose discovery document
// never shipped (an orphan body left by a crashed publish) is still
// unpublished: nothing was advertised, so its body is free to be
// overwritten by the next publish.
//
// The publish is STAGED: procedure body first, discovery digest second,
// identity record last, so a failure or crash mid-index can leave an
// invisible orphan body (repaired by the next IndexSkill of the same
// version) or an advertised digest whose identity record is still
// missing (healed by the next IndexSkill or UnpublishSkill from the
// live rows) but never an advertised digest without its loadable body;
// a prior version stays coherent throughout. Callers must serialize
// indexing of one name@version (the startup/reload/ingest lifecycle is
// single-writer per namespace).
func (s *Store) IndexSkill(ctx context.Context, ns string, meta SkillMeta, body string) error {
	if err := validateSkillMeta(&meta); err != nil {
		return err
	}
	if !meta.Active {
		return errors.New("memory: IndexSkill takes active skills; deactivate with SetSkillActive after indexing")
	}
	docKey, bodyKey, idKey := skillDocKey(meta.Name, meta.Version), skillBodyKey(meta.Name, meta.Version), skillIdentityKey(meta.Name, meta.Version)
	live, err := s.liveByKeys(ctx, ns, []string{docKey, bodyKey, idKey})
	if err != nil {
		return err
	}
	var doc, bod, idf *Fact
	for i := range live {
		switch live[i].Key {
		case docKey:
			doc = &live[i]
		case bodyKey:
			bod = &live[i]
		case idKey:
			idf = &live[i]
		}
	}
	// The identity record is the durable pin: it outlives unpublishing
	// and retention sweeps of every row the version ever had, so a
	// shipped version conflicts on changed content regardless of what
	// happened to its discovery document and body.
	if idf != nil && idf.Body != skillIdentityHash(meta, body) {
		return fmt.Errorf("index skill %s@%s: %w", meta.Name, meta.Version, ErrSkillVersionConflict)
	}
	if doc == nil {
		// No live digest: either the version never shipped (a fresh
		// staged publish; an orphan body from a crashed earlier publish
		// was never advertised and is simply overwritten) or it was
		// tombstoned and identical content re-publishes it (the
		// identity check above already refused changed content).
		return s.publishSkillStaged(ctx, ns, meta, body, bod, idf)
	}
	// A live digest pins its published metadata and body directly; the
	// identity record is consulted for what the live rows cannot answer
	// and healed when missing (a publish that crashed between the
	// digest and identity writes).
	cur, ok := skillMetaFromFact(*doc)
	if !ok || !skillMetaContentEqual(cur, meta) {
		return fmt.Errorf("index skill %s@%s: %w", meta.Name, meta.Version, ErrSkillVersionConflict)
	}
	if bod != nil && bod.Body != body {
		return fmt.Errorf("index skill %s@%s: %w", meta.Name, meta.Version, ErrSkillVersionConflict)
	}
	if bod == nil {
		// A live digest with a missing body (an earlier crash between
		// the staged writes, or a partial unpublish): restore the
		// missing body, leave the digest - and its active flag -
		// untouched.
		if _, err := s.Write(ctx, WriteInput{
			Namespace: ns, Key: bodyKey, Body: body,
			Attributes: map[string]any{"skill_name": meta.Name, "skill_version": meta.Version},
			Writer:     "skill-index",
		}); err != nil {
			return fmt.Errorf("index skill body %s@%s: %w", meta.Name, meta.Version, err)
		}
	}
	if idf == nil {
		return s.writeSkillIdentity(ctx, ns, meta, body)
	}
	return nil // idempotent re-index of identical live content
}

// publishSkillStaged writes the procedure body first, the discovery
// digest second and the identity record last, so a failure can only
// leave an invisible orphan body or an advertised digest whose identity
// record the next IndexSkill/UnpublishSkill heals - never an advertised
// digest without its loadable body, and never an identity record for a
// version whose digest never shipped. A live body that already matches
// is left alone: the staged write restores a tombstoned or partially
// published version as often as it publishes a fresh one. An identity
// record that already exists (re-publish of a tombstoned version) is
// never rewritten: it keeps exactly one revision for its whole life.
func (s *Store) publishSkillStaged(ctx context.Context, ns string, meta SkillMeta, body string, bod, idf *Fact) error {
	if bod == nil || bod.Body != body {
		if _, err := s.Write(ctx, WriteInput{
			Namespace: ns, Key: skillBodyKey(meta.Name, meta.Version), Body: body,
			Attributes: map[string]any{"skill_name": meta.Name, "skill_version": meta.Version},
			Writer:     "skill-index",
		}); err != nil {
			return fmt.Errorf("index skill body %s@%s: %w", meta.Name, meta.Version, err)
		}
	}
	if _, err := s.Write(ctx, WriteInput{
		Namespace: ns, Key: skillDocKey(meta.Name, meta.Version), Body: skillDigest(meta),
		Attributes: skillAttributes(meta), Writer: "skill-index",
	}); err != nil {
		return fmt.Errorf("index skill digest %s@%s: %w", meta.Name, meta.Version, err)
	}
	if idf == nil {
		return s.writeSkillIdentity(ctx, ns, meta, body)
	}
	return nil
}

// writeSkillIdentity mints the durable identity record for one shipped
// version: the content address of its metadata and procedure, stored
// WITHOUT the procedure bytes - retention purging an unpublished body
// must stay permanent, only the digest is remembered. The record is
// retention-exempt by construction: it keeps a single revision that the
// skill lifecycle never supersedes, tombstones or expires, and both
// purge paths (SweepRetention, Consolidate) delete only superseded,
// dead or expired rows while keeping every key's latest live revision,
// so the pin outlives the rows it was minted from.
func (s *Store) writeSkillIdentity(ctx context.Context, ns string, meta SkillMeta, body string) error {
	if _, err := s.Write(ctx, WriteInput{
		Namespace: ns, Key: skillIdentityKey(meta.Name, meta.Version),
		Body:       skillIdentityHash(meta, body),
		Attributes: map[string]any{"skill_name": meta.Name, "skill_version": meta.Version},
		Writer:     "skill-index",
	}); err != nil {
		return fmt.Errorf("index skill identity %s@%s: %w", meta.Name, meta.Version, err)
	}
	return nil
}

// pinSkillIdentity heals a missing identity record from the live rows
// before a removal: a publish that crashed between the digest and
// identity writes still advertised the version, so its identity must be
// pinned before the tombstones - and the later retention sweep - take
// the published rows away. No-op when the record already exists, when
// no coherent published pair is live (nothing shippable to pin), or for
// unknown namespaces.
func (s *Store) pinSkillIdentity(ctx context.Context, ns, name, version string) error {
	docKey, bodyKey, idKey := skillDocKey(name, version), skillBodyKey(name, version), skillIdentityKey(name, version)
	live, err := s.liveByKeys(ctx, ns, []string{docKey, bodyKey, idKey})
	if err != nil {
		return err
	}
	var doc, bod, idf *Fact
	for i := range live {
		switch live[i].Key {
		case docKey:
			doc = &live[i]
		case bodyKey:
			bod = &live[i]
		case idKey:
			idf = &live[i]
		}
	}
	if idf != nil || doc == nil || bod == nil {
		return nil
	}
	meta, ok := skillMetaFromFact(*doc)
	if !ok {
		return nil
	}
	return s.writeSkillIdentity(ctx, ns, meta, bod.Body)
}

// UnpublishSkill tombstones one skill version's discovery document and
// procedure body: it vanishes from discovery and load (latest wins),
// while its identity record stays. The identity pin does not ride the
// purgeable history: re-publishing identical content restores the
// version and re-publishing changed content under the tombstoned
// name@version stays ErrSkillVersionConflict even after retention
// sweeps have purged the tombstoned rows and the procedure bytes with
// them. A missing identity record (a publish that crashed mid-index)
// is healed from the live rows BEFORE the removal, so an unpublish can
// never take the last copy of what shipped. Already-absent keys are a
// no-op, so lifecycle sweeps can call it unconditionally.
//
// The removal mirrors the staged publish in reverse; the discovery
// document is tombstoned FIRST and the first failure stops the sweep,
// so a failed unpublish never deletes the body of a still-advertised
// digest (search only ever shows loadable procedures) and the coherent
// prior state survives. A body left behind by a failed second step is
// an invisible orphan the safe retry - or the next IndexSkill of the
// same version - cleans up.
func (s *Store) UnpublishSkill(ctx context.Context, ns, name, version string) error {
	if err := s.pinSkillIdentity(ctx, ns, name, version); err != nil {
		return fmt.Errorf("unpublish skill %s@%s: %w", name, version, err)
	}
	if err := s.Forget(ctx, ns, skillDocKey(name, version), "skill-index"); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("unpublish skill %s@%s: %w", name, version, err)
	}
	if err := s.Forget(ctx, ns, skillBodyKey(name, version), "skill-index"); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("unpublish skill %s@%s: %w", name, version, err)
	}
	return nil
}

// SetSkillActive flips a skill version's discovery visibility by writing
// a new revision of its discovery document; the stored body is kept.
// Unknown skills are ErrSkillNotFound, never a silent no-op.
func (s *Store) SetSkillActive(ctx context.Context, ns, name, version string, active bool) error {
	meta, _, err := s.loadSkillFact(ctx, ns, name, version)
	if err != nil {
		return err
	}
	meta.Active = active
	_, err = s.Write(ctx, WriteInput{
		Namespace: ns, Key: skillDocKey(meta.Name, meta.Version), Body: skillDigest(meta),
		Attributes: skillAttributes(meta), Writer: "skill-index",
	})
	return err
}

// loadSkillFact fetches the live discovery document for one exact
// name/version and projects it. Inactive is an error here (loaders want
// the active record or nothing); discovery filters it instead.
func (s *Store) loadSkillFact(ctx context.Context, ns, name, version string) (SkillMeta, Fact, error) {
	if !skillNameRE.MatchString(name) {
		return SkillMeta{}, Fact{}, fmt.Errorf("memory: skill name %q: %w", name, ErrSkillNotFound)
	}
	if version == "" {
		return SkillMeta{}, Fact{}, fmt.Errorf("memory: skill version is required: %w", ErrSkillNotFound)
	}
	facts, err := s.liveByKeys(ctx, ns, []string{skillDocKey(name, version)})
	if err != nil {
		return SkillMeta{}, Fact{}, err
	}
	if len(facts) == 0 {
		return SkillMeta{}, Fact{}, fmt.Errorf("memory: %s@%s: %w", name, version, ErrSkillNotFound)
	}
	meta, ok := skillMetaFromFact(facts[0])
	if !ok {
		return SkillMeta{}, Fact{}, fmt.Errorf("memory: %s@%s: %w", name, version, ErrSkillNotFound)
	}
	return meta, facts[0], nil
}

// LoadSkill returns the exact versioned procedure body for name. An
// empty version resolves only when exactly one version is active;
// several live versions make it an error naming the choices. Inactive
// skills refuse to load (ErrSkillInactive), unknown ones ErrSkillNotFound.
func (s *Store) LoadSkill(ctx context.Context, ns, name, version string) (SkillMeta, string, error) {
	if version == "" {
		facts, err := s.Recall(ctx, ns, "/skills/"+name+"/", 0)
		if err != nil {
			return SkillMeta{}, "", err
		}
		var live []string
		for _, f := range facts {
			if m, ok := skillMetaFromFact(f); ok && m.Active {
				live = append(live, m.Version)
			}
		}
		switch len(live) {
		case 0:
			return SkillMeta{}, "", fmt.Errorf("memory: %s: %w", name, ErrSkillNotFound)
		case 1:
			version = live[0]
		default:
			sort.Strings(live)
			return SkillMeta{}, "", fmt.Errorf("memory: %s has %d live versions (%s); pass an exact version",
				name, len(live), strings.Join(live, ", "))
		}
	}
	meta, _, err := s.loadSkillFact(ctx, ns, name, version)
	if err != nil {
		return SkillMeta{}, "", err
	}
	if !meta.Active {
		return SkillMeta{}, "", fmt.Errorf("memory: %s@%s: %w", name, version, ErrSkillInactive)
	}
	bodies, err := s.liveByKeys(ctx, ns, []string{skillBodyKey(name, version)})
	if err != nil {
		return SkillMeta{}, "", err
	}
	if len(bodies) == 0 {
		return SkillMeta{}, "", fmt.Errorf("memory: %s@%s body: %w", name, version, ErrSkillNotFound)
	}
	return meta, bodies[0].Body, nil
}

// ListSkills returns the metadata of every active skill version in ns.
func (s *Store) ListSkills(ctx context.Context, ns string) ([]SkillMeta, error) {
	all, err := s.ListSkillsAll(ctx, ns)
	if err != nil {
		return nil, err
	}
	out := []SkillMeta{}
	for _, m := range all {
		if m.Active {
			out = append(out, m)
		}
	}
	return out, nil
}

// ListSkillsAll returns every live skill version in ns, including
// deactivated ones. Lifecycle sync (skillmine.SyncBundle/SyncDrafts)
// sweeps against this complete view so an inactive skill whose source
// vanished is still unpublished rather than lingering forever.
func (s *Store) ListSkillsAll(ctx context.Context, ns string) ([]SkillMeta, error) {
	facts, err := s.Recall(ctx, ns, "/skills/", 0)
	if err != nil {
		return nil, err
	}
	out := []SkillMeta{}
	for _, f := range facts {
		if m, ok := skillMetaFromFact(f); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// skillFTSSQL is searchMatch's latest-live-revision FTS query with an
// added key-prefix predicate ($5), so discovery only ever ranks the
// /skills/ collection. memory.go's searchMatch is not extended with a
// prefix parameter because S01 may not touch it.
const skillFTSSQLite = `
SELECT ` + factCols + `
FROM memories_fts f
JOIN memories m ON m.rowid = f.rowid
WHERE memories_fts MATCH $2 AND m.namespace_id = $1 AND m.action <> 'tombstone'
  AND m.key LIKE $5 ESCAPE '\'
  AND m.created_at = (SELECT MAX(created_at) FROM memories
                      WHERE namespace_id = m.namespace_id AND key = m.key)
  AND (m.expiration_date IS NULL OR m.expiration_date > $4)
ORDER BY rank
LIMIT $3`

const skillFTSPostgres = `
SELECT ` + factCols + `
FROM memories m
WHERE m.body_tsv @@ to_tsquery('english', $2)
  AND m.namespace_id = $1 AND m.action <> 'tombstone'
  AND m.key LIKE $5 ESCAPE '\'
  AND m.created_at = (SELECT MAX(created_at) FROM memories
                      WHERE namespace_id = m.namespace_id AND key = m.key)
  AND (m.expiration_date IS NULL OR m.expiration_date > $4)
ORDER BY ts_rank(m.body_tsv, to_tsquery('english', $2)) DESC, m.key ASC
LIMIT $3`

// skillFTSArm ranks the discovery documents matching query, newest live
// revision per key. An empty sanitized match or a missing namespace is a
// normal empty result.
func (s *Store) skillFTSArm(ctx context.Context, ns, query string, limit int) ([]Fact, error) {
	var q, match string
	switch s.db.Driver {
	case "sqlite":
		q, match = skillFTSSQLite, SanitizeFTS(query)
	case "postgres":
		q, match = skillFTSPostgres, SanitizeTSQuery(query)
	default:
		return nil, fmt.Errorf("memory: no FTS for driver %q", s.db.Driver)
	}
	if match == "" {
		return []Fact{}, nil
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Fact{}, nil
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(q),
		nsID, match, limit, store.TimeToDB(s.now()), likePrefix("/skills/"))
	if err != nil {
		return nil, fmt.Errorf("skill search query: %w", err)
	}
	defer rows.Close()
	return s.scanFacts(ctx, ns, nsID, rows)
}

// SearchSkills ranks active skill metadata for query: scoped FTS over
// the /skills/ collection, fused (RRF k=60) with a prefix-filtered
// vector arm when an embedder is wired. Without an embedder the lexical
// arm alone answers; "requires an embedder" never escapes discovery.
// Results are metadata-only; limit defaults to 10 and caps at
// SkillDiscoveryMaxHits.
func (s *Store) SearchSkills(ctx context.Context, ns, query string, limit int) ([]SkillMeta, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("memory: empty search query")
	}
	if limit <= 0 {
		limit = skillDiscoveryDefaultHits
	}
	if limit > SkillDiscoveryMaxHits {
		limit = SkillDiscoveryMaxHits
	}
	fts, err := s.skillFTSArm(ctx, ns, query, skillSearchCandidates)
	if err != nil {
		return nil, err
	}
	ranked := fts
	if s.embedder != nil {
		vec, err := s.VectorSearch(ctx, ns, query, skillSearchCandidates)
		if err != nil {
			return nil, fmt.Errorf("skill vector arm: %w", err)
		}
		skillVec := vec[:0]
		for _, f := range vec {
			if strings.HasPrefix(f.Key, "/skills/") {
				skillVec = append(skillVec, f)
			}
		}
		ranked = rrfFuseSkills(fts, skillVec)
	}
	out := []SkillMeta{}
	seen := map[string]bool{}
	for _, f := range ranked {
		if len(out) >= limit {
			break
		}
		m, ok := skillMetaFromFact(f)
		if !ok || !m.Active || seen[m.Name+"\x00"+m.Version] {
			continue
		}
		seen[m.Name+"\x00"+m.Version] = true
		out = append(out, m)
	}
	return out, nil
}

// rrfFuseSkills fuses two ranked lists (slice position = rank) with
// Reciprocal Rank Fusion at k=60, the canonical constant HybridSearch
// uses. Ties break by key ascending so output is deterministic.
func rrfFuseSkills(arms ...[]Fact) []Fact {
	const k = 60.0
	score := map[string]float64{}
	byID := map[string]Fact{}
	for _, arm := range arms {
		for rank, f := range arm {
			score[f.ID] += 1 / (k + float64(rank+1))
			byID[f.ID] = f
		}
	}
	out := make([]Fact, 0, len(byID))
	for _, f := range byID {
		out = append(out, f)
	}
	sort.Slice(out, func(a, b int) bool {
		sa, sb := score[out[a].ID], score[out[b].ID]
		if sa != sb {
			return sa > sb
		}
		return out[a].Key < out[b].Key
	})
	return out
}
