-- Work-claim leases: a satellite claims a sub-key before working it so
-- others skip it. Append-only history; released_at closes a claim.
CREATE TABLE work_claims (
    id BIGSERIAL PRIMARY KEY,
    namespace TEXT NOT NULL,
    key TEXT NOT NULL,
    holder TEXT NOT NULL,
    claimed_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    released_at TEXT
);
CREATE INDEX idx_claims_active ON work_claims(namespace, key) WHERE released_at IS NULL;
