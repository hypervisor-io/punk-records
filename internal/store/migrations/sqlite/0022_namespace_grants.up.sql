-- Namespace authorization grants (task A01): a verified API-key subject
-- holds exact-match read/write/admin permissions per namespace. The
-- table is inert unless authz.enforcement: deny is configured.
CREATE TABLE namespace_grants (
    id INTEGER PRIMARY KEY,
    subject TEXT NOT NULL,
    namespace TEXT NOT NULL,
    op TEXT NOT NULL,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);
-- One active grant per (subject, namespace, op); revoked rows keep
-- history without blocking re-grants.
CREATE UNIQUE INDEX namespace_grants_active ON namespace_grants(subject, namespace, op) WHERE revoked_at IS NULL;
