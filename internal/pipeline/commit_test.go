package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/store"
)

// commitTestRepo builds a throwaway repo with one commit on main.
func commitTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestCommitWorktreeChanges_CommitsAgentWork(t *testing.T) {
	dir := commitTestRepo(t)
	// Simulate the build agent's output: a brand-new file + a modification, left
	// uncommitted (the agent is sandboxed from `git commit`).
	if err := os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "r1", WorktreePath: dir, Task: &store.Task{Title: "Add feature X"}}
	if err := commitWorktreeChanges(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if subj := gitOut(t, dir, "log", "-1", "--pretty=%s"); subj != "Add feature X" {
		t.Errorf("commit subject = %q, want \"Add feature X\"", subj)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("tree not clean after commit: %q", st)
	}
	// The new file is actually in the commit.
	if files := gitOut(t, dir, "show", "--name-only", "--pretty=format:"); !strings.Contains(files, "feature.go") {
		t.Errorf("feature.go not in commit; files=%q", files)
	}
}

func TestCommitWorktreeChanges_NoopOnCleanTree(t *testing.T) {
	dir := commitTestRepo(t)
	before := gitOut(t, dir, "rev-parse", "HEAD")
	run := &Run{ID: "r1", WorktreePath: dir, Task: &store.Task{Title: "x"}}
	if err := commitWorktreeChanges(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if after := gitOut(t, dir, "rev-parse", "HEAD"); after != before {
		t.Errorf("HEAD moved on a clean tree (should be a no-op): %s -> %s", before, after)
	}
}

func TestCommitWorktreeChanges_EmptyWorktreePathNoop(t *testing.T) {
	if err := commitWorktreeChanges(context.Background(), &Run{ID: "r1"}); err != nil {
		t.Errorf("empty worktree path should be a no-op, got %v", err)
	}
}

func TestBuildCommitMessageFallback(t *testing.T) {
	if got := buildCommitMessage(nil); got != "Hive build output" {
		t.Errorf("nil task = %q, want fallback", got)
	}
	if got := buildCommitMessage(&store.Task{Title: "  "}); got != "Hive build output" {
		t.Errorf("blank title = %q, want fallback", got)
	}
	if got := buildCommitMessage(&store.Task{Title: "Real title"}); got != "Real title" {
		t.Errorf("got %q, want \"Real title\"", got)
	}
}
