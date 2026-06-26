package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PutConfigSnapshot stores the JSON-serialized effective *config.Config
// on the run row. Callers (dispatch) marshal the config + pass payload.
// Returns ErrNotFound if no run row with the given ID exists.
func (s *Store) PutConfigSnapshot(ctx context.Context, runID string, payload []byte) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET config_snapshot = ? WHERE id = ?`,
		string(payload), runID,
	)
	if err != nil {
		return fmt.Errorf("put config_snapshot: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetConfigSnapshot returns the JSON payload stored for runID, or
// ErrNotFound if the run row doesn't exist or its config_snapshot
// column is NULL (no snapshot was ever written for this run).
func (s *Store) GetConfigSnapshot(ctx context.Context, runID string) ([]byte, error) {
	var snap sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT config_snapshot FROM runs WHERE id = ?`, runID,
	).Scan(&snap)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get config_snapshot: %w", err)
	}
	if !snap.Valid {
		return nil, ErrNotFound
	}
	return []byte(snap.String), nil
}
