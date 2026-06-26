package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureSetupScript = "../../testdata/setup.sh"

func setupFixture(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("bash", fixtureSetupScript).CombinedOutput()
	if err != nil {
		t.Fatalf("setup fixture: %v\n%s", err, out)
	}
	abs, err := filepath.Abs("../../testdata/fixtures/repos/simple-go")
	if err != nil {
		t.Fatal(err)
	}
	// After each test, prune stale worktree registrations and delete any
	// hive/* branches left behind so the shared fixture repo stays clean
	// across re-runs. Manager.Remove intentionally leaves branches in place
	// (spec: "branch is left behind for inspection") so this cleanup lives
	// in the test layer.
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", abs, "worktree", "prune").Run()
		listOut, err := exec.Command("git", "-C", abs, "for-each-ref",
			"--format=%(refname:short)", "refs/heads/hive/").Output()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(listOut), "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			_ = exec.Command("git", "-C", abs, "branch", "-D", line).Run()
		}
	})
	return abs
}

func TestManagerCreateAndRemove(t *testing.T) {
	repo := setupFixture(t)
	wtRoot := t.TempDir()
	mgr := NewManager(Config{WorktreeRoot: wtRoot})

	ctx := context.Background()
	info, err := mgr.Create(ctx, CreateRequest{
		RunID:      "run-test1",
		RepoPath:   repo,
		BaseBranch: "main",
		TaskTitle:  "Fix login bug",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.BranchName != "hive/run-test1/fix-login-bug" {
		t.Errorf("branch=%s", info.BranchName)
	}
	if _, err := exec.Command("test", "-f", filepath.Join(info.Path, "go.mod")).CombinedOutput(); err != nil {
		t.Errorf("worktree missing go.mod at %s: %v", info.Path, err)
	}

	if err := mgr.Remove(ctx, repo, "run-test1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestManagerCreateNoTaskSlug(t *testing.T) {
	repo := setupFixture(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})

	info, err := mgr.Create(context.Background(), CreateRequest{
		RunID: "run-2", RepoPath: repo, BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.BranchName != "hive/run-2" {
		t.Errorf("branch=%s", info.BranchName)
	}
}

func TestListReturnsCreatedWorktrees(t *testing.T) {
	repo := setupFixture(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})
	ctx := context.Background()

	_, _ = mgr.Create(ctx, CreateRequest{RunID: "a", RepoPath: repo, BaseBranch: "main", TaskTitle: "task a"})
	_, _ = mgr.Create(ctx, CreateRequest{RunID: "b", RepoPath: repo, BaseBranch: "main", TaskTitle: "task b"})

	list, err := mgr.List(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var seenA, seenB bool
	for _, info := range list {
		if info.RunID == "a" {
			seenA = true
		}
		if info.RunID == "b" {
			seenB = true
		}
	}
	if !seenA || !seenB {
		t.Errorf("missing worktrees: a=%v b=%v", seenA, seenB)
	}
}

func TestCleanStaleRemovesUnknownRuns(t *testing.T) {
	repo := setupFixture(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})
	ctx := context.Background()

	_, _ = mgr.Create(ctx, CreateRequest{RunID: "active", RepoPath: repo, BaseBranch: "main"})
	_, _ = mgr.Create(ctx, CreateRequest{RunID: "orphan", RepoPath: repo, BaseBranch: "main"})

	removed, err := mgr.CleanStale(ctx, repo, []string{"active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "orphan" {
		t.Errorf("removed=%v want [orphan]", removed)
	}
}

func TestManagerCreateRespectsBranchName(t *testing.T) {
	repo := setupFixture(t)
	wtRoot := t.TempDir()
	mgr := NewManager(Config{WorktreeRoot: wtRoot})

	const wantBranch = "rohil/HBA-42-add-login"
	// setupFixture's cleanup only deletes hive/* branches; this test
	// creates a non-hive-prefixed branch so we have to clean it up
	// ourselves. Force-remove the worktree first so git releases the
	// branch ref, then delete the branch. Without the worktree-remove,
	// `git branch -D` refuses because the branch is still checked out.
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force",
			filepath.Join(wtRoot, "run-linear-1")).Run()
		_ = exec.Command("git", "-C", repo, "worktree", "prune").Run()
		_ = exec.Command("git", "-C", repo, "branch", "-D", wantBranch).Run()
	})

	info, err := mgr.Create(context.Background(), CreateRequest{
		RunID:      "run-linear-1",
		RepoPath:   repo,
		BaseBranch: "main",
		TaskTitle:  "Add login (ignored when BranchName set)",
		BranchName: wantBranch,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.BranchName != wantBranch {
		t.Errorf("Info.BranchName=%q want %q", info.BranchName, wantBranch)
	}

	// Verify git actually checked out the requested branch in the worktree.
	out, err := exec.Command("git", "-C", info.Path, "branch", "--show-current").Output()
	if err != nil {
		t.Fatalf("git branch --show-current: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != wantBranch {
		t.Errorf("git branch --show-current = %q, want %q", got, wantBranch)
	}
}

func TestManagerCreateGeneratesWhenBranchNameEmpty(t *testing.T) {
	repo := setupFixture(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})

	info, err := mgr.Create(context.Background(), CreateRequest{
		RunID:      "run-gen-1",
		RepoPath:   repo,
		BaseBranch: "main",
		TaskTitle:  "Some task",
		// BranchName intentionally empty
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(info.BranchName, "hive/") {
		t.Errorf("Info.BranchName=%q, want hive/ prefix (auto-generated)", info.BranchName)
	}
	if info.BranchName != "hive/run-gen-1/some-task" {
		t.Errorf("Info.BranchName=%q want hive/run-gen-1/some-task", info.BranchName)
	}
}

func TestManagerCreateRejectsInvalidBranchName(t *testing.T) {
	repo := setupFixture(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})

	// `..` is the prototypical bad-branch token — git-check-ref-format
	// forbids it and Manager.Create must reject BEFORE git is invoked.
	_, err := mgr.Create(context.Background(), CreateRequest{
		RunID:      "run-bad-1",
		RepoPath:   repo,
		BaseBranch: "main",
		BranchName: "rohil/HBA..42",
	})
	if err == nil {
		t.Fatal("expected error for branch name containing '..', got nil")
	}
	if !strings.Contains(err.Error(), "invalid branch name") {
		t.Errorf("error=%v, want 'invalid branch name' substring", err)
	}
}

func TestValidateBranchName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"linear-style", "rohil/HBA-42-add-login", false},
		{"hive-auto-gen", "hive/run-1/something", false},
		{"empty rejected", "", true},
		{"dotdot rejected", "rohil/HBA..42", true},
		{"space rejected", "rohil/HBA 42", true},
		{"tab rejected", "rohil/HBA\t42", true},
		{"newline rejected", "rohil/HBA\n42", true},
		{"leading dash rejected", "-feature", true},
		{"trailing dot rejected", "feature.", true},
		{"colon rejected", "rohil/HBA:42", true},
		{"question-mark rejected", "rohil/HBA?", true},
		{"glob-star rejected", "rohil/HBA*", true},
		{"backslash rejected", "rohil\\HBA-42", true},
		{"caret rejected", "rohil/HBA^42", true},
		{"tilde rejected", "rohil/HBA~42", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBranchName(tc.input)
			if tc.wantErr && err == nil {
				t.Errorf("validateBranchName(%q) = nil, want error", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateBranchName(%q) = %v, want nil", tc.input, err)
			}
		})
	}
}

func TestManagerRejectsInvalidRunID(t *testing.T) {
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})
	badIDs := []string{"", "../../etc", "foo/bar", "foo:bar", "with spaces", ".hidden"}
	for _, bad := range badIDs {
		_, err := mgr.Create(context.Background(), CreateRequest{
			RunID: bad, RepoPath: "/tmp", BaseBranch: "main",
		})
		if err == nil {
			t.Errorf("Create(%q) should have rejected the run_id", bad)
		}
	}
}

