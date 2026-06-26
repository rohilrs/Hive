CREATE TABLE approval_rules (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scope       TEXT NOT NULL,
    tool_name   TEXT NOT NULL,
    arg_matcher TEXT,
    decision    TEXT NOT NULL,
    source      TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_approval_rules_tool ON approval_rules(tool_name);

CREATE TABLE approvals (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id       TEXT NOT NULL,
    stage        TEXT NOT NULL,
    tool_name    TEXT NOT NULL,
    tool_input   TEXT NOT NULL,
    decision     TEXT NOT NULL,
    resolved_by  TEXT NOT NULL,
    reason       TEXT,
    requested_at INTEGER NOT NULL,
    resolved_at  INTEGER NOT NULL
);
CREATE INDEX idx_approvals_run ON approvals(run_id);
