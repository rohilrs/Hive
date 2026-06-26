package store

import (
	"context"
	"testing"
)

func TestInsertStallAndListForRun(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	stageID := int64(42)
	id, err := s.InsertStall(ctx, &Stall{
		RunID:       "run-1",
		StageID:     &stageID,
		Layer:       2,
		DetectedAt:  1000,
		ClearedAt:   nil,
		ActionTaken: "killed_subprocess",
		DetailsJSON: `{"tool":"Bash"}`,
	})
	if err != nil {
		t.Fatalf("InsertStall: %v", err)
	}
	if id == 0 {
		t.Fatal("InsertStall returned id=0")
	}

	rows, err := s.ListStallsForRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListStallsForRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows)=%d want 1", len(rows))
	}
	r := rows[0]
	if r.Layer != 2 || r.ActionTaken != "killed_subprocess" {
		t.Errorf("got layer=%d action=%q", r.Layer, r.ActionTaken)
	}
	if r.StageID == nil || *r.StageID != 42 {
		t.Errorf("StageID = %v want 42", r.StageID)
	}
	if r.ClearedAt != nil {
		t.Errorf("ClearedAt = %v want nil", r.ClearedAt)
	}
}

func TestInsertStallNullStageID(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	_, err := s.InsertStall(ctx, &Stall{
		RunID:       "run-nostage",
		StageID:     nil,
		Layer:       1,
		DetectedAt:  500,
		ActionTaken: "surfaced",
	})
	if err != nil {
		t.Fatalf("InsertStall: %v", err)
	}
	rows, _ := s.ListStallsForRun(ctx, "run-nostage")
	if len(rows) != 1 {
		t.Fatalf("len(rows)=%d", len(rows))
	}
	if rows[0].StageID != nil {
		t.Errorf("StageID = %v want nil", rows[0].StageID)
	}
}

func TestClearActiveStall(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	id, err := s.InsertStall(ctx, &Stall{
		RunID:       "run-2",
		Layer:       1,
		DetectedAt:  1000,
		ActionTaken: "surfaced",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.ClearStall(ctx, id, 2000); err != nil {
		t.Fatalf("ClearStall: %v", err)
	}

	rows, _ := s.ListStallsForRun(ctx, "run-2")
	if len(rows) != 1 {
		t.Fatalf("len(rows)=%d", len(rows))
	}
	if rows[0].ClearedAt == nil || *rows[0].ClearedAt != 2000 {
		t.Errorf("ClearedAt = %v want 2000", rows[0].ClearedAt)
	}
}

func TestListActiveStallsForRun(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	cleared := int64(2000)
	_, _ = s.InsertStall(ctx, &Stall{RunID: "r", Layer: 1, DetectedAt: 1000, ClearedAt: &cleared, ActionTaken: "surfaced"})
	_, _ = s.InsertStall(ctx, &Stall{RunID: "r", Layer: 1, DetectedAt: 1500, ActionTaken: "surfaced"})

	active, err := s.ListActiveStallsForRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("len(active)=%d want 1", len(active))
	}
	if active[0].DetectedAt != 1500 {
		t.Errorf("got DetectedAt=%d want 1500", active[0].DetectedAt)
	}
}
