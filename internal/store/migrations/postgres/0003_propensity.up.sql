ALTER TABLE routing_decisions ADD COLUMN propensity REAL NOT NULL DEFAULT 1.0;
ALTER TABLE routing_decisions DROP CONSTRAINT routing_decisions_method_check;
ALTER TABLE routing_decisions ADD CONSTRAINT routing_decisions_method_check
    CHECK (method IN ('rule','llm','explore','fallback'));
