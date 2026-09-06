package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Pipeline run records (task P01): every enrichment stage execution is
// tracked as one row in memory_pipeline_runs, keyed by work key
// (namespace, stage, stage_version, source_key, source_revision).
// At-least-once delivery collapses onto that row: a redelivery of the
// same revision's work is a no-op claim, and a crash between stage
// output and acknowledgment leaves the run non-terminal so recovery
// retries it - safe because the stages themselves are idempotent
// (EnrichKey's linkTargetsAll guard, the entity apply's mention guard)
// and fenced TRANSACTIONALLY at the write boundary (fencedStageTx): the
// derived writes commit in the same transaction that re-verifies both
// the active claim (run still running at the claiming attempt) and the
// source revision's liveness - and on Postgres LOCKS the source key's
// head revision row, so a concurrent source Write or Forget serializes
// behind the fence transaction instead of slipping a newer revision in
// between its check and its commit - so a worker that lost its claim -
// or read an older revision - never writes derived state a replacement
// attempt or a newer revision's run owns. The durable row is also the delivery
// boundary: the outbox drain persists pending work before acknowledging
// a delivery (persistPipelineWork), and recovery drains pending rows
// too, so no crash window between the ephemeral bus publish and the
// claim loses work. A changed source revision or stage version is a NEW
// row, so stale work never overwrites a newer run's lineage.

// Stage names recorded in memory_pipeline_runs.
const (
	StageEmbedLink = "embed_link"
	StageEntities  = "entities"
)

// Stage versions: bump when a stage's derivation logic changes so every
// source revision gets a fresh run under the new version.
const (
	embedLinkStageVersion = 1
	entitiesStageVersion  = 1
)

// Run statuses.
const (
	RunPending    = "pending"
	RunRunning    = "running"
	RunSucceeded  = "succeeded"
	RunFailed     = "failed"
	RunSuperseded = "superseded"
)

const (
	// pipelineRunLease bounds how long a run may stay running before
	// recovery presumes the claiming worker dead and requeues it.
	pipelineRunLease = 5 * time.Minute
	// pipelineMaxAttempts bounds automatic retries of a failed run; an
	// exhausted run stays failed and visible instead of retrying forever.
	pipelineMaxAttempts = 3
	// pipelineRecoveryInterval is how often the enricher re-checks for
	// interrupted or retryable runs (startup recovery always runs once).
	pipelineRecoveryInterval = time.Minute
)

// PipelineRun is one persisted enrichment stage execution record.
type PipelineRun struct {
	ID             int64      `json:"id"`
	Namespace      string     `json:"namespace"`
	Stage          string     `json:"stage"`
	StageVersion   int        `json:"stage_version"`
	SourceKey      string     `json:"source_key"`
	SourceRevision string     `json:"source_revision"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	Items          *int64     `json:"items,omitempty"`
	ErrorClass     string     `json:"error_class,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

// classifyStageError maps an error to a small fixed vocabulary. The raw
// message is deliberately never recorded: upstream model/API errors can
// echo credentials or request bodies, and run rows are shown on status
// endpoints. fallback names the failure domain the call site knows
// ("model" for extraction, "store" for database work).
func classifyStageError(err error, fallback string) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return fallback
	}
}

const runCols = `id, namespace, stage, stage_version, source_key, source_revision,
       status, attempts, items, COALESCE(error_class,''), created_at, started_at, finished_at`

func scanRun(row interface{ Scan(...any) error }) (PipelineRun, error) {
	var r PipelineRun
	var items sql.NullInt64
	var created string
	var started, finished sql.NullString
	err := row.Scan(&r.ID, &r.Namespace, &r.Stage, &r.StageVersion, &r.SourceKey, &r.SourceRevision,
		&r.Status, &r.Attempts, &items, &r.ErrorClass, &created, &started, &finished)
	if err != nil {
		return r, err
	}
	if items.Valid {
		r.Items = &items.Int64
	}
	if r.CreatedAt, err = store.TimeFromDB(created); err != nil {
		return r, fmt.Errorf("run %d created_at: %w", r.ID, err)
	}
	parse := func(ns sql.NullString, dst **time.Time) error {
		if ns.Valid && ns.String != "" {
			v, err := store.TimeFromDB(ns.String)
			if err != nil {
				return err
			}
			*dst = &v
		}
		return nil
	}
	if err := parse(started, &r.StartedAt); err != nil {
		return r, fmt.Errorf("run %d started_at: %w", r.ID, err)
	}
	if err := parse(finished, &r.FinishedAt); err != nil {
		return r, fmt.Errorf("run %d finished_at: %w", r.ID, err)
	}
	return r, nil
}