// localRepo inits a throwaway repo (no origin) with one commit on main.
func localRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

// TestManagerCreateReclaimsStaleBranchOnRerun covers the re-run cleanup gap: a
// Linear task reuses a FIXED branchName, so a re-run collides with the prior
// run's branch + worktree. Create must reclaim them and recreate.
func TestManagerCreateReclaimsStaleBranchOnRerun(t *testing.T) {
	ctx := context.Background()
	repo := localRepo(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})

	i1, err := mgr.Create(ctx, CreateRequest{RunID: "run-1", RepoPath: repo, BaseBranch: "main", BranchName: "rohil/task-x"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Re-run the same task (same fixed branchName, new run id): branch + worktree
	// from run-1 still exist. Must reclaim + recreate rather than fail.
	i2, err := mgr.Create(ctx, CreateRequest{RunID: "run-2", RepoPath: repo, BaseBranch: "main", BranchName: "rohil/task-x"})
	if err != nil {
		t.Fatalf("re-run should reclaim the stale branch and succeed, got: %v", err)
	}
	if i2.BranchName != "rohil/task-x" {
		t.Errorf("branch=%s, want rohil/task-x", i2.BranchName)
	}
	if _, err := os.Stat(i1.Path); !os.IsNotExist(err) {
		t.Errorf("stale run-1 worktree should be removed; stat err=%v", err)
	}
	if _, err := os.Stat(i2.Path); err != nil {
		t.Errorf("run-2 worktree missing: %v", err)
	}
}

// TestManagerCreateReclaimsLeftoverBranch covers Manager.Remove's documented
// behavior of leaving the branch behind: a re-run must delete that branchless
// leftover and recreate.
func TestManagerCreateReclaimsLeftoverBranch(t *testing.T) {
	ctx := context.Background()
	repo := localRepo(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})

	if _, err := mgr.Create(ctx, CreateRequest{RunID: "run-1", RepoPath: repo, BaseBranch: "main", BranchName: "rohil/task-y"}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Remove(ctx, repo, "run-1"); err != nil { // leaves the branch
		t.Fatal(err)
	}
	if _, err := mgr.Create(ctx, CreateRequest{RunID: "run-2", RepoPath: repo, BaseBranch: "main", BranchName: "rohil/task-y"}); err != nil {
		t.Fatalf("re-run with a leftover (worktree-less) branch should succeed, got: %v", err)
	}
}

// TestManagerCreateRefusesNonHiveWorktreeBranch guards operator state: if the
// colliding branch is checked out by a worktree OUTSIDE the Hive root, Create
// must refuse rather than clobber it.
func TestManagerCreateRefusesNonHiveWorktreeBranch(t *testing.T) {
	ctx := context.Background()
	repo := localRepo(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})

	// An operator's own worktree (outside the Hive root) holds the branch.
	extWt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "rohil/task-z", extWt, "main").CombinedOutput(); err != nil {
		t.Fatalf("setup external worktree: %v\n%s", err, out)
	}
	_, err := mgr.Create(ctx, CreateRequest{RunID: "run-1", RepoPath: repo, BaseBranch: "main", BranchName: "rohil/task-z"})
	if err == nil {
		t.Fatal("expected Create to refuse clobbering a non-Hive worktree's branch")
	}
	if !strings.Contains(err.Error(), "non-Hive worktree") {
		t.Errorf("err=%v, want a non-Hive-worktree refusal", err)
	}
}

