-- Rollback drops only diagnostic snapshots; members, messages and ACKs stay.
DROP TABLE agent_message_diagnostics;
