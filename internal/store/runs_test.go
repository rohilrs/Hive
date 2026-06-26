package store

import (
	"context"
	"testing"
	"time"
)

func TestLatestNonTerminalRunForTaskReturnsNilWhenAllTerminal(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r2", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "abandoned"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LatestNonTerminalRunForTask(ctx, "t1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("got run %+v, want nil (all terminal)", got)
	}
}

func TestLatestNonTerminalRunForTaskPicksMostRecent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}

	// Use distinct CreatedAt values so COALESCE(started_at, created_at) gives
	// deterministic ordering even when started_at is NULL (pending).
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldPending := &Run{
		ID: "old-pending", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "pending",
		CreatedAt: base.Add(100 * time.Second),
	}
	midAttention := &Run{
		ID: "mid-attention", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "needs_attention",
		CreatedAt: base.Add(200 * time.Second),
	}
	newestRunning := &Run{
		ID: "newest-running", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "running",
		CreatedAt: base.Add(300 * time.Second),
	}
	// Terminal — must be ignored even with the latest created_at.
	newerButDone := &Run{
		ID: "newer-but-done", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "done",
		CreatedAt: base.Add(999 * time.Second),
	}

	for _, r := range []*Run{oldPending, midAttention, newestRunning, newerButDone} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun %s: %v", r.ID, err)
		}
	}

	got, err := s.LatestNonTerminalRunForTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("got nil, want newest-running")
	}
	if got.ID != "newest-running" {
		t.Errorf("ID=%q, want newest-running", got.ID)
	}
}

func TestLatestNonTerminalRunForTaskUnknownTaskReturnsNil(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.LatestNonTerminalRunForTask(ctx, "ghost")
	if err != nil {
		t.Fatalf("unknown task should not error: %v", err)
	}
	if got != nil {
		t.Errorf("got run %+v, want nil for unknown task", got)
	}
}

func TestLatestDoneBuildRunForTaskReturnsNilWhenNoDoneRun(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	// A running build run — should NOT be returned.
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LatestDoneBuildRunForTask(ctx, "t1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("got run %+v, want nil (no done build run)", got)
	}
}

func TestLatestDoneBuildRunForTaskReturnsDoneOverRunning(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	doneBuild := &Run{
		ID: "done-build", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "done",
		CreatedAt: base.Add(100 * time.Second),
	}
	runningBuild := &Run{
		ID: "running-build", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "running",
		CreatedAt: base.Add(200 * time.Second),
	}
	// A done finish-branch run — pipeline != 'build', must be ignored.
	doneFinish := &Run{
		ID: "done-finish", TaskID: "t1", ProjectID: "p1",
		Pipeline: "finish-branch", Status: "done",
		CreatedAt: base.Add(300 * time.Second),
	}
	// An auto-fix child build run (RunChildFix): pipeline='build', status='done',
	// and MORE RECENT than done-build — but it carries a parent_run_id and never
	// records its own branch, so it must be excluded. Without the parent_run_id
	// IS NULL filter this would be picked and `hive run finish` would fail with
	// "no recorded branch" (live-smoke regression, 2026-06-06).
	childFix := &Run{
		ID: "child-fix", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "done", ParentRunID: "done-finish",
		CreatedAt: base.Add(400 * time.Second),
	}

	for _, r := range []*Run{doneBuild, runningBuild, doneFinish, childFix} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun %s: %v", r.ID, err)
		}
	}

	got, err := s.LatestDoneBuildRunForTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("got nil, want done-build")
	}
	if got.ID != "done-build" {
		t.Errorf("ID=%q, want done-build", got.ID)
	}
}

func TestLatestDoneBuildRunForTaskUnknownTaskReturnsNil(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.LatestDoneBuildRunForTask(ctx, "ghost")
	if err != nil {
		t.Fatalf("unknown task should not error: %v", err)
	}
	if got != nil {
		t.Errorf("got run %+v, want nil for unknown task", got)
	}
}

