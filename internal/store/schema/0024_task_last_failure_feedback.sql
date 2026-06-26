-- JSON blob of a build run's final feedback (summary + file_refs + exhaust_reason),
-- written when the run gives up (needs_attention) and injected into the iter-0
-- implement prompt of the next run for this task. Cleared on success.
ALTER TABLE tasks ADD COLUMN last_failure_feedback TEXT NOT NULL DEFAULT '';
