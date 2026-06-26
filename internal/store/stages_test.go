package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func setupForStageTest(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// FK requires project + task + run before stages.
	_ = s.InsertProject(ctx, &Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "T", Status: "pending"})
	_ = s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "running"})
	return s, "r1"
}

func TestInsertAndUpdateStageEnd(t *testing.T) {
	s, runID := setupForStageTest(t)
	defer s.Close()
	ctx := context.Background()

	id, err := s.InsertStage(ctx, &Stage{
		RunID: runID, Name: "implement", Iter: 0, Model: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("InsertStage: %v", err)
	}
	if id == 0 {
		t.Errorf("InsertStage returned id=0")
	}

	conf := 0.85
	cost := 0.42
	upd := &StageEndUpdate{
		ID:                id,
		Verdict:           "APPROVE",
		VerdictConfidence: &conf,
		TokensIn:          5000,
		TokensOut:         1200,
		CacheHitTokens:    800,
		CostUSD:           &cost,
	}
	if err := s.UpdateStageEnd(ctx, upd); err != nil {
		t.Fatalf("UpdateStageEnd: %v", err)
	}

	got, err := s.GetStage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "implement" || got.Iter != 0 || got.Model != "claude-sonnet-4-6" {
		t.Errorf("identity mismatch: %+v", got)
	}
	if got.TokensIn != 5000 || got.TokensOut != 1200 || got.CacheHitTokens != 800 {
		t.Errorf("token mismatch: %+v", got)
	}
	if got.Verdict != "APPROVE" || got.VerdictConfidence == nil || *got.VerdictConfidence != 0.85 {
		t.Errorf("verdict mismatch: %+v vc=%v", got, got.VerdictConfidence)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.42 {
		t.Errorf("cost mismatch: %v", got.CostUSD)
	}
	if got.EndedAt == 0 {
		t.Errorf("EndedAt should be set after UpdateStageEnd")
	}
}

func TestInsertStageDuplicateReplaces(t *testing.T) {
	// (run_id, name, iter) is UNIQUE. INSERT OR REPLACE means a second
	// insert for the same triple overwrites the row.
	s, runID := setupForStageTest(t)
	defer s.Close()
	ctx := context.Background()

	id1, _ := s.InsertStage(ctx, &Stage{RunID: runID, Name: "implement", Iter: 0, Model: "claude-haiku-4-5"})
	id2, err := s.InsertStage(ctx, &Stage{RunID: runID, Name: "implement", Iter: 0, Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatalf("second InsertStage: %v", err)
	}
	if id1 == id2 {
		t.Logf("INSERT OR REPLACE reused id %d (acceptable)", id1)
	}

	got, err := s.GetStage(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("Model=%q want claude-sonnet-4-6 (REPLACE should have overwritten)", got.Model)
	}
}

func TestListStagesForRunOrdered(t *testing.T) {
	s, runID := setupForStageTest(t)
	defer s.Close()
	ctx := context.Background()

	_, _ = s.InsertStage(ctx, &Stage{RunID: runID, Name: "implement", Iter: 0, Model: "claude-haiku-4-5"})
	_, _ = s.InsertStage(ctx, &Stage{RunID: runID, Name: "review", Iter: 0, Model: "claude-haiku-4-5"})
	_, _ = s.InsertStage(ctx, &Stage{RunID: runID, Name: "implement", Iter: 1, Model: "claude-sonnet-4-6"})

	rows, err := s.ListStagesForRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d stages want 3", len(rows))
	}
	// Ordered by started_at ASC.
	if rows[0].Name != "implement" || rows[0].Iter != 0 {
		t.Errorf("first row %+v", rows[0])
	}
	if rows[2].Iter != 1 {
		t.Errorf("third row should be iter 1: %+v", rows[2])
	}
}

func TestGetStageNotFound(t *testing.T) {
	s, _ := setupForStageTest(t)
	defer s.Close()
	_, err := s.GetStage(context.Background(), 9999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}
