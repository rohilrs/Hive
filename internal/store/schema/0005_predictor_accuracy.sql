-- Phase 2c.1: per-run predictor accuracy (precision/recall).
--
-- Computed asynchronously in executePipeline after MarkRunEnded by
-- comparing runs.prediction.Files (predicted set) against
-- `git diff --name-only main..HEAD` in the run's worktree (touched
-- set). One row per completed run.
--
-- Rows are inserted even when compute was skipped (no_prediction,
-- no_predictions_files, no_edits, no_worktree, git_diff_failed)
-- so `hive predict accuracy backfill` can distinguish "not yet
-- attempted" (no row) from "attempted and skipped" (row with
-- skipped_reason set). When skipped, precision_ / recall_ are NULL
-- and the count columns reflect what was knowable (e.g., touched
-- count = 0 for no_edits; predicted count = 0 for no_predictions_files).
--
-- Deliberate FK omission on run_id matches the predictor_metrics
-- convention (observability data; orphan rows after a run delete
-- are harmless; the table will grow indefinitely anyway pending a
-- separate operator cleanup story).
--
-- Column naming: precision_ and recall_ have trailing underscores
-- per spec to signal "computed metric" and avoid any future SQL
-- keyword adjacency. SQLite doesn't actually reserve them today.
CREATE TABLE predictor_accuracy (
    run_id          TEXT PRIMARY KEY,
    precision_      REAL,
    recall_         REAL,
    predicted_count INTEGER NOT NULL,
    touched_count   INTEGER NOT NULL,
    intersect_count INTEGER NOT NULL,
    computed_at     INTEGER NOT NULL,
    skipped_reason  TEXT
);

CREATE INDEX idx_accuracy_computed ON predictor_accuracy(computed_at);
