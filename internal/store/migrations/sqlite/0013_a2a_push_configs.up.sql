-- A2A push-notification configs: webhooks the server calls on task updates.
-- One task may register several configs, each identified by config_id.
CREATE TABLE a2a_push_configs (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL,
    config_id TEXT NOT NULL,
    url TEXT NOT NULL,
    token TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE (task_id, config_id)
);
CREATE INDEX idx_a2a_push_task ON a2a_push_configs(task_id);
