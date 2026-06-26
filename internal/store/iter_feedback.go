package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PutFeedbackJSON stores reviewer feedback for one (run, iter). The
// callers (pipeline) marshal a pipeline.Feedback record (summary +
// file_refs) before calling; the column name is historical. Upserts
// — re-running an iter (shouldn't happen in normal flow, but is
// defensive) replaces the prior row.
func (s *Store) PutFeedbackJSON(ctx context.Context, runID string, iter int, refsJSON []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO iter_feedback (run_id, iter, file_refs, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(run_id, iter) DO UPDATE SET
			file_refs  = excluded.file_refs,
			created_at = excluded.created_at
	`, runID, iter, string(refsJSON), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("put iter_feedback: %w", err)
	}
	return nil
}

// GetFeedbackJSON returns the FileRefs JSON for one (run, iter), or
// ErrNotFound when no row exists.
func (s *Store) GetFeedbackJSON(ctx context.Context, runID string, iter int) ([]byte, error) {
	var refs string
	err := s.db.QueryRowContext(ctx,
		`SELECT file_refs FROM iter_feedback WHERE run_id = ? AND iter = ?`,
		runID, iter,
	).Scan(&refs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get iter_feedback: %w", err)
	}
	return []byte(refs), nil
}
