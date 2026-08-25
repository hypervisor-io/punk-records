-- Edge-sentence embedding (cognee-borrow): each link carries a one-sentence
-- NL restatement of the fact, embedded and independently retrievable so
-- triplet search can rank on relation relevance, not just endpoints.
ALTER TABLE memory_links ADD COLUMN description TEXT;
ALTER TABLE memory_links ADD COLUMN embedding BYTEA;
