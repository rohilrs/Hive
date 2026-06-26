package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ctx := context.Background()

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()

	v, err := s2.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != MaxSchemaVersion {
		t.Errorf("SchemaVersion=%d want %d", v, MaxSchemaVersion)
	}
}

func TestOpenInMemory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != MaxSchemaVersion {
		t.Errorf("SchemaVersion=%d want %d", v, MaxSchemaVersion)
	}
}

func TestProjectInsertAndGet(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	repoPath := "/home/u/code/auth"
	p := &Project{
		ID:       "proj-1",
		Slug:     "auth-service",
		Name:     "Auth Service",
		RepoPath: &repoPath,
		Sources:  map[string]any{"linear": map[string]any{"team": "AUTH"}},
		Config:   map[string]any{"concurrency_cap": float64(2)},
		Status:   "active",
	}
	if err := s.InsertProject(ctx, p); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetProject(ctx, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "auth-service" {
		t.Errorf("slug=%s", got.Slug)
	}
	if got.RepoPath == nil || *got.RepoPath != repoPath {
		t.Errorf("repo_path=%v", got.RepoPath)
	}
	if got.Sources["linear"] == nil {
		t.Errorf("sources lost")
	}
}

func TestProjectListActive(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	_ = s.InsertProject(ctx, &Project{ID: "a", Slug: "a", Name: "A", Status: "active"})
	_ = s.InsertProject(ctx, &Project{ID: "b", Slug: "b", Name: "B", Status: "paused"})
	_ = s.InsertProject(ctx, &Project{ID: "c", Slug: "c", Name: "C", Status: "active"})

	projects, err := s.ListProjects(ctx, "active")
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Errorf("got %d active, want 2", len(projects))
	}
}

// helper used in later tests
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTaskInsertAndGet(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()
	_ = s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"})

	task := &Task{
		ID:             "t1",
		ProjectID:      "p1",
		Source:         "inbox",
		Title:          "fix login bug",
		Body:           "session expires too soon",
		Priority:       "P1",
		Status:         "pending",
		PredictedFiles: []string{"src/auth/login.ts", "src/middleware/session.ts"},
		Metadata:       map[string]any{"linear_url": "https://linear.app/x"},
	}
	if err := s.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "fix login bug" {
		t.Errorf("title=%s", got.Title)
	}
	if len(got.PredictedFiles) != 2 {
		t.Errorf("predicted_files=%v", got.PredictedFiles)
	}
}

func TestListPendingTasksOrderedByPriorityThenAge(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()
	_ = s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"})

	now := time.Now().UTC()
	older := now.Add(-2 * time.Hour)
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "low old", Priority: "P3", Status: "pending", Source: "inbox", CreatedAt: older})
	_ = s.InsertTask(ctx, &Task{ID: "t2", ProjectID: "p1", Title: "high new", Priority: "P1", Status: "pending", Source: "inbox", CreatedAt: now})
	_ = s.InsertTask(ctx, &Task{ID: "t3", ProjectID: "p1", Title: "high old", Priority: "P1", Status: "pending", Source: "inbox", CreatedAt: older})

	list, err := s.ListPendingTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d tasks", len(list))
	}
	if list[0].ID != "t3" || list[1].ID != "t2" || list[2].ID != "t1" {
		t.Errorf("order: %s %s %s", list[0].ID, list[1].ID, list[2].ID)
	}
}

func TestUpdateTaskPrediction(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()
	_ = s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Priority: "P3", Status: "pending", Source: "inbox"})

	err := s.UpdateTaskPrediction(ctx, "t1",
		[]string{"src/a.go", "src/b.go"},
		[]string{"src/a.go", "src/b.go", "src/c.go"},
		82,
	)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetTask(ctx, "t1")
	if got.PredictConfidence != 82 {
		t.Errorf("confidence=%d", got.PredictConfidence)
	}
	if len(got.ConflictSet) != 3 {
		t.Errorf("conflict_set=%v", got.ConflictSet)
	}
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()
	_ = s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"})

	r := &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "pending"}
	if err := s.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkRunStarted(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRunEnded(ctx, "r1", "done", "shipped"); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetRun(ctx, "r1")
	if got.Status != "done" {
		t.Errorf("status=%s", got.Status)
	}
	if got.Summary != "shipped" {
		t.Errorf("summary=%s", got.Summary)
	}
	if got.StartedAt == nil || got.EndedAt == nil {
		t.Error("timestamps not set")
	}
}

