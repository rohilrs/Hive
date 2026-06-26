-- Linear write-back Phase 1: the last logical Linear workflow state this task's
-- mirrored Linear issue was pushed to (todo|in_progress|in_review|blocked|done|
-- canceled). Empty = never pushed. Stored as a logical label (not a team state-id)
-- so it is decoupled from per-team workflow configuration. The Linear issue id is
-- tasks.source_id (with source='linear').
ALTER TABLE tasks ADD COLUMN linear_synced_state TEXT NOT NULL DEFAULT '';