// workerPIDTestSetup creates an in-memory store with a project + task so the
// new worker_pid tests can insert runs against a real FK target. Returns the
// store; the caller is responsible for Close.
func workerPIDTestSetup(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func insertWorkerPIDTestRun(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.InsertRun(context.Background(), &Run{
		ID: id, TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running",
	}); err != nil {
		t.Fatalf("InsertRun %s: %v", id, err)
	}
}

func TestSetRunWorkerPIDRoundtrip(t *testing.T) {
	s := workerPIDTestSetup(t)
	defer s.Close()
	ctx := context.Background()

	insertWorkerPIDTestRun(t, s, "r1")
	if err := s.SetRunWorkerPID(ctx, "r1", 12345); err != nil {
		t.Fatalf("SetRunWorkerPID: %v", err)
	}

	rows, err := s.ListRunsWithWorkerPID(ctx)
	if err != nil {
		t.Fatalf("ListRunsWithWorkerPID: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if !rows[0].WorkerPID.Valid {
		t.Errorf("WorkerPID not Valid")
	}
	if rows[0].WorkerPID.Int64 != 12345 {
		t.Errorf("WorkerPID=%d, want 12345", rows[0].WorkerPID.Int64)
	}
}

func TestClearRunWorkerPIDNullsTheColumn(t *testing.T) {
	s := workerPIDTestSetup(t)
	defer s.Close()
	ctx := context.Background()

	insertWorkerPIDTestRun(t, s, "r1")
	if err := s.SetRunWorkerPID(ctx, "r1", 12345); err != nil {
		t.Fatalf("SetRunWorkerPID: %v", err)
	}
	if err := s.ClearRunWorkerPID(ctx, "r1"); err != nil {
		t.Fatalf("ClearRunWorkerPID: %v", err)
	}
	rows, err := s.ListRunsWithWorkerPID(ctx)
	if err != nil {
		t.Fatalf("ListRunsWithWorkerPID: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after clear, got %d", len(rows))
	}
}

func TestClearRunWorkerPIDIdempotent(t *testing.T) {
	s := workerPIDTestSetup(t)
	defer s.Close()
	ctx := context.Background()

	insertWorkerPIDTestRun(t, s, "r1")
	// Clear without ever setting — should not error.
	if err := s.ClearRunWorkerPID(ctx, "r1"); err != nil {
		t.Errorf("ClearRunWorkerPID on never-set: %v", err)
	}
	// Clear a non-existent run — should not error (no-op).
	if err := s.ClearRunWorkerPID(ctx, "nonexistent-run"); err != nil {
		t.Errorf("ClearRunWorkerPID on missing run: %v", err)
	}
}

func TestListRunsWithWorkerPIDReturnsOnlyNonNull(t *testing.T) {
	s := workerPIDTestSetup(t)
	defer s.Close()
	ctx := context.Background()

	// Insert two runs; set pid on only one.
	insertWorkerPIDTestRun(t, s, "r-with-pid")
	insertWorkerPIDTestRun(t, s, "r-no-pid") // no pid set; should not appear

	if err := s.SetRunWorkerPID(ctx, "r-with-pid", 99999); err != nil {
		t.Fatalf("SetRunWorkerPID: %v", err)
	}
	rows, err := s.ListRunsWithWorkerPID(ctx)
	if err != nil {
		t.Fatalf("ListRunsWithWorkerPID: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].ID != "r-with-pid" {
		t.Errorf("got run %s, want r-with-pid", rows[0].ID)
	}
}

func TestSetRunBranchAndPR(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	// Fresh run: branch/PR empty.
	got, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BranchName != "" || got.PRURL != "" || got.PRNumber.Valid {
		t.Fatalf("fresh run not empty: %+v", got)
	}

	// Set branch.
	if err := s.SetRunBranch(ctx, "r1", "hive/run-r1/x"); err != nil {
		t.Fatal(err)
	}
	// Set PR.
	if err := s.SetRunPR(ctx, "r1", "https://github.com/o/repo/pull/42", 42); err != nil {
		t.Fatal(err)
	}

	got, err = s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BranchName != "hive/run-r1/x" {
		t.Errorf("branch = %q, want hive/run-r1/x", got.BranchName)
	}
	if got.PRURL != "https://github.com/o/repo/pull/42" {
		t.Errorf("pr_url = %q", got.PRURL)
	}
	if !got.PRNumber.Valid || got.PRNumber.Int64 != 42 {
		t.Errorf("pr_number = %+v, want 42", got.PRNumber)
	}
}

func TestListRunsByTask(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "task one", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t2", ProjectID: "p1", Title: "task two", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Two runs for t1: a build (done) and a finish-branch (done).
	r1 := &Run{
		ID: "r1", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "done",
		CreatedAt: base.Add(100 * time.Second),
	}
	r2 := &Run{
		ID: "r2", TaskID: "t1", ProjectID: "p1",
		Pipeline: "finish-branch", Status: "done",
		CreatedAt: base.Add(200 * time.Second),
	}
	// One run for t2 — must not appear in t1 results.
	r3 := &Run{
		ID: "r3", TaskID: "t2", ProjectID: "p1",
		Pipeline: "build", Status: "done",
		CreatedAt: base.Add(300 * time.Second),
	}

	for _, r := range []*Run{r1, r2, r3} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun %s: %v", r.ID, err)
		}
	}

	got, err := s.ListRunsByTask(ctx, "t1")
	if err != nil {
		t.Fatalf("ListRunsByTask: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d runs, want 2", len(got))
	}
	// Newest-first: r2 (finish-branch, created_at+200s) before r1 (build, created_at+100s).
	if got[0].ID != "r2" {
		t.Errorf("got[0].ID = %q, want r2 (newest)", got[0].ID)
	}
	if got[1].ID != "r1" {
		t.Errorf("got[1].ID = %q, want r1", got[1].ID)
	}
}

func TestListRecentDoneTasks(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "task one", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t2", ProjectID: "p1", Title: "task two", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}

	// t1: 3 done runs with distinct ended_at values:
	//   r-build       ended=100  (earliest)
	//   r-child-build ended=150
	//   r-finish      ended=200  (latest — should be the representative row)
	for _, r := range []*Run{
		{ID: "r-build", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "done"},
		{ID: "r-child-build", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "done", ParentRunID: "r-build"},
		{ID: "r-finish", TaskID: "t1", ProjectID: "p1", Pipeline: "finish-branch", Status: "done"},
	} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun %s: %v", r.ID, err)
		}
	}
	// Set ended_at directly since MarkRunEnded uses time.Now() which doesn't
	// give us precise deterministic values.
	for id, ts := range map[string]int64{"r-build": 100, "r-child-build": 150, "r-finish": 200} {
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET ended_at = ? WHERE id = ?`, ts, id); err != nil {
			t.Fatalf("set ended_at for %s: %v", id, err)
		}
	}

	// t2: 1 done run, ended=300 (newer than t1's latest, so t2 should appear first)
	if err := s.InsertRun(ctx, &Run{ID: "r-t2", TaskID: "t2", ProjectID: "p1", Pipeline: "build", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET ended_at = 300 WHERE id = 'r-t2'`); err != nil {
		t.Fatalf("set ended_at for r-t2: %v", err)
	}

	got, err := s.ListRecentDoneTasks(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentDoneTasks: %v", err)
	}

	// Must collapse to exactly 2 rows (one per task, not one per run).
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (one per task)", len(got))
	}

	// Newest-first: t2 (ended=300) before t1 (ended=200).
	if got[0].TaskID != "t2" {
		t.Errorf("got[0].TaskID = %q, want t2 (ended=300 is newest)", got[0].TaskID)
	}
	if got[1].TaskID != "t1" {
		t.Errorf("got[1].TaskID = %q, want t1", got[1].TaskID)
	}

	// The t1 row must be the latest done run (r-finish, ended=200), not r-build or r-child-build.
	if got[1].ID != "r-finish" {
		t.Errorf("t1 representative run ID = %q, want r-finish (latest done, ended=200)", got[1].ID)
	}
}

