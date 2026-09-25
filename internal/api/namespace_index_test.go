package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

type nsIndexOut struct {
	Namespaces []struct {
		Name  string `json:"name"`
		Facts int    `json:"facts"`
		Tasks int    `json:"tasks"`
	} `json:"namespaces"`
}

func TestNamespaceIndex(t *testing.T) {
	s, _, _ := brainTestServer(t)
	for _, w := range []struct{ ns, key, body string }{
		{"board", "/tasks/A", "first task"},
		{"board", "/tasks/A/status", "done: landed"},
		{"notes", "/notes/one", "a note"},
	} {
		rr := do(t, s, http.MethodPost, "/v1/namespaces/"+w.ns+"/memories",
			`{"key":"`+w.key+`","body":"`+w.body+`","author":"tester"}`)
		if rr.Code != http.StatusCreated {
			t.Fatalf("remember %s = %d: %s", w.key, rr.Code, rr.Body)
		}
	}

	var all nsIndexOut
	rr := do(t, s, http.MethodGet, "/v1/namespaces", "")
	if err := json.Unmarshal(rr.Body.Bytes(), &all); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("index = %d %v: %s", rr.Code, err, rr.Body)
	}
	if len(all.Namespaces) != 2 || all.Namespaces[0].Name != "board" || all.Namespaces[1].Name != "notes" {
		t.Fatalf("index = %+v; want board then notes", all.Namespaces)
	}
	if all.Namespaces[0].Tasks != 1 || all.Namespaces[0].Facts != 2 {
		t.Fatalf("board = %+v; want 2 facts, 1 task", all.Namespaces[0])
	}

	var tasksOnly nsIndexOut
	rr = do(t, s, http.MethodGet, "/v1/namespaces?tasks=1", "")
	if err := json.Unmarshal(rr.Body.Bytes(), &tasksOnly); err != nil {
		t.Fatal(err)
	}
	if len(tasksOnly.Namespaces) != 1 || tasksOnly.Namespaces[0].Name != "board" {
		t.Fatalf("tasks=1 = %+v; want board only", tasksOnly.Namespaces)
	}
}

// TestNamespaceIndexAuthz: /v1/namespaces spans namespaces, so the A01
// path hook cannot gate it - the handler filters rows by the verified
// subject's read grant instead, and an unauthenticated request is 401
// like every other /v1 route once a key exists.
func TestNamespaceIndexAuthz(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "nsindex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	mem := memory.New(db, now)
	keys := NewKeys(db, now)
	az := authz.New(db, nil)
	keys.SetAuthorizer(az)
	s := New(testLogger(), Deps{Memory: mem, Keys: keys})

	for _, ns := range []string{"ns-a", "ns-b"} {
		if _, err := mem.Remember(ctx, ns, "/tasks/A", "a task", nil, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	token, err := keys.Create(ctx, "alice-key", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := az.Grant(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}

	get := func(tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/namespaces", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, r)
		return rec
	}
	if code := get("").Code; code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", code)
	}
	rec := get(token)
	if rec.Code != http.StatusOK {
		t.Fatalf("index = %d: %s", rec.Code, rec.Body)
	}
	var out nsIndexOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Namespaces) != 1 || out.Namespaces[0].Name != "ns-a" {
		t.Fatalf("index = %+v; want ns-a only (no grant on ns-b)", out.Namespaces)
	}
}
