package api

import (
	"context"
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

func authServer(t *testing.T) (*Server, *Keys, *store.DB) {
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
	return s, keys, db
}

func TestAuthBootstrapThenEnforced(t *testing.T) {
	s, keys, _ := authServer(t)
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
	ok, _, _, err := keys.Check(ctx, token)
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

func TestForgedSubjectHeaderIsReplaced(t *testing.T) {
	s, keys, _ := authServer(t)
	ctx := context.Background()
	token, err := keys.Create(ctx, "test-key", "real-subject")
	if err != nil {
		t.Fatal(err)
	}

	// probe handler mounted behind the auth middleware reads the header
	// the middleware is supposed to own
	var got string
	probe := http.NewServeMux()
	probe.Handle("/probe", s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Punk-Subject")
	})))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Punk-Subject", "forged-attacker")
	probe.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe = %d", rec.Code)
	}
	if got != "real-subject" {
		t.Fatalf("subject = %q, want real-subject (forged value must be replaced)", got)
	}
}

func authedGet(t *testing.T, s *Server, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// Red proof: with enforcement wired (config authz.enforcement=deny), a
// subject holding only namespace-A read is denied namespace B and denied
// writes to A.
func TestNamespaceAuthzEnforcement(t *testing.T) {
	s, keys, db := authServer(t)
	ctx := context.Background()
	az := authz.New(db, nil)
	keys.SetAuthorizer(az)

	token, err := keys.Create(ctx, "alice-key", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := az.Grant(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}

	if rr := authedGet(t, s, token, "/v1/namespaces/ns-a/keys"); rr.Code != http.StatusOK {
		t.Fatalf("alice read ns-a = %d, want 200: %s", rr.Code, rr.Body)
	}
	if rr := authedGet(t, s, token, "/v1/namespaces/ns-b/keys"); rr.Code != http.StatusForbidden {
		t.Fatalf("alice read ns-b = %d, want 403", rr.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/namespaces/ns-a/memories",
		strings.NewReader(`{"key":"/k","body":"v"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("alice write ns-a = %d, want 403 (read grant must not write)", rec.Code)
	}

	// revocation takes effect on the next request
	if err := az.Revoke(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	if rr := authedGet(t, s, token, "/v1/namespaces/ns-a/keys"); rr.Code != http.StatusForbidden {
		t.Fatalf("post-revoke read ns-a = %d, want 403", rr.Code)
	}
}

// Enabled mode denies an empty verified subject: a key without a subject
// claim authenticates but holds no grants.
func TestNamespaceAuthzEmptySubjectDenied(t *testing.T) {
	s, keys, db := authServer(t)
	ctx := context.Background()
	keys.SetAuthorizer(authz.New(db, nil))

	token, err := keys.Create(ctx, "no-subject", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr := authedGet(t, s, token, "/v1/namespaces/ns-a/keys"); rr.Code != http.StatusForbidden {
		t.Fatalf("empty subject = %d, want 403", rr.Code)
	}
}

// Enabled mode denies the zero-key bootstrap on namespaced routes: no
// keys means no verified subject, and deny-by-default gives the empty
// subject nothing. First key and grants are provisioned locally (CLI),
// never through an unauthenticated HTTP path.
func TestNamespaceAuthzZeroKeyBootstrapDenied(t *testing.T) {
	s, keys, db := authServer(t)
	keys.SetAuthorizer(authz.New(db, nil))

	rr := do(t, s, http.MethodGet, "/v1/namespaces/ns-a/keys", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bootstrap with enforcement = %d, want 403", rr.Code)
	}
}

// Compatibility mode: with no authorizer wired (authz.enforcement=off,
// the default), the current trusted behavior is unchanged - any valid
// key reaches any namespace.
func TestNamespaceAuthzDisabledCompat(t *testing.T) {
	s, keys, _ := authServer(t)
	ctx := context.Background()
	token, err := keys.Create(ctx, "trusted", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rr := authedGet(t, s, token, "/v1/namespaces/ns-b/keys"); rr.Code != http.StatusOK {
		t.Fatalf("enforcement-off read = %d, want 200: %s", rr.Code, rr.Body)
	}
}

// Enabled-mode enforcement authorizes exactly the namespace the handler
// resolves. chi matches the {ns} parameter against the escaped request
// path and never percent-decodes it, so "ns-a%2Fns-b" and "%6Es-a"
// reach the memory store as those literal names - namespaces distinct
// from the granted "ns-a" whose decoded aliases they resemble.
// Regression (A02 round-2 review): enforcement used to check the
// decoded URL.Path while the handlers read the raw chi parameter, so
// /v1/namespaces/ns-a%2Fns-b/memories and /v1/namespaces/%6Es-a/
// memories answered 200 with facts from namespaces alice held no grant
// on. Read, write and delete must all authorize the raw segment, and a
// grant on a literal encoded name must authorize exactly that name.
func TestNamespaceAuthzEncodedPathIsolation(t *testing.T) {
	s, keys, db := authServer(t)
	ctx := context.Background()
	az := authz.New(db, nil)
	keys.SetAuthorizer(az)
	token, err := keys.Create(ctx, "reviewer-key", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := az.Grant(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"ns-a%2Fns-b", "ns-a/ns-b", "%6Es-a"} {
		if _, err := s.mem.Write(ctx, memory.WriteInput{Namespace: ns, Key: "/reviewer", Body: "other-namespace-fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/v1/namespaces/ns-a%2Fns-b/memories", "/v1/namespaces/%6Es-a/memories"} {
		if rr := authedGet(t, s, token, path); rr.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403: the encoded name is not the granted ns-a: %s", path, rr.Code, rr.Body)
		}
		if rr := authedGet(t, s, token, path+"/search?q=reviewer"); rr.Code != http.StatusForbidden {
			t.Errorf("GET %s/search = %d, want 403", path, rr.Code)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"key":"/k","body":"v"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403 (write on an encoded name)", path, rec.Code)
		}
		req = httptest.NewRequest(http.MethodDelete, path+"?key=/reviewer", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec = httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("DELETE %s = %d, want 403 (delete on an encoded name)", path, rec.Code)
		}
	}
	// positive control: the plain granted namespace still reads
	if rr := authedGet(t, s, token, "/v1/namespaces/ns-a/keys"); rr.Code != http.StatusOK {
		t.Fatalf("plain granted ns-a = %d, want 200: %s", rr.Code, rr.Body)
	}
	// a grant on the literal encoded name authorizes exactly that name:
	// the check and the handler resolve the same final namespace in both
	// directions
	if err := az.Grant(ctx, "alice", "%6Es-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	if rr := authedGet(t, s, token, "/v1/namespaces/%6Es-a/memories"); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), "other-namespace-fixture") {
		t.Fatalf("explicit grant on literal %%6Es-a = %d, want 200 with its own fixture: %s", rr.Code, rr.Body)
	}
}
