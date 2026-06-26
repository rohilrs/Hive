-- Phase 3.1: per-stage observability foundation.
--
-- Replaces the initial stub stages table (TEXT PK, no UNIQUE constraint,
-- non-nullable tokens/verdict/cost) with the production schema:
--
-- - INTEGER PRIMARY KEY AUTOINCREMENT for efficient joins + LastInsertId
-- - UNIQUE (run_id, name, iter) enables INSERT OR REPLACE retry semantics
-- - model, tokens_in/out/cache_hit, verdict, verdict_confidence, cost_usd
--   are all nullable — NULL == "no data" (distinguishable from zero)
-- - idx_stages_started added for time-range queries
-- - FK omission on run_id matches predictor_metrics / predictor_accuracy
--   precedent (observability data; orphans harmless)
--
-- The initial schema had FK REFERENCES runs(id) on run_id (TEXT PK).
-- We drop and recreate without that FK — consistent with the observability
-- table convention established in migrations 0004 and 0005.
DROP TABLE IF EXISTS stages;

CREATE TABLE stages (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id             TEXT NOT NULL,
    name               TEXT NOT NULL,
    iter               INTEGER NOT NULL,
    model              TEXT,
    started_at         INTEGER NOT NULL,
    ended_at           INTEGER,
    tokens_in          INTEGER,
    tokens_out         INTEGER,
    cache_hit_tokens   INTEGER,
    verdict            TEXT,
    verdict_confidence REAL,
    cost_usd           REAL,
    UNIQUE (run_id, name, iter)
);

CREATE INDEX idx_stages_run ON stages(run_id);
CREATE INDEX idx_stages_started ON stages(started_at);
