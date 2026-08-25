package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/hypervisor-io/punk-records/internal/bus"
)

func TestProvenanceRoundTrip(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	_, err := s.Write(ctx, WriteInput{
		Namespace: "ns", Key: "/svc/db", Body: "pool leak confirmed",
		Author: "agent:database", Writer: "agent:database",
		TaskID: "", SourceRef: "incident:42", Confidence: 0.9,
	})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := s.Recall(ctx, "ns", "/svc", 0)
	if err != nil || len(facts) != 1 {
		t.Fatalf("recall: %v %v", facts, err)
	}
	f := facts[0]
	if f.Writer != "agent:database" || f.SourceRef != "incident:42" || f.Confidence < 0.89 || f.Confidence > 0.91 {
		t.Fatalf("provenance lost: %+v", f)
	}
	if f.ValidAt.IsZero() {
		t.Fatal("valid_at unset")
	}
}

func TestRecallAsOf(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()

	if _, err := s.Remember(ctx, "ns", "/k", "v1", nil, "a"); err != nil {
		t.Fatal(err)
	}
	t1 := clk.t // just after v1 write
	clk.t = clk.t.Add(time.Hour)
	if _, err := s.Remember(ctx, "ns", "/k", "v2", nil, "a"); err != nil {
		t.Fatal(err)
	}

	// as-of between writes sees v1
	facts, err := s.RecallAsOf(ctx, "ns", "/", t1.Add(time.Minute), 0)
	if err != nil || len(facts) != 1 || facts[0].Body != "v1" {
		t.Fatalf("asOf mid = %+v err=%v", facts, err)
	}
	// as-of now sees v2
	facts, err = s.RecallAsOf(ctx, "ns", "/", clk.t.Add(time.Minute), 0)
	if err != nil || len(facts) != 1 || facts[0].Body != "v2" {
		t.Fatalf("asOf now = %+v err=%v", facts, err)
	}
	// as-of before everything sees nothing
	facts, err = s.RecallAsOf(ctx, "ns", "/", t1.Add(-time.Hour), 0)
	if err != nil || len(facts) != 0 {
		t.Fatalf("asOf past = %+v err=%v", facts, err)
	}

	// tombstone closes the window
	clk.t = clk.t.Add(time.Hour)
	if err := s.Forget(ctx, "ns", "/k", "a"); err != nil {
		t.Fatal(err)
	}
	facts, err = s.RecallAsOf(ctx, "ns", "/", clk.t.Add(time.Minute), 0)
	if err != nil || len(facts) != 0 {
		t.Fatalf("asOf post-tombstone = %+v err=%v", facts, err)
	}
}

func TestOutboxDrainAndTailer(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()

	if _, err := s.Remember(ctx, "ns", "/a", "x", nil, "w"); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, "ns", "/a", "w"); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memory_outbox WHERE delivered_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Fatalf("outbox pending = %d, want 2", pending)
	}

	var got []OutboxEvent
	n, err := s.DrainOutbox(ctx, 10, func(e OutboxEvent) error { got = append(got, e); return nil })
	if err != nil || n != 2 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	if got[0].Key != "ns:/a" || got[0].Payload["action"] != "add" || got[1].Payload["action"] != "tombstone" {
		t.Fatalf("events = %+v", got)
	}
	// idempotent: nothing left
	n, err = s.DrainOutbox(ctx, 10, func(OutboxEvent) error { return nil })
	if err != nil || n != 0 {
		t.Fatalf("second drain n=%d", n)
	}

	// tailer end to end into the bus
	b := bus.New()
	events, cancel := b.Subscribe()
	defer cancel()
	tctx, tcancel := context.WithCancel(ctx)
	defer tcancel()
	go s.RunOutboxTailer(tctx, b, 10*time.Millisecond, slog.New(slog.DiscardHandler))
	if _, err := s.Remember(ctx, "ns", "/b", "y", nil, "w"); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-events:
		if e.Kind != "memory" || e.Key != "ns:/b" {
			t.Fatalf("bus event = %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tailer delivered nothing")
	}
}

