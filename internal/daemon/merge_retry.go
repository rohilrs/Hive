package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// MergeRetryParams is the params envelope for merge.retry.
type MergeRetryParams struct {
	TaskID string `json:"task_id"`
}

// handleMergeRetry recovers a task parked at the terminal merge_failed gate.
// It re-checks the PR: if it already merged into the project's base, the task
// is reconciled to satisfied; otherwise the task is re-armed (gate ->
// awaiting_merge, attempt counter reset, merge queue kicked) so the auto-
// resolver gets a fresh budget. This is the ONLY sanctioned way out of
// merge_failed — manual `hive resolve` is refused for such tasks.
func (s *RPCServer) handleMergeRetry(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p MergeRetryParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.TaskID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "task_id required"}
	}
	task, err := s.d.store.GetTask(ctx, p.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrTaskNotFound, Message: "task not found"}
		}
		return nil, internalErr(err)
	}
	if task.GateState != sequence.GateMergeFailed {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams,
			Message: "task " + task.ID + " is not parked at merge_failed (gate=" + task.GateState + ")"}
	}
	proj, err := s.d.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return nil, internalErr(err)
	}

	prURL, _, _ := s.d.store.PRForTask(ctx, task.ID)
	if prURL != "" {
		merged, baseRef, serr := s.d.prGateway.State(ctx, prURL)
		base := s.d.scheduler.effectiveWorktreeBaseForProject(proj.Slug)
		if serr == nil && merged && baseRef == base {
			if uerr := s.d.store.UpdateTaskGateState(ctx, task.ID, sequence.GateSatisfied); uerr != nil {
				return nil, internalErr(uerr)
			}
			s.d.mergeAttempts.reset(task.ID)
			s.d.refreshTaskStatus(ctx, task.ID)
			s.d.scheduler.emitGateChanged(proj, task.ID, sequence.GateSatisfied)
			s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskMerged, Data: map[string]any{
				"task_id": task.ID, "pr_url": prURL,
			}})
			// Fast-forward the local base for parity with checkOneMerge's
			// confirmed-merge path, so the operator's checkout doesn't lag a
			// commit behind after a reconcile. Best-effort + guarded.
			if proj.RepoPath != nil && *proj.RepoPath != "" {
				s.d.syncLocalBaseAfterMerge(*proj.RepoPath, base, proj.Slug)
			}
			log.Printf("merge.retry: task %s already merged into %s -> satisfied", task.ID, base)
			out, _ := json.Marshal(map[string]any{"task_id": task.ID, "action": "satisfied"})
			return out, nil
		}
	}

	s.d.mergeAttempts.reset(task.ID)
	if uerr := s.d.store.UpdateTaskGateState(ctx, task.ID, sequence.GateAwaitingMerge); uerr != nil {
		return nil, internalErr(uerr)
	}
	s.d.scheduler.emitGateChanged(proj, task.ID, sequence.GateAwaitingMerge)
	s.d.kickMergeQueue()
	log.Printf("merge.retry: task %s re-armed (awaiting_merge, fresh cap budget)", task.ID)
	out, _ := json.Marshal(map[string]any{"task_id": task.ID, "action": "rearmed"})
	return out, nil
}
