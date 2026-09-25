package region

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// Durable agent inboxes. Members of one namespace address each other by
// their registered agent name; the namespace is the authorization
// boundary (surfaces enforce grants), agent names are routing addresses.
// The agent_messages table is the source of truth: bus events are only
// wake-up hints, so a lost or never-published hint delays delivery by at
// most one poll interval and never loses a message.

var (
	// ErrMessageInvalid: malformed input (empty, oversized, bad bytes).
	ErrMessageInvalid = errors.New("message: invalid input")
	// ErrMessageNotFound: unregistered sender/recipient or a reply_to id
	// that does not exist in the namespace.
	ErrMessageNotFound = errors.New("message: not found")
	// ErrMessageConflict: an idempotency key reused with different content.
	ErrMessageConflict = errors.New("message: idempotency key reused with different content")
	// ErrMessageBacklog: recipient must ACK messages before new sends fit.
	ErrMessageBacklog = errors.New("message: unread recipient backlog is full")
)

const (
	// MaxMessageIDLen bounds namespace, agent names, task ids, reply ids,
	// message ids and idempotency keys, in bytes.
	MaxMessageIDLen = 256
	// MaxMessageBody bounds a message body, in bytes (16 KiB).
	MaxMessageBody = 16 * 1024
	// MaxMessageBatch bounds read limits and ack id lists.
	MaxMessageBatch = 100
	// MessageEventKind is the bus.Event kind for inbox-change hints.
	MessageEventKind = "agent_message"
)

// messagePollInterval is WaitMessages' storage reconciliation period:
// recovery for dropped bus events and for writes from other processes
// sharing the database. A var so tests can shorten it.
var messagePollInterval = 2 * time.Second

// Message is one durable addressed message.
type Message struct {
	ID          string `json:"id"`
	Namespace   string `json:"namespace"`
	Sender      string `json:"sender"`
	Recipient   string `json:"recipient"`
	Body        string `json:"body"`
	TaskID      string `json:"task_id,omitempty"`
	ReplyTo     string `json:"reply_to,omitempty"`
	CreatedAt   string `json:"created_at"`
	AckedAt     string `json:"acked_at,omitempty"`
	LeasedUntil string `json:"leased_until,omitempty"`
	LeasedBy    string `json:"leased_by,omitempty"`
}

