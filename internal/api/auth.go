package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// namespacesPrefix prefixes every /v1 route that is scoped to one
// namespace; the next path segment is the namespace name.
const namespacesPrefix = "/v1/namespaces/"

// namespaceFromPath extracts the {ns} segment from a namespaced route
// path, or "" for any other path.
func namespaceFromPath(path string) string {
	rest, ok := strings.CutPrefix(path, namespacesPrefix)
	if !ok {
		return ""
	}
	ns, _, _ := strings.Cut(rest, "/")
	return ns
}

// Keys manages API keys. Tokens are random 256-bit values shown once;
// only SHA-256 hashes are stored, and lookup is by hash so comparison is
// constant-time by construction.
type Keys struct {
	db  *store.DB
	now func() time.Time
	az  *authz.Authorizer
}

func NewKeys(db *store.DB, now func() time.Time) *Keys {
	if now == nil {
		now = time.Now
	}
	return &Keys{db: db, now: now}
}

// SetAuthorizer wires namespace authorization (config
// authz.enforcement: deny). nil - the default - keeps the legacy
// trusted behavior where any verified key reaches every namespace.
func (k *Keys) SetAuthorizer(az *authz.Authorizer) { k.az = az }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create mints a new key and returns the token exactly once. subject is
// an optional external agent-identity principal (Okta/Entra) - the
// ledger already is the audit surface those registries want.
func (k *Keys) Create(ctx context.Context, name, subject string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("api key name is required")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := "prk_" + hex.EncodeToString(raw)
	_, err := k.db.ExecContext(ctx, k.db.Rebind(`
		INSERT INTO api_keys (name, token_hash, subject, created_at) VALUES ($1, $2, $3, $4)`),
		name, hashToken(token), subject, store.TimeToDB(k.now()))
	if err != nil {
		return "", fmt.Errorf("create api key: %w", err)
	}
	return token, nil
}

// Revoke disables a key by name.
func (k *Keys) Revoke(ctx context.Context, name string) error {
	res, err := k.db.ExecContext(ctx, k.db.Rebind(`
		UPDATE api_keys SET revoked_at = $1 WHERE name = $2 AND revoked_at IS NULL`),
		store.TimeToDB(k.now()), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("api key %q not found or already revoked", name)
	}
	return nil
}

// ActiveCount reports how many usable keys exist.
func (k *Keys) ActiveCount(ctx context.Context) (int, error) {
	var n int
	err := k.db.QueryRowContext(ctx,
		`SELECT count(*) FROM api_keys WHERE revoked_at IS NULL`).Scan(&n)
	return n, err
}

// Check validates a bearer token and returns the key's subject claim.
func (k *Keys) Check(ctx context.Context, token string) (bool, string, error) {
	var subject string
	err := k.db.QueryRowContext(ctx, k.db.Rebind(`
		SELECT subject FROM api_keys WHERE token_hash = $1 AND revoked_at IS NULL`),
		hashToken(token)).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, subject, nil
}

// AuthMiddleware guards /v1. Bootstrap mode: while ZERO active keys
// exist, requests pass (first key is created via CLI, not HTTP). The
// moment a key exists, every request needs one.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A client-supplied subject is never trusted; the middleware
		// replaces it with the one the key check verifies below.
		r.Header.Del("X-Punk-Subject")
		if s.keys == nil {
			next.ServeHTTP(w, r)
			return
		}
		n, err := s.keys.ActiveCount(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if n == 0 {
			// Bootstrap pass-through. With authorization enforced there is
			// no verified subject, and deny-by-default grants the empty
			// subject nothing on namespaced routes; first key and grants
			// are provisioned locally (CLI), never over unauthenticated
			// HTTP.
			if !s.enforceNamespace(w, r, "") {
				return
			}
			next.ServeHTTP(w, r) // bootstrap
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		valid, subject, err := s.keys.Check(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !valid {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		if !s.enforceNamespace(w, r, subject) {
			return
		}
		r.Header.Set("X-Punk-Subject", subject)
		next.ServeHTTP(w, r)
	})
}

// enforceNamespace is the A01 authorization hook: when an authorizer is
// wired (authz.enforcement: deny), a route carrying a {ns} path
// parameter requires a grant for the verified subject - GET/HEAD need
// read, every other method needs write. Without an authorizer, or on
// routes with no {ns} parameter, behavior is unchanged; task A02
// extends enforcement to the full boundary inventory (agent hooks,
// brain, tasks, MCP, subscriptions).
func (s *Server) enforceNamespace(w http.ResponseWriter, r *http.Request, subject string) bool {
	az := s.keys.az
	if az == nil {
		return true
	}
	// The {ns} chi param is not yet parsed when this middleware runs
	// (v1 middlewares execute before the sub-router matches the route),
	// so the namespace comes from the well-known route prefix.
	ns := namespaceFromPath(r.URL.Path)
	if ns == "" {
		return true
	}
	op := authz.OpWrite
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		op = authz.OpRead
	}
	if az.Allow(r.Context(), subject, ns, op) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "namespace grant required: " + string(op) + " on " + ns,
	})
	return false
}
