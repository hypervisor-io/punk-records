package memory

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

func TestNameSimilarity(t *testing.T) {
	// leading-token / prefix extensions: still merge.
	if s := nameSimilarity("Alice", "Alice Chen"); s < 0.85 {
		t.Fatalf(`nameSimilarity("Alice", "Alice Chen") = %v, want >= 0.85`, s)
	}
	if s := nameSimilarity("John", "John Smith"); s < 0.85 {
		t.Fatalf(`nameSimilarity("John", "John Smith") = %v, want >= 0.85`, s)
	}
	if s := nameSimilarity("Paris", "Paris Hilton"); s < 0.85 {
		t.Fatalf(`nameSimilarity("Paris", "Paris Hilton") = %v, want >= 0.85`, s)
	}
	// trailing-token matches: must NOT merge (the false-merge bug).
	if s := nameSimilarity("Alice", "Bob Alice"); s >= 0.85 {
		t.Fatalf(`nameSimilarity("Alice", "Bob Alice") = %v, want < 0.85`, s)
	}
	if s := nameSimilarity("John", "Elton John"); s >= 0.85 {
		t.Fatalf(`nameSimilarity("John", "Elton John") = %v, want < 0.85`, s)
	}
	if s := nameSimilarity("Alice", "Alicia"); s >= 0.85 {
		t.Fatalf(`nameSimilarity("Alice", "Alicia") = %v, want < 0.85`, s)
	}
	if s := nameSimilarity("Alice", "Alice"); s != 1.0 {
		t.Fatalf(`nameSimilarity("Alice", "Alice") = %v, want 1.0`, s)
	}
	if s := nameSimilarity("Bob", "Alice"); s >= 0.5 {
		t.Fatalf(`nameSimilarity("Bob", "Alice") = %v, want low`, s)
	}
}

