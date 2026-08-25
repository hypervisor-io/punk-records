package memory

import (
	"context"
	"strings"
	"testing"
)

// rinneganMapFixture is a two-domain map in the exact shape Rinnegan's
// map() returns (domains with files/entrypoints/topSymbols, plus weighted
// inter-domain edges). Kept as a literal document rather than built from
// the Go structs on purpose: this is the wire contract with another
// project, so the test has to exercise the DECODE, not just the render.
const rinneganMapFixture = `{
  "domains": [
    {
      "name": "memory",
      "label": "internal/memory",
      "files": ["internal/memory/memory.go", "internal/memory/links.go"],
      "entrypoints": ["internal/memory/memory.go"],
      "topSymbols": [
        {"name": "Store.Write", "file": "internal/memory/memory.go", "line": 188},
        {"name": "Store.Recall", "file": "internal/memory/memory.go", "line": 470}
      ]
    },
    {
      "name": "api",
      "label": "internal/api",
      "files": ["internal/api/agent_handlers.go"],
      "entrypoints": ["internal/api/server.go"],
      "topSymbols": [{"name": "AgentNamespace", "file": "internal/api/agent_handlers.go", "line": 56}]
    }
  ],
  "edges": [
    {"from": "api", "to": "memory", "fromLabel": "api", "toLabel": "memory", "weight": 12},
    {"from": "api", "to": "api", "fromLabel": "api", "toLabel": "api", "weight": 99}
  ]
}`

func seedFrom(t *testing.T, s *Store, ns, doc string) SeedCodeMapStats {
	t.Helper()
	stats, err := s.SeedCodeMap(context.Background(), ns, strings.NewReader(doc))
	if err != nil {
		t.Fatalf("SeedCodeMap: %v", err)
	}
	return stats
}

func codeMapBodies(t *testing.T, s *Store, ns string) map[string]string {
	t.Helper()
	facts, err := s.Recall(context.Background(), ns, CodeMapPrefix, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range facts {
		out[f.Key] = f.Body
	}
	return out
}

// TestSeedCodeMapWritesOneFactPerDomain pins the decode and the render:
// every field this seeder reads off Rinnegan's payload must reach the
// stored body, or a renamed upstream field would silently produce an empty
// summary that still looks like a successful seed.
func TestSeedCodeMapWritesOneFactPerDomain(t *testing.T) {
	s := newTestStore(t)
	stats := seedFrom(t, s, "ns", rinneganMapFixture)
	if stats.Written != 2 || stats.Unchanged != 0 || stats.Removed != 0 {
		t.Fatalf("stats = %+v, want 2 written", stats)
	}

	bodies := codeMapBodies(t, s, "ns")
	if len(bodies) != 2 {
		t.Fatalf("stored %d facts, want 2: %v", len(bodies), bodies)
	}

	mem, ok := bodies[CodeMapPrefix+"memory"]
	if !ok {
		t.Fatalf("no fact at %smemory: %v", CodeMapPrefix, bodies)
	}
	for _, want := range []string{
		"memory",                   // domain name
		"internal/memory",          // label
		"2 files",                  // file count
		"Store.Write",              // top symbol
		"memory.go:188",            // symbol location
		"Depended on by",           // reverse edge, derived not given
		"api (weight 12)",          // the edge's weight
		"internal/memory/links.go", // file list
	} {
		if !strings.Contains(mem, want) {
			t.Fatalf("memory domain body missing %q:\n%s", want, mem)
		}
	}

	api := bodies[CodeMapPrefix+"api"]
	if !strings.Contains(api, "Depends on: memory (weight 12)") {
		t.Fatalf("api domain body missing its forward edge:\n%s", api)
	}
	// A self-edge carries no information and must never be rendered.
	if strings.Contains(api, "api (weight 99)") {
		t.Fatalf("self-edge leaked into the body:\n%s", api)
	}
}

// TestSeedCodeMapProvenance pins that seeded facts are attributable and
// outrank an incidental raw fact without ever outranking a curated one.
func TestSeedCodeMapProvenance(t *testing.T) {
	s := newTestStore(t)
	seedFrom(t, s, "ns", rinneganMapFixture)

	facts, err := s.Recall(context.Background(), "ns", CodeMapPrefix, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Writer != codeMapWriter || f.Author != codeMapWriter {
			t.Fatalf("%s: writer/author = %q/%q, want %q", f.Key, f.Writer, f.Author, codeMapWriter)
		}
		if f.SourceRef != "rinnegan" {
			t.Fatalf("%s: source_ref = %q, want rinnegan", f.Key, f.SourceRef)
		}
		if f.Importance <= 0.5 || f.Importance >= 1.0 {
			t.Fatalf("%s: importance = %v, want above an ordinary fact and below a curated one", f.Key, f.Importance)
		}
		if f.Attributes["domain"] == nil {
			t.Fatalf("%s: attributes lost the domain name: %v", f.Key, f.Attributes)
		}
	}
}

// TestSeedCodeMapIsDeltaBased is the property that makes re-seeding on
// every commit affordable: an unchanged domain must not produce a revision,
// an outbox row, or an embedding job.
func TestSeedCodeMapIsDeltaBased(t *testing.T) {
	s := newTestStore(t)
	seedFrom(t, s, "ns", rinneganMapFixture)

	stats := seedFrom(t, s, "ns", rinneganMapFixture)
	if stats.Written != 0 || stats.Unchanged != 2 {
		t.Fatalf("re-seed of an identical map: %+v, want 0 written / 2 unchanged", stats)
	}

	// A changed domain rewrites only itself.
	changed := strings.Replace(rinneganMapFixture, `"line": 188`, `"line": 190`, 1)
	stats = seedFrom(t, s, "ns", changed)
	if stats.Written != 1 || stats.Unchanged != 1 {
		t.Fatalf("re-seed with one changed domain: %+v, want 1 written / 1 unchanged", stats)
	}
	if body := codeMapBodies(t, s, "ns")[CodeMapPrefix+"memory"]; !strings.Contains(body, "memory.go:190") {
		t.Fatalf("the changed domain kept its old body:\n%s", body)
	}
}

// TestSeedCodeMapTombstonesVanishedDomains covers the failure this whole
// delta path exists to prevent: a deleted package that keeps being injected
// into every session as though it still existed.
func TestSeedCodeMapTombstonesVanishedDomains(t *testing.T) {
	s := newTestStore(t)
	seedFrom(t, s, "ns", rinneganMapFixture)

	single := `{"domains":[{"name":"memory","label":"internal/memory","files":["internal/memory/memory.go"],
		"entrypoints":[],"topSymbols":[]}],"edges":[]}`
	stats := seedFrom(t, s, "ns", single)
	if stats.Removed != 1 {
		t.Fatalf("stats = %+v, want the vanished domain removed", stats)
	}
	bodies := codeMapBodies(t, s, "ns")
	if _, still := bodies[CodeMapPrefix+"api"]; still {
		t.Fatalf("the vanished domain is still live: %v", bodies)
	}
	if _, kept := bodies[CodeMapPrefix+"memory"]; !kept {
		t.Fatalf("the surviving domain was removed too: %v", bodies)
	}
}

// TestSeedCodeMapLeavesHandWrittenFactsAlone pins the ownership rule: the
// seeder tombstones only what it wrote. A human note filed under the same
// prefix must survive a re-seed that no longer mentions it.
func TestSeedCodeMapLeavesHandWrittenFactsAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{
		Namespace: "ns", Key: CodeMapPrefix + "hand-written",
		Body: "the scheduler package is deliberately undocumented", Author: "human", Writer: "human",
	}); err != nil {
		t.Fatal(err)
	}
	seedFrom(t, s, "ns", rinneganMapFixture)
	seedFrom(t, s, "ns", `{"domains":[{"name":"memory","files":["a.go"],"entrypoints":[],"topSymbols":[]}],"edges":[]}`)

	bodies := codeMapBodies(t, s, "ns")
	if _, kept := bodies[CodeMapPrefix+"hand-written"]; !kept {
		t.Fatalf("a hand-written fact was tombstoned by the seeder: %v", bodies)
	}
}

