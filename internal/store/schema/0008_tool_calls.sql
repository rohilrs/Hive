-- Phase 3.1: per-tool-call audit log.
--
-- One row per tool invocation by the worker (Read, Edit, Bash, etc.).
-- Populated by the BuildPipeline at stage end from
-- StageOutput.ToolCalls, which the adapter reconstructs by pairing
-- tool_use + tool_result events from the claude stream.
--
-- args_hash is a stable short hash (sha256 first 16 hex chars) of the
-- raw args_json bytes as the provider reported them. Phase 4's approval
-- rules can match on it but must accept that semantically-equivalent
-- calls with different JSON formatting (key ordering, whitespace) will
-- hash differently. Match on args_json directly if exact-args matching
-- matters more than hash speed.
--
-- ended_at + duration_ms + success are NULL while the call is running.
-- For 3.1 we persist all rows at stage end (always complete), but the
-- nullable columns let Phase 3.2's stall monitor write rows
-- incrementally if we ever move to live persistence.
--
-- decision is NULL in 3.1 (no approval engine yet); Phase 4 populates
-- 'allow' / 'deny'.
--
-- The idx_tool_calls_running partial index targets Phase 3.2's L2
-- stall scan: SELECT ... WHERE ended_at IS NULL AND started_at < ?.
--
-- Specifically: tool_calls.stage_id has NO foreign key to stages.id by
-- design. The stages table uses INSERT OR REPLACE semantics for retry
-- (Phase 1 design supports re-running the same iter). With an FK +
-- ON DELETE CASCADE, a retry would delete the prior iter's tool_calls
-- as a side-effect of replacing the stage row — losing retry history.
-- Phase 4 authors: do NOT add a FK here unless retry semantics change.
CREATE TABLE tool_calls (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      TEXT NOT NULL,
    stage_id    INTEGER NOT NULL,
    name        TEXT NOT NULL,
    args_hash   TEXT NOT NULL,
    args_json   TEXT NOT NULL,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    duration_ms INTEGER,
    success     INTEGER,
    decision    TEXT
);

CREATE INDEX idx_tool_calls_stage ON tool_calls(stage_id);
CREATE INDEX idx_tool_calls_running ON tool_calls(run_id) WHERE ended_at IS NULL;
