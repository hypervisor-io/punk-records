// Package memory is the fact store: namespaced, append-only with
// tombstones, hierarchical keys, FTS. The database is the single source
// of truth; JSONL export is derived data (duckbrain lesson).
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// contentHash fingerprints a write's meaningful content so two agents
// appending the identical fact to the same key collapse to one live
// revision (idempotent remember, B3).
func contentHash(key, body, attrsJSON string) string {
	sum := sha256.Sum256([]byte(key + "\x00" + body + "\x00" + attrsJSON))
	return hex.EncodeToString(sum[:])
}

// ErrNotFound is returned when acting on a key with no live fact.
var ErrNotFound = errors.New("memory: key not found")

// Store reads and writes facts. now is injectable for tests.
type Store struct {
	db              *store.DB
	now             func() time.Time
	embedder        Embedder          // nil disables vector search
	entityExtractor EntityExtractor   // nil disables entity enrichment
	reranker        Reranker          // nil disables cross-encoder reranking (R2 Task 9)
	defense         string            // "" / "off" / "redact" / "block" — global write-time secret scrubbing
	defensePolicies map[string]string // per-namespace override of defense, else falls back to defense
	quantize        bool              // true: write-time embeddings store int8 (4x smaller); reads handle both regardless

	ivfMu       sync.Mutex
	ivf         map[string]*ivfIndex // per-namespace approximate vector index; nil entry/missing = brute force
	ivfNprobe   int                  // <=0 means unset: VectorSearch defaults to 8 whenever an index exists
	ivfMinFacts int                  // <=0 means unset: VectorSearch defaults to 2000

	consolMu         sync.Mutex           // guards lastConsolidated
	lastConsolidated map[string]time.Time // per-ns last consolidation pass in this process (see MarkConsolidated)
}

// SetEmbedder wires write-time embedding (P12.5).
func (s *Store) SetEmbedder(e Embedder) { s.embedder = e }

// SetQuantizeVectors controls whether new embeddings are stored int8
// (true, ~4x smaller, ~2% recall cost) or float32 (false, unchanged).
// Existing rows of either format remain readable regardless of this flag.
func (s *Store) SetQuantizeVectors(q bool) { s.quantize = q }

// SetReranker wires optional cross-encoder reranking into scored search.
func (s *Store) SetReranker(r Reranker) { s.reranker = r }

// SetEntityExtractor wires entity extraction into the enricher (R2).
func (s *Store) SetEntityExtractor(e EntityExtractor) { s.entityExtractor = e }

// SetIVFConfig wires the approximate vector index knobs. nprobe is how
// many nearest centroids VectorSearch probes per query; minFacts is the
// namespace size below which brute force is used even if an index is
// built. <=0 for either means "unset": defaults of 8 and 2000 apply.
func (s *Store) SetIVFConfig(nprobe, minFacts int) {
	s.ivfNprobe, s.ivfMinFacts = nprobe, minFacts
}

func New(db *store.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

// Fact is the latest live value of a key, or one appended revision.
type Fact struct {
	ID         string         `json:"id"`
	Namespace  string         `json:"namespace"`
	Key        string         `json:"key"`
	Action     string         `json:"action"`
	Body       string         `json:"body"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Author     string         `json:"author,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`

	// v2 provenance + temporal validity (P12.1, P12.2)
	Writer     string     `json:"writer,omitempty"`     // agent:<name> | api | mcp
	TaskID     string     `json:"task_id,omitempty"`    // originating investigation
	SourceRef  string     `json:"source_ref,omitempty"` // e.g. incident:42
	Confidence float64    `json:"confidence,omitempty"` // 0 = unset
	ValidAt    time.Time  `json:"valid_at"`
	InvalidAt  *time.Time `json:"invalid_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`

	// Salience: feeds ranking only, never deletion.
	Importance  float64 `json:"importance,omitempty"`   // author-declared weight [0,1]
	AccessCount int64   `json:"access_count,omitempty"` // bumped on search hits

	// FeedbackWeight: EWMA of user ratings on answers that
	// used this fact. Starts neutral at 0.5; feeds ranking only.
	FeedbackWeight float64 `json:"feedback_weight,omitempty"`

	// Reinforcements counts idempotent duplicate writes of this exact
	// content: independent corroboration, ranking only.
	Reinforcements int64 `json:"reinforcements,omitempty"`
}

// WriteInput is the full-provenance write surface. Remember wraps it.
type WriteInput struct {
	Namespace  string
	Key        string
	Body       string
	Attributes map[string]any
	Author     string
	Writer     string
	TaskID     string
	SourceRef  string
	Confidence float64
	ExpiresAt  *time.Time
	Importance float64 // author-declared weight, clamped to [0,1] in Write
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not a recoverable condition
	}
	return hex.EncodeToString(b)
}

