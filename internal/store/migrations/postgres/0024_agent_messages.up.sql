-- Durable agent inboxes (task M1): addressed messages between members
-- of one namespace. seq gives a stable delivery order independent of
-- clock ties; id is the public message id. Unread = acked_at IS NULL.
-- The in-process bus only carries wake-up hints; this table is the
-- source of truth for delivery and catch-up.
CREATE TABLE agent_messages (
    seq BIGSERIAL PRIMARY KEY,
    id TEXT NOT NULL UNIQUE,
    namespace TEXT NOT NULL,
    sender TEXT NOT NULL,
    recipient TEXT NOT NULL,
    body TEXT NOT NULL,
    task_id TEXT NOT NULL DEFAULT '',
    reply_to TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT,
    created_at TEXT NOT NULL,
    acked_at TEXT
);
-- Retry deduplication: one message per (namespace, sender, key); keyless
-- sends never collide.
CREATE UNIQUE INDEX agent_messages_idempotency ON agent_messages(namespace, sender, idempotency_key) WHERE idempotency_key IS NOT NULL;
-- Inbox reads: a recipient's unread messages in delivery order.
CREATE INDEX agent_messages_unread ON agent_messages(namespace, recipient, seq) WHERE acked_at IS NULL;
