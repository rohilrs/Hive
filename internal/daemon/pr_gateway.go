package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// prGateway abstracts the `gh` PR operations the merge-poller needs, so tests
// can stub GitHub. Real impl shells out to `gh`; a full PR URL infers the repo,
// so no repo cwd is required.
type prGateway interface {
	// State reports whether the PR merged and, if so, the base branch it
	// merged into (so the poller can confirm it landed on the target branch).
	State(ctx context.Context, prURL string) (merged bool, baseRef string, err error)
	// Mergeable reports GitHub's asynchronously-computed mergeability:
	// "MERGEABLE" | "CONFLICTING" | "UNKNOWN" (still recomputing). After a push
	// GitHub returns "UNKNOWN" until its background job settles, so a merge
	// attempted immediately races a stale verdict — callers should poll this
	// until it leaves "UNKNOWN" before merging.
	Mergeable(ctx context.Context, prURL string) (string, error)
	// Merge merges the PR with the given method ("squash"|"merge"|"rebase").
	Merge(ctx context.Context, prURL, method string) error
	// OpenPR creates a PR from head into base and returns the new PR URL.
	OpenPR(ctx context.Context, repoPath, head, base, title, body string, draft bool) (prURL string, err error)
}

type ghPRGateway struct{}

type ghPRView struct {
	State    string `json:"state"` // OPEN | MERGED | CLOSED
	MergedAt string `json:"mergedAt"`
	BaseRef  string `json:"baseRefName"`
}

func (ghPRGateway) State(ctx context.Context, prURL string) (bool, string, error) {
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", prURL,
		"--json", "state,mergedAt,baseRefName").Output()
	if err != nil {
		return false, "", fmt.Errorf("gh pr view %s: %w", prURL, ghErr(err))
	}
	var v ghPRView
	if err := json.Unmarshal(out, &v); err != nil {
		return false, "", fmt.Errorf("parse gh pr view: %w", err)
	}
	merged := v.State == "MERGED" || strings.TrimSpace(v.MergedAt) != ""
	return merged, v.BaseRef, nil
}

func (ghPRGateway) Mergeable(ctx context.Context, prURL string) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", prURL,
		"--json", "mergeable", "-q", ".mergeable").Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view %s mergeable: %w", prURL, ghErr(err))
	}
	return strings.TrimSpace(string(out)), nil
}

func (ghPRGateway) Merge(ctx context.Context, prURL, method string) error {
	flag := "--squash"
	switch method {
	case "merge":
		flag = "--merge"
	case "rebase":
		flag = "--rebase"
	}
	// NOTE: deliberately NOT using `--delete-branch`. gh deletes the LOCAL branch
	// too, which fails (exit 1) while the run's worktree still has that branch
	// checked out — `cannot delete branch 'X' used by worktree at ...`. gh then
	// aborts the whole step, deleting NEITHER local nor remote and reporting the
	// merge as failed even though the PR merged — so the task spuriously parks
	// needs_attention and both branches leak. Branch cleanup (worktree + local +
	// remote) is owned by the daemon at the confirmed-merge point instead
	// (cleanupMergedBranch in merge_detect.go).
	out, err := exec.CommandContext(ctx, "gh", "pr", "merge", prURL, flag).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr merge %s: %w: %s", prURL, ghErr(err), strings.TrimSpace(string(out)))
	}
	return nil
}

func (ghPRGateway) OpenPR(ctx context.Context, repoPath, head, base, title, body string, draft bool) (string, error) {
	args := []string{"pr", "create", "--head", head, "--base", base, "--title", title, "--body", body}
	if draft {
		args = append(args, "--draft")
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr create (%s -> %s): %w", head, base, ghErr(err))
	}
	// gh prints the PR URL as the last non-empty line.
	url := strings.TrimSpace(string(out))
	if i := strings.LastIndex(url, "\n"); i >= 0 {
		url = strings.TrimSpace(url[i+1:])
	}
	return url, nil
}

// ghErr surfaces gh stderr (exec.ExitError swallows it otherwise).
func ghErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
