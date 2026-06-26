package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohilrs/Hive/internal/store"
)

// runWorktreesChecks audits Hive's worktree+scratch layout against the
// store's run rows. Three emitted checks:
//   - worktrees.orphans:            dirs in <hiveDir>/worktrees/ with no matching run row.
//   - worktrees.missing_for_running: runs in 'running' state with no worktree dir.
//   - worktrees.stale_scratch:      per-run scratch dirs (<hiveDir>/r-*) for runs in
//     a terminal state (anything not pending/running — done /
//     abandoned / error / needs_attention / future).
//
// Daemon-down-safe: opens the store read-side directly. If the DB is
// missing or unopenable we still report the readdir results; with an
// empty known-set everything in worktrees/ becomes "orphan".
func runWorktreesChecks(ctx context.Context, hiveDir string, client RPCClient) []Check {
	wtRoot := filepath.Join(hiveDir, "worktrees")
	entries, err := os.ReadDir(wtRoot)
	if os.IsNotExist(err) {
		return []Check{
			{Name: "worktrees.orphans", Subsystem: "worktrees", Status: StatusOK, Message: "no worktrees dir (none expected on fresh install)"},
			{Name: "worktrees.missing_for_running", Subsystem: "worktrees", Status: StatusOK, Message: "no worktrees dir; nothing to verify"},
			{Name: "worktrees.stale_scratch", Subsystem: "worktrees", Status: StatusOK, Message: "no worktrees dir; nothing to verify"},
		}
	}
	if err != nil {
		return []Check{
			{Name: "worktrees.orphans", Subsystem: "worktrees", Status: StatusError, Message: "readdir " + wtRoot + ": " + err.Error()},
			skipCheck("worktrees.missing_for_running", "worktrees", "skipped — readdir failed"),
			skipCheck("worktrees.stale_scratch", "worktrees", "skipped — readdir failed"),
		}
	}

	// Build the set of known run IDs from the store. ListRecentRuns
	// filters out status='running' (it's the "recently ended" list), so
	// W2 must also pull ListRunningRuns or it would never find any
	// case to flag.
	known := map[string]string{} // runID → status
	dbPath := filepath.Join(hiveDir, "db.sqlite")
	if _, statErr := os.Stat(dbPath); statErr == nil {
		s, err := store.Open(ctx, dbPath)
		if err == nil {
			defer s.Close()
			running, _ := s.ListRunningRuns(ctx)
			for _, r := range running {
				known[r.ID] = "running"
			}
			recent, _ := s.ListRecentRuns(ctx, 10000)
			for _, r := range recent {
				// Don't overwrite running (recent may include the
				// same IDs if the filter loosens later).
				if _, exists := known[r.ID]; !exists {
					known[r.ID] = r.Status
				}
			}
		}
	}

	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := known[e.Name()]; !ok {
			orphans = append(orphans, filepath.Join(wtRoot, e.Name()))
		}
	}

	var checks []Check
	if len(orphans) == 0 {
		checks = append(checks, Check{Name: "worktrees.orphans", Subsystem: "worktrees", Status: StatusOK, Message: "none"})
	} else {
		checks = append(checks, Check{
			Name: "worktrees.orphans", Subsystem: "worktrees", Status: StatusWarn,
			Message: fmt.Sprintf("%d orphan dirs", len(orphans)),
			Hint:    "no matching runs; run `hive clean` to reclaim, or remove:\n  " + strings.Join(orphans, "\n  "),
		})
	}

	var missing []string
	for runID, status := range known {
		if status != "running" {
			continue
		}
		if _, err := os.Stat(filepath.Join(wtRoot, runID)); os.IsNotExist(err) {
			missing = append(missing, runID)
		}
	}
	if len(missing) == 0 {
		checks = append(checks, Check{Name: "worktrees.missing_for_running", Subsystem: "worktrees", Status: StatusOK, Message: "none"})
	} else {
		checks = append(checks, Check{
			Name: "worktrees.missing_for_running", Subsystem: "worktrees", Status: StatusError,
			Message: fmt.Sprintf("%d running runs missing worktree", len(missing)),
			Hint:    "runs: " + strings.Join(missing, ", "),
		})
	}

	// W3 stale scratch: <hiveDir>/<run-id>/ for terminal-state runs.
	var staleScratch []string
	rootEntries, _ := os.ReadDir(hiveDir)
	for _, e := range rootEntries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Per-run scratch dirs are named "run-<unixnano>" (Hive's run ID format).
		if !strings.HasPrefix(name, "run-") {
			continue
		}
		status, ok := known[name]
		if !ok {
			continue
		}
		// Inverted check so future terminal statuses (e.g.
		// needs_attention, which the original whitelist missed)
		// don't silently regress. Anything that's not pending and
		// not running is effectively terminal.
		if status != "running" && status != "pending" {
			staleScratch = append(staleScratch, filepath.Join(hiveDir, name))
		}
	}
	if len(staleScratch) == 0 {
		checks = append(checks, Check{Name: "worktrees.stale_scratch", Subsystem: "worktrees", Status: StatusOK, Message: "none"})
	} else {
		checks = append(checks, Check{
			Name: "worktrees.stale_scratch", Subsystem: "worktrees", Status: StatusWarn,
			Message: fmt.Sprintf("%d stale scratch dirs (runs in terminal state)", len(staleScratch)),
			Hint:    "run `hive clean` to reclaim, or remove:\n  " + strings.Join(staleScratch, "\n  "),
		})
	}

	return checks
}
