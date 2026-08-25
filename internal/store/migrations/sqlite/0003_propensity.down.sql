CREATE TABLE routing_decisions_old (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    inputs TEXT NOT NULL DEFAULT '{}',
    candidates TEXT NOT NULL DEFAULT '[]',
    chosen_agent TEXT NOT NULL DEFAULT '',
    method TEXT NOT NULL CHECK (method IN ('rule','llm','fallback')),
    rule_id TEXT,
    created_at TEXT NOT NULL
);
INSERT INTO routing_decisions_old (id, task_id, inputs, candidates, chosen_agent, method, rule_id, created_at)
    SELECT id, task_id, inputs, candidates, chosen_agent, method, rule_id, created_at
    FROM routing_decisions WHERE method IN ('rule','llm','fallback');
DROP INDEX IF EXISTS idx_routing_task;
DROP TABLE routing_decisions;
ALTER TABLE routing_decisions_old RENAME TO routing_decisions;
CREATE INDEX idx_routing_task ON routing_decisions(task_id);
