CREATE TABLE chat_sessions (
    id             TEXT PRIMARY KEY,
    surface        TEXT NOT NULL,
    started_at     INTEGER NOT NULL,
    ended_at       INTEGER,
    total_cost_usd REAL NOT NULL DEFAULT 0
);
CREATE TABLE chat_messages (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    role         TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    tool_calls   TEXT,
    tool_results TEXT,
    tokens_in    INTEGER NOT NULL DEFAULT 0,
    tokens_out   INTEGER NOT NULL DEFAULT 0,
    cost_usd     REAL NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_chat_messages_session ON chat_messages(session_id, created_at);
