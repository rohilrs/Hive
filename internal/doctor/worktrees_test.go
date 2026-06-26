package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohilrs/Hive/internal/store"
)

func TestWorktreesNoDirsIsOK(t *testing.T) {
	hiveDir := t.TempDir()
	// No worktrees dir at all — should be ok with empty-set findings.
	checks := runWorktreesChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "worktrees.orphans")
	if c.Status != StatusOK {
		t.Errorf("no worktrees: status=%s, want ok", c.Status)
	}
}

func TestWorktreesOrphanDetected(t *testing.T) {
	hiveDir := t.TempDir()
	wtRoot := filepath.Join(hiveDir, "worktrees")
	if err := os.MkdirAll(filepath.Join(wtRoot, "run-orphan-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create a DB but no matching run.
	s, err := store.Open(context.Background(), filepath.Join(hiveDir, "db.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	s.Close()
	checks := runWorktreesChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "worktrees.orphans")
	if c.Status != StatusWarn {
		t.Errorf("1 orphan: status=%s, want warn; msg=%q", c.Status, c.Message)
	}
}

// TestWorktreesMissingForRunningDetected exercises W2's positive case
// (regression for the original bug: ListRecentRuns excludes
// status='running', so W2 used to be dead code). We insert a running
// run with a worktree path that does NOT exist on disk and expect W2
// to fire.
func TestWorktreesMissingForRunningDetected(t *testing.T) {
	hiveDir := t.TempDir()
	// Create empty worktrees/ so the readdir succeeds and we proceed
	// to the W2 check (without a worktrees/ dir at all the whole
	// function short-circuits to OK).
	if err := os.MkdirAll(filepath.Join(hiveDir, "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(hiveDir, "db.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// FK constraints: must insert project + task before run.
	if err := s.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := s.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	// Insert a run with status='running' but no matching worktree dir.
	if err := s.InsertRun(ctx, &store.Run{
		ID: "run-runner-1", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "running",
	}); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	s.Close()

	checks := runWorktreesChecks(ctx, hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "worktrees.missing_for_running")
	if c.Status != StatusError {
		t.Errorf("running run with no worktree: status=%s, want error; msg=%q", c.Status, c.Message)
	}
}

// TestWorktreesStaleScratchDetected exercises W3's positive case for
// needs_attention (the status the original whitelist missed). Create a
// scratch dir <hiveDir>/run-na-1/ for a run in status='needs_attention'
// and expect W3 to flag it as stale.
func TestWorktreesStaleScratchDetected(t *testing.T) {
	hiveDir := t.TempDir()
	// Empty worktrees/ so we get past the early-return.
	if err := os.MkdirAll(filepath.Join(hiveDir, "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	// Per-run scratch dir at <hiveDir>/run-na-1/ for the
	// needs_attention run we'll insert.
	if err := os.MkdirAll(filepath.Join(hiveDir, "run-na-1"), 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(hiveDir, "db.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := s.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := s.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if err := s.InsertRun(ctx, &store.Run{
		ID: "run-na-1", TaskID: "t1", ProjectID: "p1",
		Pipeline: "build", Status: "needs_attention",
	}); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	s.Close()

	checks := runWorktreesChecks(ctx, hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "worktrees.stale_scratch")
	if c.Status != StatusWarn {
		t.Errorf("needs_attention scratch dir: status=%s, want warn; msg=%q", c.Status, c.Message)
	}
}