// MessageInput is a send request. IdempotencyKey, when set, deduplicates
// retries per (namespace, sender): an identical resend returns the
// original message, a resend with different content is ErrMessageConflict.
type MessageInput struct {
	Namespace      string `json:"namespace"`
	Sender         string `json:"sender"`
	Recipient      string `json:"recipient"`
	Body           string `json:"body"`
	TaskID         string `json:"task_id,omitempty"`
	ReplyTo        string `json:"reply_to,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// MessageEventKey is the bus.Event key for a recipient's inbox:
// ns+":"+agent. Namespaces never contain ':' (validated), agent names may
// (opencode:<sessionID>), so consumers split on the FIRST colon.
func MessageEventKey(ns, agent string) string { return ns + ":" + agent }

// MessageEvent is the hint surfaces publish after a successful durable
// SendMessage. It carries no body: subscribers re-read storage.
func MessageEvent(m *Message) bus.Event {
	return bus.Event{Kind: MessageEventKind, Key: MessageEventKey(m.Namespace, m.Recipient),
		Data: map[string]string{"id": m.ID}}
}

func newMessageID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not a recoverable condition
	}
	return hex.EncodeToString(b)
}

// checkIdent validates a required or optional identifier field.
func checkIdent(field, v string, required bool) error {
	if v == "" {
		if required {
			return fmt.Errorf("%w: %s required", ErrMessageInvalid, field)
		}
		return nil
	}
	if len(v) > MaxMessageIDLen {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrMessageInvalid, field, MaxMessageIDLen)
	}
	if !utf8.ValidString(v) || strings.TrimSpace(v) != v {
		return fmt.Errorf("%w: %s must be valid UTF-8 without surrounding whitespace", ErrMessageInvalid, field)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s contains control characters", ErrMessageInvalid, field)
		}
	}
	return nil
}

func checkNamespace(ns string) error {
	if err := checkIdent("namespace", ns, true); err != nil {
		return err
	}
	if strings.Contains(ns, ":") {
		// the bus key is ns+":"+agent, split on the first colon
		return fmt.Errorf("%w: namespace must not contain ':'", ErrMessageInvalid)
	}
	return nil
}

func checkInbox(ns, agent string) error {
	if err := checkNamespace(ns); err != nil {
		return err
	}
	return checkIdent("agent", agent, true)
}

func (in MessageInput) validate() error {
	if err := checkNamespace(in.Namespace); err != nil {
		return err
	}
	if err := checkIdent("sender", in.Sender, true); err != nil {
		return err
	}
	if err := checkIdent("recipient", in.Recipient, true); err != nil {
		return err
	}
	if err := checkIdent("task_id", in.TaskID, false); err != nil {
		return err
	}
	if err := checkIdent("reply_to", in.ReplyTo, false); err != nil {
		return err
	}
	if err := checkIdent("idempotency_key", in.IdempotencyKey, false); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(in.Body) == "":
		return fmt.Errorf("%w: body required", ErrMessageInvalid)
	case len(in.Body) > MaxMessageBody:
		return fmt.Errorf("%w: body exceeds %d bytes", ErrMessageInvalid, MaxMessageBody)
	case !utf8.ValidString(in.Body) || strings.ContainsRune(in.Body, 0):
		// Postgres TEXT rejects NUL; keep both engines identical
		return fmt.Errorf("%w: body must be valid UTF-8 without NUL", ErrMessageInvalid)
	}
	return nil
}

const messageCols = `id, namespace, sender, recipient, body, task_id, reply_to, created_at, COALESCE(acked_at, ''), COALESCE(leased_until, ''), COALESCE(leased_by, '')`

type rowScanner interface{ Scan(dest ...any) error }

func scanMessage(r rowScanner) (Message, error) {
	var m Message
	err := r.Scan(&m.ID, &m.Namespace, &m.Sender, &m.Recipient, &m.Body, &m.TaskID, &m.ReplyTo, &m.CreatedAt, &m.AckedAt, &m.LeasedUntil, &m.LeasedBy)
	return m, err
}

// sameContent reports whether a stored message matches a resend.
func sameContent(m Message, in MessageInput) bool {
	return m.Recipient == in.Recipient && m.Body == in.Body && m.TaskID == in.TaskID && m.ReplyTo == in.ReplyTo
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// findIdempotent returns the message already stored under the input's
// idempotency key, nil if none.
func (s *Store) findIdempotent(ctx context.Context, q queryRower, in MessageInput) (*Message, error) {
	m, err := scanMessage(q.QueryRowContext(ctx, s.db.Rebind(`SELECT `+messageCols+` FROM agent_messages
		WHERE namespace = $1 AND sender = $2 AND idempotency_key = $3`), in.Namespace, in.Sender, in.IdempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !sameContent(m, in) {
		return nil, fmt.Errorf("%w: key %q", ErrMessageConflict, in.IdempotencyKey)
	}
	return &m, nil
}

// SendMessage durably stores a message from one registered member to
// another. It does not publish: surfaces publish MessageEvent after it
// returns successfully (also on an idempotent resend, which is harmless).
func (s *Store) SendMessage(ctx context.Context, in MessageInput) (*Message, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	m := &Message{ID: newMessageID(), Namespace: in.Namespace, Sender: in.Sender, Recipient: in.Recipient,
		Body: in.Body, TaskID: in.TaskID, ReplyTo: in.ReplyTo, CreatedAt: store.TimeToDB(s.now())}
	var existing *Message
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockMessageInbox(ctx, tx, in.Namespace, in.Recipient); err != nil {
			return err
		}
		if in.IdempotencyKey != "" {
			found, err := s.findIdempotent(ctx, tx, in)
			if err != nil || found != nil {
				existing = found
				return err
			}
		}
		for _, a := range []struct{ role, agent string }{{"sender", in.Sender}, {"recipient", in.Recipient}} {
			var one int
			err := tx.QueryRowContext(ctx, s.db.Rebind(
				`SELECT 1 FROM region_members WHERE namespace = $1 AND agent = $2`), in.Namespace, a.agent).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s %q is not registered in namespace %q", ErrMessageNotFound, a.role, a.agent, in.Namespace)
			}
			if err != nil {
				return err
			}
		}
		if in.ReplyTo != "" {
			var one int
			err := tx.QueryRowContext(ctx, s.db.Rebind(
				`SELECT 1 FROM agent_messages WHERE namespace = $1 AND id = $2`), in.Namespace, in.ReplyTo).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: reply_to %q not in namespace %q", ErrMessageNotFound, in.ReplyTo, in.Namespace)
			}
			if err != nil {
				return err
			}
		}
		cap := s.MaxUnreadPerRecipient
		if cap <= 0 {
			cap = 200
		}
		var unread int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT count(*) FROM agent_messages
			WHERE namespace = $1 AND recipient = $2 AND acked_at IS NULL`), in.Namespace, in.Recipient).Scan(&unread); err != nil {
			return err
		}
		if unread >= cap {
			return fmt.Errorf("%w: limit %d for %q", ErrMessageBacklog, cap, in.Recipient)
		}
		var key any
		if in.IdempotencyKey != "" {
			key = in.IdempotencyKey
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO agent_messages
			(id, namespace, sender, recipient, body, task_id, reply_to, idempotency_key, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`),
			m.ID, m.Namespace, m.Sender, m.Recipient, m.Body, m.TaskID, m.ReplyTo, key, m.CreatedAt)
		return err
	})
	if err != nil && in.IdempotencyKey != "" && !errors.Is(err, ErrMessageInvalid) &&
		!errors.Is(err, ErrMessageNotFound) && !errors.Is(err, ErrMessageConflict) && ctx.Err() == nil {
		// A concurrent retry with the same key may have won the unique
		// index between our lookup and insert (Postgres; SQLite runs one
		// tx at a time). The failed tx is rolled back, so look again.
		if found, ferr := s.findIdempotent(ctx, s.db, in); ferr == nil && found != nil {
			return found, nil
		} else if errors.Is(ferr, ErrMessageConflict) {
			return nil, ferr
		}
	}
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	return m, nil
}

// ReadMessages returns up to limit unacknowledged messages for agent in
// ns, oldest first. limit <= 0 or above MaxMessageBatch means
// MaxMessageBatch. Active delivery leases are hidden; reading never acknowledges.
func (s *Store) ReadMessages(ctx context.Context, ns, agent string, limit int) ([]Message, error) {
	return s.ReadMessagesWithOptions(ctx, ns, agent, MessageReadOptions{Limit: limit})
}

// MessageReadOptions extends the legacy unread read. ID retrieves one full
// message even after ACK (until retention); Box="sent" routes by sender.
// Neither mode claims delivery. Agent names route, namespace grants authorize.
type MessageReadOptions struct {
	Limit        int
	Box          string
	ID           string
	LeaseSeconds int
	LeasedBy     string
}

// Validate is also used by streaming surfaces before committing HTTP 200.
func (o MessageReadOptions) Validate(ns, agent string) error {
	if err := checkInbox(ns, agent); err != nil {
		return err
	}
	if o.Box != "" && o.Box != "inbox" && o.Box != "sent" {
		return fmt.Errorf("%w: box must be inbox or sent", ErrMessageInvalid)
	}
	if err := checkIdent("id", o.ID, false); err != nil {
		return err
	}
	if err := checkIdent("leased_by", o.LeasedBy, o.LeaseSeconds > 0); err != nil {
		return err
	}
	if o.LeaseSeconds < 0 || o.LeaseSeconds > 300 || (o.LeaseSeconds == 0 && o.LeasedBy != "") {
		return fmt.Errorf("%w: lease_seconds must be 1 to 300 with leased_by, or omit both", ErrMessageInvalid)
	}
	if o.LeaseSeconds > 0 && (o.ID != "" || o.Box == "sent") {
		return fmt.Errorf("%w: only unread inbox reads can lease", ErrMessageInvalid)
	}
	return nil
}

// lockMessageInbox serializes sends and lease acquisition across processes.
// This MUST be the transaction's first statement: SQLite takes its writer lock
// before any read snapshot (avoiding SQLITE_BUSY_SNAPSHOT on lock upgrade).
// Postgres locks only this namespace+recipient's existing membership row.
func (s *Store) lockMessageInbox(ctx context.Context, tx *sql.Tx, ns, agent string) error {
	_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE region_members SET agent = agent WHERE namespace = $1 AND agent = $2`), ns, agent)
	return err
}