// TestSeedCodeMapRejectsUselessInput pins that a malformed document and an
// empty map are both errors rather than a silent zero-fact success, which
// would read as "seeded" in CI while injecting nothing.
func TestSeedCodeMapRejectsUselessInput(t *testing.T) {
	s := newTestStore(t)
	for name, doc := range map[string]string{
		"not json":     `{"domains":`,
		"no domains":   `{"domains":[],"edges":[]}`,
		"wrong shape":  `{"domains":"everything"}`,
		"empty stream": ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.SeedCodeMap(context.Background(), "ns", strings.NewReader(doc)); err == nil {
				t.Fatal("want an error, got a successful seed")
			}
		})
	}
}

// TestCodeMapSlug pins the key derivation: domain names come from directory
// names, so they carry separators and dots that must never reach a key.
func TestCodeMapSlug(t *testing.T) {
	cases := map[string]string{
		"memory":            "memory",
		"internal/memory":   "internal-memory",
		"src/lib.core":      "src-lib-core",
		"Some Domain":       "some-domain",
		"...":               "unnamed",
		"":                  "unnamed",
		"../../etc/passwd":  "etc-passwd",
		"trailing-dashes--": "trailing-dashes",
	}
	for in, want := range cases {
		if got := codeMapSlug(in); got != want {
			t.Errorf("codeMapSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSeedCodeMapClipsLongLists pins the context-budget guard: one domain
// with hundreds of files must not be able to fill the injected context on
// its own, and the body must say so rather than implying it is exhaustive.
func TestSeedCodeMapClipsLongLists(t *testing.T) {
	var files strings.Builder
	for i := 0; i < 200; i++ {
		if i > 0 {
			files.WriteString(",")
		}
		files.WriteString(`"pkg/file`)
		files.WriteString(strings.Repeat("x", 3))
		files.WriteString(string(rune('0' + i%10)))
		files.WriteString(`.go"`)
	}
	doc := `{"domains":[{"name":"big","files":[` + files.String() + `],"entrypoints":[],"topSymbols":[]}],"edges":[]}`

	s := newTestStore(t)
	seedFrom(t, s, "ns", doc)
	body := codeMapBodies(t, s, "ns")[CodeMapPrefix+"big"]
	if !strings.Contains(body, "and 188 more") {
		t.Fatalf("long file list was not clipped with a remainder count:\n%s", body)
	}
	if len(body) > 2000 {
		t.Fatalf("one domain rendered %d bytes; it would dominate the context budget", len(body))
	}
}