func TestEntitySlug(t *testing.T) {
	cases := map[string]string{
		"Alice Chen":   "alice-chen",
		"OpenAI, Inc.": "openai-inc",
		"  spaced  ":   "spaced",
	}
	for in, want := range cases {
		if got := EntitySlug(in); got != want {
			t.Fatalf("EntitySlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeExtractor returns a fixed entity list regardless of input body.
type fakeExtractor struct{ names []string }

func (f fakeExtractor) Extract(_ context.Context, _ string) ([]string, error) {
	return f.names, nil
}

func TestEnrichEntities(t *testing.T) {
	s := newTestStore(t)
	s.SetEntityExtractor(fakeExtractor{names: []string{"Alice", "Google"}})
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact", Body: "Alice works at Google"}); err != nil {
		t.Fatal(err)
	}

	n, err := s.EnrichEntities(ctx, "ns", "/fact")
	if err != nil || n != 2 {
		t.Fatalf("EnrichEntities = %d, %v; want 2, nil", n, err)
	}

	alice, err := s.liveByKeys(ctx, "ns", []string{"/entities/alice"})
	if err != nil || len(alice) != 1 {
		t.Fatalf("recall /entities/alice: %v %v", alice, err)
	}
	if alice[0].Attributes["mention_count"].(float64) != 1 {
		t.Fatalf("alice mention_count = %v, want 1", alice[0].Attributes["mention_count"])
	}
	if alice[0].Body != "Alice" {
		t.Fatalf("alice body = %q, want %q", alice[0].Body, "Alice")
	}
	google, err := s.liveByKeys(ctx, "ns", []string{"/entities/google"})
	if err != nil || len(google) != 1 {
		t.Fatalf("recall /entities/google: %v %v", google, err)
	}
	if google[0].Attributes["mention_count"].(float64) != 1 {
		t.Fatalf("google mention_count = %v, want 1", google[0].Attributes["mention_count"])
	}

	links, err := s.Neighbors(ctx, "ns", "/fact", "out")
	if err != nil {
		t.Fatal(err)
	}
	var sawAlice, sawGoogle bool
	for _, l := range links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/alice" {
			sawAlice = true
		}
		if l.LinkType == "mentions" && l.ToKey == "/entities/google" {
			sawGoogle = true
		}
	}
	if !sawAlice || !sawGoogle {
		t.Fatalf("links = %v, want mentions to both /entities/alice and /entities/google", links)
	}

	// co_occurs must be discoverable from EITHER entity (symmetric relation,
	// same guarantee as similar_to) — query from both the smaller key
	// (alice) and the alphabetically larger key (google).
	coWeightFrom := func(from, to string) float64 {
		links, err := s.Neighbors(ctx, "ns", from, "out")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range links {
			if l.LinkType == "co_occurs" && l.ToKey == to {
				return l.Weight
			}
		}
		return -1
	}
	if w := coWeightFrom("/entities/alice", "/entities/google"); w != 1 {
		t.Fatalf("co_occurs weight from alice = %v, want 1", w)
	}
	if w := coWeightFrom("/entities/google", "/entities/alice"); w != 1 {
		t.Fatalf("co_occurs weight from google = %v, want 1", w)
	}

	// a second contributing fact (new mentions on this fact key) bumps
	// co_occurs in both directions to 2.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact2", Body: "Alice works at Google"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/fact2"); err != nil {
		t.Fatal(err)
	}
	if w := coWeightFrom("/entities/alice", "/entities/google"); w != 2 {
		t.Fatalf("co_occurs weight from alice after second fact = %v, want 2", w)
	}
	if w := coWeightFrom("/entities/google", "/entities/alice"); w != 2 {
		t.Fatalf("co_occurs weight from google after second fact = %v, want 2", w)
	}

	// re-run of an already-processed fact is idempotent: no double-count in
	// either direction.
	n, err = s.EnrichEntities(ctx, "ns", "/fact")
	if err != nil || n != 2 {
		t.Fatalf("second EnrichEntities = %d, %v; want 2, nil", n, err)
	}
	alice2, err := s.liveByKeys(ctx, "ns", []string{"/entities/alice"})
	if err != nil || len(alice2) != 1 || alice2[0].Attributes["mention_count"].(float64) != 2 {
		t.Fatalf("alice mention_count after re-run = %v %v, want 2", alice2, err)
	}
	if w := coWeightFrom("/entities/alice", "/entities/google"); w != 2 {
		t.Fatalf("co_occurs weight from alice after re-run = %v, want still 2 (not double-incremented)", w)
	}
	if w := coWeightFrom("/entities/google", "/entities/alice"); w != 2 {
		t.Fatalf("co_occurs weight from google after re-run = %v, want still 2 (not double-incremented)", w)
	}
}

