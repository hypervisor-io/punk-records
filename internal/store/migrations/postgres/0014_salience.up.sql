-- Salience (AutoMem-informed): importance is author-declared weight,
-- access_count is bumped on search hits. Both feed ranking only —
-- facts are never deleted for low salience.
ALTER TABLE memories ADD COLUMN importance REAL NOT NULL DEFAULT 0;
ALTER TABLE memories ADD COLUMN access_count INTEGER NOT NULL DEFAULT 0;
