package store

import (
	"context"
	"time"
)

// ApprovalRule is a persisted auto-allow/deny rule for tool use.
type ApprovalRule struct {
	ID         int64
	Scope      string // "global" | "project:<slug>" | "stage:<name>"
	ToolName   string // or "*"
	ArgMatcher string // glob over canonical arg; "" = any
	Decision   string // "allow" | "deny"
	Source     string // "default" | "project_config" | "user"
}

// ApprovalAudit is one recorded approval decision.
type ApprovalAudit struct {
	RunID, Stage, ToolName, ToolInputJSON, Decision, ResolvedBy, Reason string
	RequestedAt, ResolvedAt                                             int64
}

// InsertApprovalRule adds a rule and returns its id.
func (s *Store) InsertApprovalRule(ctx context.Context, r ApprovalRule) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO approval_rules (scope, tool_name, arg_matcher, decision, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.Scope, r.ToolName, nullString(r.ArgMatcher), r.Decision, r.Source, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListApprovalRules returns all persisted rules in id order.
func (s *Store) ListApprovalRules(ctx context.Context) ([]ApprovalRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, tool_name, COALESCE(arg_matcher, ''), decision, source FROM approval_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRule
	for rows.Next() {
		var r ApprovalRule
		if err := rows.Scan(&r.ID, &r.Scope, &r.ToolName, &r.ArgMatcher, &r.Decision, &r.Source); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertApproval writes an audit row.
func (s *Store) InsertApproval(ctx context.Context, a ApprovalAudit) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approvals (run_id, stage, tool_name, tool_input, decision, resolved_by, reason, requested_at, resolved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.RunID, a.Stage, a.ToolName, a.ToolInputJSON, a.Decision, a.ResolvedBy, nullString(a.Reason),
		a.RequestedAt, a.ResolvedAt)
	return err
}

// ListApprovals returns recent audit rows (most recent first), optionally
// filtered by run.
func (s *Store) ListApprovals(ctx context.Context, runID string, limit int) ([]ApprovalAudit, error) {
	q := `SELECT run_id, stage, tool_name, tool_input, decision, resolved_by, COALESCE(reason,''), requested_at, resolved_at FROM approvals`
	args := []any{}
	if runID != "" {
		q += ` WHERE run_id = ?`
		args = append(args, runID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalAudit
	for rows.Next() {
		var a ApprovalAudit
		if err := rows.Scan(&a.RunID, &a.Stage, &a.ToolName, &a.ToolInputJSON, &a.Decision,
			&a.ResolvedBy, &a.Reason, &a.RequestedAt, &a.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
