package registry

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

const agentV1 = `---
name: database
version: 0.1.0
description: db specialist
triggers:
  - source: acme
---
prompt v1
`

const agentV2 = `---
name: database
version: 0.2.0
description: db specialist
triggers:
  - source: acme
---
prompt v2
`

const agentBroken = `---
name: database
description: no version, no triggers
---
prompt
`

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func testLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestLoadAndVersionPersistence(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "agents", "database.md"), agentV1)
	db := testDB(t)
	r := New(dir, db, testLog())
	ctx := context.Background()

	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	a, verID, ok := r.Get("database")
	if !ok || a.Version != "0.1.0" || verID == 0 {
		t.Fatalf("Get = %+v ver=%d ok=%v", a, verID, ok)
	}

	// same content: no new version row
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	_, verID2, _ := r.Get("database")
	if verID2 != verID {
		t.Fatalf("identical content produced new version id %d != %d", verID2, verID)
	}

	// changed content: new row, new id
	write(t, filepath.Join(dir, "agents", "database.md"), agentV2)
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	a3, verID3, _ := r.Get("database")
	if a3.Version != "0.2.0" || verID3 == verID {
		t.Fatalf("update not versioned: %+v id=%d", a3, verID3)
	}

	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM agent_versions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("agent_versions rows = %d, want 2", rows)
	}
}

func TestLoadRejectsBadTreeKeepsOldSnapshot(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "agents", "database.md"), agentV1)
	r := New(dir, nil, testLog())
	ctx := context.Background()
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	v1 := r.Current().Version

	write(t, filepath.Join(dir, "agents", "database.md"), agentBroken)
	if err := r.Load(ctx); err == nil {
		t.Fatal("broken spec accepted")
	}
	if r.Current().Version != v1 {
		t.Fatal("snapshot swapped despite validation failure")
	}
	if a, _, _ := r.Get("database"); a.Version != "0.1.0" {
		t.Fatalf("old spec lost: %+v", a)
	}
}

func waitVersion(t *testing.T, r *Registry, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := r.Current(); s != nil && s.Version >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("snapshot never reached version %d (at %d)", want, r.Current().Version)
}

func TestWatchHotReload(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "agents", "database.md"), agentV1)
	r := New(dir, nil, testLog())
	r.Debounce = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	reload := make(chan struct{}, 1)
	go func() { _ = r.Watch(ctx, reload) }()
	time.Sleep(100 * time.Millisecond) // watcher arming

	// edit -> new snapshot
	write(t, filepath.Join(dir, "agents", "database.md"), agentV2)
	waitVersion(t, r, 2)
	if a, _, _ := r.Get("database"); a.Version != "0.2.0" {
		t.Fatalf("hot reload lost edit: %+v", a)
	}

	// bad edit -> rejected, snapshot stays
	prev := r.Current().Version
	write(t, filepath.Join(dir, "agents", "database.md"), agentBroken)
	time.Sleep(300 * time.Millisecond) // debounce + reload attempt
	if r.Current().Version != prev {
		t.Fatal("bad edit swapped snapshot")
	}
	if a, _, _ := r.Get("database"); a.Version != "0.2.0" {
		t.Fatalf("bad edit corrupted active spec: %+v", a)
	}

	// manual reload channel (SIGHUP path) after fixing the file
	write(t, filepath.Join(dir, "agents", "database.md"), agentV1)
	time.Sleep(300 * time.Millisecond) // let fs debounce settle first
	reload <- struct{}{}
	waitVersion(t, r, prev+1)
}