// TestEnrichEntitiesAliasMerge: "Alice" (fact1) and "Alice Chen" (fact2) are
// aliases of the same person - they must merge into ONE /entities/ node
// rather than the old slug-exact behavior creating /entities/alice-chen as
// a duplicate.
func TestEnrichEntitiesAliasMerge(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	s.SetEntityExtractor(fakeExtractor{names: []string{"Alice"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact1", Body: "Alice joined the call"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/fact1"); err != nil {
		t.Fatal(err)
	}

	s.SetEntityExtractor(fakeExtractor{names: []string{"Alice Chen"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact2", Body: "Alice Chen filed the report"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/fact2"); err != nil {
		t.Fatal(err)
	}

	entities, err := s.Recall(ctx, "ns", "/entities", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 {
		t.Fatalf("entities = %v, want exactly 1 (alias should merge)", entities)
	}
	if entities[0].Key != "/entities/alice" {
		t.Fatalf("entity key = %q, want /entities/alice", entities[0].Key)
	}
	if entities[0].Attributes["mention_count"].(float64) != 2 {
		t.Fatalf("mention_count = %v, want 2", entities[0].Attributes["mention_count"])
	}

	if dup, err := s.liveByKeys(ctx, "ns", []string{"/entities/alice-chen"}); err != nil || len(dup) != 0 {
		t.Fatalf("/entities/alice-chen should not exist: %v %v", dup, err)
	}

	links1, err := s.Neighbors(ctx, "ns", "/fact1", "out")
	if err != nil {
		t.Fatal(err)
	}
	links2, err := s.Neighbors(ctx, "ns", "/fact2", "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, links := range [][]Link{links1, links2} {
		var sawMerged bool
		for _, l := range links {
			if l.LinkType == "mentions" && l.ToKey == "/entities/alice" {
				sawMerged = true
			}
		}
		if !sawMerged {
			t.Fatalf("expected a mentions link to /entities/alice, got %v", links)
		}
	}
}

// TestEnrichEntitiesTrailingTokenNotMerged: a fact mentions "Bob Alice", then
// a later fact mentions bare "Alice". Alice is the TRAILING token of
// "Bob Alice", not the leading one, so this must NOT be treated as an alias
// - it's the false-merge case the prefix-only containment rule guards
// against ("John" must not silently merge into "Elton John").
func TestEnrichEntitiesTrailingTokenNotMerged(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	s.SetEntityExtractor(fakeExtractor{names: []string{"Bob Alice"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact1", Body: "Bob Alice joined the call"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/fact1"); err != nil {
		t.Fatal(err)
	}

	s.SetEntityExtractor(fakeExtractor{names: []string{"Alice"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact2", Body: "Alice filed the report"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/fact2"); err != nil {
		t.Fatal(err)
	}

	entities, err := s.Recall(ctx, "ns", "/entities", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 2 {
		t.Fatalf("entities = %v, want exactly 2 (trailing-token match must not merge)", entities)
	}
}

// TestEnrichEntitiesDistinctNotMerged: "Alice" and "Bob" are unrelated names
// and must stay as two separate entities.
func TestEnrichEntitiesDistinctNotMerged(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	s.SetEntityExtractor(fakeExtractor{names: []string{"Alice", "Bob"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact", Body: "Alice and Bob met"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/fact"); err != nil {
		t.Fatal(err)
	}
	entities, err := s.Recall(ctx, "ns", "/entities", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 2 {
		t.Fatalf("entities = %v, want 2 (distinct names must not merge)", entities)
	}
}

func TestEnrichEntitiesNoExtractor(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact", Body: "Alice works at Google"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.EnrichEntities(ctx, "ns", "/fact")
	if err != nil || n != 0 {
		t.Fatalf("EnrichEntities with no extractor = %d, %v; want 0, nil", n, err)
	}
	entities, err := s.Recall(ctx, "ns", "/entities", 10)
	if err != nil || len(entities) != 0 {
		t.Fatalf("entities = %v, %v; want none", entities, err)
	}
}

// recordingObserver records every fact it is asked to consolidate, so the
// test can assert entity facts never reach the raw pool.
type recordingObserver struct{ seen []Fact }

func (r *recordingObserver) Observe(_ context.Context, facts []Fact) ([]Observation, error) {
	r.seen = append(r.seen, facts...)
	return nil, nil
}

// TestEntityArmSurfacesMentionedFacts: a fact whose body has zero textual
// overlap with the query must still surface via the entity arm when it is
// linked to an entity ("mentions") that fuzzy-matches the query token -
// the graph-stream analogue of TestEntityArmSilentWithoutEntities below.
func TestEntityArmSurfacesMentionedFacts(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	// fact whose body does NOT contain the query token
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/n1", Body: "decided to defer the migration", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	// entity + mentions edge, as the enricher would write them
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/alice-chen", Body: "Alice Chen",
		Attributes: map[string]any{"mention_count": 1.0}, Writer: "enricher"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLinkWeighted(ctx, "ns", "/notes/n1", "/entities/alice-chen", "mentions", 1.0); err != nil {
		t.Fatal(err)
	}

	// distractor: an unrelated entity + its own mentioning fact, neither of
	// which has any textual/vector overlap with "alice" - and whose name
	// does not clear entityAliasThreshold against the query token either -
	// so the test proves the entity arm surfaces the RIGHT fact rather than
	// just anything reachable via a mentions edge.
	if sim := nameSimilarity("alice", "Bogdan Chen"); sim >= entityAliasThreshold {
		t.Fatalf("test fixture invalid: nameSimilarity(%q,%q) = %v clears entityAliasThreshold (%v)",
			"alice", "Bogdan Chen", sim, entityAliasThreshold)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/n2", Body: "renewed the office lease", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/bogdan-chen", Body: "Bogdan Chen",
		Attributes: map[string]any{"mention_count": 1.0}, Writer: "enricher"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLinkWeighted(ctx, "ns", "/notes/n2", "/entities/bogdan-chen", "mentions", 1.0); err != nil {
		t.Fatal(err)
	}

	hits, err := s.HybridSearchScored(ctx, "ns", "alice", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Key == "/notes/n2" {
			t.Fatalf("distractor fact /notes/n2 (mentions Bogdan Chen, not a match for 'alice') must not surface: %+v", h)
		}
		if h.Key == "/notes/n1" {
			found = true
			if h.Components["entity"] <= 0 {
				t.Fatalf("entity component missing: %+v", h.Components)
			}
		}
	}
	if !found {
		t.Fatal("fact mentioning Alice must surface for query 'alice' despite zero textual match")
	}
}

// TestEntityArmSilentWithoutEntities: a namespace with no /entities/ facts
// must produce a search result set byte-identical to the pre-entity-arm
// shape - the "entity" component key must be entirely absent, not present
// at zero, mirroring how "bridge" is only ever set for candidates that
// actually cross the >=2-seed threshold.
func TestEntityArmSilentWithoutEntities(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "plain fact about pools", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.HybridSearchScored(ctx, "ns", "pools", 10, 0)
	if err != nil || len(hits) != 1 {
		t.Fatal(err, hits)
	}
	if _, ok := hits[0].Components["entity"]; ok {
		t.Fatal("no entities in namespace: entity component must be absent")
	}
}

func TestEntitiesExcludedFromObservationPool(t *testing.T) {
	s := newTestStore(t)
	s.SetEntityExtractor(fakeExtractor{names: []string{"Alice"}})
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact", Body: "Alice is on call"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/fact"); err != nil {
		t.Fatal(err)
	}
	if entities, err := s.Recall(ctx, "ns", "/entities", 10); err != nil || len(entities) != 1 {
		t.Fatalf("expected 1 entity fact present before consolidation: %v %v", entities, err)
	}

	rec := &recordingObserver{}
	if _, err := s.ConsolidateObservations(ctx, "ns", rec); err != nil {
		t.Fatal(err)
	}
	for _, f := range rec.seen {
		if hasPrefix(f.Key, "/entities/") {
			t.Fatalf("entity fact %q leaked into the raw observation pool", f.Key)
		}
	}
}

// TestBumpCoOccursStampsValidAt proves bumpCoOccurs stamps valid_at with
// the same instant as created_at on a freshly-created co_occurs edge (same
// guarantee as AddLinkDescribed).
func TestBumpCoOccursStampsValidAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.bumpCoOccurs(ctx, "ns", "/entities/a", "/entities/b"); err != nil {
		t.Fatal(err)
	}
	var validAt, createdAt sql.NullString
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT valid_at, created_at FROM memory_links WHERE namespace = $1 AND from_key = $2 AND to_key = $3 AND link_type = 'co_occurs'`),
		"ns", "/entities/a", "/entities/b").Scan(&validAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	if !validAt.Valid || validAt.String == "" {
		t.Fatal("bumpCoOccurs must stamp valid_at")
	}
	if validAt.String != createdAt.String {
		t.Fatalf("valid_at = %q, want == created_at %q", validAt.String, createdAt.String)
	}
}

// TestEnrichEntitiesClosedMentionsEdgeNotReattempted proves a closed
// mentions edge still counts toward EnrichEntities' idempotency guard - the
// dedup set must be built from ALL outgoing mentions edges (live or
// closed), not just live ones, or a closed edge gets silently re-created on
// every re-enrich: a fresh mention_count bump AND a fresh co_occurs weight
// bump each time, mirroring TestEnrichKeyClosedTargetNotReattempted.
func TestEnrichEntitiesClosedMentionsEdgeNotReattempted(t *testing.T) {
	s := newTestStore(t)
	s.SetEntityExtractor(fakeExtractor{names: []string{"Alice", "Google"}})
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/fact", Body: "Alice works at Google"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.EnrichEntities(ctx, "ns", "/fact"); err != nil || n != 2 {
		t.Fatalf("seed EnrichEntities = %d, %v; want 2, nil", n, err)
	}

	coWeight := func() float64 {
		links, err := s.Neighbors(ctx, "ns", "/entities/alice", "out")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range links {
			if l.LinkType == "co_occurs" && l.ToKey == "/entities/google" {
				return l.Weight
			}
		}
		return -1
	}
	mentionCount := func() float64 {
		alice, err := s.liveByKeys(ctx, "ns", []string{"/entities/alice"})
		if err != nil || len(alice) != 1 {
			t.Fatalf("recall /entities/alice: %v %v", alice, err)
		}
		return alice[0].Attributes["mention_count"].(float64)
	}
	if mentionCount() != 1 {
		t.Fatalf("mention_count after seed = %v, want 1", mentionCount())
	}
	if coWeight() != 1 {
		t.Fatalf("co_occurs weight after seed = %v, want 1", coWeight())
	}

	// close the /fact -> /entities/alice mentions edge.
	if _, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memory_links SET invalid_at = $1 WHERE namespace = $2 AND from_key = $3 AND to_key = $4 AND link_type = 'mentions'`),
		store.TimeToDB(time.Now()), "ns", "/fact", "/entities/alice"); err != nil {
		t.Fatal(err)
	}

	// re-enrich twice: the closed mentions edge must not be re-attempted, so
	// mention_count and the live co_occurs weight must both stay unchanged.
	for i := 0; i < 2; i++ {
		if _, err := s.EnrichEntities(ctx, "ns", "/fact"); err != nil {
			t.Fatal(err)
		}
		if mentionCount() != 1 {
			t.Fatalf("mention_count after re-enrich %d = %v, want still 1 (closed mentions edge must not be re-attempted)", i+1, mentionCount())
		}
		if coWeight() != 1 {
			t.Fatalf("co_occurs weight after re-enrich %d = %v, want still 1 (closed mentions edge must not be re-attempted)", i+1, coWeight())
		}
	}
}

// TestEntityArmRanksByLinkWeight proves entityArm's final rank sort
// actually drives HybridSearchScored's RRF, not just entityArm's own
// return value: two facts each mention a DIFFERENT matched entity via a
// "mentions" edge, the entities have IDENTICAL bodies so they get the
// SAME fuzzy-match score against the query - isolating link weight
// (1.0 vs 0.3) as the only thing that can separate the two facts' rank.
// Neither fact's body shares a token with the query and no embedder is
// configured, so fts/vector contribute nothing to either candidate;
// salience (recency/importance/access/feedback/reinforce) is identical
// for both since they're written back to back under the same frozen
// test clock. That isolation is what makes this test lethal to a
// reversed entityArm sort: reversing flips which fact's rank/RRF
// fraction is larger, which flips the resulting Score order, so the
// assertion below flips too - a plain "some ordering exists" check
// would survive the mutation, this one does not.
func TestEntityArmRanksByLinkWeight(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/high", Body: "unrelated passage about pools", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/notes/low", Body: "distinct passage about lakes", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	// Two separate entity nodes with the IDENTICAL body "Alice" -> identical
	// nameSimilarity score against the query "alice", so matched[e1] ==
	// matched[e2] exactly. Written directly (not via EnrichEntities), so
	// the alias-merge path never runs and both keys stay distinct.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/e-high", Body: "Alice",
		Attributes: map[string]any{"mention_count": 1.0}, Writer: "enricher"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/e-low", Body: "Alice",
		Attributes: map[string]any{"mention_count": 1.0}, Writer: "enricher"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLinkWeighted(ctx, "ns", "/notes/high", "/entities/e-high", "mentions", 1.0); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLinkWeighted(ctx, "ns", "/notes/low", "/entities/e-low", "mentions", 0.3); err != nil {
		t.Fatal(err)
	}

	// White-box: entityArm's own return order must already put the
	// high-weight fact first.
	ent, err := s.entityArm(ctx, "ns", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(ent) != 2 || ent[0].Key != "/notes/high" || ent[1].Key != "/notes/low" {
		t.Fatalf("entityArm order = %v, want [/notes/high, /notes/low] (weight 1.0 fact must rank first)", ent)
	}

	// Black-box: the ordering must survive through to the public search's
	// RRF fusion, where entityArm's rank sets the "entity" component
	// weight (1/(60+rank)).
	hits, err := s.HybridSearchScored(ctx, "ns", "alice", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	idx := map[string]int{}
	for i, h := range hits {
		idx[h.Key] = i
	}
	hi, hiOK := idx["/notes/high"]
	lo, loOK := idx["/notes/low"]
	if !hiOK || !loOK {
		t.Fatalf("expected both /notes/high and /notes/low in results, got %+v", hits)
	}
	if hi >= lo {
		t.Fatalf("search order: /notes/high at %d, /notes/low at %d; want high-weight fact ranked strictly above low-weight fact", hi, lo)
	}
}

// TestEntityArmEmptyQuerySkipsRecall proves entityArm's tokenless-query
// guard runs BEFORE the /entities Recall, not just before the fuzzy-match
// loop - i.e. it actually skips the up-to-1000-row Recall rather than
// running it and discarding the result. HybridSearchScored's own blank-
// query check (Store.Search errors "memory: empty search query" before
// entityArm is ever called) already makes the guard unreachable via a
// literal blank/whitespace query end-to-end, so this exercises entityArm
// directly. The namespace has a live entity, and the store's DB
// connection is closed before the call: if the guard were missing (or
// placed after the Recall), entityArm would try to query the closed DB
// and return an error: only running Recall AFTER a successful tokenless
// bail-out, i.e. never running it at all, produces (nil, nil) here.
func TestEntityArmEmptyQuerySkipsRecall(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/x", Body: "X",
		Attributes: map[string]any{"mention_count": 1.0}, Writer: "enricher"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	facts, err := s.entityArm(ctx, "ns", "")
	if err != nil {
		t.Fatalf(`entityArm(ns, "") = _, %v; want nil error (guard must return before the closed DB is touched)`, err)
	}
	if facts != nil {
		t.Fatalf(`entityArm(ns, "") = %v; want nil`, facts)
	}
}

// TestBumpCoOccursClosedEdgeNotResurrected proves a user-closed co_occurs
// edge stops accumulating weight: the ON CONFLICT DO UPDATE weight bump
// must be gated on invalid_at IS NULL, or a closed edge keeps drifting
// upward every time the same entity pair co-occurs again.
func TestBumpCoOccursClosedEdgeNotResurrected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.bumpCoOccurs(ctx, "ns", "/entities/a", "/entities/b"); err != nil {
		t.Fatal(err)
	}

	// close both directions (bumpCoOccurs writes a row each way).
	now := store.TimeToDB(time.Now())
	for _, pair := range [][2]string{{"/entities/a", "/entities/b"}, {"/entities/b", "/entities/a"}} {
		if _, err := s.db.ExecContext(ctx, s.db.Rebind(
			`UPDATE memory_links SET invalid_at = $1 WHERE namespace = $2 AND from_key = $3 AND to_key = $4 AND link_type = 'co_occurs'`),
			now, "ns", pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.bumpCoOccurs(ctx, "ns", "/entities/a", "/entities/b"); err != nil {
		t.Fatal(err)
	}

	for _, pair := range [][2]string{{"/entities/a", "/entities/b"}, {"/entities/b", "/entities/a"}} {
		var weight float64
		var invalidAt sql.NullString
		if err := s.db.QueryRowContext(ctx, s.db.Rebind(
			`SELECT weight, invalid_at FROM memory_links WHERE namespace = $1 AND from_key = $2 AND to_key = $3 AND link_type = 'co_occurs'`),
			"ns", pair[0], pair[1]).Scan(&weight, &invalidAt); err != nil {
			t.Fatal(err)
		}
		if weight != 1 {
			t.Fatalf("weight for %s->%s = %v, want still 1 (closed edge must not accumulate)", pair[0], pair[1], weight)
		}
		if !invalidAt.Valid || invalidAt.String == "" {
			t.Fatalf("edge %s->%s was resurrected (invalid_at cleared)", pair[0], pair[1])
		}
	}
}

// batchingExtractor implements both Extract and ExtractBatch, counting
// calls to each so tests can pin which path ran.
type batchingExtractor struct {
	perBody      map[string][]string
	batchCalls   int
	extractCalls int
	miscount     bool // answer one list too few, forcing the fallback
}

func (b *batchingExtractor) Extract(_ context.Context, body string) ([]string, error) {
	b.extractCalls++
	return b.perBody[body], nil
}

func (b *batchingExtractor) ExtractBatch(_ context.Context, bodies []string) ([][]string, error) {
	b.batchCalls++
	out := make([][]string, 0, len(bodies))
	for _, body := range bodies {
		out = append(out, b.perBody[body])
	}
	if b.miscount && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, nil
}

// TestEnrichEntitiesBatch pins the batched path: one ExtractBatch call
// covers every key, entities and mentions land per fact, and /entities/
// keys are never fed back in.
func TestEnrichEntitiesBatch(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "alice ships postgres"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "bob ships redis"}); err != nil {
		t.Fatal(err)
	}
	ex := &batchingExtractor{perBody: map[string][]string{
		"alice ships postgres": {"Alice", "Postgres"},
		"bob ships redis":      {"Bob", "Redis"},
	}}
	s.SetEntityExtractor(ex)

	n, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/a", "/b", "/entities/ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("applied = %d, want 4", n)
	}
	if ex.batchCalls != 1 || ex.extractCalls != 0 {
		t.Fatalf("batch=%d extract=%d, want 1/0", ex.batchCalls, ex.extractCalls)
	}
	ents, err := s.ListEntities(ctx, "ns")
	if err != nil || len(ents) != 4 {
		t.Fatalf("entities: %d %v", len(ents), err)
	}
}

// TestEnrichEntitiesBatchMiscountFallsBack pins the safety property: a
// batch answer with the wrong number of lists degrades to per-fact
// Extract calls instead of misattributing entities across facts.
func TestEnrichEntitiesBatchMiscountFallsBack(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "alice ships postgres"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "bob ships redis"}); err != nil {
		t.Fatal(err)
	}
	ex := &batchingExtractor{miscount: true, perBody: map[string][]string{
		"alice ships postgres": {"Alice"},
		"bob ships redis":      {"Bob"},
	}}
	s.SetEntityExtractor(ex)

	n, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/a", "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("applied = %d, want 2", n)
	}
	if ex.batchCalls != 1 || ex.extractCalls != 2 {
		t.Fatalf("batch=%d extract=%d, want 1 batch then 2 fallback extracts", ex.batchCalls, ex.extractCalls)
	}
}

// TestEnrichEntitiesBatchSingleUsesExtract pins that a one-fact batch
// skips the batching ceremony and calls plain Extract.
func TestEnrichEntitiesBatchSingleUsesExtract(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "alice ships postgres"}); err != nil {
		t.Fatal(err)
	}
	ex := &batchingExtractor{perBody: map[string][]string{"alice ships postgres": {"Alice"}}}
	s.SetEntityExtractor(ex)
	if _, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/a"}); err != nil {
		t.Fatal(err)
	}
	if ex.batchCalls != 0 || ex.extractCalls != 1 {
		t.Fatalf("batch=%d extract=%d, want 0/1", ex.batchCalls, ex.extractCalls)
	}
}
