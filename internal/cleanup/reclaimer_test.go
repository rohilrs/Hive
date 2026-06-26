package cleanup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/worktree"
)

func TestForceRemoveAllReadOnly(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "run-x")
	mod := filepath.Join(scratch, "home", "go", "pkg", "mod", "x@v1")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(mod, "readonly.go")
	if err := os.WriteFile(f, []byte("package x"), 0o444); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(mod, 0o555)
	if err := forceRemoveAll(scratch); err != nil {
		t.Fatalf("forceRemoveAll on read-only tree: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch not removed: %v", err)
	}
}

func TestReclaimDryRunRemovesNothing(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "run-1")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(scratch, "f"), make([]byte, 100), 0o644)
	r := &Reclaimer{}
	plan := Plan{Reclaim: []ReclaimItem{{RunID: "run-1", Scratch: scratch}}}
	res := r.Reclaim(context.Background(), plan, true, false)
	if res.Runs != 1 || res.Bytes < 100 {
		t.Errorf("dry-run result = %+v, want 1 run / >=100 bytes", res)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("dry-run must NOT remove the dir: %v", err)
	}
}

func TestReclaimRealWorktreeAndBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "init")

	wtRoot := t.TempDir()
	wtPath := filepath.Join(wtRoot, "run-1")
	run("worktree", "add", "-q", "-b", "hive/run-1", wtPath)

	wt := worktree.NewManager(worktree.Config{WorktreeRoot: wtRoot})
	r := &Reclaimer{WT: wt}
	plan := Plan{Reclaim: []ReclaimItem{{RunID: "run-1", Worktree: wtPath, RepoPath: repo, BranchName: "hive/run-1"}}}
	res := r.Reclaim(context.Background(), plan, false, true)
	if len(res.Errors) != 0 {
		t.Errorf("unexpected errors: %v", res.Errors)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree not removed: %v", err)
	}
	out, _ := exec.Command("git", "-C", repo, "branch", "--list", "hive/run-1").CombinedOutput()
	if len(out) != 0 {
		t.Errorf("branch hive/run-1 should be deleted; git branch --list -> %q", out)
	}
	_ = time.Now
}
