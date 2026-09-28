-- Messaging delivery diagnostics: exactly one latest bridge observation
-- per namespace member (primary key), replaced on every report, so the
-- table is bounded by membership and never grows as an attempt ledger.
-- Snapshots describe what a bridge last reported; they never prove model
-- receipt and never override region_members liveness or agent_messages
-- ACK state. Member removal deletes the row in the same transaction.
-- Enumerations are validated by the server so new states stay additive;
-- timestamps use the fixed-width UTC TEXT encoding shared by both engines,
-- and empty string means "not reported".
CREATE TABLE agent_message_diagnostics (
    namespace TEXT NOT NULL,
    agent TEXT NOT NULL,
    client TEXT NOT NULL,
    delivery_mode TEXT NOT NULL,
    state TEXT NOT NULL,
    last_attempt_at TEXT NOT NULL DEFAULT '',
    next_attempt_at TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    pending_ack_count INTEGER NOT NULL DEFAULT 0 CHECK (pending_ack_count >= 0),
    wake_count INTEGER NOT NULL DEFAULT 0 CHECK (wake_count >= 0),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (namespace, agent)
);
