package worktree

import (
	"context"
	"errors"
	"fmt"
)

// CleanStale removes every Hive-managed worktree under this Manager's
// WorktreeRoot whose RunID is NOT in activeRunIDs. Returns the list of
// removed run IDs. Best-effort: a failure on one worktree does not stop
// the iteration; all errors are joined into the returned error.
func (m *Manager) CleanStale(ctx context.Context, repoPath string, activeRunIDs []string) ([]string, error) {
	active := make(map[string]bool, len(activeRunIDs))
	for _, id := range activeRunIDs {
		active[id] = true
	}

	infos, err := m.List(ctx, repoPath)
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, info := range infos {
		if info.RunID == "" || active[info.RunID] {
			continue
		}
		if err := m.Remove(ctx, repoPath, info.RunID); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", info.RunID, err))
			continue
		}
		removed = append(removed, info.RunID)
	}
	return removed, errors.Join(errs...)
}
