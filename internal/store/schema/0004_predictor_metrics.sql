-- Phase 2b.5: denormalized per-dispatch predictor metrics for fast
-- `hive predict stats` aggregation.
--
-- This duplicates fields already present in runs.prediction (JSON,
-- added in migration 0003) but flattens them into typed columns so
-- `hive predict stats` can run vanilla SQL aggregations + partial-index
-- scans instead of SQLite JSON1 queries (slow and brittle).
--
-- One row per Predict invocation. Joined on run_id when needed.
-- Inserts are best-effort from dispatch (errors logged, dispatch
-- continues) so this table never blocks the critical path.
--
-- Spec deviation: tokens_in/tokens_out columns from spec migration
-- 0009 omitted — neither claudecli nor anthropic SDK currently parses
-- usage events. Adding them is a future migration once a provider
-- surfaces the data.
--
-- Deliberate FK omission: run_id and project_id intentionally do NOT
-- have REFERENCES constraints. Metrics are observability data; orphan
-- rows after a run/project deletion are harmless and the table will
-- grow indefinitely anyway (cleanup is a separate operator concern).
-- If we ever add aggressive run/project deletion, revisit.
CREATE TABLE predictor_metrics (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id           TEXT NOT NULL,
    project_id       TEXT NOT NULL,
    haiku_latency_ms INTEGER,
    fetch_latency_ms INTEGER,
    candidate_count  INTEGER,
    inline_count     INTEGER,
    overflow_count   INTEGER,
    truncated        INTEGER,    -- 0 or 1
    error            TEXT,       -- empty string when no error
    created_at       INTEGER NOT NULL
);

CREATE INDEX idx_pm_project_created
    ON predictor_metrics(project_id, created_at);