// ValidateKey enforces hierarchical /path/like keys so ListKeys prefix
// semantics stay meaningful.
func ValidateKey(key string) error {
	if !strings.HasPrefix(key, "/") || len(key) < 2 {
		return fmt.Errorf("memory: key %q must be a /path", key)
	}
	if strings.Contains(key, "//") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("memory: key %q has empty segment", key)
	}
	return nil
}

func (s *Store) ensureNamespace(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	if name == "" {
		return 0, errors.New("memory: empty namespace")
	}
	_, err := tx.ExecContext(ctx, s.db.Rebind(
		`INSERT INTO namespaces (name, created_at) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`),
		name, store.TimeToDB(s.now()))
	if err != nil {
		return 0, fmt.Errorf("ensure namespace: %w", err)
	}
	var id int64
	err = tx.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM namespaces WHERE name = $1`), name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("lookup namespace: %w", err)
	}
	return id, nil
}

func (s *Store) namespaceID(ctx context.Context, name string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM namespaces WHERE name = $1`), name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup namespace: %w", err)
	}
	return id, true, nil
}

// Remember appends a fact revision: action=add when the key has no live
// value, update otherwise.
func (s *Store) Remember(ctx context.Context, ns, key, body string, attrs map[string]any, author string) (*Fact, error) {
	return s.Write(ctx, WriteInput{Namespace: ns, Key: key, Body: body,
		Attributes: attrs, Author: author, Writer: author})
}

// Write appends a fact revision with full provenance. The predecessor
// revision is invalidated (invalid_at) in the same transaction: history
// is bi-temporal, never destroyed (P12.2). An outbox row rides the same
// commit for subscriptions (P12.3).
func (s *Store) Write(ctx context.Context, in WriteInput) (*Fact, error) {
	return s.write(ctx, in, true)
}

// writeNoOutbox is Write without the memory_outbox row: for enrichment
// passes (entity facts) that must never re-trigger the outbox tailer ->
// bus -> RunEnricher loop, the same way EnrichKey's direct SQL never does.
func (s *Store) writeNoOutbox(ctx context.Context, in WriteInput) (*Fact, error) {
	return s.write(ctx, in, false)
}

