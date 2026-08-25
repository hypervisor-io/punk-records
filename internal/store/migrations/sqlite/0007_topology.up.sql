-- Minimal service topology (GAPS2 C2#7): the 5-element model every
-- vendor converges on. No CMDB ambitions.
CREATE TABLE services (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    namespace TEXT NOT NULL DEFAULT 'default',
    owner TEXT NOT NULL DEFAULT '',
    oncall_ref TEXT NOT NULL DEFAULT '',
    tier INTEGER NOT NULL DEFAULT 3,
    updated_at TEXT NOT NULL,
    UNIQUE (name, namespace)
);

CREATE TABLE service_edges (
    id INTEGER PRIMARY KEY,
    from_service TEXT NOT NULL,
    to_service TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'declared' CHECK (kind IN ('declared','observed')),
    updated_at TEXT NOT NULL,
    UNIQUE (from_service, to_service, kind)
);
