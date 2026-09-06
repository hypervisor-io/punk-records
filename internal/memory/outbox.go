package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// outboxTx enqueues a change event in the SAME transaction as the fact
// write: at-least-once delivery without LISTEN/NOTIFY's commit-serializing
// lock (GAPS2 C1). The tailer drains it.
func (s *Store) outboxTx(ctx context.Context, tx *sql.Tx, kind, key string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.db.Rebind(`
		INSERT INTO memory_outbox (kind, key, payload, created_at)
		VALUES ($1, $2, $3, $4)`),
		kind, key, string(raw), store.TimeToDB(s.now()))
	if err != nil {
		return fmt.Errorf("outbox enqueue: %w", err)
	}
	return nil
}

// OutboxEvent is one drained row.
type OutboxEvent struct {
	ID      int64
	Kind    string
	Key     string
	Payload map[string]string
}

// DrainOutbox delivers undelivered rows oldest-first through deliver,
// marking each delivered on success. Returns the number delivered.
// A failed delivery stops the batch (retried next tick): ordering holds.
// Memory events have their durable pipeline work persisted BEFORE the
// delivery is acknowledged (P01): the bus deliver publishes to is
// in-memory only, so without that row a crash after the delivered_at
// mark but before any consumer claimed the work would leave restart
// recovery with nothing to find. The persisted row, not the ephemeral
// publish, is the delivery boundary.
func (s *Store) DrainOutbox(ctx context.Context, limit int, deliver func(OutboxEvent) error) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`
		SELECT id, kind, key, payload FROM memory_outbox
		WHERE delivered_at IS NULL ORDER BY id LIMIT $1`), limit)
	if err != nil {
		return 0, err
	}
	var events []OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		var payload string
		if err := rows.Scan(&e.ID, &e.Kind, &e.Key, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		_ = json.Unmarshal([]byte(payload), &e.Payload)
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range events {
		if err := s.persistStageWork(ctx, e); err != nil {
			return n, err
		}
		if err := deliver(e); err != nil {
			return n, err
		}
		if _, err := s.db.ExecContext(ctx, s.db.Rebind(`
			UPDATE memory_outbox SET delivered_at = $1 WHERE id = $2`),
			store.TimeToDB(s.now()), e.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// persistStageWork records the durable stage intent for one drained
// event before its delivery is acknowledged: pending pipeline runs
// tied to the live source revision and current stage versions for a
// memory write, or retirement of pending work for a tombstoned key.
// Non-memory events (defense, task) carry no pipeline stages. An error
// stops the batch before the deliver and the delivered_at mark, so the
// event is redelivered next tick rather than acknowledged without its
// durable work.
func (s *Store) persistStageWork(ctx context.Context, e OutboxEvent) error {
	if e.Kind != "memory" {
		return nil
	}
	ns, key := e.Payload["namespace"], e.Payload["key"]
	if ns == "" || key == "" {
		return nil
	}
	if e.Payload["action"] == "tombstone" {
		return s.supersedePendingPipelineRuns(ctx, ns, key)
	}
	return s.persistPipelineWork(ctx, ns, key)
}

// RunOutboxTailer polls the outbox and fans events into the bus until
// ctx ends. The bus feeds MCP subscriptions and the SSE handler.
func (s *Store) RunOutboxTailer(ctx context.Context, b *bus.Bus, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_, err := s.DrainOutbox(ctx, 100, func(e OutboxEvent) error {
				b.Publish(bus.Event{Kind: e.Kind, Key: e.Key, Data: e.Payload})
				return nil
			})
			if err != nil && ctx.Err() == nil {
				log.Error("outbox drain failed", "err", err)
			}
		}
	}
}