func (s *Store) ReadMessagesWithOptions(ctx context.Context, ns, agent string, opts MessageReadOptions) ([]Message, error) {
	if err := opts.Validate(ns, agent); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 || limit > MaxMessageBatch {
		limit = MaxMessageBatch
	}
	column := "recipient"
	if opts.Box == "sent" {
		column = "sender"
	}
	query := `SELECT ` + messageCols + ` FROM agent_messages WHERE namespace = $1 AND ` + column + ` = $2`
	args := []any{ns, agent}
	if opts.ID != "" {
		query += ` AND id = $3`
		args = append(args, opts.ID)
	} else if opts.Box != "sent" {
		query += ` AND acked_at IS NULL AND (leased_until IS NULL OR leased_until <= $3)`
		args = append(args, store.TimeToDB(s.now()))
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY seq LIMIT $%d`, len(args))
	if opts.LeaseSeconds == 0 {
		rows, err := s.db.QueryContext(ctx, s.db.Rebind(query), args...)
		if err != nil {
			return nil, err
		}
		return collectMessages(rows)
	}
	var out []Message
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockMessageInbox(ctx, tx, ns, agent); err != nil {
			return err
		}
		now := s.now()
		args[2] = store.TimeToDB(now)
		if s.db.Driver == "postgres" {
			query += ` FOR UPDATE`
		}
		rows, err := tx.QueryContext(ctx, s.db.Rebind(query), args...)
		if err != nil {
			return err
		}
		out, err = collectMessages(rows)
		if err != nil || len(out) == 0 {
			return err
		}
		until := store.TimeToDB(now.Add(time.Duration(opts.LeaseSeconds) * time.Second))
		updateArgs := []any{until, opts.LeasedBy, ns, agent}
		ph := make([]string, len(out))
		for i := range out {
			out[i].LeasedUntil, out[i].LeasedBy = until, opts.LeasedBy
			updateArgs = append(updateArgs, out[i].ID)
			ph[i] = fmt.Sprintf("$%d", len(updateArgs))
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE agent_messages SET leased_until = $1, leased_by = $2
			WHERE namespace = $3 AND recipient = $4 AND id IN (`+strings.Join(ph, ",")+`)`), updateArgs...)
		return err
	})
	return out, err
}

func collectMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AckMessages marks the supplied ids received, scoped to messages
// addressed to agent in ns. Unknown, foreign or already-acknowledged ids
// are ignored, so retries are safe. Returns the number newly acknowledged.
// ACK means received, not that any linked task is complete.
func (s *Store) AckMessages(ctx context.Context, ns, agent string, ids []string) (int64, error) {
	return s.AckMessagesWithLease(ctx, ns, agent, ids, "")
}

// AckMessagesWithLease checks a supplied owner against a still-live lease.
// Omitting the owner preserves legacy explicit-ID ACKs. Mismatches are ignored
// like foreign IDs, so stale invocations cannot ACK a newer owner's delivery.
func (s *Store) AckMessagesWithLease(ctx context.Context, ns, agent string, ids []string, owner string) (int64, error) {
	if err := checkInbox(ns, agent); err != nil {
		return 0, err
	}
	if err := checkIdent("leased_by", owner, false); err != nil {
		return 0, err
	}
	if len(ids) == 0 || len(ids) > MaxMessageBatch {
		return 0, fmt.Errorf("%w: 1 to %d ids required", ErrMessageInvalid, MaxMessageBatch)
	}
	args := []any{store.TimeToDB(s.now()), ns, agent}
	seen := map[string]bool{}
	ph := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := checkIdent("id", id, true); err != nil {
			return 0, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
		ph = append(ph, fmt.Sprintf("$%d", len(args)))
	}
	query := `UPDATE agent_messages SET acked_at = $1, leased_until = NULL, leased_by = NULL
		WHERE namespace = $2 AND recipient = $3 AND acked_at IS NULL
		AND id IN (` + strings.Join(ph, ", ") + `)`
	if owner != "" {
		args = append(args, owner)
		query += fmt.Sprintf(` AND leased_by = $%d AND leased_until > $1`, len(args))
	}
	res, err := s.db.ExecContext(ctx, s.db.Rebind(query), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReleaseMessageLeases makes undelivered rows available immediately, without
// ACKing them. Only supplied IDs with this owner's live lease are changed;
// expired, foreign and already released rows are ignored, making retries safe.
func (s *Store) ReleaseMessageLeases(ctx context.Context, ns, agent string, ids []string, owner string) (int64, error) {
	if err := checkInbox(ns, agent); err != nil {
		return 0, err
	}
	if err := checkIdent("leased_by", owner, true); err != nil {
		return 0, err
	}
	if len(ids) == 0 || len(ids) > MaxMessageBatch {
		return 0, fmt.Errorf("%w: 1 to %d ids required", ErrMessageInvalid, MaxMessageBatch)
	}
	args := []any{ns, agent, owner, store.TimeToDB(s.now())}
	ph := make([]string, len(ids))
	for i, id := range ids {
		if err := checkIdent("id", id, true); err != nil {
			return 0, err
		}
		args = append(args, id)
		ph[i] = fmt.Sprintf("$%d", len(args))
	}
	res, err := s.db.ExecContext(ctx, s.db.Rebind(`UPDATE agent_messages SET leased_until = NULL, leased_by = NULL
		WHERE namespace = $1 AND recipient = $2 AND leased_by = $3 AND leased_until > $4
		AND acked_at IS NULL AND id IN (`+strings.Join(ph, ",")+`)`), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountUnreadMessages includes leased rows: leasing is not receipt.
func (s *Store) CountUnreadMessages(ctx context.Context, ns, agent string) (int64, error) {
	if err := checkInbox(ns, agent); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT count(*) FROM agent_messages
		WHERE namespace = $1 AND recipient = $2 AND acked_at IS NULL`), ns, agent).Scan(&n)
	return n, err
}

// SweepMessageRetention deletes only messages ACKed before the retention
// cutoff in ns. Unread messages never expire; full-ID recovery is available
// until this sweep. Zero/negative MessageRetention disables deletion.
func (s *Store) SweepMessageRetention(ctx context.Context, ns string) (int64, error) {
	if err := checkNamespace(ns); err != nil {
		return 0, err
	}
	if s.MessageRetention <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, s.db.Rebind(`DELETE FROM agent_messages
		WHERE namespace = $1 AND acked_at IS NOT NULL AND acked_at < $2`), ns, store.TimeToDB(s.now().Add(-s.MessageRetention)))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// WaitMessages returns agent's unread messages, blocking up to timeout
// until at least one exists. It subscribes to b (nil = storage polling
// only) BEFORE the first read so a send between read and subscribe
// cannot be missed, returns existing unread immediately, treats matching
// bus events as hints to re-read storage, and re-reads every
// messagePollInterval to recover lost hints and cross-process writes.
// timeout <= 0 performs one non-blocking read. Expiry returns an empty
// slice and nil error; ctx cancellation or deadline returns ctx.Err().
// Authorization is the caller's job and must be rechecked after return.
func (s *Store) WaitMessages(ctx context.Context, b *bus.Bus, ns, agent string, limit int, timeout time.Duration) ([]Message, error) {
	if err := checkInbox(ns, agent); err != nil {
		return nil, err
	}
	var events <-chan bus.Event
	if b != nil {
		ch, cancel := b.Subscribe()
		defer cancel()
		events = ch
	}
	msgs, err := s.ReadMessages(ctx, ns, agent, limit)
	if err != nil || len(msgs) > 0 || timeout <= 0 {
		return msgs, err
	}
	key := MessageEventKey(ns, agent)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(messagePollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return []Message{}, nil
		case <-poll.C:
		case e, open := <-events:
			if !open {
				events = nil // bus gone: keep polling storage
				continue
			}
			if e.Kind != MessageEventKind || e.Key != key {
				continue
			}
		}
		msgs, err := s.ReadMessages(ctx, ns, agent, limit)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if len(msgs) > 0 {
			return msgs, nil
		}
	}
}
