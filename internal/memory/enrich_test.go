package memory

import (
	"database/sql"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

func TestEnrichKeyLinksNearNeighbors(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"ceph osd flapping on node 3":  {1, 0, 0},
		"osd flap traced to nic reset": {0.98, 0.15, 0},
		"tuesday lunch menu":           {0, 1, 0},
	}})
	ctx := t.Context()
	for k, b := range map[string]string{
		"/inc/1": "ceph osd flapping on node 3",
		"/inc/2": "osd flap traced to nic reset",
		"/misc":  "tuesday lunch menu",
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	// a pre-existing authored link of a different type must not suppress
	// the similar_to edge enrichment would otherwise add.
	if err := s.AddLink(ctx, "ns", "/inc/1", "/inc/2", "relates_to"); err != nil {
		t.Fatal(err)
	}
	n, err := s.EnrichKey(ctx, "ns", "/inc/1", 0.75, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("added %d links, want 1", n)
	}
	links, err := s.Neighbors(ctx, "ns", "/inc/1", "out")
	if err != nil {
		t.Fatal(err)
	}
	var sawSimilar bool
	for _, l := range links {
		if l.ToKey == "/inc/2" && l.LinkType == "similar_to" {
			sawSimilar = true
		}
	}
	if !sawSimilar {
		t.Fatalf("links = %v, want a similar_to edge to /inc/2", links)
	}
	// idempotent
	n, err = s.EnrichKey(ctx, "ns", "/inc/1", 0.75, 2)
	if err != nil || n != 0 {
		t.Fatalf("second enrich added %d (err %v), want 0", n, err)
	}
}

// TestEnrichKeyClosedTargetNotReattempted proves a closed similar_to target
// does not consume EnrichKey's topN budget: the idempotency set must
// include closed edges (not just live ones), or every re-enrich re-proposes
// the closed target (a no-op insert) instead of spending the budget on a
// genuinely new candidate.
func TestEnrichKeyClosedTargetNotReattempted(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"ceph osd flapping on node 3":  {1, 0, 0},
		"osd flap traced to nic reset": {0.99, 0.15, 0}, // highest similarity to /inc/1
		"ceph osd reset event":         {0.9, 0.3, 0},   // second-highest, within threshold
	}})
	ctx := t.Context()
	for _, kv := range []struct{ k, b string }{
		{"/inc/1", "ceph osd flapping on node 3"},
		{"/inc/2", "osd flap traced to nic reset"},
		{"/inc/3", "ceph osd reset event"},
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: kv.k, Body: kv.b}); err != nil {
			t.Fatal(err)
		}
	}

	// topN=1: first enrich picks the single best candidate, /inc/2.
	n, err := s.EnrichKey(ctx, "ns", "/inc/1", 0.75, 1)
	if err != nil || n != 1 {
		t.Fatalf("seed enrich = %d, %v; want 1", n, err)
	}
	links, err := s.Neighbors(ctx, "ns", "/inc/1", "out")
	if err != nil || len(links) != 1 || links[0].ToKey != "/inc/2" {
		t.Fatalf("links after seed enrich = %v (err %v), want one similar_to /inc/1->/inc/2", links, err)
	}

	// close the /inc/1 -> /inc/2 edge.
	if _, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memory_links SET invalid_at = $1 WHERE namespace = $2 AND from_key = $3 AND to_key = $4 AND link_type = 'similar_to'`),
		store.TimeToDB(time.Now()), "ns", "/inc/1", "/inc/2"); err != nil {
		t.Fatal(err)
	}

	// re-enrich with the same topN=1 budget: the closed /inc/2 target must
	// not be re-attempted, so the budget goes to /inc/3 instead.
	n, err = s.EnrichKey(ctx, "ns", "/inc/1", 0.75, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-enrich added %d, want 1 (/inc/1->/inc/3) - budget must not be burned by the closed /inc/2 target", n)
	}
	links, err = s.Neighbors(ctx, "ns", "/inc/1", "out")
	if err != nil {
		t.Fatal(err)
	}
	var sawInc3, sawInc2 bool
	for _, l := range links {
		if l.LinkType == "similar_to" && l.ToKey == "/inc/3" {
			sawInc3 = true
		}
		if l.LinkType == "similar_to" && l.ToKey == "/inc/2" {
			sawInc2 = true
		}
	}
	if !sawInc3 {
		t.Fatalf("links after re-enrich = %v, want a similar_to edge to /inc/3", links)
	}
	if sawInc2 {
		t.Fatalf("links after re-enrich = %v, closed /inc/2 target must not resurface as a live edge", links)
	}

	// the closed /inc/1 -> /inc/2 edge must stay closed.
	var invalidAt sql.NullString
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT invalid_at FROM memory_links WHERE namespace = $1 AND from_key = $2 AND to_key = $3 AND link_type = 'similar_to'`),
		"ns", "/inc/1", "/inc/2").Scan(&invalidAt); err != nil {
		t.Fatal(err)
	}
	if !invalidAt.Valid || invalidAt.String == "" {
		t.Fatal("closed /inc/1->/inc/2 similar_to edge was resurrected (invalid_at cleared)")
	}
}

func TestEnrichKeyNoEmbedderIsNoop(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Write(t.Context(), WriteInput{Namespace: "ns", Key: "/k", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.EnrichKey(t.Context(), "ns", "/k", 0, 0)
	if err != nil || n != 0 {
		t.Fatalf("no-embedder enrich = %d, %v; want 0, nil", n, err)
	}
}
