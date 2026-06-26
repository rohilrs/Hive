package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// setupDivergedRepo builds a repo where `feature` and `main` have diverged by
// one commit each, touching DIFFERENT files (so a rebase/merge is conflict-free).
// Returns the repo path; branches are "feature" (target "main").
func setupDivergedRepo(t *testing.T) string {
	t.Helper()
	repo := initGitRepo(t) // on main, one "initial" commit (scheduler_test.go helper)

	// feature branches off main's current tip.
	mustRun(t, repo, "git", "branch", "feature")

	// Advance main by one commit on its own file.
	if err := os.WriteFile(filepath.Join(repo, "main.txt"), []byte("main work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "main ahead")

	// Add a feature commit on a different file → diverged, conflict-free.
	mustRun(t, repo, "git", "checkout", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "feature work")

	// Leave the repo checked out on main (the canonical idle state).
	mustRun(t, repo, "git", "checkout", "main")
	return repo
}

func behindCount(t *testing.T, repo string) int {
	t.Helper()
	out, err := gitC(repo, "rev-list", "--count", "feature..main")
	if err != nil {
		t.Fatalf("rev-list: %v (%s)", err, out)
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("parse behind count %q: %v", out, err)
	}
	return n
}

func TestRebaseFeatureBranch_ClearsBehind(t *testing.T) {
	repo := setupDivergedRepo(t)
	if behindCount(t, repo) != 1 {
		t.Fatalf("precondition: feature should be 1 behind main")
	}
	if err := rebaseFeatureBranch(repo, "feature", "main"); err != nil {
		t.Fatalf("rebaseFeatureBranch: %v", err)
	}
	if got := behindCount(t, repo); got != 0 {
		t.Errorf("behind after rebase = %d, want 0", got)
	}
	head, _ := gitC(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if head != "feature" {
		t.Errorf("HEAD after rebase = %q, want feature", head)
	}
}

func TestMergeTargetIntoFeature_ClearsBehind(t *testing.T) {
	repo := setupDivergedRepo(t)
	if err := mergeTargetIntoFeature(repo, "feature", "main"); err != nil {
		t.Fatalf("mergeTargetIntoFeature: %v", err)
	}
	if got := behindCount(t, repo); got != 0 {
		t.Errorf("behind after merge = %d, want 0", got)
	}
	head, _ := gitC(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if head != "feature" {
		t.Errorf("HEAD after merge = %q, want feature", head)
	}
	// A real merge commit must exist (feature had its own commit → no fast-forward).
	out, err := gitC(repo, "rev-list", "--merges", "--count", "main..feature")
	if err != nil {
		t.Fatalf("rev-list merges: %v (%s)", err, out)
	}
	if out != "1" {
		t.Errorf("merge commit count = %q, want 1", out)
	}
}

func TestRebaseFeatureBranch_ConflictAutoAborts(t *testing.T) {
	repo := initGitRepo(t)
	// Both branches modify the SAME file differently → guaranteed conflict.
	mustRun(t, repo, "git", "branch", "feature")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "main shared")
	mustRun(t, repo, "git", "checkout", "feature")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "feature shared")
	mustRun(t, repo, "git", "checkout", "main")

	if err := rebaseFeatureBranch(repo, "feature", "main"); err == nil {
		t.Fatal("expected rebase to fail on conflict")
	}
	// Auto-abort must have left a clean tree with no unmerged entries.
	if u, _ := gitC(repo, "ls-files", "-u"); u != "" {
		t.Errorf("unmerged entries remain after abort: %q", u)
	}
	if st, _ := gitC(repo, "status", "--porcelain"); st != "" {
		t.Errorf("working tree not clean after abort: %q", st)
	}
}

func TestMergeTargetIntoFeature_ConflictAutoAborts(t *testing.T) {
	repo := initGitRepo(t)
	mustRun(t, repo, "git", "branch", "feature")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "main shared")
	mustRun(t, repo, "git", "checkout", "feature")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "feature shared")
	mustRun(t, repo, "git", "checkout", "main")

	if err := mergeTargetIntoFeature(repo, "feature", "main"); err == nil {
		t.Fatal("expected merge to fail on conflict")
	}
	if u, _ := gitC(repo, "ls-files", "-u"); u != "" {
		t.Errorf("unmerged entries remain after abort: %q", u)
	}
	if st, _ := gitC(repo, "status", "--porcelain"); st != "" {
		t.Errorf("working tree not clean after abort: %q", st)
	}
}
