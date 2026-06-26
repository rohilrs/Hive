package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestInsertAndGetPredictorAccuracy(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	p := 0.6
	r := 0.75
	a := &PredictorAccuracy{
		RunID:          "r1",
		Precision:      &p,
		Recall:         &r,
		PredictedCount: 5,
		TouchedCount:   4,
		IntersectCount: 3,
	}
	if err := s.InsertPredictorAccuracy(ctx, a); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := s.GetPredictorAccuracy(ctx, "r1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RunID != "r1" || got.Precision == nil || *got.Precision != 0.6 || got.Recall == nil || *got.Recall != 0.75 {
		t.Errorf("round-trip mismatch: %+v p=%v r=%v", got, got.Precision, got.Recall)
	}
	if got.PredictedCount != 5 || got.TouchedCount != 4 || got.IntersectCount != 3 {
		t.Errorf("counts mismatch: %+v", got)
	}
	if got.ComputedAt == 0 {
		t.Errorf("ComputedAt not populated")
	}
	if got.SkippedReason != "" {
		t.Errorf("SkippedReason=%q want empty", got.SkippedReason)
	}
}

func TestInsertPredictorAccuracySkipped(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()

	a := &PredictorAccuracy{
		RunID:          "r2",
		PredictedCount: 0,
		TouchedCount:   3,
		SkippedReason:  "no_predictions_files",
	}
	if err := s.InsertPredictorAccuracy(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPredictorAccuracy(ctx, "r2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Precision != nil || got.Recall != nil {
		t.Errorf("skipped row should have NULL precision/recall, got p=%v r=%v", got.Precision, got.Recall)
	}
	if got.SkippedReason != "no_predictions_files" {
		t.Errorf("SkippedReason=%q want no_predictions_files", got.SkippedReason)
	}
}

func TestGetPredictorAccuracyMissing(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	_, err := s.GetPredictorAccuracy(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}

func TestListPredictorAccuracyFilters(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()

	p1 := 0.5
	r1 := 0.5
	// Old row (project alpha)
	_ = s.InsertPredictorAccuracy(ctx, &PredictorAccuracy{RunID: "rA", Precision: &p1, Recall: &r1, PredictedCount: 2, TouchedCount: 2, IntersectCount: 1})
	oldTS := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := s.db.ExecContext(ctx, `UPDATE predictor_accuracy SET computed_at = ? WHERE run_id = 'rA'`, oldTS); err != nil {
		t.Fatal(err)
	}
	// Recent row
	_ = s.InsertPredictorAccuracy(ctx, &PredictorAccuracy{RunID: "rB", Precision: &p1, Recall: &r1, PredictedCount: 2, TouchedCount: 2, IntersectCount: 1})

	cutoff := time.Now().Add(-1 * time.Hour)
	rows, err := s.ListPredictorAccuracy(ctx, ListPredictorAccuracyFilter{Since: cutoff})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RunID != "rB" {
		t.Errorf("Since-filter returned %d rows, want 1 (rB only)", len(rows))
	}
}

func TestListRunsWithoutAccuracy(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()

	// InsertRun has FK constraints to projects and tasks.
	_ = s.InsertProject(ctx, &Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p", Title: "task 1", Source: "inbox", Status: "pending"})
	_ = s.InsertTask(ctx, &Task{ID: "t2", ProjectID: "p", Title: "task 2", Source: "inbox", Status: "pending"})
	_ = s.InsertTask(ctx, &Task{ID: "t3", ProjectID: "p", Title: "task 3", Source: "inbox", Status: "pending"})

	// Three runs in the runs table; one has an accuracy row already.
	_ = s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "done"})
	_ = s.InsertRun(ctx, &Run{ID: "r2", TaskID: "t2", ProjectID: "p", Pipeline: "build", Status: "done"})
	_ = s.InsertRun(ctx, &Run{ID: "r3", TaskID: "t3", ProjectID: "p", Pipeline: "build", Status: "running"})
	p := 0.5
	_ = s.InsertPredictorAccuracy(ctx, &PredictorAccuracy{RunID: "r1", Precision: &p, Recall: &p, PredictedCount: 1, TouchedCount: 1, IntersectCount: 1})

	got, err := s.ListRunsWithoutAccuracy(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// r2 is "done" without accuracy → returned. r1 has accuracy → not returned.
	// r3 is "running" → not done yet, not returned (we only backfill terminal runs).
	if len(got) != 1 || got[0] != "r2" {
		t.Errorf("got %v, want [r2]", got)
	}
}
