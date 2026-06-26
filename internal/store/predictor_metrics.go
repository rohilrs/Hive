package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PredictorMetric is one row of denormalized per-dispatch predictor
// data. Mirrors columns in migration 0004; ID + CreatedAt are
// populated by Insert.
type PredictorMetric struct {
	ID             int64
	RunID          string
	ProjectID      string
	HaikuLatencyMS int64
	FetchLatencyMS int64
	CandidateCount int
	InlineCount    int
	OverflowCount  int
	Truncated      bool
	Error          string
	CreatedAt      int64
}

// ListPredictorMetricsFilter narrows ListPredictorMetrics results.
// Zero-valued fields are ignored (no filter applied).
type ListPredictorMetricsFilter struct {
	ProjectID string
	Since     time.Time
}

// InsertPredictorMetrics writes one row and back-fills ID + CreatedAt
// on the caller's struct. created_at defaults to time.Now().Unix() if
// the struct's field is 0 (the common case from dispatch).
func (s *Store) InsertPredictorMetrics(ctx context.Context, m *PredictorMetric) error {
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().Unix()
	}
	truncatedInt := 0
	if m.Truncated {
		truncatedInt = 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO predictor_metrics
		   (run_id, project_id, haiku_latency_ms, fetch_latency_ms,
		    candidate_count, inline_count, overflow_count, truncated,
		    error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.RunID, m.ProjectID, m.HaikuLatencyMS, m.FetchLatencyMS,
		m.CandidateCount, m.InlineCount, m.OverflowCount, truncatedInt,
		m.Error, m.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert predictor_metrics: %w", err)
	}
	id, err := res.LastInsertId()
	if err == nil {
		m.ID = id
	}
	return nil
}

// ListPredictorMetrics returns rows ordered by created_at descending
// (newest first) so consumers can stream the most recent results
// without sorting.
func (s *Store) ListPredictorMetrics(ctx context.Context, f ListPredictorMetricsFilter) ([]*PredictorMetric, error) {
	var (
		clauses []string
		args    []any
	)
	if f.ProjectID != "" {
		clauses = append(clauses, "project_id = ?")
		args = append(args, f.ProjectID)
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, "created_at >= ?")
		args = append(args, f.Since.Unix())
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, project_id, haiku_latency_ms, fetch_latency_ms,
		        candidate_count, inline_count, overflow_count, truncated,
		        error, created_at
		   FROM predictor_metrics`+where+`
		  ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list predictor_metrics: %w", err)
	}
	defer rows.Close()

	var out []*PredictorMetric
	for rows.Next() {
		m, err := scanPredictorMetric(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanPredictorMetric(r rowScanner) (*PredictorMetric, error) {
	var (
		m         PredictorMetric
		truncated int
		errStr    sql.NullString
		latencyH  sql.NullInt64
		latencyF  sql.NullInt64
		cands     sql.NullInt64
		inlines   sql.NullInt64
		over      sql.NullInt64
	)
	if err := r.Scan(&m.ID, &m.RunID, &m.ProjectID, &latencyH, &latencyF,
		&cands, &inlines, &over, &truncated, &errStr, &m.CreatedAt); err != nil {
		return nil, err
	}
	m.HaikuLatencyMS = latencyH.Int64
	m.FetchLatencyMS = latencyF.Int64
	m.CandidateCount = int(cands.Int64)
	m.InlineCount = int(inlines.Int64)
	m.OverflowCount = int(over.Int64)
	m.Truncated = truncated != 0
	if errStr.Valid {
		m.Error = errStr.String
	}
	return &m, nil
}
