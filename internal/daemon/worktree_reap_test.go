package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rohilrs/Hive/internal/store"
)

func reapTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestWorktreeInUsePath(t *testing.T) {
	in := "preparing worktree\nfatal: 'rohilrshah/hba-87-x' is already used by worktree at '/home/rohil/.hive/worktrees/run-123'\n"
	if got := worktreeInUsePath(in); got != "/home/rohil/.hive/worktrees/run-123" {
		t.Errorf("path = %q, want the worktree path", got)
	}
	if got := worktreeInUsePath("fatal: some unrelated error"); got != "" {
		t.Errorf("non-matching error must yield empty, got %q", got)
	}
}

// TestReapStaleWorktree covers the resolve provision self-heal: a TERMINAL-run
// worktree under Hive's root is reaped, an ACTIVE-run worktree is refused, and a
// path outside the worktrees root is refused.
func TestReapStaleWorktree(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	repo := initGitRepo(t)
	wtRoot := filepath.Join(d.HiveDir(), "worktrees")
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// Satisfy the runs FK (project + task) before inserting run rows.
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active", RepoPath: &repo}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t", ProjectID: "p", Source: "inbox", Title: "x", Status: "needs_attention", Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}

	// Terminal-run worktree → reaped.
	termWT := filepath.Join(wtRoot, "run-term")
	reapTestGit(t, repo, "worktree", "add", "-b", "feat-term", termWT, "HEAD")
	if err := d.store.InsertRun(ctx, &store.Run{ID: "run-term", TaskID: "t", ProjectID: "p", Pipeline: "build", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if !d.scheduler.reapStaleWorktree(ctx, repo, termWT) {
		t.Fatal("expected reap of a terminal-run worktree")
	}
	if _, err := os.Stat(termWT); !os.IsNotExist(err) {
		t.Error("terminal worktree dir should be removed")
	}

	// Active-run worktree → refused.
	activeWT := filepath.Join(wtRoot, "run-active")
	reapTestGit(t, repo, "worktree", "add", "-b", "feat-active", activeWT, "HEAD")
	if err := d.store.InsertRun(ctx, &store.Run{ID: "run-active", TaskID: "t", ProjectID: "p", Pipeline: "build", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if d.scheduler.reapStaleWorktree(ctx, repo, activeWT) {
		t.Error("must refuse to reap an active run's worktree")
	}
	if _, err := os.Stat(activeWT); err != nil {
		t.Error("active-run worktree must survive")
	}

	// PENDING-run worktree → refused. "pending" is the InsertRun default, held
	// while the worktree already exists but before MarkRunStarted flips it to
	// "running" — reaping it would destroy a live, mid-dispatch run's checkout.
	pendingWT := filepath.Join(wtRoot, "run-pending")
	reapTestGit(t, repo, "worktree", "add", "-b", "feat-pending", pendingWT, "HEAD")
	if err := d.store.InsertRun(ctx, &store.Run{ID: "run-pending", TaskID: "t", ProjectID: "p", Pipeline: "build", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if d.scheduler.reapStaleWorktree(ctx, repo, pendingWT) {
		t.Error("must refuse to reap a PENDING run's worktree (live, mid-dispatch)")
	}
	if _, err := os.Stat(pendingWT); err != nil {
		t.Error("pending-run worktree must survive")
	}

	// The worktrees root itself → refused (not strictly under the root).
	if d.scheduler.reapStaleWorktree(ctx, repo, wtRoot) {
		t.Error("must refuse to reap the worktrees root itself")
	}

	// Path outside the worktrees root → refused (defense against a bad parse).
	outside := filepath.Join(t.TempDir(), "not-hive")
	reapTestGit(t, repo, "worktree", "add", "-b", "feat-out", outside, "HEAD")
	if d.scheduler.reapStaleWorktree(ctx, repo, outside) {
		t.Error("must refuse a path outside the worktrees root")
	}
}
