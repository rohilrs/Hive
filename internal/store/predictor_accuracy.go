package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PredictorAccuracy is one row of computed-or-skipped per-run accuracy.
// Mirrors columns in migration 0005. Precision and Recall use pointer
// types so NULL (skipped) is distinguishable from 0.0 (computed but
// no intersection).
type PredictorAccuracy struct {
	RunID          string
	Precision      *float64
	Recall         *float64
	PredictedCount int
	TouchedCount   int
	IntersectCount int
	ComputedAt     int64
	SkippedReason  string
}

// ListPredictorAccuracyFilter narrows ListPredictorAccuracy results.
// Zero-valued fields are ignored. Mirrors ListPredictorMetricsFilter
// shape so future joins are obvious.
type ListPredictorAccuracyFilter struct {
	Since time.Time
}

// InsertPredictorAccuracy writes one row. ComputedAt defaults to
// time.Now().Unix() if the caller's field is 0 (the common case).
// Uses INSERT OR REPLACE on run_id PRIMARY KEY so re-running backfill
// for the same run is idempotent.
func (s *Store) InsertPredictorAccuracy(ctx context.Context, a *PredictorAccuracy) error {
	if a.ComputedAt == 0 {
		a.ComputedAt = time.Now().Unix()
	}
	var p, r any
	if a.Precision != nil {
		p = *a.Precision
	}
	if a.Recall != nil {
		r = *a.Recall
	}
	var reason any
	if a.SkippedReason != "" {
		reason = a.SkippedReason
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO predictor_accuracy
		   (run_id, precision_, recall_, predicted_count, touched_count,
		    intersect_count, computed_at, skipped_reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.RunID, p, r, a.PredictedCount, a.TouchedCount,
		a.IntersectCount, a.ComputedAt, reason,
	)
	if err != nil {
		return fmt.Errorf("insert predictor_accuracy: %w", err)
	}
	return nil
}

// GetPredictorAccuracy returns the row for runID, or ErrNotFound.
func (s *Store) GetPredictorAccuracy(ctx context.Context, runID string) (*PredictorAccuracy, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT run_id, precision_, recall_, predicted_count, touched_count,
		        intersect_count, computed_at, skipped_reason
		   FROM predictor_accuracy WHERE run_id = ?`, runID)
	a, err := scanPredictorAccuracy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ListPredictorAccuracy returns rows ordered by computed_at descending.
func (s *Store) ListPredictorAccuracy(ctx context.Context, f ListPredictorAccuracyFilter) ([]*PredictorAccuracy, error) {
	var (
		clauses []string
		args    []any
	)
	if !f.Since.IsZero() {
		clauses = append(clauses, "computed_at >= ?")
		args = append(args, f.Since.Unix())
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, precision_, recall_, predicted_count, touched_count,
		        intersect_count, computed_at, skipped_reason
		   FROM predictor_accuracy`+where+`
		  ORDER BY computed_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list predictor_accuracy: %w", err)
	}
	defer rows.Close()
	var out []*PredictorAccuracy
	for rows.Next() {
		a, err := scanPredictorAccuracy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListRunsWithoutAccuracy returns IDs of terminal runs (status
// in 'done', 'needs_attention', 'error') that have no predictor_accuracy
// row. Optional since filter limits to runs created after the cutoff.
// Used by `hive predict accuracy backfill`.
func (s *Store) ListRunsWithoutAccuracy(ctx context.Context, since time.Time) ([]string, error) {
	var (
		args  []any
		extra string
	)
	if !since.IsZero() {
		extra = " AND r.created_at >= ?"
		args = append(args, since.Unix())
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id FROM runs r
		LEFT JOIN predictor_accuracy a ON a.run_id = r.id
		WHERE a.run_id IS NULL
		  AND r.status IN ('done', 'needs_attention', 'error')`+extra+`
		ORDER BY r.created_at ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs without accuracy: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func scanPredictorAccuracy(r rowScanner) (*PredictorAccuracy, error) {
	var (
		a      PredictorAccuracy
		p      sql.NullFloat64
		rec    sql.NullFloat64
		reason sql.NullString
	)
	if err := r.Scan(&a.RunID, &p, &rec, &a.PredictedCount, &a.TouchedCount,
		&a.IntersectCount, &a.ComputedAt, &reason); err != nil {
		return nil, err
	}
	if p.Valid {
		v := p.Float64
		a.Precision = &v
	}
	if rec.Valid {
		v := rec.Float64
		a.Recall = &v
	}
	if reason.Valid {
		a.SkippedReason = reason.String
	}
	return &a, nil
}
