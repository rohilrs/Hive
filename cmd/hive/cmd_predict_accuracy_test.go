package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/store"
)

func TestRunAccuracyForRunComputed(t *testing.T) {
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p := 0.75
	r := 0.6
	_ = s.InsertPredictorAccuracy(ctx, &store.PredictorAccuracy{
		RunID:          "r1",
		Precision:      &p,
		Recall:         &r,
		PredictedCount: 4,
		TouchedCount:   5,
		IntersectCount: 3,
	})

	var buf bytes.Buffer
	if err := runAccuracyForRun(ctx, &buf, s, "r1", "human"); err != nil {
		t.Fatalf("runAccuracyForRun: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"run-id: r1", "precision: 0.750", "recall: 0.600", "predicted: 4", "touched: 5", "intersect: 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunAccuracyForRunSkipped(t *testing.T) {
	s, _ := store.Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()
	_ = s.InsertPredictorAccuracy(ctx, &store.PredictorAccuracy{
		RunID:          "r2",
		SkippedReason:  "no_edits",
		PredictedCount: 3,
	})

	var buf bytes.Buffer
	if err := runAccuracyForRun(ctx, &buf, s, "r2", "human"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "skipped: no_edits") {
		t.Errorf("expected skip reason in output: %s", out)
	}
}

func TestRunAccuracyForRunJSON(t *testing.T) {
	s, _ := store.Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()
	p := 0.5
	_ = s.InsertPredictorAccuracy(ctx, &store.PredictorAccuracy{
		RunID:          "r1",
		Precision:      &p,
		Recall:         &p,
		PredictedCount: 2,
		TouchedCount:   2,
		IntersectCount: 1,
	})

	var buf bytes.Buffer
	if err := runAccuracyForRun(ctx, &buf, s, "r1", "json"); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, buf.String())
	}
	if got["run_id"] != "r1" {
		t.Errorf("got %+v want run_id=r1", got)
	}
}

func TestRunAccuracyBackfillComputesPendingRuns(t *testing.T) {
	s, _ := store.Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()

	// FK constraints on runs require project + task rows first.
	_ = s.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = s.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "T1", Status: "done"})
	_ = s.InsertTask(ctx, &store.Task{ID: "t2", ProjectID: "p", Source: "inbox", Title: "T2", Status: "done"})

	// Two terminal runs without accuracy rows.
	_ = s.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "done"})
	_ = s.InsertRun(ctx, &store.Run{ID: "r2", TaskID: "t2", ProjectID: "p", Pipeline: "build", Status: "needs_attention"})

	// Backfill should skip them all with "no_prediction" (no prediction JSON).
	var buf bytes.Buffer
	processed, err := runAccuracyBackfill(ctx, &buf, s, "")
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if processed != 2 {
		t.Errorf("processed=%d want 2", processed)
	}
	for _, id := range []string{"r1", "r2"} {
		row, err := s.GetPredictorAccuracy(ctx, id)
		if err != nil {
			t.Fatalf("missing accuracy row for %s: %v", id, err)
		}
		if row.SkippedReason != "no_prediction" {
			t.Errorf("%s SkippedReason=%q want no_prediction", id, row.SkippedReason)
		}
	}
}
