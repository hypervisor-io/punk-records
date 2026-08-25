DROP INDEX IF EXISTS idx_memories_content;
ALTER TABLE memories DROP COLUMN content_hash;
