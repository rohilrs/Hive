package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestInsertAndListPredictorMetrics(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	m := &PredictorMetric{
		RunID:          "r1",
		ProjectID:      "p1",
		HaikuLatencyMS: 1200,
		FetchLatencyMS: 45,
		CandidateCount: 7,
		InlineCount:    5,
		OverflowCount:  2,
		Truncated:      false,
		Error:          "",
	}
	if err := s.InsertPredictorMetrics(ctx, m); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	rows, err := s.ListPredictorMetrics(ctx, ListPredictorMetricsFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows want 1", len(rows))
	}
	got := rows[0]
	if got.RunID != "r1" || got.ProjectID != "p1" || got.HaikuLatencyMS != 1200 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.CreatedAt == 0 {
		t.Errorf("CreatedAt not populated")
	}
}

func TestListPredictorMetricsProjectFilter(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()
	_ = s.InsertPredictorMetrics(ctx, &PredictorMetric{RunID: "r1", ProjectID: "alpha", HaikuLatencyMS: 100})
	_ = s.InsertPredictorMetrics(ctx, &PredictorMetric{RunID: "r2", ProjectID: "beta", HaikuLatencyMS: 200})
	_ = s.InsertPredictorMetrics(ctx, &PredictorMetric{RunID: "r3", ProjectID: "alpha", HaikuLatencyMS: 300})

	rows, err := s.ListPredictorMetrics(ctx, ListPredictorMetricsFilter{ProjectID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d want 2 alpha rows", len(rows))
	}
	for _, r := range rows {
		if r.ProjectID != "alpha" {
			t.Errorf("got project_id=%s want alpha", r.ProjectID)
		}
	}
}

func TestListPredictorMetricsSinceFilter(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()

	// Insert with explicit older timestamp via direct UPDATE; the
	// Insert helper always uses time.Now().
	_ = s.InsertPredictorMetrics(ctx, &PredictorMetric{RunID: "old", ProjectID: "p"})
	oldTS := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := s.db.ExecContext(ctx, `UPDATE predictor_metrics SET created_at = ? WHERE run_id = 'old'`, oldTS); err != nil {
		t.Fatal(err)
	}
	_ = s.InsertPredictorMetrics(ctx, &PredictorMetric{RunID: "recent", ProjectID: "p"})

	cutoff := time.Now().Add(-1 * time.Hour)
	rows, err := s.ListPredictorMetrics(ctx, ListPredictorMetricsFilter{Since: cutoff})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RunID != "recent" {
		t.Errorf("got %d rows (want 1 'recent'): %+v", len(rows), rows)
	}
}
