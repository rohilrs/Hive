package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PutPredictionJSON stores the JSON-serialized predictor.Result on the
// run row. Pipeline callers marshal the *Result before calling.
func (s *Store) PutPredictionJSON(ctx context.Context, runID string, payload []byte) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET prediction = ? WHERE id = ?`,
		string(payload), runID,
	)
	if err != nil {
		return fmt.Errorf("put prediction: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetPredictionJSON returns the JSON-serialized predictor.Result for the
// run, or ErrNotFound if the run doesn't exist or has no prediction.
func (s *Store) GetPredictionJSON(ctx context.Context, runID string) ([]byte, error) {
	var pred sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT prediction FROM runs WHERE id = ?`, runID,
	).Scan(&pred)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get prediction: %w", err)
	}
	if !pred.Valid {
		return nil, ErrNotFound
	}
	return []byte(pred.String), nil
}

// SetWaitingOn writes the waiting_on list as JSON, or NULL when the
// list is empty. Empty/nil clears the field, so a re-dispatched run
// can become "not blocked" without a separate Clear method.
func (s *Store) SetWaitingOn(ctx context.Context, runID string, runIDs []string) error {
	var arg any
	if len(runIDs) == 0 {
		arg = nil
	} else {
		raw, err := json.Marshal(runIDs)
		if err != nil {
			return fmt.Errorf("marshal waiting_on: %w", err)
		}
		arg = string(raw)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET waiting_on = ? WHERE id = ?`,
		arg, runID,
	)
	if err != nil {
		return fmt.Errorf("set waiting_on: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetWaitingOn returns the parsed waiting_on list, or an empty slice
// when the field is NULL or empty.
func (s *Store) GetWaitingOn(ctx context.Context, runID string) ([]string, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT waiting_on FROM runs WHERE id = ?`, runID,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get waiting_on: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil, fmt.Errorf("unmarshal waiting_on: %w", err)
	}
	return out, nil
}

// ListPendingWithWaitingOn returns runs in status=pending with a
// non-NULL waiting_on field — i.e., queued runs the scheduler needs
// to re-evaluate.
func (s *Store) ListPendingWithWaitingOn(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		runSelectClause+` WHERE status = 'pending' AND waiting_on IS NOT NULL
		                  ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
