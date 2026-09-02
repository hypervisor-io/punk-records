package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// TestDiagnoseDeterministicAcrossQuarantine proves Diagnose gives the same
// answer on repeated calls even when one of its own scans quarantines a
// corrupt row along the way. Reviewer repro: a corrupt row under an
// /observations/ key has a NULL embedding, so if MissingEmbeddings is
// counted before the observations scan quarantines the row, call 1 counts
// it as "missing an embedding" and call 2 (row already quarantined) does
// not - two different answers for the same namespace. Every counter that
// reads memories or memories_quarantine must run after every scan that can
// mutate those tables.
func TestDiagnoseDeterministicAcrossQuarantine(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()
	s.SetEmbedder(&fakeEmbedder{})

	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "x", Writer: "t"}); err != nil {
		t.Fatal(err)
	}

	var nsID int64
	if err := db.QueryRowContext(ctx, db.Rebind(`SELECT id FROM namespaces WHERE name = $1`), "ns").Scan(&nsID); err != nil {
		t.Fatal(err)
	}
	// poison a row directly under an observations key (bypassing the API,
	// like a bug or bad import) - reviewer's repro shape.
	if _, err := db.ExecContext(ctx, db.Rebind(
		`INSERT INTO memories (id, namespace_id, key, action, body, attributes, author, created_at)
		 VALUES ('poison1', $1, '/observations/bad', 'add', 'x', 'NOT-JSON{', '', $2)`),
		nsID, store.TimeToDB(time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}

	d1, err := s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if *d1 != *d2 {
		t.Fatalf("Diagnose not deterministic across quarantine: call1=%+v call2=%+v", *d1, *d2)
	}
	if d1.Quarantined != 1 {
		t.Fatalf("call1 quarantined = %d, want 1", d1.Quarantined)
	}
	if d2.Quarantined != 1 {
		t.Fatalf("call2 quarantined = %d, want 1", d2.Quarantined)
	}
}

func TestDiagnose(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "x", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	// orphan link: /a -> /gone (no live fact at /gone)
	if err := s.AddLink(ctx, "ns", "/a", "/gone", "relates_to"); err != nil {
		t.Fatal(err)
	}
	// stale observation: consolidated before a newer raw fact
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/o1", Body: "belief",
		Attributes: map[string]any{"consolidated_at": "2020-01-01T00:00:00.000000000Z"}, Writer: "c"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if d.OrphanLinks != 1 {
		t.Fatalf("orphan links = %d, want 1", d.OrphanLinks)
	}
	if d.StaleObservations != 1 {
		t.Fatalf("stale observations = %d, want 1", d.StaleObservations)
	}
	if d.MissingEmbeddings != -1 {
		t.Fatalf("no embedder: missing_embeddings = %d, want -1", d.MissingEmbeddings)
	}
	if d.Quarantined != 0 || d.ExpiredClaims != 0 {
		t.Fatalf("clean store: %+v", d)
	}

	// unknown namespace, no embedder: sentinel -1.
	d2, err := s.Diagnose(ctx, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if d2.MissingEmbeddings != -1 {
		t.Fatalf("unknown ns, no embedder: missing_embeddings = %d, want -1", d2.MissingEmbeddings)
	}
	if d2.OrphanLinks != 0 || d2.StaleObservations != 0 || d2.Quarantined != 0 || d2.ExpiredClaims != 0 {
		t.Fatalf("unknown ns: %+v, want all zero", d2)
	}

	// unknown namespace, embedder attached: 0, not the "no embedder"
	// sentinel - there are simply no facts to be missing an embedding.
	s.SetEmbedder(&fakeEmbedder{})
	d3, err := s.Diagnose(ctx, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if d3.MissingEmbeddings != 0 {
		t.Fatalf("unknown ns, embedder attached: missing_embeddings = %d, want 0", d3.MissingEmbeddings)
	}
}

// TestDiagnoseMissingEmbeddings proves MissingEmbeddings actually counts
// live facts with a NULL embedding, not just the sentinel/no-sentinel
// distinction: facts written before an embedder is attached stay
// unembedded, a fact written after does not.
func TestDiagnoseMissingEmbeddings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/pre1", Body: "before embedder", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/pre2", Body: "also before", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{"after embedder": {1, 0, 0}}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/post1", Body: "after embedder", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if d.MissingEmbeddings != 2 {
		t.Fatalf("missing embeddings = %d, want 2 (the two pre-embedder writes)", d.MissingEmbeddings)
	}
}