func (s *Store) write(ctx context.Context, in WriteInput, outbox bool) (*Fact, error) {
	if err := ValidateKey(in.Key); err != nil {
		return nil, err
	}
	// ponytail: body only; scrub attributes when a consumer stores secrets there
	mode := s.defenseMode(in.Namespace)
	var auditLabels, auditFPs []string
	switch mode {
	case "redact":
		scrubbed, labels, fps := ScrubAudited(in.Body)
		in.Body = scrubbed
		auditLabels, auditFPs = labels, fps
	case "block":
		_, labels, fps := ScrubAudited(in.Body)
		if len(labels) > 0 {
			// best-effort standalone audit row: a blocked write is still
			// audited, even though the fact itself is never stored.
			_ = s.db.WithTx(ctx, func(tx *sql.Tx) error {
				return s.outboxTx(ctx, tx, "defense", in.Namespace+":"+in.Key, map[string]any{
					"namespace": in.Namespace, "key": in.Key,
					"labels": strings.Join(labels, ","), "fingerprints": strings.Join(fps, ","),
				})
			})
			return nil, fmt.Errorf("%w: %v", ErrSensitiveContent, labels)
		}
	}
	attrsJSON := "{}"
	if len(in.Attributes) > 0 {
		raw, err := json.Marshal(in.Attributes)
		if err != nil {
			return nil, fmt.Errorf("marshal attributes: %w", err)
		}
		attrsJSON = string(raw)
	}

	if in.Importance < 0 {
		in.Importance = 0
	}
	if in.Importance > 1 {
		in.Importance = 1
	}
	now := s.now().UTC()
	f := &Fact{
		ID: newID(), Namespace: in.Namespace, Key: in.Key, Body: in.Body,
		Attributes: in.Attributes, Author: in.Author, CreatedAt: now,
		Writer: in.Writer, TaskID: in.TaskID, SourceRef: in.SourceRef,
		Confidence: in.Confidence, ValidAt: now, ExpiresAt: in.ExpiresAt,
		Importance: in.Importance, FeedbackWeight: 0.5, // matches the column default
	}
	var embedded []byte
	if s.embedder != nil {
		if vecs, err := s.embedder.Embed(ctx, []string{in.Body}); err == nil && len(vecs) == 1 {
			embedded = encodeVector(vecs[0], s.quantize)
		} else if err != nil {
			// embedding failure never blocks a write; backfill catches up
			embedded = nil
		}
	}
	var taskID, expires any
	if in.TaskID != "" {
		taskID = in.TaskID
	}
	if in.ExpiresAt != nil {
		expires = store.TimeToDB(*in.ExpiresAt)
	}
	hash := contentHash(in.Key, in.Body, attrsJSON)
	deduped := false
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		nsID, err := s.ensureNamespace(ctx, tx, in.Namespace)
		if err != nil {
			return err
		}
		// idempotent: if the current live revision has this exact content,
		// this is a duplicate concurrent write - do nothing but bump
		// reinforcements (B3: duplicate content is
		// independent corroboration, not a no-op).
		var liveID, liveHash, liveAction string
		herr := tx.QueryRowContext(ctx, s.db.Rebind(`
			SELECT id, content_hash, action FROM memories
			WHERE namespace_id = $1 AND key = $2
			ORDER BY created_at DESC, id DESC LIMIT 1`), nsID, in.Key).Scan(&liveID, &liveHash, &liveAction)
		if herr == nil && liveHash == hash && liveAction != "tombstone" {
			deduped = true
			_, err := tx.ExecContext(ctx, s.db.Rebind(
				`UPDATE memories SET reinforcements = reinforcements + 1 WHERE id = $1`), liveID)
			return err
		}
		action, err := s.liveAction(ctx, tx, nsID, in.Key)
		if err != nil {
			return err
		}
		f.Action = action
		// invalidate the predecessor's validity window
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`
			UPDATE memories SET invalid_at = $1
			WHERE namespace_id = $2 AND key = $3 AND invalid_at IS NULL`),
			store.TimeToDB(now), nsID, in.Key); err != nil {
			return fmt.Errorf("invalidate predecessor: %w", err)
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(
			`INSERT INTO memories (id, namespace_id, key, action, body, attributes, author, created_at,
			                       writer, task_id, source_ref, confidence, valid_at, expiration_date, embedding, content_hash, importance)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`),
			f.ID, nsID, in.Key, action, in.Body, attrsJSON, in.Author, store.TimeToDB(now),
			in.Writer, taskID, in.SourceRef, in.Confidence, store.TimeToDB(now), expires, embedded, hash, in.Importance)
		if err != nil {
			return fmt.Errorf("insert memory: %w", err)
		}
		if outbox {
			if err := s.outboxTx(ctx, tx, "memory", in.Namespace+":"+in.Key, map[string]any{
				"namespace": in.Namespace, "key": in.Key, "action": action, "writer": in.Writer,
			}); err != nil {
				return err
			}
		}
		if len(auditLabels) > 0 {
			return s.outboxTx(ctx, tx, "defense", in.Namespace+":"+in.Key, map[string]any{
				"namespace": in.Namespace, "key": in.Key,
				"labels": strings.Join(auditLabels, ","), "fingerprints": strings.Join(auditFPs, ","),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if deduped {
		// return the existing live fact rather than a phantom new one
		facts, rerr := s.Recall(ctx, in.Namespace, in.Key, 1)
		if rerr == nil && len(facts) == 1 {
			return &facts[0], nil
		}
	}
	return f, nil
}

// TouchAccess bumps access_count for search hits. Best-effort ranking
// signal: callers may ignore the error.
func (s *Store) TouchAccess(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	_, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memories SET access_count = access_count + 1 WHERE id IN (`+strings.Join(ph, ",")+`)`), args...)
	return err
}

// feedbackAlpha is the EWMA smoothing factor for RecordFeedback: each
// rating moves feedback_weight 30% of the way toward it.
const feedbackAlpha = 0.3

// RecordFeedback EWMA-updates feedback_weight for the given fact IDs,
// scoped to a namespace (a user rating on an answer folds back into
// the facts it drew on). rating is
// clamped to [0,1]; an unknown namespace is a no-op, not an error.
func (s *Store) RecordFeedback(ctx context.Context, ns string, ids []string, rating float64) error {
	if len(ids) == 0 {
		return nil
	}
	if rating < 0 {
		rating = 0
	} else if rating > 1 {
		rating = 1
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return err
	}
	if !ok {
		return nil // unknown namespace: nothing to update
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+3)
	args = append(args, rating, feedbackAlpha)
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+3)
		args = append(args, id)
	}
	args = append(args, nsID)
	nsPh := fmt.Sprintf("$%d", len(ids)+3)
	// ponytail: SQLite's MAX/MIN double as scalar clamp functions;
	// Postgres reserves MAX/MIN for aggregates and needs GREATEST/LEAST
	// instead — same driver-switch shape as Search's FTS dialects.
	expr := "feedback_weight + $2*($1 - feedback_weight)"
	clamp := "MAX(0, MIN(1, " + expr + "))"
	if s.db.Driver == "postgres" {
		clamp = "LEAST(1, GREATEST(0, " + expr + "))"
	}
	_, err = s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE memories SET feedback_weight = `+clamp+`
		 WHERE id IN (`+strings.Join(ph, ",")+`) AND namespace_id = `+nsPh), args...)
	return err
}

