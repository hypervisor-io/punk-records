ALTER TABLE memories ALTER COLUMN importance TYPE real;
ALTER TABLE memories ALTER COLUMN feedback_weight TYPE real;
ALTER TABLE memories ALTER COLUMN confidence TYPE real;
ALTER TABLE memory_links ALTER COLUMN weight TYPE real;
ALTER TABLE routing_decisions ALTER COLUMN propensity TYPE real;
