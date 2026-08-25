package region

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// ErrClaimed is returned when a live, unexpired claim already holds a key.
type ErrClaimed struct {
	Key    string
	Holder string
}

func (e *ErrClaimed) Error() string {
	return fmt.Sprintf("work_claims: %q already claimed by %s", e.Key, e.Holder)
}

// Claim is one work lease.
type Claim struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Holder    string `json:"holder"`
	ClaimedAt string `json:"claimed_at"`
	ExpiresAt string `json:"expires_at"`
}

// ClaimWork takes a lease on a sub-key so no other satellite works it.
// Atomic: the whole check-and-insert runs in one transaction against the
// live claim, so two racing claimers cannot both win (the append-only
// store's conflict-free guarantee for WORK, not just facts). A holder
// re-claiming its own key extends the lease.
func (s *Store) ClaimWork(ctx context.Context, ns, key, holder string, ttl time.Duration) (*Claim, error) {
	if ns == "" || key == "" || holder == "" {
		return nil, fmt.Errorf("claim: namespace, key, holder required")
	}
	now := s.now().UTC()
	c := &Claim{Namespace: ns, Key: key, Holder: holder,
		ClaimedAt: store.TimeToDB(now), ExpiresAt: store.TimeToDB(now.Add(ttl))}
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var existingHolder, expiresAt string
		q := `SELECT holder, expires_at FROM work_claims
		      WHERE namespace = $1 AND key = $2 AND released_at IS NULL
		      ORDER BY id DESC LIMIT 1`
		if s.db.Driver == "postgres" {
			// serialize concurrent claimers on the same (ns,key)
			q = `SELECT holder, expires_at FROM work_claims
			     WHERE namespace = $1 AND key = $2 AND released_at IS NULL
			     ORDER BY id DESC LIMIT 1 FOR UPDATE`
		}
		err := tx.QueryRowContext(ctx, s.db.Rebind(q), ns, key).Scan(&existingHolder, &expiresAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			exp, perr := store.TimeFromDB(expiresAt)
			if perr == nil && exp.After(now) && existingHolder != holder {
				return &ErrClaimed{Key: key, Holder: existingHolder}
			}
			// stale or own claim: release it before re-claiming
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`
				UPDATE work_claims SET released_at = $1
				WHERE namespace = $2 AND key = $3 AND released_at IS NULL`),
				store.TimeToDB(now), ns, key); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(`
			INSERT INTO work_claims (namespace, key, holder, claimed_at, expires_at)
			VALUES ($1,$2,$3,$4,$5)`),
			ns, key, holder, c.ClaimedAt, c.ExpiresAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ReleaseWork closes a holder's live claim.
func (s *Store) ReleaseWork(ctx context.Context, ns, key, holder string) error {
	res, err := s.db.ExecContext(ctx, s.db.Rebind(`
		UPDATE work_claims SET released_at = $1
		WHERE namespace = $2 AND key = $3 AND holder = $4 AND released_at IS NULL`),
		store.TimeToDB(s.now()), ns, key, holder)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("release: no live claim on %q by %s", key, holder)
	}
	return nil
}

// ListClaims returns the live (unexpired, unreleased) claims in a region.
func (s *Store) ListClaims(ctx context.Context, ns string) ([]Claim, error) {
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT namespace, key, holder, claimed_at, expires_at FROM work_claims
		WHERE namespace = $1 AND released_at IS NULL AND expires_at > $2
		ORDER BY key`), ns, store.TimeToDB(s.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Claim{}
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.Namespace, &c.Key, &c.Holder, &c.ClaimedAt, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
