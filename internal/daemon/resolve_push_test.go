package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeF(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePushCommitsAndPushes(t *testing.T) {
	origin := t.TempDir()
	mustGit(t, "", "init", "-q", "--bare", "-b", "main", origin)
	work := t.TempDir()
	mustGit(t, "", "clone", "-q", origin, work)
	writeF(t, work, "a.txt", "x\n")
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-qm", "init")
	mustGit(t, work, "push", "-q", "origin", "main")
	mustGit(t, work, "checkout", "-q", "-b", "feature")
	writeF(t, work, "a.txt", "resolved\n")
	mustGit(t, work, "add", "-A") // staged resolution, merge-in-progress simulated

	if err := resolvePushBranch(work, "feature"); err != nil {
		t.Fatalf("resolvePushBranch: %v", err)
	}
	out := mustGit(t, work, "ls-remote", origin, "feature")
	if !strings.Contains(out, "feature") {
		t.Errorf("feature branch should be on origin; got %q", out)
	}
}

// TestResolvePushMergeHeadPath creates a real merge conflict in a LINKED
// worktree (where .git is a file, not a directory), resolves the file,
// git-adds it (leaving MERGE_HEAD intact), calls resolvePushBranch, and
// asserts that (a) the branch is on origin and (b) MERGE_HEAD is gone.
//
// The linked-worktree setup is deliberate: os.Stat(".git/MERGE_HEAD") would
// always fail there, which is exactly the bug the --verify approach fixes.
func TestResolvePushMergeHeadPath(t *testing.T) {
	origin := t.TempDir()
	mustGit(t, "", "init", "-q", "--bare", "-b", "main", origin)

	// Clone — this is the "main" checkout; we will use it only to set up
	// branches and create the linked worktree.
	work := t.TempDir()
	mustGit(t, "", "clone", "-q", origin, work)

	// Base commit shared by both branches.
	writeF(t, work, "f.txt", "base\n")
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-qm", "base")
	mustGit(t, work, "push", "-q", "origin", "main")

	// Create branch "feat" at the base commit.
	mustGit(t, work, "checkout", "-q", "-b", "feat")
	writeF(t, work, "f.txt", "from-feat\n")
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-qm", "feat-change")

	// Create a divergent branch "target" that conflicts with feat on f.txt.
	mustGit(t, work, "checkout", "-q", "main")
	mustGit(t, work, "checkout", "-q", "-b", "target")
	writeF(t, work, "f.txt", "from-target\n")
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-qm", "target-change")

	// Add a LINKED worktree for "feat" — this is the key: inside wtDir,
	// .git is a *file* (not a directory), so os.Stat(".git/MERGE_HEAD")
	// would silently fail regardless of merge state.
	wtDir := t.TempDir()
	mustGit(t, work, "worktree", "add", "--force", wtDir, "feat")

	// Verify that the linked worktree has .git as a file (not a dir).
	fi, err := os.Stat(filepath.Join(wtDir, ".git"))
	if err != nil {
		t.Fatalf("linked worktree .git missing: %v", err)
	}
	if fi.IsDir() {
		t.Fatal("expected .git to be a FILE in a linked worktree, got a directory")
	}

	// Attempt merge of "target" into the linked worktree — must produce a conflict.
	cmd := exec.Command("git", "merge", "target")
	cmd.Dir = wtDir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	_ = cmd.Run() // expected to fail due to conflict

	// Confirm MERGE_HEAD is detectable via git (not via os.Stat) — proves the
	// linked-worktree path works as expected.
	if _, merr := runGit(wtDir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); merr != nil {
		t.Fatalf("expected MERGE_HEAD to exist in linked worktree after conflict; got error: %v", merr)
	}

	// Resolve the conflict: write clean content and stage it.
	writeF(t, wtDir, "f.txt", "resolved\n")
	mustGit(t, wtDir, "add", "f.txt")

	// resolvePushBranch must commit using --no-edit (MERGE_HEAD path) and push.
	if err := resolvePushBranch(wtDir, "feat"); err != nil {
		t.Fatalf("resolvePushBranch: %v", err)
	}

	// Branch must be on origin.
	lsOut := mustGit(t, wtDir, "ls-remote", origin, "feat")
	if !strings.Contains(lsOut, "feat") {
		t.Errorf("feat branch should be on origin after push; got %q", lsOut)
	}

	// MERGE_HEAD must be gone — merge was committed.
	if _, merr := runGit(wtDir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); merr == nil {
		t.Errorf("MERGE_HEAD should be gone after successful merge commit, but rev-parse succeeded")
	}
}
