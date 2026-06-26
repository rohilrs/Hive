package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestInsertTaskPersistsPipeline(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Pipeline: "plan"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Pipeline != "plan" {
		t.Errorf("Pipeline=%q want plan", got.Pipeline)
	}
}

func TestInsertTaskDefaultsPipelineToBuild(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Pipeline != "build" {
		t.Errorf("Pipeline=%q want build (default)", got.Pipeline)
	}
}

func TestTaskParentTaskIDRoundtrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Insert a project + parent task + child task with parent_task_id set.
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	parent := &Task{ID: "task-parent", ProjectID: "p1", Title: "parent", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}
	if err := s.InsertTask(ctx, parent); err != nil {
		t.Fatalf("InsertTask parent: %v", err)
	}
	child := &Task{
		ID: "task-child", ProjectID: "p1", Title: "child", Body: "y", Priority: "P1", Status: "pending", Pipeline: "build",
		ParentTaskID: sql.NullString{String: "task-parent", Valid: true},
	}
	if err := s.InsertTask(ctx, child); err != nil {
		t.Fatalf("InsertTask child: %v", err)
	}

	got, err := s.GetTask(ctx, "task-child")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !got.ParentTaskID.Valid || got.ParentTaskID.String != "task-parent" {
		t.Errorf("ParentTaskID=%v, want valid=task-parent", got.ParentTaskID)
	}

	// Parent should have NULL ParentTaskID.
	got, err = s.GetTask(ctx, "task-parent")
	if err != nil {
		t.Fatalf("GetTask parent: %v", err)
	}
	if got.ParentTaskID.Valid {
		t.Errorf("parent ParentTaskID unexpectedly valid: %v", got.ParentTaskID)
	}
}

func TestInsertSubtasksTransactional(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	parent := &Task{ID: "task-parent", ProjectID: "p1", Title: "parent", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}
	if err := s.InsertTask(ctx, parent); err != nil {
		t.Fatalf("InsertTask parent: %v", err)
	}

	items := []SubtaskInput{
		{Title: "child a", Body: "ba", Priority: "P0", Pipeline: "build"},
		{Title: "child b", Body: "bb", Priority: "P1", Pipeline: "debug"},
		{Title: "child c", Body: "bc", Priority: "P2", Pipeline: "build"},
	}
	ids, err := s.InsertSubtasks(ctx, "task-parent", items)
	if err != nil {
		t.Fatalf("InsertSubtasks: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("got %d ids, want 3", len(ids))
	}
	for i, id := range ids {
		got, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask %s: %v", id, err)
		}
		if !got.ParentTaskID.Valid || got.ParentTaskID.String != "task-parent" {
			t.Errorf("child[%d] parent=%v, want task-parent", i, got.ParentTaskID)
		}
		if got.ProjectID != "p1" {
			t.Errorf("child[%d] project=%s, want p1 (inherited)", i, got.ProjectID)
		}
		if got.Title != items[i].Title {
			t.Errorf("child[%d] title=%q, want %q", i, got.Title, items[i].Title)
		}
		if got.Pipeline != items[i].Pipeline {
			t.Errorf("child[%d] pipeline=%q, want %q", i, got.Pipeline, items[i].Pipeline)
		}
		if got.Priority != items[i].Priority {
			t.Errorf("child[%d] priority=%q, want %q", i, got.Priority, items[i].Priority)
		}
	}
}

func TestInsertSubtasksParentNotFoundIsError(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	items := []SubtaskInput{{Title: "x", Body: "y", Priority: "P1", Pipeline: "build"}}
	if _, err := s.InsertSubtasks(ctx, "nonexistent", items); err == nil {
		t.Error("expected error for missing parent; got nil")
	}
}

// TestInsertSubtasksRollbackOnFailure asserts that on any error path
// in InsertSubtasks (parent lookup, BeginTx, or mid-loop Exec), no
// child rows land. We cancel the context BEFORE the call so the
// store's context-aware methods return ctx.Err(). Depending on which
// step trips first (GetTask vs. BeginTx vs. tx.ExecContext), the
// rollback's defer may or may not actually do work — but the contract
// "any error => no children landed" holds either way, which is the
// regression we want to lock in.
func TestInsertSubtasksRollbackOnFailure(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.InsertProject(context.Background(), &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	parent := &Task{ID: "task-parent", ProjectID: "p1", Title: "parent", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}
	if err := s.InsertTask(context.Background(), parent); err != nil {
		t.Fatalf("InsertTask parent: %v", err)
	}

	// Cancel before the call — every ctx-aware op will fail.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	items := []SubtaskInput{
		{Title: "a", Body: "ba", Priority: "P0", Pipeline: "build"},
		{Title: "b", Body: "bb", Priority: "P1", Pipeline: "build"},
	}
	if _, err := s.InsertSubtasks(ctx, "task-parent", items); err == nil {
		t.Fatal("expected error from canceled context; got nil")
	}

	// Verify the contract: zero children land regardless of which step
	// failed. Use a fresh (uncanceled) context to inspect.
	children, err := s.ListChildTasks(context.Background(), "task-parent")
	if err != nil {
		t.Fatalf("ListChildTasks: %v", err)
	}
	if len(children) != 0 {
		t.Errorf("expected 0 children after failed InsertSubtasks, got %d", len(children))
	}
}

