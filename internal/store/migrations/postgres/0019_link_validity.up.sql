-- Temporal validity on links (agentmemory-borrow): edges get the same
-- bi-temporal treatment as facts. NULL valid_at on pre-existing rows means
-- "since created_at"; invalid_at closes an edge without deleting it.
ALTER TABLE memory_links ADD COLUMN valid_at TEXT;
ALTER TABLE memory_links ADD COLUMN invalid_at TEXT;