// liveAction decides add vs update by looking at the latest revision.
func (s *Store) liveAction(ctx context.Context, tx *sql.Tx, nsID int64, key string) (string, error) {
	var action string
	err := tx.QueryRowContext(ctx, s.db.Rebind(
		`SELECT action FROM memories WHERE namespace_id = $1 AND key = $2
		 ORDER BY created_at DESC, id DESC LIMIT 1`), nsID, key).Scan(&action)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "add", nil
	case err != nil:
		return "", fmt.Errorf("latest revision: %w", err)
	case action == "tombstone":
		return "add", nil
	default:
		return "update", nil
	}
}

// Forget appends a tombstone. Tombstoning a key that never lived is an
// error (duckbrain wrote a poisoned row here; we refuse instead).
func (s *Store) Forget(ctx context.Context, ns, key, author string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		nsID, err := s.ensureNamespace(ctx, tx, ns)
		if err != nil {
			return err
		}
		action, err := s.liveAction(ctx, tx, nsID, key)
		if err != nil {
			return err
		}
		if action == "add" { // no live value
			return fmt.Errorf("%w: %s %s", ErrNotFound, ns, key)
		}
		now := store.TimeToDB(s.now())
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`
			UPDATE memories SET invalid_at = $1
			WHERE namespace_id = $2 AND key = $3 AND invalid_at IS NULL`),
			now, nsID, key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(
			`INSERT INTO memories (id, namespace_id, key, action, body, attributes, author, created_at, writer, valid_at)
			 VALUES ($1, $2, $3, 'tombstone', '', '{}', $4, $5, $6, $5)`),
			newID(), nsID, key, author, now, author); err != nil {
			return err
		}
		return s.outboxTx(ctx, tx, "memory", ns+":"+key, map[string]any{
			"namespace": ns, "key": key, "action": "tombstone", "writer": author,
		})
	})
}

// likePrefix escapes LIKE wildcards in a key prefix.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(prefix) + "%"
}

// factCols are the columns every fact scan reads, in scanFactRow order.
const factCols = `m.id, m.key, m.action, m.body, m.attributes, m.author, m.created_at,
       COALESCE(m.writer,''), COALESCE(m.task_id,''), COALESCE(m.source_ref,''),
       COALESCE(m.confidence,0), COALESCE(m.valid_at, m.created_at), m.invalid_at, m.expiration_date,
       COALESCE(m.importance,0), COALESCE(m.access_count,0), COALESCE(m.feedback_weight,0.5),
       COALESCE(m.reinforcements,0)`

// latestLiveSQL selects the newest revision per key under a prefix,
// drops tombstoned keys and expired facts. $1 ns, $2 like-prefix,
// $3 now, $4 limit.
const latestLiveSQL = `
SELECT ` + factCols + `
FROM memories m
JOIN (
    SELECT key, MAX(created_at) AS mc
    FROM memories
    WHERE namespace_id = $1 AND key LIKE $2 ESCAPE '\'
    GROUP BY key
) latest ON m.key = latest.key AND m.created_at = latest.mc
WHERE m.namespace_id = $1 AND m.action <> 'tombstone'
  AND (m.expiration_date IS NULL OR m.expiration_date > $3)
ORDER BY m.key
LIMIT $4`