// TestListRecentDoneTasksLimitCaps verifies that the limit parameter is
// respected: seeding 3 tasks with done runs and calling with limit=1 must
// return exactly 1 row — the newest by ended_at.
func TestListRecentDoneTasksLimitCaps(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := s.InsertTask(ctx, &Task{ID: id, ProjectID: "p1", Title: id, Source: "inbox"}); err != nil {
			t.Fatalf("InsertTask %s: %v", id, err)
		}
	}
	// Three tasks with ascending ended_at so t3 is the newest.
	for i, pair := range []struct {
		runID  string
		taskID string
		ts     int64
	}{
		{"r1", "t1", 100},
		{"r2", "t2", 200},
		{"r3", "t3", 300},
	} {
		_ = i
		if err := s.InsertRun(ctx, &Run{
			ID: pair.runID, TaskID: pair.taskID, ProjectID: "p1",
			Pipeline: "build", Status: "done",
		}); err != nil {
			t.Fatalf("InsertRun %s: %v", pair.runID, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET ended_at = ? WHERE id = ?`, pair.ts, pair.runID); err != nil {
			t.Fatalf("set ended_at for %s: %v", pair.runID, err)
		}
	}

	got, err := s.ListRecentDoneTasks(ctx, 1)
	if err != nil {
		t.Fatalf("ListRecentDoneTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (limit=1)", len(got))
	}
	if got[0].TaskID != "t3" {
		t.Errorf("got TaskID=%q, want t3 (newest by ended_at=300)", got[0].TaskID)
	}
}

// TestListRecentDoneTasksSameSecondTieCollapses verifies that two done runs
// of the same task with identical ended_at values collapse to exactly one
// row (the one with the higher id, i.e. later-created).
func TestListRecentDoneTasksSameSecondTieCollapses(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "t1", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}

	// Two done runs for t1 sharing the same ended_at second. Use IDs that
	// sort lexicographically so "run-b" > "run-a", making run-b the winner.
	for _, id := range []string{"run-a", "run-b"} {
		if err := s.InsertRun(ctx, &Run{
			ID: id, TaskID: "t1", ProjectID: "p1",
			Pipeline: "build", Status: "done",
		}); err != nil {
			t.Fatalf("InsertRun %s: %v", id, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET ended_at = 500 WHERE id = ?`, id); err != nil {
			t.Fatalf("set ended_at for %s: %v", id, err)
		}
	}

	got, err := s.ListRecentDoneTasks(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentDoneTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows for one task, want 1 (same-second tie must collapse)", len(got))
	}
	// id DESC picks "run-b" over "run-a".
	if got[0].ID != "run-b" {
		t.Errorf("got ID=%q, want run-b (higher id wins same-second tie)", got[0].ID)
	}
}

