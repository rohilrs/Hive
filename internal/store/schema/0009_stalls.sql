-- Phase 3.2: stall detection persistence.
--
-- One row per detection. L1 (heartbeat) rows get cleared_at set when
-- the next event arrives; L2 (tool-call timeout) rows are written with
-- cleared_at = detected_at (single-shot, no clear path — the subprocess
-- is SIGTERM'd immediately).
--
-- stage_id is nullable: NULL when the adapter didn't receive a stage
-- ID via StageRequest (older callers, tests). In production paths the
-- BuildPipeline always populates it post-BeginStage.
--
-- layer: 1 = event heartbeat (surface only), 2 = tool-call timeout
-- (kill), 3 = iteration loop (reserved for Phase 3.3).
--
-- action_taken: "surfaced" (L1), "killed_subprocess" (L2),
-- "escalated_model" / "marked_needs_attention" (Phase 3.3).
--
-- details_json: free-form JSON for the layer's metadata.
--   L1: {"last_event_at": <unix>, "elapsed_seconds": N}
--   L2: {"tool": "Bash", "tool_id": "...", "args": <args_json>, "elapsed_seconds": N}
--
-- FK omission on run_id / stage_id matches predictor_metrics /
-- predictor_accuracy / stages / tool_calls precedent: observability
-- data, orphan rows harmless.
--
-- idx_stalls_active is the partial index Phase 3.5 TUI uses to find
-- "what's stuck right now?" without scanning the whole table.
CREATE TABLE stalls (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        TEXT NOT NULL,
    stage_id      INTEGER,
    layer         INTEGER NOT NULL,
    detected_at   INTEGER NOT NULL,
    cleared_at    INTEGER,
    action_taken  TEXT,
    details_json  TEXT
);

CREATE INDEX idx_stalls_run ON stalls(run_id);
CREATE INDEX idx_stalls_active ON stalls(run_id) WHERE cleared_at IS NULL;
