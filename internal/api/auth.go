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

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Keys manages API keys. Tokens are random 256-bit values shown once;
// only SHA-256 hashes are stored, and lookup is by hash so comparison is
// constant-time by construction.
type Keys struct {
	db  *store.DB
	now func() time.Time
}

func NewKeys(db *store.DB, now func() time.Time) *Keys {
	if now == nil {
		now = time.Now
	}
	return &Keys{db: db, now: now}
}

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
			next.ServeHTTP(w, r) // bootstrap
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		valid, _, err := s.keys.Check(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !valid {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
