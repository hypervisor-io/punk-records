package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Link is one typed edge between two facts in a region (B7).
type Link struct {
	Namespace   string  `json:"namespace"`
	FromKey     string  `json:"from_key"`
	ToKey       string  `json:"to_key"`
	LinkType    string  `json:"link_type"`
	Weight      float64 `json:"weight"`
	Description string  `json:"description,omitempty"`
}

// LinkTypes is the canonical edge taxonomy: 11
// authorable types plus similar_to, mentions, and co_occurs, which machine
// passes (creative consolidation, enrichment, entity extraction) add.
var LinkTypes = map[string]struct{}{
	"relates_to": {}, "leads_to": {}, "occurred_before": {},
	"prefers_over": {}, "exemplifies": {}, "contradicts": {},
	"reinforces": {}, "invalidated_by": {}, "evolved_into": {},
	"derived_from": {}, "part_of": {}, "similar_to": {},
	"mentions": {}, "co_occurs": {},
}

// ValidateLinkType rejects edges outside the taxonomy so link semantics
// stay queryable (recall demotes invalidated_by; bridges rank by count).
func ValidateLinkType(t string) error {
	if _, ok := LinkTypes[t]; !ok {
		return fmt.Errorf("memory: unknown link type %q", t)
	}
	return nil
}

// AddLink records from_key -> to_key with a type (idempotent), default
// weight 1.0.
func (s *Store) AddLink(ctx context.Context, ns, from, to, linkType string) error {
	return s.AddLinkWeighted(ctx, ns, from, to, linkType, 1.0)
}

// AddLinkWeighted records from_key -> to_key with a type and a [0,1]
// strength (idempotent); weight feeds bridge-discovery contribution in
// scored search. Out-of-range weight (including the zero value) falls
// back to the 1.0 default.
func (s *Store) AddLinkWeighted(ctx context.Context, ns, from, to, linkType string, weight float64) error {
	return s.AddLinkDescribed(ctx, ns, from, to, linkType, weight, "")
}

// AddLinkDescribed records from_key -> to_key with a type, a [0,1] strength,
// and an optional one-sentence NL restatement of the fact the edge encodes
// (makes the relation itself retrievable, not just its
// endpoints). When an embedder is configured and description is non-empty,
// the description is embedded and stored alongside it so TripletSearch can
// rank on relation relevance. The embed call happens here, BEFORE the write
// transaction opens - mirroring memory.go's Remember, which embeds the fact
// body before its own WithTx - because sqlite runs SetMaxOpenConns(1)
// (internal/store/store.go): a tx holds the process's only connection, and
// an embedder is typically an outbound HTTP call that can take up to
// several seconds, so embedding from inside the tx would stall every other
// DB access for that long. Idempotent like AddLinkWeighted.
func (s *Store) AddLinkDescribed(ctx context.Context, ns, from, to, linkType string, weight float64, description string) error {
	linkType, weight, err := normalizeLinkArgs(from, to, linkType, weight)
	if err != nil {
		return err
	}
	var embedding []byte
	if s.embedder != nil && description != "" {
		vecs, err := s.embedder.Embed(ctx, []string{description})
		if err != nil {
			return fmt.Errorf("embed link description: %w", err)
		}
		embedding = encodeVector(vecs[0], s.quantize)
	}
	// now is captured once and reused for both created_at and valid_at so
	// the edge's validity window starts at the exact instant it was
	// created, not a second, later call to the clock (s.now() advances
	// the fakeClock in tests, and would drift a live clock too).
	now := s.now()
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.addLinkTx(ctx, tx, ns, from, to, linkType, weight, description, embedding, now)
	})
}

// normalizeLinkArgs validates from/to/linkType and normalizes an
// out-of-range weight to the 1.0 default, shared by AddLinkDescribed's
// pre-tx validation (so an invalid call never reaches the embedder) and
// addLinkTx's own validation (the only validation DetectContradictions'
// direct addLinkTx calls get), so both apply exactly the same rules from
// one place.
func normalizeLinkArgs(from, to, linkType string, weight float64) (string, float64, error) {
	if err := ValidateKey(from); err != nil {
		return "", 0, err
	}
	if err := ValidateKey(to); err != nil {
		return "", 0, err
	}
	if linkType == "" {
		linkType = "relates_to"
	}
	if err := ValidateLinkType(linkType); err != nil {
		return "", 0, err
	}
	if weight <= 0 || weight > 1 {
		weight = 1.0
	}
	return linkType, weight, nil
}

// addLinkTx is the tx-aware, pure-SQL core shared by AddLinkDescribed and
// DetectContradictions: single-row insert against a caller-owned
// transaction instead of s.db directly. It never touches s.embedder - the
// embedding (if any) is precomputed by the caller and passed in as a byte
// slice, so no addLinkTx call can hold a transaction open across an embed
// call (finding 1, round 2). DetectContradictions writes a pair's three
// edges (contradicts both ways, invalidated_by) by calling this three times
// inside one s.db.WithTx (see TestAddLinkTxAtomicRollback) - all three edges
// in one tx, do not split it back into three separate WithTx calls - so a
// mid-triple failure rolls back everything already inserted for that pair
// instead of leaving it half-linked. now is passed in (not read via
// s.now()) so all edges written by one caller share one instant, and so the
// fakeClock in tests isn't advanced once per edge.
func (s *Store) addLinkTx(ctx context.Context, tx *sql.Tx, ns, from, to, linkType string, weight float64, description string, embedding []byte, now time.Time) error {
	linkType, weight, err := normalizeLinkArgs(from, to, linkType, weight)
	if err != nil {
		return err
	}
	dbNow := store.TimeToDB(now)
	_, err = tx.ExecContext(ctx, s.db.Rebind(`
		INSERT INTO memory_links (namespace, from_key, to_key, link_type, weight, description, embedding, created_at, valid_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (namespace, from_key, to_key, link_type) DO NOTHING`),
		ns, from, to, linkType, weight, nullableString(description), embedding, dbNow, dbNow)
	if err != nil {
		return fmt.Errorf("add link: %w", err)
	}
	return nil
}

