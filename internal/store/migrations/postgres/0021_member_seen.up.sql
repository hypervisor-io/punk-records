-- Heartbeat: when a satellite last called a coordination tool, so a
-- quiet region can be told apart from a dead one.
ALTER TABLE region_members ADD COLUMN last_seen_at TEXT;
