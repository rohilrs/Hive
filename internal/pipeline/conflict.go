package pipeline

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ConflictFile is one path git could not auto-merge.
type ConflictFile struct {
	Path       string // repo-relative
	Merged     string // working-tree content with <<<<<<< / ======= / >>>>>>> markers
	OursDiff   string // diff(merge-base, ours) — what this branch changed
	TheirsDiff string // diff(merge-base, theirs) — what the target changed
}

// ConflictContext is everything the resolve agent needs. Clean=true means the
// merge succeeded with no conflict (the conflict cleared upstream).
type ConflictContext struct {
	TargetBranch string
	Clean        bool
	Files        []ConflictFile
}

func hasConflictMarkers(s string) bool {
	// A real conflict needs an opening marker; a bare ======= alone is not one.
	return strings.Contains(s, "\n<<<<<<< ") || strings.HasPrefix(s, "<<<<<<< ")
}

func osWriteFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o644) }

// osReadFile is used by the resolve pipeline (resolve.go).
func osReadFile(dir, rel string) string {
	b, _ := os.ReadFile(filepath.Join(dir, rel))
	return string(b)
}

// gitIn runs git in dir, returning combined output + error.
func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// BuildConflictContext runs `git merge <targetBranch>` in the worktree to
// reproduce the conflict, then captures each conflicted file + each side's
// diff-vs-base. The merge is left IN PROGRESS (callers resolve + commit, or
// abort). Returns Clean=true when the merge had no conflicts.
func BuildConflictContext(worktree, targetBranch string) (*ConflictContext, error) {
	// Resolve against the FRESH base: a resolve worktree provisioned earlier can
	// hold a stale base ref, so we'd resolve against an out-of-date branch and
	// push a merge GitHub still sees as conflicting. Fetch first, then merge the
	// remote-tracking ref. Best-effort: if the fetch fails (offline), fall back
	// to the local ref rather than aborting the whole resolve.
	mergeRef := targetBranch
	if _, ferr := gitIn(worktree, "fetch", "origin", targetBranch); ferr == nil {
		mergeRef = "origin/" + targetBranch
	} else {
		log.Printf("conflict: fetch origin/%s failed (%v); merging local ref", targetBranch, ferr)
	}
	base, err := gitIn(worktree, "merge-base", "HEAD", mergeRef)
	if err != nil {
		return nil, fmt.Errorf("merge-base: %v: %s", err, base)
	}
	baseRef := strings.TrimSpace(base)
	mout, merr := gitIn(worktree, "merge", "--no-edit", "--no-ff", mergeRef)
	cc := &ConflictContext{TargetBranch: targetBranch}
	if merr == nil {
		cc.Clean = true
		return cc, nil
	}
	paths, perr := conflictedPaths(worktree)
	if perr != nil {
		return nil, fmt.Errorf("list conflicts (merge said %q): %v", mout, perr)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("merge failed but no unmerged paths found (merge output: %s)", mout)
	}
	for _, p := range paths {
		merged, _ := os.ReadFile(filepath.Join(worktree, p)) // best-effort
		oursDiff, _ := gitIn(worktree, "diff", baseRef, "HEAD", "--", p)
		theirsDiff, _ := gitIn(worktree, "diff", baseRef, mergeRef, "--", p)
		cc.Files = append(cc.Files, ConflictFile{
			Path: p, Merged: string(merged), OursDiff: oursDiff, TheirsDiff: theirsDiff,
		})
	}
	return cc, nil
}

// conflictedPaths returns repo-relative paths git marked as unmerged.
func conflictedPaths(worktree string) ([]string, error) {
	out, err := gitIn(worktree, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	var ps []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			ps = append(ps, line)
		}
	}
	return ps, nil
}
