package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func authServer(t *testing.T) (*Server, *Keys) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	keys := NewKeys(db, now)
	s := New(testLogger(), Deps{Memory: memory.New(db, now), Keys: keys})
	return s, keys
}

func TestAuthBootstrapThenEnforced(t *testing.T) {
	s, keys := authServer(t)
	ctx := context.Background()

	// bootstrap: zero keys -> open
	rr := do(t, s, http.MethodGet, "/v1/namespaces/ns/keys", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d, want 200", rr.Code)
	}

	token, err := keys.Create(ctx, "acme", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "prk_") {
		t.Fatalf("token format: %q", token)
	}

	// enforced now
	rr = do(t, s, http.MethodGet, "/v1/namespaces/ns/keys", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rr.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/ns/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token = %d: %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/namespaces/ns/keys", nil)
	req.Header.Set("Authorization", "Bearer prk_wrong")
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", rec.Code)
	}

	// health stays open
	rr = do(t, s, http.MethodGet, "/healthz", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz behind auth: %d", rr.Code)
	}

	// revoke closes the door
	if err := keys.Revoke(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/namespaces/ns/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	// zero active keys again -> bootstrap reopens (documented; first key
	// via CLI). The revoked token itself must not validate as a key.
	if rec.Code != http.StatusOK {
		t.Fatalf("post-revoke bootstrap = %d", rec.Code)
	}
	ok, _, err := keys.Check(ctx, token)
	if err != nil || ok {
		t.Fatalf("revoked token Check = %v err=%v", ok, err)
	}
}

func TestIntakeRateLimit(t *testing.T) {
	s := taskServer(t)
	over := 0
	for i := 0; i < 60; i++ {
		rr := do(t, s, http.MethodPost, "/v1/intake/webhook",
			`{"source":"x","external_ref":"r`+string(rune('a'+i%26))+`"}`)
		if rr.Code == http.StatusTooManyRequests {
			over++
		}
	}
	if over == 0 {
		t.Fatal("60 rapid intakes never rate limited (burst 40)")
	}
}
