package store

import (
	"context"
	"testing"
	"time"
)

func TestCostSummaryEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	cs, err := s.CostSummary(ctx)
	if err != nil {
		t.Fatalf("CostSummary: %v", err)
	}
	if len(cs.Daily) != 0 || len(cs.Models) != 0 || len(cs.Pipelines) != 0 || len(cs.Projects) != 0 {
		t.Errorf("empty DB should yield empty buckets, got %+v", cs)
	}
	if cs.GeneratedAt.IsZero() {
		t.Error("GeneratedAt should be set")
	}
}

func TestCostSummaryWithSeededRows(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	repoPath := "/tmp/x"
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "hive", Name: "Hive", RepoPath: &repoPath, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "done"}); err != nil {
		t.Fatal(err)
	}

	cost := func(v float64) *float64 { return &v }
	now := time.Now().Unix()

	// Three stages: two with cost, one with NULL cost (unknown model).
	s1, err := s.InsertStage(ctx, &Stage{RunID: "r1", Name: "implement", Iter: 0, Model: "sonnet", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStageEnd(ctx, &StageEndUpdate{ID: s1, TokensIn: 100, TokensOut: 50, CostUSD: cost(0.10)}); err != nil {
		t.Fatal(err)
	}
	s2, err := s.InsertStage(ctx, &Stage{RunID: "r1", Name: "review", Iter: 0, Model: "haiku", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStageEnd(ctx, &StageEndUpdate{ID: s2, TokensIn: 50, TokensOut: 20, CostUSD: cost(0.05)}); err != nil {
		t.Fatal(err)
	}
	s3, err := s.InsertStage(ctx, &Stage{RunID: "r1", Name: "test", Iter: 0, StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStageEnd(ctx, &StageEndUpdate{ID: s3, TokensIn: 0, TokensOut: 0, CostUSD: nil}); err != nil {
		t.Fatal(err)
	}

	cs, err := s.CostSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Models) != 2 {
		t.Errorf("Models len=%d want 2 (sonnet+haiku; NULL-cost excluded); got %+v", len(cs.Models), cs.Models)
	}
	var total float64
	for _, m := range cs.Models {
		total += m.TotalUSD
	}
	if total < 0.149 || total > 0.151 {
		t.Errorf("total model cost=%v want ~0.15", total)
	}
	if len(cs.Pipelines) != 1 || cs.Pipelines[0].Key != "build" {
		t.Errorf("Pipelines=%+v want one 'build' bucket", cs.Pipelines)
	}
	if len(cs.Projects) != 1 || cs.Projects[0].Key != "hive" {
		t.Errorf("Projects=%+v want one 'hive' bucket", cs.Projects)
	}
	if len(cs.Daily) == 0 {
		t.Error("Daily should have at least one bucket for today")
	}
}
