// Package cleanup decides and executes reclamation of per-run on-disk
// artifacts (worktrees, scratch dirs, branches). BuildPlan is pure; the
// Reclaimer (reclaimer.go) performs the I/O.
package cleanup

import (
	"sort"
	"time"
)

// Retention controls which runs' artifacts are kept vs reclaimed.
type Retention struct {
	KeepLastRuns int           // keep the N most-recent runs' artifacts
	OrphanGrace  time.Duration // min age before an orphan dir (no run row) is reclaimed
	Branches     bool          // also remove the hive/<run> branch
}

// RunInfo is the minimal run projection BuildPlan needs.
type RunInfo struct {
	ID          string
	Status      string
	CreatedAt   time.Time
	ParentRunID string
	RepoPath    string // resolved source repo ("" if project gone)
	BranchName  string // hive/<run> ("" if unknown)
}

// DirInfo is one on-disk artifact set discovered under HiveDir.
type DirInfo struct {
	RunID    string
	Worktree string // "" if none
	Scratch  string // "" if none
	Mtime    time.Time
}

type ReclaimItem struct {
	RunID, Worktree, Scratch, RepoPath, BranchName string
	Reason                                         string // "terminal+past-retention" | "orphan"
}

type Plan struct {
	Reclaim []ReclaimItem
	Kept    int
}

func isTerminal(status string) bool {
	switch status {
	case "done", "needs_attention", "abandoned", "error":
		return true
	}
	return false
}

// BuildPlan computes which dirs are reclaimable. Pure: no I/O.
func BuildPlan(runs []RunInfo, dirs []DirInfo, ret Retention, now time.Time) Plan {
	byID := make(map[string]RunInfo, len(runs))
	for _, r := range runs {
		byID[r.ID] = r
	}

	// keep-set: the N most-recent runs, plus all running runs, plus parents of
	// any in-flight (pending/running) child-fix run (which reuses the parent
	// worktree).
	keep := make(map[string]bool)
	sorted := append([]RunInfo(nil), runs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.After(sorted[j].CreatedAt) })
	for i, r := range sorted {
		if i < ret.KeepLastRuns || r.Status == "running" {
			keep[r.ID] = true
		}
	}
	for _, r := range runs {
		if r.ParentRunID != "" && (r.Status == "running" || r.Status == "pending") {
			keep[r.ParentRunID] = true
		}
	}

	var plan Plan
	for _, d := range dirs {
		r, known := byID[d.RunID]
		switch {
		case !known:
			if now.Sub(d.Mtime) < ret.OrphanGrace {
				plan.Kept++
				continue
			}
			plan.Reclaim = append(plan.Reclaim, ReclaimItem{
				RunID: d.RunID, Worktree: d.Worktree, Scratch: d.Scratch, Reason: "orphan",
			})
		case keep[d.RunID] || !isTerminal(r.Status):
			plan.Kept++
		default:
			plan.Reclaim = append(plan.Reclaim, ReclaimItem{
				RunID: d.RunID, Worktree: d.Worktree, Scratch: d.Scratch,
				RepoPath: r.RepoPath, BranchName: r.BranchName, Reason: "terminal+past-retention",
			})
		}
	}
	return plan
}