func TestRunParentLinkage(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()
	_ = s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"})

	parent := &Run{ID: "parent", TaskID: "t1", ProjectID: "p1", Pipeline: "finish-branch", Status: "running"}
	if err := s.InsertRun(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &Run{ID: "child", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "pending", ParentRunID: "parent"}
	if err := s.InsertRun(ctx, child); err != nil {
		t.Fatal(err)
	}

	gotChild, err := s.GetRun(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.ParentRunID != "parent" {
		t.Errorf("child ParentRunID=%q want %q", gotChild.ParentRunID, "parent")
	}

	gotParent, err := s.GetRun(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.ParentRunID != "" {
		t.Errorf("parent ParentRunID=%q want empty (root run)", gotParent.ParentRunID)
	}
}

func TestListRunningRuns(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()
	_ = s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"})

	_ = s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running"})
	_ = s.InsertRun(ctx, &Run{ID: "r2", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "done"})
	_ = s.InsertRun(ctx, &Run{ID: "r3", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running"})

	running, err := s.ListRunningRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 2 {
		t.Errorf("got %d running, want 2", len(running))
	}
}

// TestListRunningRunsExcludesChildren is Phase 4.3.1 #3 contract: child
// fix runs (parent_run_id != NULL) must not appear in ListRunningRuns —
// it surfaces only root subprocess-worker holders so capacity accounting
// counts one slot per actual worker.
func TestListRunningRunsExcludesChildren(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "t", Body: "x", Priority: "P1", Status: "running", Pipeline: "finish-branch"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r-parent", TaskID: "t1", ProjectID: "p1", Pipeline: "finish-branch", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r-child", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running", ParentRunID: "r-parent"}); err != nil {
		t.Fatal(err)
	}

	running, err := s.ListRunningRuns(ctx)
	if err != nil {
		t.Fatalf("ListRunningRuns: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("got %d rows, want 1 (children excluded)", len(running))
	}
	if running[0].ID != "r-parent" {
		t.Errorf("got %s, want r-parent", running[0].ID)
	}
}

// TestListAllRunningRunsIncludesChildren is the companion to the filter
// in ListRunningRuns — ListAllRunningRuns is the comprehensive view used
// by recovery sweeps, status panels, and chat surfaces.
func TestListAllRunningRunsIncludesChildren(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "t", Body: "x", Priority: "P1", Status: "running", Pipeline: "finish-branch"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r-parent", TaskID: "t1", ProjectID: "p1", Pipeline: "finish-branch", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r-child", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running", ParentRunID: "r-parent"}); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListAllRunningRuns(ctx)
	if err != nil {
		t.Fatalf("ListAllRunningRuns: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rows, want 2 (parent + child)", len(all))
	}
}

func TestMarkDocumentationSkipped(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()
	_ = s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"})
	_ = s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running"})

	if err := s.MarkDocumentationSkipped(ctx, "r1", "subprocess timeout"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun(ctx, "r1")
	if !got.DocumentationSkipped {
		t.Error("flag not set")
	}
	if got.DocumentationSkipReason != "subprocess timeout" {
		t.Errorf("reason=%s", got.DocumentationSkipReason)
	}
}


func TestStoreCreatesIterFeedbackTable(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(dir, "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var name string
	err = s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='iter_feedback'`).Scan(&name)
	if err != nil {
		t.Fatalf("iter_feedback table missing: %v", err)
	}
}

func TestEventAppendAndFTS(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	_ = s.AppendEvent(ctx, &Event{
		RunID:   "r1",
		StageID: "s1",
		Type:    "tool_call",
		Message: "Bash npm install fastify@5",
		Payload: map[string]any{"command": "npm install fastify@5"},
	})
	_ = s.AppendEvent(ctx, &Event{
		RunID: "r1", Type: "text", Message: "Trying to upgrade fastify version",
	})
	_ = s.AppendEvent(ctx, &Event{
		RunID: "r2", Type: "tool_call", Message: "Read login.ts",
	})

	hits, err := s.SearchEvents(ctx, "fastify", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Errorf("got %d hits for 'fastify', want 2", len(hits))
	}

	list, err := s.ListEventsForRun(ctx, "r1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d events for r1, want 2", len(list))
	}
	if list[0].Payload["command"] != "npm install fastify@5" {
		t.Errorf("payload not preserved: %+v", list[0].Payload)
	}
}