// nullableString turns "" into a SQL NULL so an unset description reads
// back as "" (via COALESCE) rather than an empty string vs NULL ambiguity.
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// Neighbors returns keys linked from a key (outgoing edges). Direction
// "in" returns incoming edges instead. Closed edges (invalid_at set) are
// live-edge filtered out, same as every other default link read.
func (s *Store) Neighbors(ctx context.Context, ns, key, direction string) ([]Link, error) {
	col, other := "from_key", "to_key"
	if direction == "in" {
		col, other = "to_key", "from_key"
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(fmt.Sprintf(`
		SELECT namespace, from_key, to_key, link_type, weight, COALESCE(description,'') FROM memory_links
		WHERE namespace = $1 AND %s = $2 AND invalid_at IS NULL ORDER BY %s`, col, other)), ns, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Namespace, &l.FromKey, &l.ToKey, &l.LinkType, &l.Weight, &l.Description); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// InvalidateLink closes the live edge's validity window (invalid_at set);
// it never deletes the row, so InvalidateLink+NeighborsAsOf can still see
// the edge as it stood before the close. Invalidating an edge with no live
// row (never linked, or already closed) is an error - mirrors Forget's
// double-tombstone refusal.
func (s *Store) InvalidateLink(ctx context.Context, ns, from, to, linkType string) error {
	if err := ValidateKey(from); err != nil {
		return err
	}
	if err := ValidateKey(to); err != nil {
		return err
	}
	if linkType == "" {
		linkType = "relates_to"
	}
	if err := ValidateLinkType(linkType); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, s.db.Rebind(`
		UPDATE memory_links SET invalid_at = $1
		WHERE namespace = $2 AND from_key = $3 AND to_key = $4 AND link_type = $5 AND invalid_at IS NULL`),
		store.TimeToDB(s.now()), ns, from, to, linkType)
	if err != nil {
		return fmt.Errorf("invalidate link: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: no live %s edge %s -> %s", ErrNotFound, linkType, from, to)
	}
	return nil
}

// NeighborsAsOf reads the edges that were valid at a past instant, mirroring
// Neighbors' column list and direction handling but windowed like
// RecallAsOf: an edge counts if its [valid_at, invalid_at) window (falling
// back to created_at when valid_at is NULL) covers asOf. A future asOf
// (past "now") returns the live edge set, same as RecallAsOf, since an
// open window (invalid_at IS NULL) covers every instant from valid_at
// onward.
func (s *Store) NeighborsAsOf(ctx context.Context, ns, key, direction string, asOf time.Time) ([]Link, error) {
	col, other := "from_key", "to_key"
	if direction == "in" {
		col, other = "to_key", "from_key"
	}
	at := store.TimeToDB(asOf)
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(fmt.Sprintf(`
		SELECT namespace, from_key, to_key, link_type, weight, COALESCE(description,'') FROM memory_links
		WHERE namespace = $1 AND %s = $2
		  AND COALESCE(valid_at, created_at) <= $3
		  AND (invalid_at IS NULL OR invalid_at > $3)
		ORDER BY %s`, col, other)), ns, key, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Namespace, &l.FromKey, &l.ToKey, &l.LinkType, &l.Weight, &l.Description); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// linkTargetsAll returns the to_key of every outgoing edge of linkType from
// key, live or closed. Deliberately unfiltered by validity: callers use
// this to build a dedup/idempotency set, and a closed edge must stay in it
// - otherwise a user-closed target gets re-proposed forever (a no-op
// insert against ON CONFLICT DO NOTHING, but one that still burns whatever
// per-call budget the caller is enforcing).
func (s *Store) linkTargetsAll(ctx context.Context, ns, from, linkType string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT to_key FROM memory_links
		WHERE namespace = $1 AND from_key = $2 AND link_type = $3 ORDER BY to_key`),
		ns, from, linkType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// linksForKeys loads every link touching any of keys, either direction.
// Closed edges (invalid_at set) are live-edge filtered out.
func (s *Store) linksForKeys(ctx context.Context, ns string, keys []string) ([]Link, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	fromPh := make([]string, len(keys))
	toPh := make([]string, len(keys))
	args := []any{ns}
	n := 2
	for i, k := range keys {
		fromPh[i] = fmt.Sprintf("$%d", n)
		args = append(args, k)
		n++
	}
	for i, k := range keys {
		toPh[i] = fmt.Sprintf("$%d", n)
		args = append(args, k)
		n++
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT namespace, from_key, to_key, link_type, weight FROM memory_links
		WHERE namespace = $1 AND (from_key IN (`+strings.Join(fromPh, ",")+`) OR to_key IN (`+strings.Join(toPh, ",")+`))
		AND invalid_at IS NULL`),
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Namespace, &l.FromKey, &l.ToKey, &l.LinkType, &l.Weight); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
