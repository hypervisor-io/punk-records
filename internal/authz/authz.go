// Package authz is the namespace authorization model (task A01): a
// verified subject holds exact-match read/write/admin grants per
// namespace, stored in the namespace_grants table (migration 0022).
//
// Identity: the subject is always a VERIFIED credential claim - the API
// key subject the HTTP auth middleware establishes after token
// verification. It is never a user-controlled namespace, header or
// agent label; the middleware already replaces any client-supplied
// X-Punk-Subject header.
//
// Enforcement: opt-in via config authz.enforcement: deny. With
// enforcement off (the default) trusted single-user deployments behave
// exactly as before and this table is inert. With enforcement on the
// policy is deny-by-default: an empty subject and the zero-key
// bootstrap are denied on enforced routes.
//
// Wildcards and prefix rules are deliberately omitted from this first
// version: grants are exact, case-sensitive string matches and '*' is
// rejected at grant time so a grant can never be mistaken for a glob.
//
// Local stdio trust: the `punk mcp` stdio server runs as the local OS
// user with no bearer credential, like the punk CLI itself. It is a
// trusted transport with the same authority as direct database access;
// authorization under authz.enforcement applies to the
// credential-verified HTTP boundary. Operators who need per-agent
// isolation must not hand the stdio server (or the DB file) to
// untrusted agents.
//
// Provisioning and recovery: grants are managed locally through this
// package (Grant/Revoke/Grants) - the same trust level as `punk keys
// create`, which also works without HTTP auth. If enforcement locks
// everyone out, recovery is local: insert a grant row (or revoke the
// enabling config) with direct DB access. Grant provisioning must never
// require an unauthenticated HTTP path.
package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Op is a namespace operation a grant can authorize. Read, write and
// admin are distinct: no op implies another.
type Op string

const (
	OpRead  Op = "read"
	OpWrite Op = "write"
	OpAdmin Op = "admin"
)

func validOp(op Op) bool {
	switch op {
	case OpRead, OpWrite, OpAdmin:
		return true
	}
	return false
}

// Grant is one active subject -> namespace permission row.
type Grant struct {
	Subject   string
	Namespace string
	Op        Op
	CreatedAt time.Time
}

// Authorizer decides and manages namespace grants against the store.
type Authorizer struct {
	db  *store.DB
	now func() time.Time

	// Log receives a Warn line whenever a store error (other than
	// sql.ErrNoRows) forces a fail-closed deny - visibility into "the DB
	// is unhappy" without weakening deny-by-default. Nil discards
	// (the default): callers that never set it see no behavior change.
	// Never logs the subject or any credential value.
	Log *slog.Logger
}

func New(db *store.DB, now func() time.Time) *Authorizer {
	if now == nil {
		now = time.Now
	}
	return &Authorizer{db: db, now: now}
}

// warn logs msg at Warn level when Log is set; a nil Log (the default)
// discards silently.
func (a *Authorizer) warn(msg string, args ...any) {
	if a.Log != nil {
		a.Log.Warn(msg, args...)
	}
}

func validateSubject(subject string) error {
	if strings.TrimSpace(subject) == "" {
		return errors.New("authz: subject is required")
	}
	if strings.Contains(subject, "*") {
		return fmt.Errorf("authz: subject %q: wildcards are not supported, grants are exact-match", subject)
	}
	return nil
}

func validateNamespace(namespace string) error {
	if strings.TrimSpace(namespace) == "" {
		return errors.New("authz: namespace is required")
	}
	if strings.Contains(namespace, "*") {
		return fmt.Errorf("authz: namespace %q: wildcards are not supported, grants are exact-match", namespace)
	}
	if strings.Contains(namespace, ":") {
		return fmt.Errorf("authz: namespace %q must not contain ':'", namespace)
	}
	return nil
}

// Grant adds (or confirms) an active grant. It is idempotent: an
// existing active grant is a no-op, and a revoked grant can be
// re-granted.
func (a *Authorizer) Grant(ctx context.Context, subject, namespace string, op Op) error {
	if err := validateSubject(subject); err != nil {
		return err
	}
	if err := validateNamespace(namespace); err != nil {
		return err
	}
	if !validOp(op) {
		return fmt.Errorf("authz: op %q: want read, write, or admin", op)
	}
	// A single upsert against the namespace_grants_active partial unique
	// index (subject, namespace, op) WHERE revoked_at IS NULL: no
	// conflict target, so both SQLite and Postgres infer the partial
	// index and silently no-op on an existing active grant instead of
	// racing a separate count-then-insert against concurrent grants of
	// the identical tuple.
	_, err := a.db.ExecContext(ctx, a.db.Rebind(`
		INSERT INTO namespace_grants (subject, namespace, op, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`),
		subject, namespace, string(op), store.TimeToDB(a.now()))
	if err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	return nil
}

// Revoke closes an active grant. Revoking a grant that does not exist
// (or is already revoked) is an error.
func (a *Authorizer) Revoke(ctx context.Context, subject, namespace string, op Op) error {
	res, err := a.db.ExecContext(ctx, a.db.Rebind(`
		UPDATE namespace_grants SET revoked_at = $1
		WHERE subject = $2 AND namespace = $3 AND op = $4 AND revoked_at IS NULL`),
		store.TimeToDB(a.now()), subject, namespace, string(op))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("authz: no active grant for subject %q on namespace %q op %q", subject, namespace, op)
	}
	return nil
}

// Grants lists a subject's active grants, ordered for stable output.
func (a *Authorizer) Grants(ctx context.Context, subject string) ([]Grant, error) {
	rows, err := a.db.QueryContext(ctx, a.db.Rebind(`
		SELECT subject, namespace, op, created_at FROM namespace_grants
		WHERE subject = $1 AND revoked_at IS NULL
		ORDER BY namespace, op`), subject)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Grant
	for rows.Next() {
		var g Grant
		var op, created string
		if err := rows.Scan(&g.Subject, &g.Namespace, &op, &created); err != nil {
			return nil, err
		}
		g.Op = Op(op)
		g.CreatedAt, err = store.TimeFromDB(created)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Allow reports whether subject holds an active grant for op on
// namespace. Deny-by-default: an empty or wildcard-shaped subject or
// namespace, an unknown op, or the absence of an exact-match grant all
// deny.
func (a *Authorizer) Allow(ctx context.Context, subject, namespace string, op Op) bool {
	if strings.TrimSpace(subject) == "" || namespace == "" {
		return false
	}
	if strings.Contains(subject, "*") || strings.Contains(namespace, "*") {
		return false
	}
	if strings.Contains(namespace, ":") {
		return false
	}
	if !validOp(op) {
		return false
	}
	var one int
	err := a.db.QueryRowContext(ctx, a.db.Rebind(`
		SELECT 1 FROM namespace_grants
		WHERE subject = $1 AND namespace = $2 AND op = $3 AND revoked_at IS NULL
		LIMIT 1`),
		subject, namespace, string(op)).Scan(&one)
	if err == nil {
		return true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		// Fail-closed either way; a genuine store error (not "no grant
		// row") is worth surfacing. Never log the subject.
		a.warn("authz: allow query failed, denying", "op", string(op), "namespace", namespace, "err", err)
	}
	return false
}
