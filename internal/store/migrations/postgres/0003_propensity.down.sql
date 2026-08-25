DELETE FROM routing_decisions WHERE method = 'explore';
ALTER TABLE routing_decisions DROP CONSTRAINT routing_decisions_method_check;
ALTER TABLE routing_decisions ADD CONSTRAINT routing_decisions_method_check
    CHECK (method IN ('rule','llm','fallback'));
ALTER TABLE routing_decisions DROP COLUMN propensity;