func TestPerFactExpiry(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	exp := clk.t.Add(time.Hour)
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/temp", Body: "short-lived",
		Author: "w", Writer: "w", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "ns", "/keep", "long-lived", nil, "w"); err != nil {
		t.Fatal(err)
	}
	facts, err := s.Recall(ctx, "ns", "/", 0)
	if err != nil || len(facts) != 2 {
		t.Fatalf("pre-expiry recall = %d err=%v", len(facts), err)
	}
	clk.t = clk.t.Add(2 * time.Hour)
	facts, err = s.Recall(ctx, "ns", "/", 0)
	if err != nil || len(facts) != 1 || facts[0].Key != "/keep" {
		t.Fatalf("post-expiry recall = %+v err=%v", facts, err)
	}
	// sweep hard-deletes it
	n, err := s.SweepRetention(ctx, 365*24*time.Hour)
	if err != nil || n < 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
}

// fakeEmbedder maps known strings to fixed 3d vectors.
type fakeEmbedder struct{ m map[string][]float32 }

func (f *fakeEmbedder) Dims() int { return 3 }
func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := f.m[t]; ok {
			out[i] = v
		} else {
			out[i] = []float32{0.1, 0.1, 0.1}
		}
	}
	return out, nil
}

func TestVectorAndHybridSearch(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	fe := &fakeEmbedder{m: map[string][]float32{
		"replication lag spikes":     {1, 0, 0},
		"packet loss on edge router": {0, 1, 0},
		"lag":                        {0.95, 0.05, 0},
	}}
	s.SetEmbedder(fe)

	if _, err := s.Remember(ctx, "ns", "/db", "replication lag spikes", nil, "w"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "ns", "/net", "packet loss on edge router", nil, "w"); err != nil {
		t.Fatal(err)
	}

	got, err := s.VectorSearch(ctx, "ns", "lag", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Key != "/db" {
		t.Fatalf("vector top = %+v", got)
	}

	hyb, err := s.HybridSearch(ctx, "ns", "lag", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hyb) == 0 || hyb[0].Key != "/db" {
		t.Fatalf("hybrid top = %+v", hyb)
	}

	// backfill covers rows written without an embedder
	s.embedder = nil
	if _, err := s.Remember(ctx, "ns", "/later", "written without vectors", nil, "w"); err != nil {
		t.Fatal(err)
	}
	s.SetEmbedder(fe)
	n, err := s.BackfillEmbeddings(ctx, "ns", 10)
	if err != nil || n != 1 {
		t.Fatalf("backfill n=%d err=%v", n, err)
	}
}

func TestHybridRecencyBoost(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := context.Background()
	// two facts matching the same query, written far apart
	if _, err := s.Remember(ctx, "ns", "/old", "database timeout observed", nil, "w"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(90 * 24 * time.Hour)
	if _, err := s.Remember(ctx, "ns", "/new", "database timeout observed again", nil, "w"); err != nil {
		t.Fatal(err)
	}
	got, err := s.HybridSearch(ctx, "ns", "database timeout", 5, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || got[0].Key != "/new" {
		keys := []string{}
		for _, f := range got {
			keys = append(keys, f.Key)
		}
		t.Fatalf("recency order = %v, want /new first", strings.Join(keys, ","))
	}
}

func TestConcurrentDedup(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()

	// two agents append the identical fact to the same key
	f1, err := s.Write(ctx, WriteInput{Namespace: "repo", Key: "/finding/root-cause",
		Body: "pool leak in api-svc after 14:02 deploy", Writer: "agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := s.Write(ctx, WriteInput{Namespace: "repo", Key: "/finding/root-cause",
		Body: "pool leak in api-svc after 14:02 deploy", Writer: "agent-2"})
	if err != nil {
		t.Fatal(err)
	}
	// deduped: same live id, no second row
	if f1.ID != f2.ID {
		t.Fatalf("duplicate write created new revision: %s != %s", f1.ID, f2.ID)
	}
	var rows int
	if err := db.QueryRowContext(ctx, db.Rebind(
		`SELECT count(*) FROM memories m JOIN namespaces n ON n.id=m.namespace_id
		 WHERE n.name='repo' AND m.key='/finding/root-cause'`)).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1 (deduped)", rows)
	}

	// a genuinely different body DOES append
	if _, err := s.Write(ctx, WriteInput{Namespace: "repo", Key: "/finding/root-cause",
		Body: "actually a lock contention issue", Writer: "agent-3"}); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.Recall(ctx, "repo", "/finding", 0)
	if len(facts) != 1 || facts[0].Body != "actually a lock contention issue" {
		t.Fatalf("latest = %+v", facts)
	}
}

type fakeSummarizer struct{ seen int }

func (f *fakeSummarizer) Summarize(_ context.Context, facts []Fact) (string, error) {
	f.seen = len(facts)
	return "consolidated: " + itoa(len(facts)) + " facts", nil
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestConsolidateCompactAndSummarize(t *testing.T) {
	s, db, clk := newTest(t)
	ctx := context.Background()

	// churn a key: 3 revisions
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := s.Remember(ctx, "region", "/k", v, nil, "w"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Remember(ctx, "region", "/other", "kept", nil, "w"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(48 * time.Hour) // past the horizon

	sum := &fakeSummarizer{}
	res, err := s.Consolidate(ctx, "region", 24*time.Hour, sum)
	if err != nil {
		t.Fatal(err)
	}
	// v1, v2 superseded -> compacted; v3 + /other live
	if res.Compacted != 2 {
		t.Fatalf("compacted = %d, want 2", res.Compacted)
	}
	if res.Summarized != 1 || sum.seen != 2 {
		t.Fatalf("summarize = %d seen=%d", res.Summarized, sum.seen)
	}
	// live view intact + a consolidated fact
	facts, _ := s.Recall(ctx, "region", "/", 0)
	var haveConsolidated, haveK bool
	for _, f := range facts {
		if f.Key == "/consolidated/region" {
			haveConsolidated = true
		}
		if f.Key == "/k" && f.Body == "v3" {
			haveK = true
		}
	}
	if !haveConsolidated || !haveK {
		t.Fatalf("post-consolidate facts = %+v", facts)
	}
	var rows int
	_ = db.QueryRowContext(ctx, db.Rebind(`SELECT count(*) FROM memories m JOIN namespaces n ON n.id=m.namespace_id WHERE n.name='region' AND m.key='/k'`)).Scan(&rows)
	if rows != 1 {
		t.Fatalf("/k rows after compaction = %d, want 1", rows)
	}
}

func TestTypedLinks(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	for _, k := range []string{"/change/42", "/file/api.go", "/agent/database"} {
		if _, err := s.Remember(ctx, "repo", k, "x", nil, "w"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink(ctx, "repo", "/change/42", "/file/api.go", "relates_to"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "repo", "/change/42", "/agent/database", "derived_from"); err != nil {
		t.Fatal(err)
	}
	// idempotent
	if err := s.AddLink(ctx, "repo", "/change/42", "/file/api.go", "relates_to"); err != nil {
		t.Fatal(err)
	}

	out, err := s.Neighbors(ctx, "repo", "/change/42", "out")
	if err != nil || len(out) != 2 {
		t.Fatalf("out neighbors = %v err=%v", out, err)
	}
	in, err := s.Neighbors(ctx, "repo", "/file/api.go", "in")
	if err != nil || len(in) != 1 || in[0].FromKey != "/change/42" {
		t.Fatalf("in neighbors = %v err=%v", in, err)
	}
}
