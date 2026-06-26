-- Phase 2b.4: persist the predictor.Result (JSON) and the waiting_on
-- list (JSON []string) on the run row.
--
-- prediction: full predictor.Result (Files, InlineCapsules, Overflow,
--   FullBundlePath, Metrics). Persisted so the scheduler tick can
--   re-hydrate it when a pending+waiting_on run becomes eligible
--   (no need to re-run Predict).
-- waiting_on: list of run IDs blocking this run. NULL means the run
--   is not blocked (either running, terminal, or freshly inserted
--   before the conflict check). Non-empty JSON array means the run
--   is queued.
--
-- The partial index targets the tick's re-eval scan (pending runs
-- with waiting_on populated).
ALTER TABLE runs ADD COLUMN prediction TEXT;
ALTER TABLE runs ADD COLUMN waiting_on TEXT;

CREATE INDEX idx_runs_pending_waiting
    ON runs(status)
    WHERE waiting_on IS NOT NULL;