// Recall returns the latest live fact for every key under prefix.
// Query errors are returned, never swallowed into an empty slice.
func (s *Store) Recall(ctx context.Context, ns, prefix string, limit int) ([]Fact, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Fact{}, nil
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(latestLiveSQL),
		nsID, likePrefix(prefix), store.TimeToDB(s.now()), limit)
	if err != nil {
		return nil, fmt.Errorf("recall query: %w", err)
	}
	defer rows.Close()
	return s.scanFacts(ctx, ns, nsID, rows)
}

// liveByKeys returns the latest live fact for each exact key given
// (bridge discovery resolves candidate keys this way, not by prefix).
func (s *Store) liveByKeys(ctx context.Context, ns string, keys []string) ([]Fact, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Fact{}, nil
	}
	ph := make([]string, len(keys))
	args := []any{nsID}
	for i, k := range keys {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, k)
	}
	args = append(args, store.TimeToDB(s.now()))
	q := `
SELECT ` + factCols + `
FROM memories m
JOIN (
    SELECT key, MAX(created_at) AS mc
    FROM memories
    WHERE namespace_id = $1 AND key IN (` + strings.Join(ph, ",") + `)
    GROUP BY key
) latest ON m.key = latest.key AND m.created_at = latest.mc
WHERE m.namespace_id = $1 AND m.action <> 'tombstone'
  AND (m.expiration_date IS NULL OR m.expiration_date > $` + fmt.Sprintf("%d", len(keys)+2) + `)
ORDER BY m.key`
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("live by keys query: %w", err)
	}
	defer rows.Close()
	return s.scanFacts(ctx, ns, nsID, rows)
}

// RecallAsOf reads the facts that were valid at a past instant: the
// revision whose [valid_at, invalid_at) window covers asOf (P12.2).
// Tombstones end a window without starting one, so a key deleted before
// asOf simply does not appear.
func (s *Store) RecallAsOf(ctx context.Context, ns, prefix string, asOf time.Time, limit int) ([]Fact, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Fact{}, nil
	}
	at := store.TimeToDB(asOf)
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
SELECT `+factCols+`
FROM memories m
WHERE m.namespace_id = $1 AND m.key LIKE $2 ESCAPE '\'
  AND m.action <> 'tombstone'
  AND COALESCE(m.valid_at, m.created_at) <= $3
  AND (m.invalid_at IS NULL OR m.invalid_at > $3)
