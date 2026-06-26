package store

import (
	"context"
	"path/filepath"
	"testing"
)

func setupForToolCallTest(t *testing.T) (*Store, int64) {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = s.InsertProject(ctx, &Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "T", Status: "pending"})
	_ = s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "running"})
	stageID, _ := s.InsertStage(ctx, &Stage{RunID: "r1", Name: "implement", Iter: 0, Model: "claude-sonnet-4-6"})
	return s, stageID
}

func TestInsertAndListToolCalls(t *testing.T) {
	s, stageID := setupForToolCallTest(t)
	defer s.Close()
	ctx := context.Background()

	end1 := int64(1700000050)
	dur1 := 150
	success := 1
	_, err := s.InsertToolCall(ctx, &ToolCall{
		RunID:      "r1",
		StageID:    stageID,
		Name:       "Read",
		ArgsHash:   "abc123",
		ArgsJSON:   `{"file_path":"a.go"}`,
		StartedAt:  1700000000,
		EndedAt:    &end1,
		DurationMS: &dur1,
		Success:    &success,
	})
	if err != nil {
		t.Fatalf("InsertToolCall: %v", err)
	}
	_, err = s.InsertToolCall(ctx, &ToolCall{
		RunID: "r1", StageID: stageID, Name: "Edit", ArgsHash: "def456",
		ArgsJSON: `{"file_path":"a.go","old_string":"x","new_string":"y"}`,
		StartedAt: 1700000100,
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListToolCallsForStage(ctx, stageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d tool calls want 2", len(rows))
	}
	// Ordered by started_at ASC.
	if rows[0].Name != "Read" {
		t.Errorf("first row %+v want Read", rows[0])
	}
	if rows[1].Name != "Edit" {
		t.Errorf("second row %+v want Edit", rows[1])
	}
	// Read completed; Edit still running.
	if rows[0].EndedAt == nil || *rows[0].EndedAt != 1700000050 {
		t.Errorf("Read EndedAt=%v want 1700000050", rows[0].EndedAt)
	}
	if rows[1].EndedAt != nil {
		t.Errorf("Edit EndedAt=%v want nil (running)", rows[1].EndedAt)
	}
}

func TestListRunningToolCalls(t *testing.T) {
	// Phase 3.2 stall monitor will use this to find tool calls that have
	// been "running" too long. Returns calls with ended_at IS NULL.
	s, stageID := setupForToolCallTest(t)
	defer s.Close()
	ctx := context.Background()

	end := int64(1700000050)
	dur := 50
	success := 1
	// One completed, one running.
	_, _ = s.InsertToolCall(ctx, &ToolCall{
		RunID: "r1", StageID: stageID, Name: "Read", ArgsHash: "h1",
		ArgsJSON: `{}`, StartedAt: 1700000000, EndedAt: &end,
		DurationMS: &dur, Success: &success,
	})
	_, _ = s.InsertToolCall(ctx, &ToolCall{
		RunID: "r1", StageID: stageID, Name: "Bash", ArgsHash: "h2",
		ArgsJSON: `{"command":"sleep 600"}`, StartedAt: 1700000100,
	})

	rows, err := s.ListRunningToolCalls(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "Bash" {
		t.Errorf("got %d rows want 1 Bash: %v", len(rows), rows)
	}
}
