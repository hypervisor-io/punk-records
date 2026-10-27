-- Rollback drops only the visibility column; the binding itself
-- (agent, namespace, set_at) and every row survive untouched. A column
-- rollback loses the setter information, never a binding.
ALTER TABLE agent_inbox_bindings DROP COLUMN set_by;
