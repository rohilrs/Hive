-- 0018: chat_sessions.kind discriminator for planner sessions (Phase 8.A).
-- 'chat' = regular chat (existing behavior); 'plan' = roadmap planner mode.
ALTER TABLE chat_sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'chat';
CREATE INDEX idx_chat_sessions_kind ON chat_sessions(kind, started_at DESC);
