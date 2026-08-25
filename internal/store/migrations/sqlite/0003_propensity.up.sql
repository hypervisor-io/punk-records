-- propensity column + 'explore' routing method (SQLite cannot alter a
-- CHECK constraint, so rebuild the table)
CREATE TABLE routing_decisions_new (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    inputs TEXT NOT NULL DEFAULT '{}',
    candidates TEXT NOT NULL DEFAULT '[]',
    chosen_agent TEXT NOT NULL DEFAULT '',
    method TEXT NOT NULL CHECK (method IN ('rule','llm','explore','fallback')),
    rule_id TEXT,
    propensity REAL NOT NULL DEFAULT 1.0,
    created_at TEXT NOT NULL
);
INSERT INTO routing_decisions_new (id, task_id, inputs, candidates, chosen_agent, method, rule_id, created_at)
    SELECT id, task_id, inputs, candidates, chosen_agent, method, rule_id, created_at FROM routing_decisions;
DROP INDEX IF EXISTS idx_routing_task;
DROP TABLE routing_decisions;
ALTER TABLE routing_decisions_new RENAME TO routing_decisions;
CREATE INDEX idx_routing_task ON routing_decisions(task_id);
