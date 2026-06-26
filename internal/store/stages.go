package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StageEndUpdate carries the fields populated when a stage completes.
// Callers leave VerdictConfidence / CostUSD as nil when the data is
// not available; the DB columns become NULL in that case.
type StageEndUpdate struct {
	ID                int64
	Verdict           string
	VerdictConfidence *float64
	TokensIn          int
	TokensOut         int
	CacheHitTokens    int
	CostUSD           *float64
}

// InsertStage writes a new row at stage start. Uses INSERT OR REPLACE
// on the UNIQUE (run_id, name, iter) constraint so re-running an iter
// overwrites instead of erroring. StartedAt defaults to time.Now().Unix()
// when the caller's field is 0.
func (s *Store) InsertStage(ctx context.Context, st *Stage) (int64, error) {
	if st.StartedAt == 0 {
		st.StartedAt = time.Now().Unix()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO stages (run_id, name, iter, model, started_at)
		 VALUES (?, ?, ?, ?, ?)`,
		st.RunID, st.Name, st.Iter, st.Model, st.StartedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("insert stage: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	st.ID = id
	return id, nil
}

// UpdateStageEnd records end-of-stage metadata (verdict, tokens, cost).
// EndedAt is set to time.Now().Unix() automatically.
func (s *Store) UpdateStageEnd(ctx context.Context, u *StageEndUpdate) error {
	var verdict any
	if u.Verdict != "" {
		verdict = u.Verdict
	}
	var conf any
	if u.VerdictConfidence != nil {
		conf = *u.VerdictConfidence
	}
	var cost any
	if u.CostUSD != nil {
		cost = *u.CostUSD
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE stages
		    SET ended_at = ?, tokens_in = ?, tokens_out = ?, cache_hit_tokens = ?,
		        verdict = ?, verdict_confidence = ?, cost_usd = ?
		  WHERE id = ?`,
		time.Now().Unix(), u.TokensIn, u.TokensOut, u.CacheHitTokens,
		verdict, conf, cost, u.ID,
	)
	if err != nil {
		return fmt.Errorf("update stage end: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetStage returns one row by ID.
func (s *Store) GetStage(ctx context.Context, id int64) (*Stage, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, run_id, name, iter, model, started_at, ended_at,
		        tokens_in, tokens_out, cache_hit_tokens,
		        verdict, verdict_confidence, cost_usd
		   FROM stages WHERE id = ?`, id)
	st, err := scanStage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return st, err
}

// ListStagesForRun returns all stages for a run, ordered by started_at ASC.
func (s *Store) ListStagesForRun(ctx context.Context, runID string) ([]*Stage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, name, iter, model, started_at, ended_at,
		        tokens_in, tokens_out, cache_hit_tokens,
		        verdict, verdict_confidence, cost_usd
		   FROM stages WHERE run_id = ? ORDER BY started_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list stages for run: %w", err)
	}
	defer rows.Close()
	var out []*Stage
	for rows.Next() {
		st, err := scanStage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func scanStage(r rowScanner) (*Stage, error) {
	var (
		st      Stage
		model   sql.NullString
		endedAt sql.NullInt64
		tIn     sql.NullInt64
		tOut    sql.NullInt64
		cache   sql.NullInt64
		verdict sql.NullString
		conf    sql.NullFloat64
		cost    sql.NullFloat64
	)
	if err := r.Scan(&st.ID, &st.RunID, &st.Name, &st.Iter, &model,
		&st.StartedAt, &endedAt, &tIn, &tOut, &cache,
		&verdict, &conf, &cost); err != nil {
		return nil, err
	}
	st.Model = model.String
	st.EndedAt = endedAt.Int64
	st.TokensIn = int(tIn.Int64)
	st.TokensOut = int(tOut.Int64)
	st.CacheHitTokens = int(cache.Int64)
	st.Verdict = verdict.String
	if conf.Valid {
		v := conf.Float64
		st.VerdictConfidence = &v
	}
	if cost.Valid {
		v := cost.Float64
		st.CostUSD = &v
	}
	return &st, nil
}
