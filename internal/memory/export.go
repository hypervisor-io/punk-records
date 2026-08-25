package memory

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// exportRecord is one JSONL line: a full revision, including tombstones,
// so an export replays into an identical chain. Export is DERIVED data;
// the database stays the single source of truth.
type exportRecord struct {
	ID         string          `json:"id"`
	Key        string          `json:"key"`
	Action     string          `json:"action"`
	Body       string          `json:"body"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
	Author     string          `json:"author,omitempty"`
	CreatedAt  string          `json:"created_at"`
}

// ExportJSONL streams every revision in the namespace, oldest first.
func (s *Store) ExportJSONL(ctx context.Context, ns string, w io.Writer) error {
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("memory: namespace %q not found", ns)
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT id, key, action, body, attributes, author, created_at
		FROM memories WHERE namespace_id = $1
		ORDER BY created_at, id`), nsID)
	if err != nil {
		return fmt.Errorf("export query: %w", err)
	}
	defer rows.Close()

	enc := json.NewEncoder(w)
	for rows.Next() {
		var r exportRecord
		var attrs string
		if err := rows.Scan(&r.ID, &r.Key, &r.Action, &r.Body, &attrs, &r.Author, &r.CreatedAt); err != nil {
			return err
		}
		if attrs != "" && attrs != "{}" {
			r.Attributes = json.RawMessage(attrs)
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ImportJSONL appends revisions from an export stream. Lines whose id
// already exists are skipped (idempotent re-import); malformed lines
// abort with a line number so bad data never lands silently. The
// store's write-time defense mode applies per record before the INSERT:
// "redact" scrubs the body, "block" drops the record (counted in
// blocked) without aborting the rest of the import.
func (s *Store) ImportJSONL(ctx context.Context, ns string, r io.Reader) (imported, skipped, blocked int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	line := 0
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		nsID, err := s.ensureNamespace(ctx, tx, ns)
		if err != nil {
			return err
		}
		for sc.Scan() {
			line++
			raw := sc.Bytes()
			if len(raw) == 0 {
				continue
			}
			var rec exportRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("line %d: %w", line, err)
			}
			if rec.ID == "" || rec.Key == "" {
				return fmt.Errorf("line %d: missing id or key", line)
			}
			switch rec.Action {
			case "add", "update", "tombstone":
			default:
				return fmt.Errorf("line %d: bad action %q", line, rec.Action)
			}
			if _, err := store.TimeFromDB(rec.CreatedAt); err != nil {
				return fmt.Errorf("line %d: %w", line, err)
			}
			attrs := "{}"
			if len(rec.Attributes) > 0 {
				if !json.Valid(rec.Attributes) {
					return fmt.Errorf("line %d: attributes not valid json", line)
				}
				attrs = string(rec.Attributes)
			}
			// ponytail: body only, mirrors the Write() switch — attributes
			// aren't scrubbed here either (same deferred scope as Write).
			switch s.defense {
			case "redact":
				rec.Body, _ = Scrub(rec.Body)
			case "block":
				if _, labels := Scrub(rec.Body); len(labels) > 0 {
					blocked++
					continue
				}
			}
			res, err := tx.ExecContext(ctx, s.db.Rebind(`
				INSERT INTO memories (id, namespace_id, key, action, body, attributes, author, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (id) DO NOTHING`),
				rec.ID, nsID, rec.Key, rec.Action, rec.Body, attrs, rec.Author, rec.CreatedAt)
			if err != nil {
				return fmt.Errorf("line %d: %w", line, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				skipped++
			} else {
				imported++
			}
		}
		return sc.Err()
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return imported, skipped, blocked, nil
}
