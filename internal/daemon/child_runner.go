package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// childRunner implements pipeline.SubRunner: it spawns a child Build run to
// fix a finish-branch gate failure. The child reuses the parent's worktree
// (the fix must land on the branch being finished) and the parent's task_id
// (same logical work), overriding only the in-memory Task.Body with a fix
// prompt. It blocks until the child Build pipeline returns; because it runs
// ON the parent pipeline's goroutine (the finish-branch gate stage), the
// parent stage is intentionally stalled for the child's full duration. The
// parent ctx is threaded through so abandoning the parent cancels the child.
type childRunner struct {
	d *Daemon
}

func (c *childRunner) RunChildFix(ctx context.Context, parent *pipeline.Run, gate, failureOutput string) (*pipeline.Result, error) {
	bp := c.d.Pipeline("build")
	if bp == nil {
		return nil, fmt.Errorf("build pipeline not registered")
	}

	// The child runs directly (not through the scheduler's capacity gate),
	// so it never deadlocks on a saturated worker pool. While it runs, both
	// this child row and the parent finish-branch row count as "running", so
	// the scheduler transiently sees one more than MaxWorkers — that only
	// suppresses NEW task dispatch during a fix (the parent does no
	// subprocess work while stalled), which is the behavior we want.
	// The child has its own registered cancel func (childCtx is derived
	// from the parent's ctx): `hive abandon <child-id>` cancels ONLY the
	// child without cascading up. Parent-cancel still cascades to the
	// child via ctx parentage, preserving the existing "abandon parent →
	// child dies" behavior. (Phase 4.3.1 auto-fix maturity sub-item #2.)
	childID := newID("run")

	// Derive a child context so the child has an independently-cancelable
	// ctx. Register the cancel in d.runCancels so the abandon RPC can find
	// it. Defer ordering: childCancel() fires first (LIFO) to release the
	// cancel goroutine even on panic paths; unregisterRunCancel then
	// removes the now-stale map entry.
	childCtx, childCancel := context.WithCancel(ctx)
	c.d.registerRunCancel(childID, childCancel)
	defer c.d.unregisterRunCancel(childID)
	defer childCancel()
	childRow := &store.Run{
		ID:          childID,
		TaskID:      parent.Task.ID,
		ProjectID:   parent.Project.ID,
		Pipeline:    "build",
		Status:      "running",
		ParentRunID: parent.ID,
	}
	if err := c.d.store.InsertRun(ctx, childRow); err != nil {
		return nil, fmt.Errorf("insert child run: %w", err)
	}
	if err := c.d.store.MarkRunStarted(ctx, childID); err != nil {
		return nil, fmt.Errorf("mark child started: %w", err)
	}
	c.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventRunStarted,
		Data: map[string]any{
			"run_id":        childID,
			"task_id":       parent.Task.ID,
			"task_title":    parent.Task.Title,
			"project_id":    parent.Project.ID,
			"pipeline":      "build",
			"parent_run_id": parent.ID,
		},
	})

	runtimeDir := filepath.Join(c.d.HiveDir(), childID)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		// The child row was already inserted + run.started emitted, so mark it
		// ended before returning to avoid a dangling "running" child row.
		mkdirErr := fmt.Errorf("mkdir child runtime: %w", err)
		_ = c.d.store.MarkRunEnded(ctx, childID, "needs_attention", mkdirErr.Error())
		c.d.bus.Publish(rpc.EventMessage{
			Type: rpc.EventRunEnded,
			Data: map[string]any{
				"run_id":  childID,
				"task_id": parent.Task.ID,
				"status":  "needs_attention",
				"summary": mkdirErr.Error(),
			},
		})
		return nil, mkdirErr
	}

	fixTask := *parent.Task // shallow copy; override body only
	fixTask.Body = childFixPrompt(gate, failureOutput)
	childPR := &pipeline.Run{
		ID:           childID,
		Task:         &fixTask,
		Project:      parent.Project,
		WorktreePath: parent.WorktreePath,
		RuntimeDir:   runtimeDir,
		BranchName:   parent.BranchName,
		Pipeline:     "build",
		Commands:     c.d.runCommandsForProject(parent.Project.Slug),
	}

	// Use childCtx so cancelRun(childID) terminates only the child without
	// cascading up to the parent's finish-branch ctx.
	result, runErr := bp.Run(childCtx, childPR)

	status, summary := "needs_attention", ""
	switch {
	case runErr != nil:
		summary = "child build error: " + runErr.Error()
	case result != nil:
		status, summary = result.Status, result.Summary
	}
	_ = c.d.store.MarkRunEnded(ctx, childID, status, summary)
	c.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventRunEnded,
		Data: map[string]any{
			"run_id":  childID,
			"task_id": parent.Task.ID,
			"status":  status,
			"summary": summary,
		},
	})
	if runErr != nil {
		return nil, runErr
	}
	return result, nil
}

// childFixPrompt frames the gate failure as a focused fix task for the
// child Build worker.
func childFixPrompt(gate, failureOutput string) string {
	return "A finish-branch gate failed and must be fixed before the branch can " +
		"be completed.\n\nGate: " + gate + "\n\nFailure output:\n" + failureOutput +
		"\n\nMake the minimal change in this worktree so the gate passes. Do not " +
		"refactor unrelated code. Do not weaken or delete the check to make it pass."
}
