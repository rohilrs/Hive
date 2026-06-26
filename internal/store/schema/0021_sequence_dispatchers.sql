-- Sequenced-dispatcher Phase 2a: per-project sequenced dispatcher state +
-- per-task gate state. The dispatcher row exists once per project running in
-- sequenced mode; gate_state tracks a task's progress through the gate state
-- machine (none -> built -> pr_open -> awaiting_merge -> satisfied | skipped).
-- In Phase 2a gate_state stays 'none' (the engine that advances it lands 2b).
CREATE TABLE sequence_dispatchers (
    project_id         TEXT PRIMARY KEY REFERENCES projects(id),
    status             TEXT NOT NULL DEFAULT 'active',   -- active | paused | completed
    advancement_policy TEXT NOT NULL DEFAULT 'pr_opened',-- pr_opened (only impl in P2); human_merge/auto_merge_on_green/manual land P3
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

ALTER TABLE tasks ADD COLUMN gate_state TEXT NOT NULL DEFAULT 'none';
