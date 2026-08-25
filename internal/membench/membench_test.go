package membench

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// newBenchStore opens an in-memory sqlite store, migrates it up, and
// wraps it in a memory.Store — the same wiring cmdMembench uses.
func newBenchStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "membench.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return memory.New(db, nil)
}

func TestRunScoresRecallAndMRR(t *testing.T) {
	s := newBenchStore(t)
	recs := []Record{
		{Type: "fact", Key: "/svc/db", Body: "postgres primary lives on host7"},
		{Type: "fact", Key: "/svc/cache", Body: "redis cache cluster on host9"},
		// SQLite FTS5 MATCH ANDs every token together with no stopword
		// removal, so a query padded with "where/is/the" can never hit a
		// short fact body; keep the query to content words present in it.
		{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}},
		{Type: "query", Q: "entirely unrelated flamingo census", Expect: []string{"/svc/cache"}},
	}
	res, err := Run(t.Context(), s, "bench", recs, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queries != 2 {
		t.Fatalf("Queries = %d, want 2", res.Queries)
	}
	if res.RecallAtK != 0.5 { // first query hits, flamingo query cannot
		t.Fatalf("RecallAtK = %v, want 0.5", res.RecallAtK)
	}
	if res.MRR != 0.5 { // first query: expected key at rank 1 => 1.0; second: 0
		t.Fatalf("MRR = %v, want 0.5", res.MRR)
	}
}

func TestLoadParsesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.jsonl")
	content := `{"type":"fact","key":"/svc/db","body":"postgres primary on host7"}
{"type":"query","q":"where is postgres","expect":["/svc/db"]}

`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("len(recs) = %d, want 2 (blank line must be skipped)", len(recs))
	}
	fact := recs[0]
	if fact.Type != "fact" || fact.Key != "/svc/db" || fact.Body != "postgres primary on host7" {
		t.Fatalf("fact record = %+v, want type=fact key=/svc/db body=%q", fact, "postgres primary on host7")
	}
	query := recs[1]
	if query.Type != "query" || query.Q != "where is postgres" || len(query.Expect) != 1 || query.Expect[0] != "/svc/db" {
		t.Fatalf("query record = %+v, want type=query q=%q expect=[/svc/db]", query, "where is postgres")
	}
}
