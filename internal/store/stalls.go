package store

import (
	"context"
	"database/sql"
	"fmt"
)

// InsertStall writes a new stalls row. Caller populates ClearedAt only
// for single-shot stalls (e.g., L2 kills); L1 rows are inserted with
// ClearedAt nil and updated later via ClearStall when events resume.
func (s *Store) InsertStall(ctx context.Context, st *Stall) (int64, error) {
	var stageID, clearedAt, action, details any
	if st.StageID != nil {
		stageID = *st.StageID
	}
	if st.ClearedAt != nil {
		clearedAt = *st.ClearedAt
	}
	if st.ActionTaken != "" {
		action = st.ActionTaken
	}
	if st.DetailsJSON != "" {
		details = st.DetailsJSON
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO stalls
		   (run_id, stage_id, layer, detected_at, cleared_at, action_taken, details_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		st.RunID, stageID, st.Layer, st.DetectedAt, clearedAt, action, details,
	)
	if err != nil {
		return 0, fmt.Errorf("insert stall: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	st.ID = id
	return id, nil
}

// ClearStall sets cleared_at on an active stall row. Used by L1 when
// events resume after a heartbeat gap.
func (s *Store) ClearStall(ctx context.Context, id, clearedAt int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE stalls SET cleared_at = ? WHERE id = ? AND cleared_at IS NULL`,
		clearedAt, id,
	)
	if err != nil {
		return fmt.Errorf("clear stall: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListStallsForRun returns every stall row for a run, ordered by
// detected_at ASC.
func (s *Store) ListStallsForRun(ctx context.Context, runID string) ([]*Stall, error) {
	return s.queryStalls(ctx,
		`SELECT id, run_id, stage_id, layer, detected_at, cleared_at, action_taken, details_json
		   FROM stalls WHERE run_id = ? ORDER BY detected_at ASC`, runID)
}

// ListActiveStallsForRun returns only stall rows with cleared_at IS NULL.
// Backed by the idx_stalls_active partial index.
func (s *Store) ListActiveStallsForRun(ctx context.Context, runID string) ([]*Stall, error) {
	return s.queryStalls(ctx,
		`SELECT id, run_id, stage_id, layer, detected_at, cleared_at, action_taken, details_json
		   FROM stalls WHERE run_id = ? AND cleared_at IS NULL ORDER BY detected_at ASC`, runID)
}

func (s *Store) queryStalls(ctx context.Context, query string, args ...any) ([]*Stall, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query stalls: %w", err)
	}
	defer rows.Close()
	var out []*Stall
	for rows.Next() {
		st, err := scanStall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func scanStall(r rowScanner) (*Stall, error) {
	var (
		st        Stall
		stageID   sql.NullInt64
		clearedAt sql.NullInt64
		action    sql.NullString
		details   sql.NullString
	)
	if err := r.Scan(&st.ID, &st.RunID, &stageID, &st.Layer,
		&st.DetectedAt, &clearedAt, &action, &details); err != nil {
		return nil, err
	}
	if stageID.Valid {
		v := stageID.Int64
		st.StageID = &v
	}
	if clearedAt.Valid {
		v := clearedAt.Int64
		st.ClearedAt = &v
	}
	st.ActionTaken = action.String
	st.DetailsJSON = details.String
	return &st, nil
}
