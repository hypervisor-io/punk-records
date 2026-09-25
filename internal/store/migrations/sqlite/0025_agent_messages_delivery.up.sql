-- Expand only: existing messages keep their bodies, order and ACK state.
ALTER TABLE agent_messages ADD COLUMN leased_until TEXT;
ALTER TABLE agent_messages ADD COLUMN leased_by TEXT;
CREATE INDEX agent_messages_sent ON agent_messages(namespace, sender, seq);
CREATE INDEX agent_messages_retention ON agent_messages(namespace, acked_at) WHERE acked_at IS NOT NULL;
