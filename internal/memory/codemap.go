package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// CodeMapPrefix is the key subtree every seeded architecture fact lives
// under. It is deliberately NOT /mental-models/: that tier is never
// superseded and outranks everything, which is right for a curated human
// synthesis and wrong for a map derived from the current source tree. A
// code map goes stale the moment the tree changes, and punk has no way to
// notice on its own - so it lives in the ordinary raw tier, where a
// re-seed can supersede it and a vanished domain can be tombstoned.
const CodeMapPrefix = "/code-map/"

// codeMapWriter is the author/writer stamped on every seeded fact, and the
// marker SeedCodeMap uses to decide which existing facts under
// CodeMapPrefix it owns. A fact a human wrote under the same prefix by
// hand is never tombstoned by a re-seed.
const codeMapWriter = "rinnegan-map"

// RinneganMap is the shape of `rinnegan map --json`, taken from the
// MapResult that Rinnegan's own map() returns (hypervisor-io/rinnegan,
// src/index.ts): the domain list each carrying its files, entrypoints and
// call-ranked top symbols, plus the weighted inter-domain edges.
//
// Only the fields this seeder actually renders are declared. Unknown
// fields decode away silently, so a Rinnegan release that adds to the
// payload does not break the seed; a release that RENAMES one of these
// shows up as an empty domain body, which the tests below pin.
type RinneganMap struct {
	Domains []RinneganDomain `json:"domains"`
	Edges   []RinneganEdge   `json:"edges"`
}

// RinneganDomain is one architectural domain: a named group of files with
// its entrypoints and the symbols most called into.
type RinneganDomain struct {
	Name        string           `json:"name"`
	Label       string           `json:"label"`
	Files       []string         `json:"files"`
	Entrypoints []string         `json:"entrypoints"`
	TopSymbols  []RinneganSymbol `json:"topSymbols"`
}

