package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// pipelineServer mirrors testServer but keeps the DB handle (to seed run
// rows) and mounts the P01 status route like cmdServe does.
func pipelineServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	s := New(testLogger(), Deps{Memory: memory.New(db, now)})
	s.MountPipeline()
	return s, db
}

func seedRun(t *testing.T, db *store.DB, ns, stage, key, rev, status string, attempts int, items *int64, errClass string, started, finished *time.Time) {
	t.Helper()
	var itemsAny, errAny, startedAny, finishedAny any
	if items != nil {
		itemsAny = *items
	}
	if errClass != "" {
		errAny = errClass
	}
	if started != nil {
		startedAny = store.TimeToDB(*started)
	}
	if finished != nil {
		finishedAny = store.TimeToDB(*finished)
	}
	if _, err := db.ExecContext(context.Background(), db.Rebind(`
		INSERT INTO memory_pipeline_runs
			(namespace, stage, stage_version, source_key, source_revision, status, attempts, items, error_class, created_at, started_at, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`),
		ns, stage, 1, key, rev, status, attempts, itemsAny, errAny,
		store.TimeToDB(time.Date(2026, 7, 6, 0, 0, 1, 0, time.UTC)), startedAny, finishedAny); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineStatusEndpoint(t *testing.T) {
	s, db := pipelineServer(t)
	one := int64(1)
	started := time.Date(2026, 7, 6, 0, 0, 1, 0, time.UTC)
	finished := started.Add(1500 * time.Millisecond)
	seedRun(t, db, "ns", "embed_link", "/a", "rev-1", "succeeded", 1, &one, "", &started, &finished)
	seedRun(t, db, "ns", "entities", "/b", "rev-2", "failed", 3, nil, "model", &started, &finished)
	seedRun(t, db, "other", "entities", "/c", "rev-3", "pending", 0, nil, "", nil, nil)

	rr := do(t, s, http.MethodGet, "/v1/namespaces/ns/pipeline", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET pipeline = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "sk-") || strings.Contains(rr.Body.String(), `"error"`) {
		t.Fatalf("response leaks raw error text: %s", rr.Body.String())
	}
	var runs []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %v, want 2 (other namespace excluded)", runs)
	}
	// newest first
	if runs[0]["stage"] != "entities" || runs[1]["stage"] != "embed_link" {
		t.Fatalf("order = %v, want id DESC", runs)
	}
	failed := runs[0]
	if failed["status"] != "failed" || failed["error_class"] != "model" || failed["attempts"].(float64) != 3 {
		t.Fatalf("failed run = %v", failed)
	}
	if failed["duration_ms"].(float64) != 1500 {
		t.Fatalf("duration_ms = %v, want 1500", failed["duration_ms"])
	}
	ok := runs[1]
	if ok["status"] != "succeeded" || ok["items"].(float64) != 1 || ok["source_revision"] != "rev-1" {
		t.Fatalf("succeeded run = %v", ok)
	}
	if _, present := ok["error_class"]; present {
		t.Fatalf("succeeded run carries error_class: %v", ok)
	}

	rr = do(t, s, http.MethodGet, "/v1/namespaces/ns/pipeline?stage=entities", "")
	var ent []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &ent); err != nil || len(ent) != 1 {
		t.Fatalf("stage filter = %v, %v; want 1", ent, err)
	}
	rr = do(t, s, http.MethodGet, "/v1/namespaces/ns/pipeline?status=succeeded", "")
	var suc []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &suc); err != nil || len(suc) != 1 || suc[0]["stage"] != "embed_link" {
		t.Fatalf("status filter = %v, %v; want the succeeded embed_link run", suc, err)
	}
}

// TestPipelineStatusAuthz: the status endpoint sits behind the same A02
// namespace gate as every /v1/namespaces/{ns} route - read grant required.
func TestPipelineStatusAuthz(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "authz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	keys := NewKeys(db, now)
	az := authz.New(db, nil)
	keys.SetAuthorizer(az)
	s := New(testLogger(), Deps{Memory: memory.New(db, now), Keys: keys})
	s.MountPipeline()

	token, err := keys.Create(ctx, "alice-key", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := az.Grant(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	seedRun(t, db, "ns-a", "embed_link", "/a", "rev-1", "succeeded", 1, nil, "", nil, nil)

	get := func(path, tok string) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, r)
		return rec.Code
	}
	if code := get("/v1/namespaces/ns-b/pipeline", token); code != http.StatusForbidden {
		t.Fatalf("ns-b pipeline = %d, want 403 (no grant)", code)
	}
	if code := get("/v1/namespaces/ns-a/pipeline", token); code != http.StatusOK {
		t.Fatalf("ns-a pipeline = %d, want 200 (read grant)", code)
	}
	if code := get("/v1/namespaces/ns-a/pipeline", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", code)
	}
}