// TestDiagnoseExpiredClaims proves ExpiredClaims counts an expired,
// unreleased work claim.
func TestDiagnoseExpiredClaims(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(ctx, s.db.Rebind(
		`INSERT INTO work_claims (namespace, key, holder, claimed_at, expires_at, released_at) VALUES ($1,$2,$3,$4,$5,NULL)`),
		"ns", "/work/k1", "holder1", store.TimeToDB(past), store.TimeToDB(past)); err != nil {
		t.Fatal(err)
	}
	d, err := s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if d.ExpiredClaims != 1 {
		t.Fatalf("expired claims = %d, want 1", d.ExpiredClaims)
	}
}

// TestDiagnoseBeyondCap proves two false-positive/false-negative bugs from
// Recall's 1000-fact, alphabetical-by-key cap are gone:
//   - OrphanLinks must not flag a link endpoint that is live but sorts
//     past any capped namespace-wide scan (liveness now comes from
//     liveByKeys on the link's own keys, never a capped scan).
//   - StaleObservations must not miss an observation that sorts past a
//     namespace-wide cap, because it scans with an /observations/-scoped
//     Recall whose cap applies to observations alone.
//
// This requires enough filler facts to push both the link target and the
// observation past a namespace-wide 1000-cap; it is the expensive case in
// this file, guarded to skip under -short.
//
// The 999 filler count below is coupled to Recall's hardcoded 1000-fact
// cap (internal/memory/memory.go, the `limit > 1000` clamp in Recall) -
// change that cap and this fixture must change with it.
func TestDiagnoseBeyondCap(t *testing.T) {
	if testing.Short() {
		t.Skip("expensive fixture: 1000+ facts")
	}
	s := newTestStore(t)
	ctx := context.Background()

	// "/a" sorts first, "/m0000".."/m0998" sort next, "/observations/ostale"
	// sorts after all of those, "/zzz-target" sorts last of all. 1 + 999
	// facts exactly fill a namespace-wide 1000-cap ending at "/m0998", so a
	// namespace-wide capped scan would never reach the observation or the
	// link target.
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "x", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/zzz-target", Body: "x", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "ns", "/a", "/zzz-target", "relates_to"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 999; i++ {
		k := fmt.Sprintf("/m%04d", i)
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: "x", Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/observations/ostale", Body: "belief",
		Attributes: map[string]any{"consolidated_at": "2020-01-01T00:00:00.000000000Z"}, Writer: "c"}); err != nil {
		t.Fatal(err)
	}

	d, err := s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if d.OrphanLinks != 0 {
		t.Fatalf("orphan links = %d, want 0 (target is live but sorts past a namespace-wide 1000-cap)", d.OrphanLinks)
	}
	if d.StaleObservations != 1 {
		t.Fatalf("stale observations = %d, want 1 (sorts past a namespace-wide cap but within the observations-only cap)", d.StaleObservations)
	}
}

func TestDiagnoseCountsOversizeEmbeddings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/small", Body: "tiny"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/big", Body: strings.Repeat("w", 4*50)}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if d.OversizeEmbeddings != -1 {
		t.Fatalf("no embedder: OversizeEmbeddings = %d, want -1", d.OversizeEmbeddings)
	}
	s.SetEmbedder(&recordingEmbedder{dims: 2})
	s.SetEmbedMaxTokens(20)
	d, err = s.Diagnose(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if d.OversizeEmbeddings != 1 {
		t.Fatalf("OversizeEmbeddings = %d, want 1", d.OversizeEmbeddings)
	}
}