// RinneganSymbol is one entry of a domain's topSymbols list.
type RinneganSymbol struct {
	Name string `json:"name"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// RinneganEdge is one weighted dependency between two domains. from/to are
// the display names; Rinnegan also emits fromLabel/toLabel for routing,
// which this seeder does not need because it groups by display name the
// same way the human-facing renderers do.
type RinneganEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Weight int    `json:"weight"`
}

// SeedCodeMapStats reports what a seed changed, so the CLI can print a
// line that distinguishes a no-op re-seed from a real update.
type SeedCodeMapStats struct {
	Written   int // domains whose fact body changed (or is new)
	Unchanged int // domains whose fact body was already identical
	Removed   int // previously seeded domains that no longer exist
}

// SeedCodeMap reads a `rinnegan map --json` document from r and writes one
// fact per architectural domain into ns under CodeMapPrefix.
//
// This is the retroactive half of agent memory: hook capture only ever
// learns what happens from the moment it is wired up, so a repository with
// ten years of history starts empty. Rinnegan derives the current
// structure deterministically from the AST; this turns that structure into
// facts the context assembler can inject, so a freshly connected agent
// opens knowing the shape of the codebase.
//
// Re-seeding is delta-based, mirroring WriteDocument's contract:
//
//   - A domain whose rendered body is byte-identical to what is already
//     stored is skipped entirely - no revision, no outbox row, no embedding
//     churn. Re-running after a no-op commit costs nothing.
//   - A domain that changed is written as a new revision, superseding the
//     old one rather than accumulating a parallel fact.
//   - A domain that no longer exists is tombstoned, so a deleted or renamed
//     package stops being injected as if it were still there. Only facts
//     this seeder wrote (writer == codeMapWriter) are ever tombstoned; a
//     hand-written fact under the same prefix is left alone.
//
// The body is plain prose rather than JSON on purpose: it is retrieved by
// full-text and vector search and then read by a model, so it has to
// tokenize like documentation, not like a serialized struct.
// SeedCodeMapOpts carries optional seed metadata.
type SeedCodeMapOpts struct {
	// Revision is the repository revision the map was produced from
	// (git HEAD). Stored as the "revision" attribute so a later search
	// can flag a code-map fact as possibly stale when the caller's
	// revision differs.
	Revision string
}

func (s *Store) SeedCodeMap(ctx context.Context, ns string, r io.Reader) (SeedCodeMapStats, error) {
	return s.SeedCodeMapWith(ctx, ns, r, SeedCodeMapOpts{})
}

func (s *Store) SeedCodeMapWith(ctx context.Context, ns string, r io.Reader, o SeedCodeMapOpts) (SeedCodeMapStats, error) {
	var stats SeedCodeMapStats

	var m RinneganMap
	dec := json.NewDecoder(r)
	if err := dec.Decode(&m); err != nil {
		return stats, fmt.Errorf("memory: decode rinnegan map: %w", err)
	}
	if len(m.Domains) == 0 {
		return stats, fmt.Errorf("memory: rinnegan map has no domains (is the repository indexed?)")
	}

	// Depended-on-by direction is not in the payload; both directions are
	// useful in a body, so build the reverse index once here.
	dependsOn := map[string][]RinneganEdge{}
	dependedBy := map[string][]RinneganEdge{}
	for _, e := range m.Edges {
		if e.From == e.To {
			continue // a domain depending on itself carries no information
		}
		dependsOn[e.From] = append(dependsOn[e.From], e)
		dependedBy[e.To] = append(dependedBy[e.To], e)
	}

	existing, err := s.Recall(ctx, ns, CodeMapPrefix, 1000)
	if err != nil {
		return stats, err
	}
	type priorFact struct {
		body     string
		writer   string
		revision string
	}
	prior := make(map[string]priorFact, len(existing))
	for _, f := range existing {
		rev, _ := f.Attributes["revision"].(string)
		prior[f.Key] = priorFact{body: f.Body, writer: f.Writer, revision: rev}
	}

	seen := map[string]bool{}
	for _, d := range m.Domains {
		key := CodeMapPrefix + codeMapSlug(d.Name)
		if seen[key] {
			// Two domains whose names slug to the same key would silently
			// overwrite each other; keep the first and skip the rest rather
			// than writing a fact whose content depends on map ordering.
			continue
		}
		seen[key] = true

		body := renderDomain(d, dependsOn[d.Name], dependedBy[d.Name])
		if p, ok := prior[key]; ok && p.body == body && p.revision == o.Revision {
			stats.Unchanged++
			continue
		}
		attrs := map[string]any{
			"domain":      d.Name,
			"files":       len(d.Files),
			"entrypoints": len(d.Entrypoints),
		}
		if o.Revision != "" {
			attrs["revision"] = o.Revision
		}
		if _, err := s.Write(ctx, WriteInput{
			Namespace: ns,
			Key:       key,
			Body:      body,
			Author:    codeMapWriter,
			Writer:    codeMapWriter,
			SourceRef: "rinnegan",
			// Above the default so a domain summary outranks an incidental
			// raw fact when the context budget is tight, but well below a
			// curated mental model: this is derived, and derived data
			// should never win against something a human wrote.
			Importance: 0.6,
			Attributes: attrs,
		}); err != nil {
			return stats, err
		}
		stats.Written++
	}

	// Tombstone domains that were seeded before and are gone now.
	staleKeys := make([]string, 0)
	for key, p := range prior {
		if seen[key] || p.writer != codeMapWriter {
			continue
		}
		staleKeys = append(staleKeys, key)
	}
	sort.Strings(staleKeys) // deterministic order, so tests and logs are stable
	for _, key := range staleKeys {
		if err := s.Forget(ctx, ns, key, codeMapWriter); err != nil {
			return stats, err
		}
		stats.Removed++
	}
	return stats, nil
}

// renderDomain turns one domain into the prose body stored as a fact.
// Every list is truncated: a domain with 400 files must not produce a fact
// that eats the whole injected-context budget on its own.
func renderDomain(d RinneganDomain, dependsOn, dependedBy []RinneganEdge) string {
	var b strings.Builder
	label := d.Label
	if label == "" {
		label = d.Name
	}
	fmt.Fprintf(&b, "Architecture domain %q (%s): %d %s.", d.Name, label, len(d.Files), plural(len(d.Files), "file"))

	if len(d.Entrypoints) > 0 {
		fmt.Fprintf(&b, "\nEntrypoints: %s.", strings.Join(clipList(d.Entrypoints, 5), ", "))
	}
	if len(d.TopSymbols) > 0 {
		names := make([]string, 0, len(d.TopSymbols))
		for _, sym := range d.TopSymbols {
			names = append(names, fmt.Sprintf("%s (%s:%d)", sym.Name, sym.File, sym.Line))
		}
		fmt.Fprintf(&b, "\nMost-called symbols: %s.", strings.Join(clipList(names, 5), ", "))
	}
	if len(dependsOn) > 0 {
		fmt.Fprintf(&b, "\nDepends on: %s.", strings.Join(edgeList(dependsOn, func(e RinneganEdge) string { return e.To }), ", "))
	}
	if len(dependedBy) > 0 {
		fmt.Fprintf(&b, "\nDepended on by: %s.", strings.Join(edgeList(dependedBy, func(e RinneganEdge) string { return e.From }), ", "))
	}
	if len(d.Files) > 0 {
		fmt.Fprintf(&b, "\nFiles include: %s.", strings.Join(clipList(d.Files, 12), ", "))
	}
	return b.String()
}

// edgeList renders edges heaviest-first as "name (weight N)", tie-broken by
// name so the body is byte-stable across runs - the delta check above
// compares bodies, so an unstable render would rewrite every fact on every
// seed.
func edgeList(edges []RinneganEdge, pick func(RinneganEdge) string) []string {
	sorted := make([]RinneganEdge, len(edges))
	copy(sorted, edges)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Weight != sorted[j].Weight {
			return sorted[i].Weight > sorted[j].Weight
		}
		return pick(sorted[i]) < pick(sorted[j])
	})
	out := make([]string, 0, len(sorted))
	for _, e := range sorted {
		out = append(out, fmt.Sprintf("%s (weight %d)", pick(e), e.Weight))
	}
	return clipList(out, 8)
}

// plural returns word or its "s" plural, so a one-file domain does not
// render as "1 files" in text a model reads back as documentation.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// clipList caps a list at n entries, appending a count of what was left
// out so the body never implies it is exhaustive when it is not.
func clipList(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := make([]string, 0, n+1)
	out = append(out, items[:n]...)
	return append(out, fmt.Sprintf("and %d more", len(items)-n))
}

// codeMapSlug turns a domain name into a key segment: lowercase, with every
// run of characters outside [a-z0-9] collapsed to a single dash. Domain
// names come from directory names, so they can contain path separators,
// dots and spaces - none of which belong in a key. An empty result falls
// back to "unnamed" rather than producing the bare prefix, which would
// collide with every other unnameable domain.
func codeMapSlug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unnamed"
	}
	return out
}
