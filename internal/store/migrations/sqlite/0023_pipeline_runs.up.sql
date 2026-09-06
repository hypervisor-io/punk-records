-- Enrichment pipeline run records (task P01): one row per unit of work,
-- keyed by (namespace, stage, stage_version, source_key, source_revision),
-- tracking pending/running/succeeded/failed/superseded, attempts,
-- timestamps, derived-output counts and a sanitized error class. Raw
-- error text is never stored, so status surfaces can expose failures
-- without leaking secrets.
CREATE TABLE memory_pipeline_runs (
    id INTEGER PRIMARY KEY,
    namespace TEXT NOT NULL,
    stage TEXT NOT NULL,
    stage_version INTEGER NOT NULL,
    source_key TEXT NOT NULL,
    source_revision TEXT NOT NULL,
    status TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    items INTEGER,
    error_class TEXT,
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);
-- The work key: at-least-once delivery collapses onto one row per unit
-- of work; a changed source revision or stage version is a new row,
-- never an overwrite of the old run's lineage.
CREATE UNIQUE INDEX memory_pipeline_runs_work ON memory_pipeline_runs(namespace, stage, stage_version, source_key, source_revision);
-- Restart recovery and stale-worker reclaim: interrupted or
-- lease-expired running runs are found by (status, started_at).
CREATE INDEX memory_pipeline_runs_running ON memory_pipeline_runs(status, started_at);
-- Status endpoint reads per namespace with optional stage/status filters.
CREATE INDEX memory_pipeline_runs_ns ON memory_pipeline_runs(namespace, stage, status);
