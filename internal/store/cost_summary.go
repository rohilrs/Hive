package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CostBucket mirrors rpc.CostBucket. Lives in store so handlers can
// return store-typed rows and let the rpc layer transcode.
type CostBucket struct {
	Key      string
	TotalUSD float64
	Count    int
}

// CostSummary is the result of CostSummary(). Fields are pre-rolled
// for the TUI's four panels; handlers transcode to rpc.CostSummaryView.
type CostSummary struct {
	Daily       []CostBucket
	Models      []CostBucket
	Pipelines   []CostBucket
	Projects    []CostBucket
	GeneratedAt time.Time
}

// CostSummary returns four pre-computed rollups of stages.cost_usd.
// Best-effort: any individual query that errors yields an empty
// slice for that rollup rather than failing the whole call.
func (s *Store) CostSummary(ctx context.Context) (*CostSummary, error) {
	out := &CostSummary{GeneratedAt: time.Now()}

	if rows, err := s.queryBuckets(ctx, `
		SELECT date(started_at, 'unixepoch') AS day, SUM(cost_usd), COUNT(*)
		  FROM stages
		 WHERE cost_usd IS NOT NULL
		 GROUP BY day
		 ORDER BY day DESC
		 LIMIT 14`); err == nil {
		out.Daily = rows
	}

	if rows, err := s.queryBuckets(ctx, `
		SELECT model, SUM(cost_usd), COUNT(*)
		  FROM stages
		 WHERE cost_usd IS NOT NULL AND model IS NOT NULL AND model != ''
		 GROUP BY model
		 ORDER BY SUM(cost_usd) DESC`); err == nil {
		out.Models = rows
	}

	if rows, err := s.queryBuckets(ctx, `
		SELECT runs.pipeline, SUM(stages.cost_usd), COUNT(stages.id)
		  FROM stages JOIN runs ON stages.run_id = runs.id
		 WHERE stages.cost_usd IS NOT NULL
		 GROUP BY runs.pipeline
		 ORDER BY SUM(stages.cost_usd) DESC`); err == nil {
		out.Pipelines = rows
	}

	if rows, err := s.queryBuckets(ctx, `
		SELECT projects.slug, SUM(stages.cost_usd), COUNT(stages.id)
		  FROM stages
		  JOIN runs ON stages.run_id = runs.id
		  JOIN projects ON runs.project_id = projects.id
		 WHERE stages.cost_usd IS NOT NULL
		 GROUP BY projects.slug
		 ORDER BY SUM(stages.cost_usd) DESC`); err == nil {
		out.Projects = rows
	}

	return out, nil
}

func (s *Store) queryBuckets(ctx context.Context, query string, args ...any) ([]CostBucket, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query buckets: %w", err)
	}
	defer rows.Close()
	var out []CostBucket
	for rows.Next() {
		var b CostBucket
		var keyNull sql.NullString
		if err := rows.Scan(&keyNull, &b.TotalUSD, &b.Count); err != nil {
			return nil, err
		}
		b.Key = keyNull.String
		out = append(out, b)
	}
	return out, rows.Err()
}