ORDER BY m.key
LIMIT $4`), nsID, likePrefix(prefix), at, limit)
	if err != nil {
		return nil, fmt.Errorf("recall as-of query: %w", err)
	}
	defer rows.Close()
	return s.scanFacts(ctx, ns, nsID, rows)
}

// scanFactRow reads one factCols row (+nExtra trailing columns returned
// to the caller). Errors that indicate a poisoned row surface as
// *rowError so scanFacts can quarantine instead of bricking reads.
type rowError struct{ id, raw, reason string }

func (e *rowError) Error() string { return e.reason }

func scanFactRow(rows *sql.Rows, nExtra int) (*Fact, []any, error) {
	var f Fact
	var attrsJSON, createdAt, validAt string
	var invalidAt, expiresAt sql.NullString
	extras := make([]any, nExtra)
	extraPtrs := make([]any, nExtra)
	for i := range extras {
		extraPtrs[i] = &extras[i]
	}
	dest := append([]any{&f.ID, &f.Key, &f.Action, &f.Body, &attrsJSON, &f.Author, &createdAt,
		&f.Writer, &f.TaskID, &f.SourceRef, &f.Confidence, &validAt, &invalidAt, &expiresAt,
		&f.Importance, &f.AccessCount, &f.FeedbackWeight, &f.Reinforcements}, extraPtrs...)
	if err := rows.Scan(dest...); err != nil {
		return nil, nil, fmt.Errorf("scan memory: %w", err)
	}
	t, err := store.TimeFromDB(createdAt)
	if err != nil {
		return nil, nil, &rowError{f.ID, createdAt, "bad created_at: " + err.Error()}
	}
	f.CreatedAt = t
	if f.ValidAt, err = store.TimeFromDB(validAt); err != nil {
		f.ValidAt = f.CreatedAt
	}
	parseNullT := func(ns sql.NullString, dst **time.Time) {
		if ns.Valid && ns.String != "" {
			if v, err := store.TimeFromDB(ns.String); err == nil {
				*dst = &v
			}
		}
	}
	parseNullT(invalidAt, &f.InvalidAt)
	parseNullT(expiresAt, &f.ExpiresAt)
	if attrsJSON != "" && attrsJSON != "{}" {
		if err := json.Unmarshal([]byte(attrsJSON), &f.Attributes); err != nil {
			return nil, nil, &rowError{f.ID, attrsJSON, "bad attributes json: " + err.Error()}
		}
	}
	return &f, extras, nil
}

// scanFacts decodes rows; malformed rows are quarantined and skipped, so
// one poisoned row can never brick a namespace (duckbrain lesson).
func (s *Store) scanFacts(ctx context.Context, ns string, nsID int64, rows *sql.Rows) ([]Fact, error) {
	out := []Fact{}
	var quarantine []*rowError
	for rows.Next() {
		f, _, err := scanFactRow(rows, 0)
		if err != nil {
			var re *rowError
			if errors.As(err, &re) {
				quarantine = append(quarantine, re)
				continue
			}
			return nil, err
		}
		f.Namespace = ns
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recall rows: %w", err)
	}
	for _, b := range quarantine {
		if err := s.quarantineRow(ctx, nsID, b.id, b.raw, b.reason); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) quarantineRow(ctx context.Context, nsID int64, id, raw, reason string) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(
			`INSERT INTO memories_quarantine (namespace_id, raw, reason, created_at) VALUES ($1, $2, $3, $4)`),
			nsID, id+": "+raw, reason, store.TimeToDB(s.now())); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM memories WHERE id = $1`), id)
		return err
	})
}

// ListKeys returns live keys under prefix: the anti-hallucination
// affordance - agents discover keys, they never invent them.
func (s *Store) ListKeys(ctx context.Context, ns, prefix string) ([]string, error) {
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []string{}, nil
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT m.key FROM memories m
		JOIN (
		    SELECT key, MAX(created_at) AS mc FROM memories
		    WHERE namespace_id = $1 AND key LIKE $2 ESCAPE '\'
		    GROUP BY key
		) latest ON m.key = latest.key AND m.created_at = latest.mc
		WHERE m.namespace_id = $1 AND m.action <> 'tombstone'
		ORDER BY m.key`), nsID, likePrefix(prefix))
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// Search runs full-text search over live fact bodies. Errors surface. On
// sqlite, query is sanitized (SanitizeFTS) into an OR-of-content-tokens
// FTS5 MATCH expression, ranked by bm25 (best match first) -- so a
// question like "what's wrong with the pool?" becomes "pool" OR-matched
// and ranked against every other surviving content token, not required to
// match verbatim. A query that sanitizes to no tokens at all (punctuation-
// only, e.g. "???") returns an empty result with a nil error, which is
// distinct from a blank/whitespace-only query ("   "), which still errors
// with "memory: empty search query" on both dialects.
func (s *Store) Search(ctx context.Context, ns, query string, limit int) ([]Fact, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("memory: empty search query")
	}
	var match, pgFn string
	switch s.db.Driver {
	case "sqlite":
		match, pgFn = SanitizeFTS(query), ""
	case "postgres":
		match, pgFn = SanitizeTSQuery(query), "to_tsquery"
	default:
		return nil, fmt.Errorf("memory: no FTS for driver %q", s.db.Driver)
	}
	return s.searchMatch(ctx, ns, match, pgFn, limit)
}

// SearchPhrase ranks live facts whose body contains phrase as adjacent
// tokens in order (FTS5 phrase syntax on sqlite, phraseto_tsquery on
// postgres). Blank phrase returns no rows, no error. Used by the anchor
// arm, where an exact identifier or error string must not be loosened
// into OR-of-tokens.
func (s *Store) SearchPhrase(ctx context.Context, ns, phrase string, limit int) ([]Fact, error) {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return []Fact{}, nil
	}
	switch s.db.Driver {
	case "sqlite":
		// FTS5 phrase: double-quoted, inner quotes doubled. The default
		// unicode61 tokenizer splits on punctuation, so "ERR_POOL_EXHAUSTED"
		// becomes the adjacent tokens err pool exhausted, which is exactly
		// the phrase we want.
		return s.searchMatch(ctx, ns, `"`+strings.ReplaceAll(phrase, `"`, `""`)+`"`, "", limit)
	case "postgres":
		return s.searchMatch(ctx, ns, phrase, "phraseto_tsquery", limit)
	default:
		return nil, fmt.Errorf("memory: no FTS for driver %q", s.db.Driver)
	}
}

