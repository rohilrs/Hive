package pipeline

import (
	"context"
	"fmt"
	"os/exec"
)

// captureWorktreeDiff runs `git diff <base>` in worktreePath and
// returns the diff text. Used by Phase 3.3's L3 loop detector to
// compare iter N vs iter N-1 worker output. The base is typically
// "main" (the branch the worktree was created from).
//
// Returns an error when the directory isn't a git work-tree or the
// command fails. Empty diff (no changes) is a normal success case.
//
// The diff is "working-tree vs base ref" because the BuildPipeline
// doesn't commit per iteration — the working tree IS the iteration's
// state.
func captureWorktreeDiff(ctx context.Context, worktreePath, base string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", base)
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git diff %s: %w: %s", base, err, exitErr.Stderr)
		}
		return "", fmt.Errorf("git diff %s: %w", base, err)
	}
	return string(out), nil
}
