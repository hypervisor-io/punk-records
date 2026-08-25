-- Link weight (AutoMem-informed): author/pass-declared edge strength
-- feeds bridge-discovery contribution in scored search.
ALTER TABLE memory_links ADD COLUMN weight REAL NOT NULL DEFAULT 1.0;
