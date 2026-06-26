-- Phase 7 hive decompose: link sub-tasks to their parent task.
ALTER TABLE tasks ADD COLUMN parent_task_id TEXT NULL;

CREATE INDEX IF NOT EXISTS tasks_parent_task_id_idx
  ON tasks(parent_task_id)
  WHERE parent_task_id IS NOT NULL;