func TestPRForTask(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	// t1 has a PR-bearing run; t2 has a run but no PR recorded.
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t2", ProjectID: "p1", Title: "y", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, &Run{ID: "r2", TaskID: "t2", ProjectID: "p1", Pipeline: "build", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunPR(ctx, "r1", "https://github.com/o/r/pull/7", 7); err != nil {
		t.Fatal(err)
	}

	url, num, err := s.PRForTask(ctx, "t1")
	if err != nil {
		t.Fatalf("PRForTask t1: %v", err)
	}
	if url != "https://github.com/o/r/pull/7" || num != 7 {
		t.Errorf("PRForTask t1 = (%q,%d), want (https://github.com/o/r/pull/7,7)", url, num)
	}

	// Task with a run but no PR recorded → ("",0,nil).
	url, num, err = s.PRForTask(ctx, "t2")
	if err != nil {
		t.Fatalf("PRForTask t2: %v", err)
	}
	if url != "" || num != 0 {
		t.Errorf("PRForTask t2 = (%q,%d), want (\"\",0)", url, num)
	}

	// Unknown task → ("",0,nil).
	url, num, err = s.PRForTask(ctx, "ghost")
	if err != nil {
		t.Fatalf("PRForTask ghost: %v", err)
	}
	if url != "" || num != 0 {
		t.Errorf("PRForTask ghost = (%q,%d), want (\"\",0)", url, num)
	}
}
