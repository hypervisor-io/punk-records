-- Dynamic inbox binding (T1): one explicit, per-address server-side
-- binding that tells the readers (punk hook inbox subprocesses and the
-- native wake listener) which namespace a session's inbox resolves to.
-- agent is the whole primary key - one binding per agent address, the
-- latest explicit registration wins (the region API upserts). The table
-- is deliberately independent of region_members: member expiry must
-- never silently delete a binding, so there is no FK and no cascade;
-- re-registering to a different namespace is what rewires it, not time.
-- Timestamps use the fixed-width UTC TEXT encoding shared by both engines.
CREATE TABLE agent_inbox_bindings (
    agent TEXT PRIMARY KEY,
    namespace TEXT NOT NULL,
    set_at TEXT NOT NULL
);