func TestListChildTasksReturnsOnlyChildren(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	parent := &Task{ID: "task-parent", ProjectID: "p1", Title: "parent", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}
	if err := s.InsertTask(ctx, parent); err != nil {
		t.Fatalf("InsertTask parent: %v", err)
	}
	unrelated := &Task{ID: "task-other", ProjectID: "p1", Title: "other", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}
	if err := s.InsertTask(ctx, unrelated); err != nil {
		t.Fatalf("InsertTask other: %v", err)
	}
	items := []SubtaskInput{
		{Title: "a", Body: "ba", Priority: "P1", Pipeline: "build"},
		{Title: "b", Body: "bb", Priority: "P1", Pipeline: "build"},
	}
	if _, err := s.InsertSubtasks(ctx, "task-parent", items); err != nil {
		t.Fatalf("InsertSubtasks: %v", err)
	}
	children, err := s.ListChildTasks(ctx, "task-parent")
	if err != nil {
		t.Fatalf("ListChildTasks: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("got %d children, want 2", len(children))
	}
	// Unrelated task should NOT appear in children of task-parent.
	for _, c := range children {
		if c.ID == "task-other" {
			t.Error("unrelated task showed up in children")
		}
	}
}

func TestListTaskSourceIDsForProject(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatalf("InsertProject p1: %v", err)
	}
	if err := s.InsertProject(ctx, &Project{ID: "p2", Slug: "p2", Name: "P2"}); err != nil {
		t.Fatalf("InsertProject p2: %v", err)
	}
	// p1: 2 github tasks + 1 linear task.
	if err := s.InsertTask(ctx, &Task{ID: "t-gh-1", ProjectID: "p1", Source: "github", SourceID: "42", Title: "gh42"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t-gh-2", ProjectID: "p1", Source: "github", SourceID: "99", Title: "gh99"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t-lin-1", ProjectID: "p1", Source: "linear", SourceID: "lin-abc", Title: "lin"}); err != nil {
		t.Fatal(err)
	}
	// p2: a github task whose source_id collides with one of p1's — must NOT
	// bleed into p1's set. This is the cross-project isolation the dedup
	// path relies on.
	if err := s.InsertTask(ctx, &Task{ID: "t-gh-other", ProjectID: "p2", Source: "github", SourceID: "42", Title: "p2gh42"}); err != nil {
		t.Fatal(err)
	}
	// A github task with empty source_id is filtered out (defensive — this
	// shouldn't happen for gh tasks today but the SQL guards against it).
	if err := s.InsertTask(ctx, &Task{ID: "t-gh-empty", ProjectID: "p1", Source: "github", SourceID: "", Title: "no sid"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListTaskSourceIDsForProject(ctx, "p1", "github")
	if err != nil {
		t.Fatalf("ListTaskSourceIDsForProject p1/github: %v", err)
	}
	if !got["42"] || !got["99"] {
		t.Errorf("p1/github = %v, want both 42 and 99", got)
	}
	if got[""] {
		t.Errorf("p1/github contained empty-string key: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("p1/github len=%d, want 2 (got %v)", len(got), got)
	}

	got, err = s.ListTaskSourceIDsForProject(ctx, "p1", "linear")
	if err != nil {
		t.Fatal(err)
	}
	if !got["lin-abc"] || len(got) != 1 {
		t.Errorf("p1/linear = %v, want {lin-abc}", got)
	}

	// p2 only sees its own row.
	got, err = s.ListTaskSourceIDsForProject(ctx, "p2", "github")
	if err != nil {
		t.Fatal(err)
	}
	if !got["42"] || len(got) != 1 {
		t.Errorf("p2/github = %v, want only {42}", got)
	}
}

func TestMergeTaskMetadataPreservesUnmentionedKeys(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{
		ID: "t1", ProjectID: "p1", Title: "x",
		Metadata: map[string]any{"branch_name": "old", "external_id": "HBA-42"},
	}); err != nil {
		t.Fatal(err)
	}

	// Merge: overwrite branch_name, add linked_github_url, leave external_id alone.
	err = s.MergeTaskMetadata(ctx, "t1", map[string]string{
		"branch_name":       "new",
		"linked_github_url": "https://github.com/rohilrs/Hive/issues/100",
	})
	if err != nil {
		t.Fatalf("MergeTaskMetadata: %v", err)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["branch_name"] != "new" {
		t.Errorf("branch_name = %v, want \"new\"", got.Metadata["branch_name"])
	}
	if got.Metadata["external_id"] != "HBA-42" {
		t.Errorf("external_id = %v, want preserved \"HBA-42\"", got.Metadata["external_id"])
	}
	if got.Metadata["linked_github_url"] != "https://github.com/rohilrs/Hive/issues/100" {
		t.Errorf("linked_github_url = %v, want added", got.Metadata["linked_github_url"])
	}
}

func TestGateStateAndListByProject(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, &Project{ID: "p2", Slug: "p2", Name: "P2", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []*Task{
		{ID: "t1", ProjectID: "p1", Source: "inbox", Title: "a", Status: "pending"},
		{ID: "t2", ProjectID: "p1", Source: "inbox", Title: "b", Status: "done"},
		{ID: "t3", ProjectID: "p2", Source: "inbox", Title: "c", Status: "pending"},
	} {
		if err := s.InsertTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.GateState != "none" {
		t.Errorf("default gate_state = %q, want none", got.GateState)
	}
	if err := s.UpdateTaskGateState(ctx, "t1", "built"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, "t1")
	if got.GateState != "built" {
		t.Errorf("gate_state = %q, want built", got.GateState)
	}
	p1tasks, err := s.ListTasksByProject(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p1tasks) != 2 {
		t.Fatalf("p1 tasks = %d, want 2", len(p1tasks))
	}
	p2tasks, _ := s.ListTasksByProject(ctx, "p2")
	if len(p2tasks) != 1 {
		t.Fatalf("p2 tasks = %d, want 1", len(p2tasks))
	}
}

func TestMergeTaskMetadataEmptyInputNoop(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{
		ID: "t1", ProjectID: "p1", Title: "x",
		Metadata: map[string]any{"k": "v"},
	}); err != nil {
		t.Fatal(err)
	}
	// Nil + empty map must NOT touch the row (no-op fast-path).
	if err := s.MergeTaskMetadata(ctx, "t1", nil); err != nil {
		t.Fatalf("MergeTaskMetadata nil: %v", err)
	}
	if err := s.MergeTaskMetadata(ctx, "t1", map[string]string{}); err != nil {
		t.Fatalf("MergeTaskMetadata empty: %v", err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["k"] != "v" {
		t.Errorf("metadata mutated by no-op merge: %v", got.Metadata)
	}
}

func TestListTasksByGateState(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Two projects to prove the query spans all projects.
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, &Project{ID: "p2", Slug: "p2", Name: "P2", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []*Task{
		{ID: "t1", ProjectID: "p1", Source: "inbox", Title: "a", Status: "running"},
		{ID: "t2", ProjectID: "p2", Source: "inbox", Title: "b", Status: "running"},
		{ID: "t3", ProjectID: "p1", Source: "inbox", Title: "c", Status: "done"},
	} {
		if err := s.InsertTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// Park t1 and t2 in awaiting_merge (across two projects); leave t3 satisfied.
	if err := s.UpdateTaskGateState(ctx, "t1", "awaiting_merge"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskGateState(ctx, "t2", "awaiting_merge"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskGateState(ctx, "t3", "satisfied"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListTasksByGateState(ctx, "awaiting_merge")
	if err != nil {
		t.Fatalf("ListTasksByGateState: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d awaiting_merge tasks, want 2", len(got))
	}
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids["t1"] || !ids["t2"] {
		t.Errorf("got ids %v, want {t1,t2}", ids)
	}
	if ids["t3"] {
		t.Errorf("satisfied task t3 leaked into awaiting_merge set")
	}

	// A gate_state nobody is in returns empty (not an error).
	none, err := s.ListTasksByGateState(ctx, "built")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("got %d for empty gate_state, want 0", len(none))
	}
}

func TestLinearSyncedStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID: "task-ls1", ProjectID: "p1", Source: "inbox",
		Title: "t", Status: "pending", Pipeline: "build",
	}
	if err := s.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LinearSyncedState != "" {
		t.Errorf("default LinearSyncedState = %q, want empty", got.LinearSyncedState)
	}
	if err := s.UpdateTaskLinearSyncedState(ctx, task.ID, "in_progress"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, task.ID)
	if got.LinearSyncedState != "in_progress" {
		t.Errorf("after update = %q, want in_progress", got.LinearSyncedState)
	}
}

func TestListPendingTasksByProject(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "demo", Name: "Demo", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	mk := func(id, status string) {
		if err := s.InsertTask(ctx, &Task{ID: id, ProjectID: "p1", Title: id, Status: status, Pipeline: "build", Priority: "P1"}); err != nil {
			t.Fatal(err)
		}
	}
	mk("t-pending", "pending")
	mk("t-running", "running")
	mk("t-done", "done")
	mk("t-needs-attention", "needs_attention")
	mk("t-source-closed", "source_closed")
	mk("t-abandoned", "abandoned")

	got, err := s.ListPendingTasksByProject(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "t-pending" {
		t.Fatalf("got %d tasks %v, want only t-pending", len(got), got)
	}
}

// TestListTasksByStatuses covers the TUI initial-state fix: the filter returns
// pending AND needs_attention (excluding done), so the needs-attention lane
// seeds on a fresh TUI load.
// TestScanTaskTolerantTimestamp asserts that a task row whose updated_at
// column was written as an RFC3339 TEXT string (the bug: some path stored
// a text timestamp instead of an integer Unix epoch) is scanned without
// error, and that the returned UpdatedAt is non-zero.
func TestScanTaskTolerantTimestamp(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "tolerant-ts", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}

	// Corrupt the updated_at column by writing a TEXT RFC3339 value directly.
	// This simulates the real-world bug where a rogue write path stored a
	// string instead of an integer epoch, causing scanTask to fail with
	// "converting string to int64".
	_, execErr := s.db.ExecContext(ctx, `UPDATE tasks SET updated_at = '2026-06-07T23:02:34Z' WHERE id = 't1'`)
	if execErr != nil {
		t.Fatalf("corrupt updated_at: %v", execErr)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask with TEXT updated_at failed: %v", err)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero; want parsed RFC3339 time")
	}
}

// TestListTasksByProjectSkipsBadRow asserts that a task row with corrupt
// metadata JSON (invalid JSON that json.Unmarshal will reject) is skipped
// with a log message instead of aborting the whole list. Sibling rows
// must still be returned.
func TestListTasksByProjectSkipsBadRow(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "good", ProjectID: "p1", Title: "good task", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "bad", ProjectID: "p1", Title: "bad task", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}

	// Corrupt the metadata column to invalid JSON — this is NOT rescued by
	// Task 7's tolerant timestamp parsing; it hits json.Unmarshal and errors.
	if _, execErr := s.db.ExecContext(ctx, `UPDATE tasks SET metadata = '{not json' WHERE id = 'bad'`); execErr != nil {
		t.Fatalf("corrupt metadata: %v", execErr)
	}

	got, err := s.ListTasksByProject(ctx, "p1")
	if err != nil {
		t.Fatalf("ListTasksByProject returned error for corrupt row: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tasks, want 1 (the good one)", len(got))
	}
	if got[0].ID != "good" {
		t.Errorf("got task ID %q, want \"good\"", got[0].ID)
	}
}

func TestTaskLastFailureFeedback(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Body: "x", Status: "needs_attention"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskLastFailureFeedback(ctx, "t1", `{"summary":"S"}`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastFailureFeedback != `{"summary":"S"}` {
		t.Errorf("got %q", got.LastFailureFeedback)
	}
	if err := s.ClearTaskLastFailureFeedback(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got2.LastFailureFeedback != "" {
		t.Errorf("expected cleared, got %q", got2.LastFailureFeedback)
	}
}

func TestListTasksByStatuses(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	mk := func(id, status string) {
		if err := s.InsertTask(ctx, &Task{ID: id, ProjectID: "p1", Source: "inbox", Title: id, Status: status, Priority: "P1"}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	mk("pend", "pending")
	mk("na", "needs_attention")
	mk("done", "done")

	got, err := s.ListTasksByStatuses(ctx, []string{"pending", "needs_attention"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, t := range got {
		ids[t.ID] = true
	}
	if !ids["pend"] || !ids["na"] {
		t.Errorf("want pend+na, got %v", ids)
	}
	if ids["done"] {
		t.Errorf("done should be excluded, got %v", ids)
	}
	// Empty statuses → no rows (no accidental full-table scan).
	if rows, err := s.ListTasksByStatuses(ctx, nil); err != nil || rows != nil {
		t.Errorf("empty statuses: want (nil,nil), got (%v,%v)", rows, err)
	}
}
