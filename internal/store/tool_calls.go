package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ToolCall is one row from the tool_calls table. Fields with pointer
// types are nullable (NULL distinguishable from zero).
type ToolCall struct {
	ID         int64
	RunID      string
	StageID    int64
	Name       string
	ArgsHash   string
	ArgsJSON   string
	StartedAt  int64
	EndedAt    *int64
	DurationMS *int
	Success    *int   // 0 = failed, 1 = ok, NULL = still running
	Decision   string // Phase 4: "allow" / "deny"; "" in 3.1
}

// InsertToolCall writes a new row. EndedAt / DurationMS / Success may
// be nil (call still running) — Phase 3.2 stall monitor exploits this.
// In 3.1, the pipeline persists completed rows at stage end so all
// three fields are populated on insert.
func (s *Store) InsertToolCall(ctx context.Context, tc *ToolCall) (int64, error) {
	var endedAt, durationMS, success, decision any
	if tc.EndedAt != nil {
		endedAt = *tc.EndedAt
	}
	if tc.DurationMS != nil {
		durationMS = *tc.DurationMS
	}
	if tc.Success != nil {
		success = *tc.Success
	}
	if tc.Decision != "" {
		decision = tc.Decision
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_calls
		   (run_id, stage_id, name, args_hash, args_json,
		    started_at, ended_at, duration_ms, success, decision)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tc.RunID, tc.StageID, tc.Name, tc.ArgsHash, tc.ArgsJSON,
		tc.StartedAt, endedAt, durationMS, success, decision,
	)
	if err != nil {
		return 0, fmt.Errorf("insert tool_call: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	tc.ID = id
	return id, nil
}

// ListToolCallsForStage returns all tool calls for one stage,
// ordered by started_at ASC.
func (s *Store) ListToolCallsForStage(ctx context.Context, stageID int64) ([]*ToolCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, stage_id, name, args_hash, args_json,
		        started_at, ended_at, duration_ms, success, decision
		   FROM tool_calls WHERE stage_id = ? ORDER BY started_at ASC`, stageID)
	if err != nil {
		return nil, fmt.Errorf("list tool calls for stage: %w", err)
	}
	defer rows.Close()
	var out []*ToolCall
	for rows.Next() {
		tc, err := scanToolCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// ListRunningToolCalls returns calls for runID with ended_at IS NULL.
// Phase 3.2's L2 stall monitor uses this to find calls that have been
// running too long.
func (s *Store) ListRunningToolCalls(ctx context.Context, runID string) ([]*ToolCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, stage_id, name, args_hash, args_json,
		        started_at, ended_at, duration_ms, success, decision
		   FROM tool_calls WHERE run_id = ? AND ended_at IS NULL
		   ORDER BY started_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list running tool calls: %w", err)
	}
	defer rows.Close()
	var out []*ToolCall
	for rows.Next() {
		tc, err := scanToolCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

func scanToolCall(r rowScanner) (*ToolCall, error) {
	var (
		tc       ToolCall
		endedAt  sql.NullInt64
		duration sql.NullInt64
		success  sql.NullInt64
		decision sql.NullString
	)
	if err := r.Scan(&tc.ID, &tc.RunID, &tc.StageID, &tc.Name, &tc.ArgsHash, &tc.ArgsJSON,
		&tc.StartedAt, &endedAt, &duration, &success, &decision); err != nil {
		return nil, err
	}
	if endedAt.Valid {
		v := endedAt.Int64
		tc.EndedAt = &v
	}
	if duration.Valid {
		v := int(duration.Int64)
		tc.DurationMS = &v
	}
	if success.Valid {
		v := int(success.Int64)
		tc.Success = &v
	}
	tc.Decision = decision.String
	return &tc, nil
}
