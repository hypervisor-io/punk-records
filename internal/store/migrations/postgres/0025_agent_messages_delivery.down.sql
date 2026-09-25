-- Rollback drops delivery metadata, never messages or ACKs.
DROP INDEX agent_messages_retention;
DROP INDEX agent_messages_sent;
ALTER TABLE agent_messages DROP COLUMN leased_by;
ALTER TABLE agent_messages DROP COLUMN leased_until;