// TestManagerCreateForksFromOriginTip is the regression for the squash-merge
// divergence fix: when the base branch is tracked on origin and origin has
// advanced past the local ref (a prior task's merge), Create must fetch and
// fork the new worktree from the ORIGIN tip, not the stale local branch.
func TestManagerCreateForksFromOriginTip(t *testing.T) {
	ctx := context.Background()
	run := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
		}
	}
	gitOut := func(dir string, args ...string) string {
		t.Helper()
		o, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			t.Fatalf("git -C %s %v: %v", dir, args, err)
		}
		return strings.TrimSpace(string(o))
	}
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Bare origin (default branch = feat so clones check it out).
	remote := t.TempDir()
	run(remote, "init", "-q", "--bare", "-b", "feat")

	// The "Hive repo" local clone: feat at commit X, pushed to origin.
	local := t.TempDir()
	run(local, "init", "-q", "-b", "feat")
	run(local, "config", "user.email", "t@t")
	run(local, "config", "user.name", "T")
	write(local, "base.txt", "base\n")
	run(local, "add", "-A")
	run(local, "commit", "-q", "-m", "X")
	run(local, "remote", "add", "origin", remote)
	run(local, "push", "-q", "-u", "origin", "feat")

	// A second clone advances feat (simulating a prior task's merge into origin).
	other := t.TempDir()
	run(other, "clone", "-q", remote, ".")
	run(other, "config", "user.email", "t2@t")
	run(other, "config", "user.name", "T2")
	write(other, "advance.txt", "more\n")
	run(other, "add", "-A")
	run(other, "commit", "-q", "-m", "Y-advance")
	run(other, "push", "-q", "origin", "feat")
	wantTip := gitOut(other, "rev-parse", "HEAD")

	// Precondition: local feat is stale (still at X).
	if gitOut(local, "rev-parse", "feat") == wantTip {
		t.Fatal("precondition failed: local feat should be stale relative to origin")
	}

	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})
	info, err := mgr.Create(ctx, CreateRequest{RunID: "run-fork", RepoPath: local, BaseBranch: "feat", TaskTitle: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := gitOut(info.Path, "rev-parse", "HEAD"); got != wantTip {
		t.Errorf("worktree forked from %s, want origin tip %s (stale-base divergence not fixed)", got, wantTip)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "advance.txt")); err != nil {
		t.Errorf("advance.txt from origin tip missing in worktree (forked from stale local): %v", err)
	}
}

