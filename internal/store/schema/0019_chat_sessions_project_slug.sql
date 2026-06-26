-- 0019: chat_sessions.project_slug for planner sessions (Phase 8.A T6).
-- The planner system prompt + planner read/write tools need the project's
-- slug (for spec listing/scoping) and the project's repo_path as cwd. We
-- persist the slug here so resumed sessions can re-derive their cwd via
-- a projects lookup. Nullable: regular chat sessions don't need it.
ALTER TABLE chat_sessions ADD COLUMN project_slug TEXT NULL;
