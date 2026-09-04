package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// brainTestServer is testServer plus a region store and a clock the test
// can read, so members and claims show up in the snapshot.
func brainTestServer(t *testing.T) (*Server, *memory.Store, *region.Store) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The clock starts at the real now (not a fixed date) because the
	// snapshot's writes_5m window is measured from time.Now(); writes
	// stamped hours in the past would fall outside it.
	var mu sync.Mutex
	clk := time.Now().UTC()
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	}
	mem := memory.New(db, now)
	reg := region.New(db, nil)
	s := New(testLogger(), Deps{Memory: mem, Region: reg})
	s.version = "vtest"
	return s, mem, reg
}

func TestBrainSnapshot(t *testing.T) {
	s, mem, reg := brainTestServer(t)
	ctx := context.Background()

	for _, w := range []struct{ ns, key, body string }{
		{"quiet", "/notes/a", "one"},
		{"busy", "/tasks/T1", "task"},
		{"busy", "/tasks/T2", "task"},
		{"busy", "/tasks/T1/status", "done: x"},
	} {
		if _, err := mem.Write(ctx, memory.WriteInput{Namespace: w.ns, Key: w.key, Body: w.body}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Ensure(ctx, "busy", "busy"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, "busy", "worker-1", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ClaimWork(ctx, "busy", "/tasks/T2", "worker-1", time.Hour); err != nil {
		t.Fatal(err)
	}

	rec := do(t, s, http.MethodGet, "/v1/brain/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var snap brainSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != "vtest" || snap.Now == "" {
		t.Fatalf("header: %+v", snap)
	}
	if len(snap.Namespaces) != 2 || snap.Namespaces[0].Name != "busy" {
		t.Fatalf("order: %+v", snap.Namespaces)
	}
	b := snap.Namespaces[0]
	if b.Facts != 3 || b.Tasks != (memory.TaskCounts{Total: 2, Done: 1, Pending: 1}) {
		t.Fatalf("busy counts: %+v", b)
	}
	if len(b.Members) != 1 || b.Members[0].Agent != "worker-1" {
		t.Fatalf("members: %+v", b.Members)
	}
	if len(b.Claims) != 1 || b.Claims[0].Key != "/tasks/T2" {
		t.Fatalf("claims: %+v", b.Claims)
	}
	if b.Writes5m != 3 || b.LastWriteAt == "" {
		t.Fatalf("activity: %+v", b)
	}
	q := snap.Namespaces[1]
	if q.Members == nil || q.Claims == nil {
		t.Fatalf("quiet lists must be empty, not null: %+v", q)
	}
}

func TestBrainSnapshotWithoutRegionStore(t *testing.T) {
	s := testServer(t)
	rec := do(t, s, http.MethodGet, "/v1/brain/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var snap brainSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Namespaces == nil {
		t.Fatal("namespaces must be [] not null")
	}
}
