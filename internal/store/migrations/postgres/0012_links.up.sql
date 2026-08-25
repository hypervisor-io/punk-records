-- Typed links between facts (dexdat pole): a graph over the brain, for
-- file -> change -> agent and cross-key relationships.
CREATE TABLE memory_links (
    id BIGSERIAL PRIMARY KEY,
    namespace TEXT NOT NULL,
    from_key TEXT NOT NULL,
    to_key TEXT NOT NULL,
    link_type TEXT NOT NULL DEFAULT 'relates_to',
    created_at TEXT NOT NULL,
    UNIQUE (namespace, from_key, to_key, link_type)
);
CREATE INDEX idx_links_from ON memory_links(namespace, from_key);
CREATE INDEX idx_links_to ON memory_links(namespace, to_key);