// searchMatch runs the latest-live-revision FTS query. match is the
// driver-specific query text; pgFn is the postgres tsquery constructor
// (to_tsquery or phraseto_tsquery) and is ignored on sqlite. Empty match
// short-circuits to no rows.
func (s *Store) searchMatch(ctx context.Context, ns, match, pgFn string, limit int) ([]Fact, error) {
	if match == "" {
		return []Fact{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Fact{}, nil
	}
	var q string
	switch s.db.Driver {
	case "sqlite":
		q = `
SELECT ` + factCols + `
FROM memories_fts f
JOIN memories m ON m.rowid = f.rowid
WHERE memories_fts MATCH $2 AND m.namespace_id = $1 AND m.action <> 'tombstone'
  AND m.created_at = (SELECT MAX(created_at) FROM memories
                      WHERE namespace_id = m.namespace_id AND key = m.key)
  AND (m.expiration_date IS NULL OR m.expiration_date > $4)
ORDER BY rank
LIMIT $3`
	case "postgres":
		// pgFn is only ever one of two literals set inside this package,
		// never caller input, so concatenating it into the SQL is safe.
		// ORDER BY is load-bearing: downstream RRF fusion uses slice
		// position as rank. ts_rank is the bm25 analogue; key ASC is the
		// deterministic tie-break.
		q = `
SELECT ` + factCols + `
FROM memories m
WHERE m.body_tsv @@ ` + pgFn + `('english', $2)
  AND m.namespace_id = $1 AND m.action <> 'tombstone'
  AND m.created_at = (SELECT MAX(created_at) FROM memories
                      WHERE namespace_id = m.namespace_id AND key = m.key)
  AND (m.expiration_date IS NULL OR m.expiration_date > $4)
ORDER BY ts_rank(m.body_tsv, ` + pgFn + `('english', $2)) DESC, m.key ASC
LIMIT $3`
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(q), nsID, match, limit, store.TimeToDB(s.now()))
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()
	return s.scanFacts(ctx, ns, nsID, rows)
}

// SweepRetention deletes superseded revisions older than cutoff and dead
// (tombstoned) chains whose tombstone is older than cutoff. The latest
// live revision of every key is never touched.
func (s *Store) SweepRetention(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := store.TimeToDB(s.now().Add(-retention))
	var total int64
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		// superseded history
		res, err := tx.ExecContext(ctx, s.db.Rebind(`
			DELETE FROM memories WHERE created_at < $1 AND EXISTS (
			    SELECT 1 FROM memories m2
			    WHERE m2.namespace_id = memories.namespace_id
			      AND m2.key = memories.key
			      AND m2.created_at > memories.created_at)`), cutoff)
		if err != nil {
			return fmt.Errorf("sweep superseded: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
		// dead chains: latest revision is an old tombstone
		res, err = tx.ExecContext(ctx, s.db.Rebind(`
			DELETE FROM memories WHERE action = 'tombstone' AND created_at < $1 AND NOT EXISTS (
			    SELECT 1 FROM memories m2
			    WHERE m2.namespace_id = memories.namespace_id
			      AND m2.key = memories.key
			      AND m2.created_at > memories.created_at)`), cutoff)
		if err != nil {
			return fmt.Errorf("sweep tombstones: %w", err)
		}
		n, _ = res.RowsAffected()
		total += n
		// hard-expired facts (per-fact expiration_date) leave entirely
		res, err = tx.ExecContext(ctx, s.db.Rebind(`
			DELETE FROM memories WHERE expiration_date IS NOT NULL AND expiration_date < $1`),
			store.TimeToDB(s.now()))
		if err != nil {
			return fmt.Errorf("sweep expired: %w", err)
		}
		n, _ = res.RowsAffected()
		total += n
		return nil
	})
	return total, err
}
