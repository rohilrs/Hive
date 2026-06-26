-- Phase 2b.0: per-iteration reviewer feedback (FileRefs) keyed by run+iter.
-- Read by BuildPipeline before each iter>0 implement stage to enumerate
-- the prior review's flagged issues into the implement system prompt.

CREATE TABLE iter_feedback (
    run_id      TEXT NOT NULL,
    iter        INTEGER NOT NULL,
    file_refs   TEXT NOT NULL,    -- JSON array of FileRef objects
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (run_id, iter)
);
