package daemon

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/worktree"
)

// TestCleanupMergedBranchRemovesWorktreeAndBranch verifies that post-merge
// cleanup removes the run's worktree (which holds the branch) and deletes the
// local branch — the step gh --delete-branch couldn't do while the worktree was
// live, which was leaking merged task branches.
func TestCleanupMergedBranchRemovesWorktreeAndBranch(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)
	repo := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "p", Name: "P", Status: "active", RepoPath: &repo,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "t1", ProjectID: "p1", Source: "inbox", Title: "t", Status: "done",
	}); err != nil {
		t.Fatal(err)
	}

	const branch = "rohil/task-1-branch"
	info, err := d.wtMgr.Create(ctx, worktree.CreateRequest{
		RunID: "run-x", RepoPath: repo, BaseBranch: "main", BranchName: branch,
	})
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{
		ID: "run-x", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.SetRunBranch(ctx, "run-x", branch); err != nil {
		t.Fatal(err)
	}

	branchExists := func() bool {
		return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
	}
	if !branchExists() {
		t.Fatal("precondition: branch should exist before cleanup")
	}

	task, _ := d.store.GetTask(ctx, "t1")
	proj, _ := d.store.GetProjectBySlug(ctx, "p")
	d.cleanupMergedBranch(ctx, task, proj) // remote delete fails (no remote) — best-effort

	if branchExists() {
		t.Error("local branch should be deleted after cleanup")
	}
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be removed; stat err=%v", err)
	}
}
