-- Postgres REAL is float4: scoring/salience values written as float64
-- (0.65, 0.9) come back truncated (0.6499999761581421), corrupting
-- EWMA feedback math and importance round-trips that are exact on
-- sqlite (whose REAL is always 8-byte). Promote every float column the
-- memory engine reads back into ranking to double precision.
ALTER TABLE memories ALTER COLUMN importance TYPE double precision;
ALTER TABLE memories ALTER COLUMN feedback_weight TYPE double precision;
ALTER TABLE memories ALTER COLUMN confidence TYPE double precision;
ALTER TABLE memory_links ALTER COLUMN weight TYPE double precision;
ALTER TABLE routing_decisions ALTER COLUMN propensity TYPE double precision;