// claimPipelineWork resolves the live revision of ns/key and claims the
// run for (stage, stageVersion) against it. Pending runs keyed by a
// stale revision are superseded (a newer revision's run does the work;
// the stale row stays visible, never executed against newer state).
// claimed=false means another worker owns the run or it is already
// terminal - the caller must not execute. The returned attempt is the
// claim token finishPipelineRun checks so a stale worker cannot
// overwrite a newer claim's outcome. A key with no live revision
// (tombstoned or gone) claims nothing and records no run.
func (s *Store) claimPipelineWork(ctx context.Context, ns, stage string, stageVersion int, key string) (*PipelineRun, int, bool, error) {
	live, err := s.liveByKeys(ctx, ns, []string{key})
	if err != nil {
		return nil, 0, false, err
	}
	if len(live) == 0 {
		return nil, 0, false, nil
	}
	rev := live[0].ID
	var claimed bool
	var run PipelineRun
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		now := store.TimeToDB(s.now())
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`
			UPDATE memory_pipeline_runs SET status = 'superseded', finished_at = $1
			WHERE namespace = $2 AND stage = $3 AND stage_version = $4 AND source_key = $5
			  AND source_revision <> $6 AND status = 'pending'`), now, ns, stage, stageVersion, key, rev); err != nil {
			return fmt.Errorf("supersede stale runs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`
			INSERT INTO memory_pipeline_runs
				(namespace, stage, stage_version, source_key, source_revision, status, created_at)
			VALUES ($1, $2, $3, $4, $5, 'pending', $6)
			ON CONFLICT DO NOTHING`), ns, stage, stageVersion, key, rev, now); err != nil {
			return fmt.Errorf("ensure run: %w", err)
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`
			UPDATE memory_pipeline_runs
			SET status = 'running', attempts = attempts + 1, started_at = $1,
			    finished_at = NULL, error_class = NULL, items = NULL
			WHERE namespace = $2 AND stage = $3 AND stage_version = $4
			  AND source_key = $5 AND source_revision = $6 AND status = 'pending'`),
			now, ns, stage, stageVersion, key, rev)
		if err != nil {
			return fmt.Errorf("claim run: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // owned by another worker or terminal
		}
		claimed = true
		run, err = scanRun(tx.QueryRowContext(ctx, s.db.Rebind(`
			SELECT `+runCols+` FROM memory_pipeline_runs
			WHERE namespace = $1 AND stage = $2 AND stage_version = $3
			  AND source_key = $4 AND source_revision = $5`), ns, stage, stageVersion, key, rev))
		return err
	})
	if err != nil || !claimed {
		return nil, 0, false, err
	}
	return &run, run.Attempts, true, nil
}

// persistPipelineWork records the durable stage intent for the live
// revision of ns/key: one pending run row per configured stage, keyed
// by the work key. The outbox drain calls this BEFORE a delivery is
// acknowledged, which makes the durable row - not the ephemeral bus
// publish - the delivery boundary: a crash after the delivered_at mark
// (or after the publish, before any consumer claimed) leaves work
// recoverPipelineRuns drains instead of losing it. ON CONFLICT DO
// NOTHING collapses redelivery onto the same row. A key with no live
// revision (tombstoned or gone) records nothing, mirroring the claim
// path.
func (s *Store) persistPipelineWork(ctx context.Context, ns, key string) error {
	live, err := s.liveByKeys(ctx, ns, []string{key})
	if err != nil {
		return err
	}
	if len(live) == 0 {
		return nil
	}
	rev := live[0].ID
	now := store.TimeToDB(s.now())
	type stage struct {
		name    string
		version int
	}
	var stages []stage
	if s.embedder != nil {
		stages = append(stages, stage{StageEmbedLink, embedLinkStageVersion})
	}
	if s.entityExtractor != nil {
		stages = append(stages, stage{StageEntities, entitiesStageVersion})
	}
	for _, st := range stages {
		if _, err := s.db.ExecContext(ctx, s.db.Rebind(`
			INSERT INTO memory_pipeline_runs
				(namespace, stage, stage_version, source_key, source_revision, status, created_at)
			VALUES ($1, $2, $3, $4, $5, 'pending', $6)
			ON CONFLICT DO NOTHING`), ns, st.name, st.version, key, rev, now); err != nil {
			return fmt.Errorf("persist %s work for %s: %w", st.name, key, err)
		}
	}
	return nil
}

// supersedePendingPipelineRuns retires durable pending work for a key
// whose live revision is gone (its tombstone drained before any claim):
// there is nothing left to derive from, and the rows must not sit
// pending where every recovery pass retries them forever. The fence is
// the CURRENT live revision: a tombstone event retires only pending work
// keyed to revisions that are no longer live. A key recreated before the
// tombstone's drain batch reached it (add, forget, re-add, then one
// drain) has pending work keyed to the NEW live revision - the old
// tombstone must not retire it, and the recreate's own drained add can
// not reactivate a superseded row (ON CONFLICT DO NOTHING), so retiring
// it here would strand the recreated fact's work forever.
func (s *Store) supersedePendingPipelineRuns(ctx context.Context, ns, key string) error {
	live, err := s.liveByKeys(ctx, ns, []string{key})
	if err != nil {
		return err
	}
	now := store.TimeToDB(s.now())
	if len(live) == 0 {
		if _, err := s.db.ExecContext(ctx, s.db.Rebind(`
			UPDATE memory_pipeline_runs SET status = 'superseded', finished_at = $1
			WHERE namespace = $2 AND source_key = $3 AND status = 'pending'`), now, ns, key); err != nil {
			return fmt.Errorf("supersede pending runs for %s: %w", key, err)
		}
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.db.Rebind(`
		UPDATE memory_pipeline_runs SET status = 'superseded', finished_at = $1
		WHERE namespace = $2 AND source_key = $3 AND status = 'pending'
		  AND source_revision <> $4`), now, ns, key, live[0].ID); err != nil {
		return fmt.Errorf("supersede pending runs for %s: %w", key, err)
	}
	return nil
}

// runFence identifies the active claim a stage's derived writes must
// still belong to at write time: the run row and the attempt token from
// claimPipelineWork, plus the source revision the claim resolved.
type runFence struct {
	runID   int64
	attempt int
	rev     string
}

// errRunFenceTripped aborts a fencedStageTx transaction without an
// error: the claim was lost or the source revision moved on between the
// stage's reads and its write boundary.
var errRunFenceTripped = errors.New("memory: pipeline run fence tripped")

// fencedStageTx runs fn in one transaction iff the fence still holds at
// write time: the run row is still running at the claiming attempt (the
// guarded no-op UPDATE doubles as the row lock - a concurrent reclaim or
// re-claim must wait for this transaction, so the checks and fn's writes
// are one atomic unit) AND rev is still the key's live source revision.
// On Postgres the liveness read locks the key's head revision row (see
// liveRevisionTx), so a concurrent source Write or Forget blocks until
// this transaction commits: the revision this transaction verified
// cannot change underneath it, making the fence a commit-time guarantee
// rather than a check-then-write race. applied=false means the fence
// tripped: fn never ran and nothing was written. This replaces the
// check-before-write approximation at the actual write boundary;
// finishPipelineRun's attempt guard alone protects only the status row,
// not the derived facts and links.
func (s *Store) fencedStageTx(ctx context.Context, ns, key string, fence runFence, fn func(tx *sql.Tx) error) (bool, error) {
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`
			UPDATE memory_pipeline_runs SET started_at = started_at
			WHERE id = $1 AND status = 'running' AND attempts = $2`), fence.runID, fence.attempt)
		if err != nil {
			return fmt.Errorf("fence claim check: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errRunFenceTripped
		}
		live, err := s.liveRevisionTx(ctx, tx, ns, key)
		if err != nil {
			return err
		}
		if live != fence.rev {
			return errRunFenceTripped
		}
		return fn(tx)
	})
	if errors.Is(err, errRunFenceTripped) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// liveRevisionTx returns the key's current live revision ID within a
// caller-owned transaction ("" when tombstoned, expired, or gone) -
// liveByKeys' revision rule at the fenced write boundary. The head
// revision is the key's one never-invalidated row (invalid_at IS NULL):
// writeTx and Forget invalidate the head in the same transaction that
// installs its successor, so on Postgres this read locks that row
// (FOR UPDATE) and any concurrent revision bump of the key must wait
// for the caller's transaction to finish - the liveness answer cannot
// go stale between the check and the commit. SQLite takes no lock: the
// single-connection store serializes writers in-process, and it rejects
// the FOR UPDATE syntax.
func (s *Store) liveRevisionTx(ctx context.Context, tx *sql.Tx, ns, key string) (string, error) {
	var nsID int64
	err := tx.QueryRowContext(ctx, s.db.Rebind(
		`SELECT id FROM namespaces WHERE name = $1`), ns).Scan(&nsID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup namespace: %w", err)
	}
	lock := ""
	if s.db.Driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var id, action string
	var expires sql.NullString
	err = tx.QueryRowContext(ctx, s.db.Rebind(`
		SELECT m.id, m.action, m.expiration_date FROM memories m
		WHERE m.namespace_id = $1 AND m.key = $2 AND m.invalid_at IS NULL
		ORDER BY m.created_at DESC, m.id DESC LIMIT 1`)+lock,
		nsID, key).Scan(&id, &action, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("live revision of %s: %w", key, err)
	}
	if action == "tombstone" {
		return "", nil
	}
	if expires.Valid && expires.String != "" && expires.String <= store.TimeToDB(s.now()) {
		return "", nil
	}
	return id, nil
}

// revisionStillLive reports whether rev is still the live revision of
// ns/key. Stage workers check it at the apply boundary - immediately
// before writing derived state - because finishPipelineRun's attempt
// guard protects only the run's status row, not the derived facts and
// links themselves: without this fence a worker that extracted from an
// older revision would still emit that revision's derived state after
// a newer revision's run finished.
func (s *Store) revisionStillLive(ctx context.Context, ns, key, rev string) (bool, error) {
	live, err := s.liveByKeys(ctx, ns, []string{key})
	if err != nil {
		return false, err
	}
	return len(live) == 1 && live[0].ID == rev, nil
}

// finishPipelineRun records a run's terminal state. The guard on
// (status='running', attempts=attempt) makes a stale worker's late
// finish a no-op once its run was reclaimed: applied=false then.
func (s *Store) finishPipelineRun(ctx context.Context, runID int64, attempt int, status string, items *int64, errClass string) (bool, error) {
	var itemsAny, errAny any
	if items != nil {
		itemsAny = *items
	}
	if errClass != "" {
		errAny = errClass
	}
	res, err := s.db.ExecContext(ctx, s.db.Rebind(`
		UPDATE memory_pipeline_runs SET status = $1, items = $2, error_class = $3, finished_at = $4
		WHERE id = $5 AND status = 'running' AND attempts = $6`),
		status, itemsAny, errAny, store.TimeToDB(s.now()), runID, attempt)
	if err != nil {
		return false, fmt.Errorf("finish run %d: %w", runID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// requeueStalePipelineRuns moves running runs whose lease expired back
// to pending and returns the rows actually flipped (restart recovery /
// stale-worker reclaim). A run that finished between the select and the
// update keeps its terminal state.
func (s *Store) requeueStalePipelineRuns(ctx context.Context, staleBefore time.Time) ([]PipelineRun, error) {
	return s.requeueRuns(ctx, `status = 'running' AND started_at < $1`, store.TimeToDB(staleBefore))
}

// requeueFailedPipelineRuns gives failed runs with remaining attempt
// budget another try; exhausted runs stay failed and visible.
func (s *Store) requeueFailedPipelineRuns(ctx context.Context, maxAttempts int) ([]PipelineRun, error) {
	return s.requeueRuns(ctx, `status = 'failed' AND attempts < $1`, maxAttempts)
}

func (s *Store) requeueRuns(ctx context.Context, where string, arg any) ([]PipelineRun, error) {
	var out []PipelineRun
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, s.db.Rebind(
			`SELECT `+runCols+` FROM memory_pipeline_runs WHERE `+where+` ORDER BY id`), arg)
		if err != nil {
			return err
		}
		var cands []PipelineRun
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				rows.Close()
				return err
			}
			cands = append(cands, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range cands {
			res, err := tx.ExecContext(ctx, s.db.Rebind(`
				UPDATE memory_pipeline_runs SET status = 'pending'
				WHERE id = $1 AND status = $2`), r.ID, r.Status)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

// pendingPipelineRuns lists durable pending work: rows persisted ahead
// of an outbox acknowledgment, or flipped back to pending by a requeue
// whose processing then crashed. Recovery must drain these too, or a
// crash between the requeue and the processing strands them forever -
// nothing else ever looks at pending rows.
func (s *Store) pendingPipelineRuns(ctx context.Context) ([]PipelineRun, error) {
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(
		`SELECT `+runCols+` FROM memory_pipeline_runs WHERE status = 'pending' ORDER BY id`))
	if err != nil {
		return nil, fmt.Errorf("list pending runs: %w", err)
	}
	defer rows.Close()
	var out []PipelineRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// recoverPipelineRuns requeues interrupted (lease-expired running) and
// retryable failed runs and lists durable pending work (persisted ahead
// of an outbox acknowledgment, or left pending by a requeue that
// crashed before processing), then processes all of it through the
// normal claim path: claimPipelineWork supersedes runs whose source
// revision moved on, and the fenced idempotent stages make
// re-execution safe. Called once at enricher startup (restart
// recovery) and on the recovery tick.
func (s *Store) recoverPipelineRuns(ctx context.Context, log *slog.Logger) {
	stale, err := s.requeueStalePipelineRuns(ctx, s.now().Add(-pipelineRunLease))
	if err != nil && ctx.Err() == nil {
		log.Error("pipeline stale-run recovery failed", "err", err)
	}
	failed, err := s.requeueFailedPipelineRuns(ctx, pipelineMaxAttempts)
	if err != nil && ctx.Err() == nil {
		log.Error("pipeline failed-run recovery failed", "err", err)
	}
	pending, err := s.pendingPipelineRuns(ctx)
	if err != nil && ctx.Err() == nil {
		log.Error("pipeline pending-run recovery failed", "err", err)
	}
	type work struct{ ns, key string }
	seen := map[string]bool{}
	var embedWork, entityWork []work
	for _, r := range append(append(stale, failed...), pending...) {
		id := r.Stage + "\x00" + r.Namespace + "\x00" + r.SourceKey
		if seen[id] {
			continue
		}
		seen[id] = true
		switch r.Stage {
		case StageEmbedLink:
			embedWork = append(embedWork, work{r.Namespace, r.SourceKey})
		case StageEntities:
			entityWork = append(entityWork, work{r.Namespace, r.SourceKey})
		}
	}
	for _, w := range embedWork {
		s.runEmbedLinkStage(ctx, w.ns, w.key, log)
	}
	byNs := map[string][]string{}
	for _, w := range entityWork {
		byNs[w.ns] = append(byNs[w.ns], w.key)
	}
	for ns, keys := range byNs {
		s.flushEntityStage(ctx, ns, keys, log)
	}
}

// ListPipelineRuns reports runs for a namespace, newest first, with
// optional exact stage/status filters. Powers the status endpoint.
func (s *Store) ListPipelineRuns(ctx context.Context, ns, stage, status string, limit int) ([]PipelineRun, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT ` + runCols + ` FROM memory_pipeline_runs WHERE namespace = $1`
	args := []any{ns}
	if stage != "" {
		args = append(args, stage)
		q += fmt.Sprintf(" AND stage = $%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status = $%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("list pipeline runs: %w", err)
	}
	defer rows.Close()
	out := []PipelineRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
