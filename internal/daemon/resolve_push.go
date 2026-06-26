package daemon

import (
	"fmt"
	"os/exec"
	"strings"
)

// resolvePushBranch commits the in-progress (or already-staged) merge in the
// worktree and pushes the branch to origin. Never force-pushes. If there is
// nothing to commit (already committed), it still pushes the branch.
func resolvePushBranch(worktree, branch string) error {
	// Check if a merge is in progress using git itself so this works in both
	// regular clones (.git is a directory) AND linked worktrees (.git is a
	// file pointing at the common .git dir). os.Stat(".git/MERGE_HEAD") always
	// fails in a linked worktree; "git rev-parse --verify --quiet MERGE_HEAD"
	// exits 0 iff MERGE_HEAD exists, regardless of worktree type.
	_, revParseErr := runGit(worktree, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	mergeInProgress := revParseErr == nil

	var commitArgs []string
	if mergeInProgress {
		// Real merge in progress — use --no-edit to keep the generated merge message.
		commitArgs = []string{"commit", "--no-edit"}
	} else {
		// Staged resolution without a MERGE_HEAD (e.g. already committed or staged
		// by the resolver). Commit with a standard message.
		commitArgs = []string{"commit", "-m", "resolve conflicts"}
	}

	out, commitErr := runGit(worktree, commitArgs...)
	if commitErr != nil {
		// "nothing to commit" is fine (e.g. clean fast-forward); other errors fail.
		lo := strings.ToLower(out)
		if !strings.Contains(lo, "nothing to commit") && !strings.Contains(lo, "nothing added to commit") {
			return fmt.Errorf("commit merge: %v: %s", commitErr, out)
		}
		// genuinely nothing to commit — fall through to push
	}

	if out, err := runGit(worktree, "push", "origin", branch); err != nil {
		return fmt.Errorf("push %s: %v: %s", branch, err, out)
	}
	return nil
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