// TestManagerCreateFallsBackToTargetWhenBaseMissing covers the configured-but-
// nonexistent feature branch case: BaseBranch resolves nowhere, so Create forks
// from FallbackBase (the target) instead of dying with "invalid reference".
func TestManagerCreateFallsBackToTargetWhenBaseMissing(t *testing.T) {
	repo := setupFixture(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})
	info, err := mgr.Create(context.Background(), CreateRequest{
		RunID:        "run-fallback",
		RepoPath:     repo,
		BaseBranch:   "feat/does-not-exist", // never created/pushed
		FallbackBase: "main",                // exists in the fixture
		TaskTitle:    "task",
	})
	if err != nil {
		t.Fatalf("create should fall back to target, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "go.mod")); err != nil {
		t.Errorf("worktree not provisioned from fallback base: %v", err)
	}
}

// TestManagerCreateErrorsWhenBaseAndFallbackMissing: with neither base nor
// fallback resolvable, Create returns an actionable error (not a cryptic git
// "invalid reference") and provisions nothing.
func TestManagerCreateErrorsWhenBaseAndFallbackMissing(t *testing.T) {
	repo := setupFixture(t)
	mgr := NewManager(Config{WorktreeRoot: t.TempDir()})
	_, err := mgr.Create(context.Background(), CreateRequest{
		RunID:        "run-noref",
		RepoPath:     repo,
		BaseBranch:   "feat/nope",
		FallbackBase: "also-nope",
		TaskTitle:    "task",
	})
	if err == nil {
		t.Fatal("expected an error when neither base nor fallback exists")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should name the missing branch, got: %v", err)
	}
}
