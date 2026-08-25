-- Brain regions (namespaces agents register to) + membership (satellites
-- connected to Punk Records).
CREATE TABLE regions (
    id BIGSERIAL PRIMARY KEY,
    namespace TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE region_members (
    id BIGSERIAL PRIMARY KEY,
    namespace TEXT NOT NULL,
    agent TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT '',
    joined_at TEXT NOT NULL,
    UNIQUE (namespace, agent)
);
CREATE INDEX idx_region_members_ns ON region_members(namespace);
CREATE INDEX idx_region_members_agent ON region_members(agent);
