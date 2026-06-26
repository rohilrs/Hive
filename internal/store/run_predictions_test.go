package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestPutGetPredictionJSON(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// Seed project + task to satisfy FK constraints on runs.
	_ = s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"})

	// Predictions row depends on the run row existing.
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "pending"}); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"files":["a.go","b.go"],"inline_capsules":[]}`)
	if err := s.PutPredictionJSON(ctx, "r1", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.GetPredictionJSON(ctx, "r1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("got=%s want=%s", got, payload)
	}
}

func TestGetPredictionMissingReturnsNotFound(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	_, err := s.GetPredictionJSON(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}

func TestSetGetWaitingOn(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()
	_ = s.InsertProject(ctx, &Project{ID: "p2", Slug: "p2", Name: "P2", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t2", ProjectID: "p2", Title: "x", Source: "inbox"})
	if err := s.InsertRun(ctx, &Run{ID: "r2", TaskID: "t2", ProjectID: "p2", Pipeline: "build", Status: "pending"}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetWaitingOn(ctx, "r2", []string{"rA", "rB"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.GetWaitingOn(ctx, "r2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "rA" || got[1] != "rB" {
		t.Errorf("got=%v want [rA rB]", got)
	}

	// Clearing via empty slice writes NULL (queryable as "not waiting").
	if err := s.SetWaitingOn(ctx, "r2", nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetWaitingOn(ctx, "r2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got=%v want empty after clear", got)
	}
}

func TestListPendingWithWaitingOn(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()

	// Seed project + tasks to satisfy FK constraints.
	_ = s.InsertProject(ctx, &Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "tA", ProjectID: "p", Title: "A", Source: "inbox"})
	_ = s.InsertTask(ctx, &Task{ID: "tB", ProjectID: "p", Title: "B", Source: "inbox"})
	_ = s.InsertTask(ctx, &Task{ID: "tC", ProjectID: "p", Title: "C", Source: "inbox"})

	// Run A: pending, no waiting_on (fresh, not yet dispatched)
	_ = s.InsertRun(ctx, &Run{ID: "rA", TaskID: "tA", ProjectID: "p", Pipeline: "build", Status: "pending"})
	// Run B: pending, waiting_on=[rA] (queued)
	_ = s.InsertRun(ctx, &Run{ID: "rB", TaskID: "tB", ProjectID: "p", Pipeline: "build", Status: "pending"})
	_ = s.SetWaitingOn(ctx, "rB", []string{"rA"})
	// Run C: running (in-flight, should not appear)
	_ = s.InsertRun(ctx, &Run{ID: "rC", TaskID: "tC", ProjectID: "p", Pipeline: "build", Status: "pending"})
	_ = s.MarkRunStarted(ctx, "rC")

	got, err := s.ListPendingWithWaitingOn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "rB" {
		t.Errorf("ListPendingWithWaitingOn=%v want only rB", runIDs(got))
	}
}

func runIDs(rs []*Run) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
