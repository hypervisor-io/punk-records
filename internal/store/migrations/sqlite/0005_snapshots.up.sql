-- Full tool-call results for offline replay (ledger events clip results;
-- replay needs the frozen world verbatim).
CREATE TABLE task_snapshots (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    seq INTEGER NOT NULL,
    tool TEXT NOT NULL,
    args TEXT NOT NULL DEFAULT '{}',
    result TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ok',
    created_at TEXT NOT NULL,
    UNIQUE (task_id, seq)
);
