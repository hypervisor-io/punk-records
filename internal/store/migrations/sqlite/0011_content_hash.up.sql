ALTER TABLE memories ADD COLUMN content_hash TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_memories_content ON memories(namespace_id, key, content_hash);
