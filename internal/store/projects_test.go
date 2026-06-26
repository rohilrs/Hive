package store

import (
	"context"
	"errors"
	"testing"
)

func ptr(s string) *string { return &s }

func TestUpdateProject(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateProject(ctx, "p1", ptr("New Name"), nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProject(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "New Name" {
		t.Errorf("Name=%q want New Name", got.Name)
	}
	if got.Status != "active" {
		t.Errorf("Status=%q want active (unchanged)", got.Status)
	}

	if err := s.UpdateProject(ctx, "p1", nil, nil, ptr("archived")); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetProject(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "archived" {
		t.Errorf("Status=%q want archived", got.Status)
	}
	if got.Name != "New Name" {
		t.Errorf("Name=%q want New Name (unchanged)", got.Name)
	}
}

func TestUpdateProjectNotFound(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.UpdateProject(ctx, "missing", ptr("x"), nil, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}

func TestDeleteProject(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProjectBySlug(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetProjectBySlug err=%v want ErrNotFound", err)
	}
}

func TestDeleteProjectGuardedByRunningRun(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Pipeline: "build"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	err = s.DeleteProject(ctx, "a")
	if err == nil {
		t.Fatal("DeleteProject err=nil, want non-nil (running-run guard)")
	}
	if !contains(err.Error(), "running") {
		t.Errorf("err=%q want message containing \"running\"", err.Error())
	}
	if _, err := s.GetProjectBySlug(ctx, "a"); err != nil {
		t.Errorf("GetProjectBySlug after guarded delete err=%v want nil (project preserved)", err)
	}
}

func TestDeleteProjectCascadesTasksRuns(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Pipeline: "build"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "done"}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteProject(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTask(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTask after cascade err=%v want ErrNotFound", err)
	}
	if _, err := s.GetProjectBySlug(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetProjectBySlug after delete err=%v want ErrNotFound", err)
	}
}

// TestDeleteProjectRemovesSequenceDispatcher pins the Phase 2b smoke bug:
// a project with a sequence_dispatchers row (FK to projects) must delete
// cleanly — the dispatcher row is removed in the same transaction, not left
// to fail the FK constraint.
func TestDeleteProjectRemovesSequenceDispatcher(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSequenceDispatcher(ctx, &SequenceDispatcher{ProjectID: "p1", Status: "paused", AdvancementPolicy: "pr_opened"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "a"); err != nil {
		t.Fatalf("DeleteProject with a sequence dispatcher row failed: %v", err)
	}
	if _, err := s.GetSequenceDispatcher(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dispatcher row after delete err=%v want ErrNotFound", err)
	}
}

func TestTaskCountsByProject(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Two projects: p1 has 3 pending + 1 running + 1 done; p2 has 2 pending.
	// p3 exists but has zero tasks — should be absent from the result map.
	for _, p := range []*Project{
		{ID: "p1", Slug: "a", Name: "A", Status: "active"},
		{ID: "p2", Slug: "b", Name: "B", Status: "active"},
		{ID: "p3", Slug: "c", Name: "C", Status: "active"},
	} {
		if err := s.InsertProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// Insert tasks. Status defaults to "pending" if unset (InsertTask),
	// so we set explicit statuses for the running/done ones.
	tasks := []*Task{
		{ID: "t1", ProjectID: "p1", Title: "a1", Pipeline: "build", Status: "pending"},
		{ID: "t2", ProjectID: "p1", Title: "a2", Pipeline: "build", Status: "pending"},
		{ID: "t3", ProjectID: "p1", Title: "a3", Pipeline: "build", Status: "pending"},
		{ID: "t4", ProjectID: "p1", Title: "a4", Pipeline: "build", Status: "running"},
		{ID: "t5", ProjectID: "p1", Title: "a5", Pipeline: "build", Status: "done"},
		{ID: "t6", ProjectID: "p2", Title: "b1", Pipeline: "build", Status: "pending"},
		{ID: "t7", ProjectID: "p2", Title: "b2", Pipeline: "build", Status: "pending"},
	}
	for _, task := range tasks {
		if err := s.InsertTask(ctx, task); err != nil {
			t.Fatalf("InsertTask %s: %v", task.ID, err)
		}
	}

	counts, err := s.TaskCountsByProject(ctx)
	if err != nil {
		t.Fatalf("TaskCountsByProject: %v", err)
	}
	// p1 buckets
	if got := counts["p1"]["pending"]; got != 3 {
		t.Errorf("p1.pending=%d, want 3", got)
	}
	if got := counts["p1"]["running"]; got != 1 {
		t.Errorf("p1.running=%d, want 1", got)
	}
	if got := counts["p1"]["done"]; got != 1 {
		t.Errorf("p1.done=%d, want 1", got)
	}
	// p2 buckets
	if got := counts["p2"]["pending"]; got != 2 {
		t.Errorf("p2.pending=%d, want 2", got)
	}
	// p3 has no tasks → absent
	if _, ok := counts["p3"]; ok {
		t.Errorf("p3 should be absent from counts (no tasks); got %+v", counts["p3"])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
