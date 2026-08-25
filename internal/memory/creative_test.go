package memory

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// fakeEmbedder is defined in memory_v2_test.go; reused here.

func TestCreativePass(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"postgres failover steps":   {1, 0, 0},
		"postgres failover runbook": {0.99, 0.1, 0},
		"lunch menu tuesday":        {0, 1, 0},
	}})
	ctx := context.Background()
	for k, b := range map[string]string{
		"/a": "postgres failover steps",
		"/b": "postgres failover runbook",
		"/c": "lunch menu tuesday",
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.CreativePass(ctx, "ns", 0.8, 20)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("added %d links, want 1 (/a<->/b only)", n)
	}
	// idempotent: second run adds nothing
	n, err = s.CreativePass(ctx, "ns", 0.8, 20)
	if err != nil || n != 0 {
		t.Fatalf("second pass added %d (err %v), want 0", n, err)
	}
	links, err := s.Neighbors(ctx, "ns", "/a", "out")
	if err != nil || len(links) != 1 || links[0].LinkType != "similar_to" || links[0].ToKey != "/b" {
		t.Fatalf("links = %v (err %v), want one similar_to /a->/b", links, err)
	}
}

// TestCreativePassClosedEdgeNotResurrected proves a user-closed similar_to
// pair does not burn CreativePass's maxLinks budget: the dedup scan must
// include closed edges (not just live ones), or every tick re-proposes the
// closed pair (a no-op ON CONFLICT DO NOTHING insert) and counts it toward
// added, starving a genuinely new pair of budget.
func TestCreativePassClosedEdgeNotResurrected(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"postgres failover steps":   {1, 0, 0},
		"postgres failover runbook": {0.99, 0.1, 0},
		"mysql failover steps":      {0, 1, 0},
		"mysql failover runbook":    {0, 0.99, 0.1},
	}})
	ctx := context.Background()
	// keys /a, /b sort ahead of /c, /d in CreativePass's scan order (key-
	// ascending, from liveVectorRows' ORDER BY m.key) - the position
	// where the pre-fix bug would burn the maxLinks=1 budget on the closed
	// pair before ever reaching the fresh one.
	for _, kv := range []struct{ k, b string }{
		{"/a", "postgres failover steps"},
		{"/b", "postgres failover runbook"},
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: kv.k, Body: kv.b}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CreativePass(ctx, "ns", 0.8, 20); err != nil || n != 1 {
		t.Fatalf("seed pass added %d (err %v), want 1 (/a<->/b)", n, err)
	}
	if _, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memory_links SET invalid_at = $1 WHERE namespace = $2 AND link_type = 'similar_to' AND from_key = $3 AND to_key = $4`),
		store.TimeToDB(time.Now()), "ns", "/a", "/b"); err != nil {
		t.Fatal(err)
	}

	for _, kv := range []struct{ k, b string }{
		{"/c", "mysql failover steps"},
		{"/d", "mysql failover runbook"},
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: kv.k, Body: kv.b}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.CreativePass(ctx, "ns", 0.8, 1)
	if err != nil {
		t.Fatal(err)
	}
	// n != 1 alone only checks a count, not which pair got linked; the
	// Neighbors assertion below is the discriminating check - it proves the
	// added link actually landed on the fresh /c<->/d pair rather than
	// somewhere else.
	if n != 1 {
		t.Fatalf("second pass added %d, want 1", n)
	}
	links, err := s.Neighbors(ctx, "ns", "/c", "out")
	if err != nil || len(links) != 1 || links[0].LinkType != "similar_to" || links[0].ToKey != "/d" {
		t.Fatalf("links from /c = %v (err %v), want one similar_to /c->/d - fresh pair must get linked", links, err)
	}

	// the closed /a<->/b pair must stay closed: no resurrection, no
	// duplicate row.
	var invalidAt sql.NullString
	var count int
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT invalid_at FROM memory_links WHERE namespace = $1 AND from_key = $2 AND to_key = $3 AND link_type = 'similar_to'`),
		"ns", "/a", "/b").Scan(&invalidAt); err != nil {
		t.Fatal(err)
	}
	if !invalidAt.Valid || invalidAt.String == "" {
		t.Fatal("closed /a->/b similar_to edge was resurrected (invalid_at cleared)")
	}
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT count(*) FROM memory_links WHERE namespace = $1 AND from_key = $2 AND to_key = $3 AND link_type = 'similar_to'`),
		"ns", "/a", "/b").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count for /a->/b similar_to = %d, want 1 (no duplicate)", count)
	}
}
