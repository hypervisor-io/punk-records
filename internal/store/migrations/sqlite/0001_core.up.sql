-- Punk Records core schema (SQLite dialect). Timestamps are fixed-width
-- RFC3339 UTC TEXT (store.TimeToDB) on both engines.

CREATE TABLE namespaces (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    namespace_id INTEGER NOT NULL REFERENCES namespaces(id),
    key TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('add','update','tombstone')),
    body TEXT NOT NULL DEFAULT '',
    attributes TEXT NOT NULL DEFAULT '{}',
    author TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_memories_ns_key ON memories(namespace_id, key, created_at);

CREATE TABLE memories_quarantine (
    id INTEGER PRIMARY KEY,
    namespace_id INTEGER,
    raw TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE VIRTUAL TABLE memories_fts USING fts5(body, content='memories');

CREATE TRIGGER memories_fts_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, body) VALUES (new.rowid, new.body);
END;

CREATE TRIGGER memories_fts_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, body) VALUES ('delete', old.rowid, old.body);
END;

CREATE TABLE agent_versions (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    version TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    source_path TEXT NOT NULL DEFAULT '',
    loaded_at TEXT NOT NULL,
    active INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_agent_versions_name ON agent_versions(name, loaded_at);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    external_ref TEXT,
    source TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'investigate',
    status TEXT NOT NULL CHECK (status IN ('submitted','working','input_required','completed','failed','canceled')),
    agent_name TEXT NOT NULL DEFAULT '',
    agent_version_id INTEGER REFERENCES agent_versions(id),
    parent_task_id TEXT REFERENCES tasks(id),
    handoff_contract TEXT NOT NULL DEFAULT 'results_only'
        CHECK (handoff_contract IN ('results_only','summary','full_trace')),
    labels TEXT NOT NULL DEFAULT '{}',
    budget_tokens INTEGER NOT NULL DEFAULT 0,
    budget_tool_calls INTEGER NOT NULL DEFAULT 0,
    budget_wall_ms INTEGER NOT NULL DEFAULT 0,
    budget_subagents INTEGER NOT NULL DEFAULT 0,
    spent_tokens INTEGER NOT NULL DEFAULT 0,
    spent_tool_calls INTEGER NOT NULL DEFAULT 0,
    started_at TEXT,
    lease_expires_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_tasks_open_ref ON tasks(external_ref)
    WHERE external_ref IS NOT NULL AND status IN ('submitted','working','input_required');
CREATE INDEX idx_tasks_status ON tasks(status, created_at);

CREATE TABLE task_events (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    seq INTEGER NOT NULL,
    type TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT 'system',
    payload TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    UNIQUE (task_id, seq)
);

CREATE TABLE routing_decisions (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    inputs TEXT NOT NULL DEFAULT '{}',
    candidates TEXT NOT NULL DEFAULT '[]',
    chosen_agent TEXT NOT NULL DEFAULT '',
    method TEXT NOT NULL CHECK (method IN ('rule','llm','fallback')),
    rule_id TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_routing_task ON routing_decisions(task_id);

CREATE TABLE proposals (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    action_class TEXT NOT NULL,
    target TEXT NOT NULL,
    params TEXT NOT NULL DEFAULT '{}',
    status TEXT NOT NULL CHECK (status IN ('proposed','submitted','approved','rejected','expired')),
    external_ref TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_proposals_task ON proposals(task_id);

CREATE TABLE llm_calls (
    id INTEGER PRIMARY KEY,
    task_id TEXT REFERENCES tasks(id),
    profile TEXT NOT NULL,
    model TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_llm_calls_task ON llm_calls(task_id);
