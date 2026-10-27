-- Binding visibility (P3-D): who set an inbox binding. set_at has been
-- recorded since 0027; set_by names the setter (an agent address, an
-- API-key subject, ...). Rows written before this migration - and
-- setters that do not name themselves, like every server call site
-- today - carry the default '' = unknown, which the hook's routing
-- block renders honestly (no "by" clause, never a fabricated name).
-- Plain ADD COLUMN with a constant default: one statement that works
-- on both engines, no table rewrite on either.
ALTER TABLE agent_inbox_bindings ADD COLUMN set_by TEXT NOT NULL DEFAULT '';
