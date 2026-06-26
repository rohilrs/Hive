package pipeline

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rohilrs/Hive/internal/store"
)

// commitWorktreeChanges stages and commits all changes in the build run's
// worktree on its branch. The build worker is sandboxed away from `git commit`
// (the claudecode adapter denies it), so Hive owns committing the agent's final
// output here — otherwise finish-branch's `git push -u origin HEAD` has nothing
// new to push and opens an empty PR. No-op when the tree is clean (the agent
// produced nothing, or committed it already). .gitignore is respected, so
// node_modules / build artifacts are not staged. Best-effort: the caller logs
// the error and proceeds; the build itself already succeeded.
func commitWorktreeChanges(ctx context.Context, run *Run) error {
	if run == nil || run.WorktreePath == "" {
		return nil
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = run.WorktreePath
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %v (%s)", err, status)
	}
	if status == "" {
		return nil // clean tree — nothing to commit
	}
	if out, err := git("add", "-A"); err != nil {
		return fmt.Errorf("git add -A: %v (%s)", err, out)
	}
	// `add -A` over an all-ignored change set can leave nothing staged (e.g. only
	// gitignored paths changed); committing then would fail "nothing to commit".
	if staged, _ := git("diff", "--cached", "--name-only"); staged == "" {
		return nil
	}
	if out, err := git("commit", "-m", buildCommitMessage(run.Task)); err != nil {
		return fmt.Errorf("git commit: %v (%s)", err, out)
	}
	return nil
}

// buildCommitMessage derives the commit subject from the task. Uses the repo's
// configured git identity as author (Hive does not override it).
func buildCommitMessage(t *store.Task) string {
	if t != nil {
		if title := strings.TrimSpace(t.Title); title != "" {
			return title
		}
	}
	return "Hive build output"
}
