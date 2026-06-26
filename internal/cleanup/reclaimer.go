package cleanup

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rohilrs/Hive/internal/worktree"
)

// Reclaimer executes a Plan. WT is reused for `git worktree remove --force`;
// Log is the (best-effort) logger for non-fatal per-item failures.
type Reclaimer struct {
	WT  *worktree.Manager
	Log func(format string, args ...any)
}

// Result summarizes a reclamation pass.
type Result struct {
	Runs   int
	Bytes  int64
	Errors []string
}

func (r *Reclaimer) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// Reclaim removes the artifacts in p. dryRun accounts bytes without deleting.
// branches gates `git branch -D`. Per-item failures are collected, not fatal.
func (r *Reclaimer) Reclaim(ctx context.Context, p Plan, dryRun, branches bool) Result {
	var res Result
	for _, it := range p.Reclaim {
		bytes := dirBytes(it.Worktree) + dirBytes(it.Scratch)
		if dryRun {
			res.Runs++
			res.Bytes += bytes
			continue
		}
		if it.Worktree != "" {
			removed := false
			if it.RepoPath != "" && r.WT != nil {
				if err := r.WT.Remove(ctx, it.RepoPath, it.RunID); err == nil {
					removed = true
				} else {
					r.logf("cleanup: worktree remove %s: %v (falling back to rm)", it.RunID, err)
				}
			}
			if !removed {
				if err := forceRemoveAll(it.Worktree); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("worktree %s: %v", it.RunID, err))
				}
			}
		}
		if it.Scratch != "" {
			if err := forceRemoveAll(it.Scratch); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("scratch %s: %v", it.RunID, err))
			}
		}
		if branches && it.RepoPath != "" && it.BranchName != "" {
			if out, err := exec.CommandContext(ctx, "git", "-C", it.RepoPath,
				"branch", "-D", it.BranchName).CombinedOutput(); err != nil {
				r.logf("cleanup: branch -D %s in %s: %v: %s", it.BranchName, it.RepoPath, err, strings.TrimSpace(string(out)))
			}
		}
		res.Runs++
		res.Bytes += bytes
	}
	return res
}

// forceRemoveAll makes the tree writable before removing it — Go's module
// cache writes read-only files (and dirs), which otherwise defeat RemoveAll.
func forceRemoveAll(path string) error {
	if path == "" {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// dirBytes sums file sizes under path (0 for "" or a missing dir).
func dirBytes(path string) int64 {
	if path == "" {
		return 0
	}
	var n int64
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
