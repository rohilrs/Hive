-- Phase 1 schema. Other tables (tool_calls, approvals, approval_rules,
-- predictions, chat_*, stalls) land in their respective phases.

CREATE TABLE projects (
    id           TEXT PRIMARY KEY,
    slug         TEXT UNIQUE NOT NULL,
    name         TEXT NOT NULL,
    repo_path    TEXT,                   -- nullable: no-repo projects allowed
    sources      TEXT NOT NULL DEFAULT '{}',   -- JSON: linear, github, inbox
    config       TEXT NOT NULL DEFAULT '{}',   -- JSON: project overrides
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE tasks (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(id),
    source              TEXT NOT NULL,         -- 'linear' | 'github' | 'inbox'
    source_id           TEXT,
    title               TEXT NOT NULL,
    body                TEXT NOT NULL DEFAULT '',
    priority            TEXT NOT NULL DEFAULT 'P3',
    status              TEXT NOT NULL DEFAULT 'pending',
    predicted_files     TEXT,                  -- JSON array
    conflict_set        TEXT,                  -- JSON array
    predict_confidence  INTEGER,
    metadata            TEXT NOT NULL DEFAULT '{}',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);
CREATE INDEX idx_tasks_status         ON tasks(status);
CREATE INDEX idx_tasks_project_status ON tasks(project_id, status);

CREATE TABLE runs (
    id                          TEXT PRIMARY KEY,
    task_id                     TEXT NOT NULL REFERENCES tasks(id),
    project_id                  TEXT NOT NULL REFERENCES projects(id),
    pipeline                    TEXT NOT NULL,
    status                      TEXT NOT NULL DEFAULT 'pending',
    started_at                  INTEGER,
    ended_at                    INTEGER,
    total_cost_usd              REAL NOT NULL DEFAULT 0,
    summary                     TEXT NOT NULL DEFAULT '',
    documentation_skipped       INTEGER NOT NULL DEFAULT 0,
    documentation_skip_reason   TEXT,
    created_at                  INTEGER NOT NULL
);
CREATE INDEX idx_runs_status      ON runs(status);
CREATE INDEX idx_runs_project     ON runs(project_id, status);

CREATE TABLE stages (
    id                  TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL REFERENCES runs(id),
    name                TEXT NOT NULL,
    iter                INTEGER NOT NULL DEFAULT 0,
    model               TEXT NOT NULL,
    started_at          INTEGER NOT NULL,
    ended_at            INTEGER,
    tokens_in           INTEGER NOT NULL DEFAULT 0,
    tokens_out          INTEGER NOT NULL DEFAULT 0,
    cache_hit_tokens    INTEGER NOT NULL DEFAULT 0,
    verdict             TEXT NOT NULL DEFAULT '',
    verdict_confidence  INTEGER NOT NULL DEFAULT 0,
    cost_usd            REAL NOT NULL DEFAULT 0
);
CREATE INDEX idx_stages_run ON stages(run_id);

CREATE TABLE events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id    TEXT NOT NULL,
    stage_id  TEXT,
    ts        INTEGER NOT NULL,
    type      TEXT NOT NULL,
    message   TEXT NOT NULL DEFAULT '',
    payload   TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_events_run    ON events(run_id);
CREATE INDEX idx_events_run_ts ON events(run_id, ts);

-- FTS5 virtual table mirrors events.message for full-text search.
CREATE VIRTUAL TABLE events_fts USING fts5(
    message,
    content=events,
    content_rowid=id,
    tokenize='unicode61'
);

CREATE TRIGGER events_ai AFTER INSERT ON events BEGIN
    INSERT INTO events_fts(rowid, message) VALUES (new.id, new.message);
END;
CREATE TRIGGER events_ad AFTER DELETE ON events BEGIN
    INSERT INTO events_fts(events_fts, rowid, message) VALUES('delete', old.id, old.message);
END;
CREATE TRIGGER events_au AFTER UPDATE ON events BEGIN
    INSERT INTO events_fts(events_fts, rowid, message) VALUES('delete', old.id, old.message);
    INSERT INTO events_fts(rowid, message) VALUES (new.id, new.message);
END;
