-- No-op on sqlite: its REAL storage class is always an 8-byte IEEE
-- float, so the float4 truncation this migration fixes on Postgres
-- (see the postgres pair) cannot occur here. Version kept in lockstep.
SELECT 1;
