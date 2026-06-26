package daemon

import (
	"context"
	"log"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// DeriveTaskStatus is the single source of truth for a task's status,
// computed purely from its gate state and its runs. See
// docs/superpowers/specs/2026-06-07-task-status-cluster-design.md.
//
// Priority:
//  1. any run still running          -> "running"
//  2. gate == satisfied              -> "done"  (merged; overrides stale needs_attention)
//  3. latest NON-abandoned terminal run: done -> "done"; needs_attention|error -> "needs_attention"
//  4. no non-abandoned terminal run  -> "pending"
func DeriveTaskStatus(gate string, runs []*store.Run) string {
	for _, r := range runs {
		if r.Status == "running" {
			return "running"
		}
	}
	if gate == sequence.GateSatisfied {
		return "done"
	}
	var latest *store.Run
	for _, r := range runs {
		if r.Status == "abandoned" {
			continue
		}
		if r.Status == "running" {
			continue
		}
		if latest == nil || endedKey(r) > endedKey(latest) ||
			(endedKey(r) == endedKey(latest) && r.CreatedAt.After(latest.CreatedAt)) {
			latest = r
		}
	}
	if latest == nil {
		return "pending"
	}
	switch latest.Status {
	case "done":
		return "done"
	default: // needs_attention, error
		return "needs_attention"
	}
}

// refreshTaskStatus recomputes a task's status from its gate + runs and
// writes it only on change, publishing EventTaskUpdated so the reconcile
// loop and TUI snapshot follow. Best-effort: load failures log and return.
func (d *Daemon) refreshTaskStatus(ctx context.Context, taskID string) {
	task, err := d.store.GetTask(ctx, taskID)
	if err != nil {
		log.Printf("refreshTaskStatus: get task %s: %v", taskID, err)
		return
	}
	runs, err := d.store.ListRunsByTask(ctx, taskID)
	if err != nil {
		log.Printf("refreshTaskStatus: list runs %s: %v", taskID, err)
		return
	}
	desired := DeriveTaskStatus(task.GateState, runs)
	if desired == task.Status {
		return
	}
	if err := d.store.UpdateTaskStatus(ctx, taskID, desired); err != nil {
		log.Printf("refreshTaskStatus: update %s->%s: %v", taskID, desired, err)
		return
	}
	d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventTaskUpdated,
		Data: map[string]any{
			"task_id":    taskID,
			"status":     desired,
			"project_id": task.ProjectID,
		},
	})
}

// endedKey orders terminal runs by ended_at, falling back to 0 when unset.
func endedKey(r *store.Run) int64 {
	if r.EndedAt != nil {
		return r.EndedAt.Unix()
	}
	return 0
}
