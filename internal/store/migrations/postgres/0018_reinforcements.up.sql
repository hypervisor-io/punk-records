-- Reinforcement (agentmemory-borrow): a duplicate write of identical content
-- bumps this counter in place instead of silently no-oping. Feeds ranking
-- only, like access_count.
ALTER TABLE memories ADD COLUMN reinforcements INTEGER NOT NULL DEFAULT 0;
