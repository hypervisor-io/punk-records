-- Memory plane v2: provenance, temporal validity, expiry, embeddings,
-- and the durable outbox feeding subscriptions.
ALTER TABLE memories ADD COLUMN writer TEXT NOT NULL DEFAULT '';
ALTER TABLE memories ADD COLUMN task_id TEXT;
ALTER TABLE memories ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE memories ADD COLUMN confidence REAL;
ALTER TABLE memories ADD COLUMN valid_at TEXT;
ALTER TABLE memories ADD COLUMN invalid_at TEXT;
ALTER TABLE memories ADD COLUMN expiration_date TEXT;
ALTER TABLE memories ADD COLUMN embedding BYTEA;

CREATE TABLE memory_outbox (
    id BIGSERIAL PRIMARY KEY,
    kind TEXT NOT NULL,
    key TEXT NOT NULL,
    payload TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    delivered_at TEXT
);
CREATE INDEX idx_outbox_undelivered ON memory_outbox(id) WHERE delivered_at IS NULL;
