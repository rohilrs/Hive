package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"

	"github.com/rohilrs/Hive/internal/roadmap"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// setSequenceStatus is the shared pause/resume body.
func (s *RPCServer) setSequenceStatus(ctx context.Context, slug, status string) *rpc.RPCError {
	if status != "active" && status != "paused" {
		return &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "invalid status: " + status}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + slug}
		}
		return internalErr(err)
	}
	disp, err := s.d.store.GetSequenceDispatcher(ctx, proj.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "sequenced dispatch not enabled for " + slug}
		}
		return internalErr(err)
	}
	disp.Status = status
	if err := s.d.store.UpsertSequenceDispatcher(ctx, disp); err != nil {
		return internalErr(err)
	}
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventSequenceUpdated, Data: map[string]any{
		"project_id": proj.ID, "slug": proj.Slug, "status": status,
	}})
	return nil
}

func (s *RPCServer) handleSequencePause(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p sequenceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	if rerr := s.setSequenceStatus(ctx, p.ProjectSlug, "paused"); rerr != nil {
		return nil, rerr
	}
	out, _ := json.Marshal(map[string]any{"project_slug": p.ProjectSlug, "status": "paused"})
	return out, nil
}

func (s *RPCServer) handleSequenceResume(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p sequenceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	if rerr := s.setSequenceStatus(ctx, p.ProjectSlug, "active"); rerr != nil {
		return nil, rerr
	}
	out, _ := json.Marshal(map[string]any{"project_slug": p.ProjectSlug, "status": "active"})
	return out, nil
}

type sequenceSkipParams struct {
	TaskID string `json:"task_id"`
}

// handleSequenceSkip marks gate_state=skipped for a task to unblock its phase.
// It does NOT verify the task belongs to a sequenced project: the scheduler
// only reads gate_state for sequenced-mode tasks, so this is a harmless no-op
// on non-sequenced tasks. The leniency is intentional (Phase 2b plan, Task 3) —
// do not add a dispatcher-membership precondition without revisiting that.
func (s *RPCServer) handleSequenceSkip(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p sequenceSkipParams
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
	if err := s.d.store.UpdateTaskGateState(ctx, task.ID, sequence.GateSkipped); err != nil {
		return nil, internalErr(err)
	}
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventSequenceGateChanged, Data: map[string]any{
		"project_id": task.ProjectID, "task_id": task.ID, "gate_state": sequence.GateSkipped,
	}})
	out, _ := json.Marshal(map[string]any{"task_id": task.ID, "gate_state": "skipped"})
	return out, nil
}

type sequenceCompleteParams struct {
	ProjectSlug string `json:"project_slug"`
	Phase       string `json:"phase"`
}

// handleSequenceComplete marks a roadmap phase as operator-complete via
// MarkPhaseComplete, after verifying every task in the phase is resolved
// (gate_state = satisfied or skipped). Best-effort writes a ✅ Done status
// line back to the on-disk roadmap. Emits EventSequenceGateChanged and
// returns the newly derived active_phase.
func (s *RPCServer) handleSequenceComplete(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p sequenceCompleteParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" || p.Phase == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug and phase required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
		}
		return nil, internalErr(err)
	}
	rm, roadmapPath, err := s.d.loadProjectRoadmap(proj)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if _, ok := rm.FindPhase(p.Phase); !ok {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "no such phase in roadmap: " + p.Phase}
	}
	plan, err := s.d.derivePlan(ctx, proj, rm)
	if err != nil {
		return nil, internalErr(err)
	}
	// Guard: every task in the target phase must be resolved (satisfied or skipped).
	for _, ph := range plan.Phases {
		if ph.Number != p.Phase {
			continue
		}
		for _, tv := range ph.Tasks {
			if tv.GateState != sequence.GateSatisfied && tv.GateState != sequence.GateSkipped {
				return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams,
					Message: "phase " + p.Phase + " has unresolved task " + tv.ID + " — finish it or `hive sequence skip` it first"}
			}
		}
	}
	if err := s.d.store.MarkPhaseComplete(ctx, proj.ID, p.Phase); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "sequenced dispatch not enabled for " + p.ProjectSlug}
		}
		return nil, internalErr(err)
	}
	// Best-effort: write ✅ Done status back to the roadmap file.
	roadmapUpdated := false
	if data, rerr := os.ReadFile(roadmapPath); rerr == nil {
		if outMd, changed := roadmap.SetPhaseStatus(string(data), p.Phase, "✅ Done — marked complete via Hive"); changed {
			if werr := atomicWriteFile(roadmapPath, []byte(outMd), 0o644); werr != nil {
				log.Printf("sequence complete: write roadmap %s: %v", roadmapPath, werr)
			} else {
				roadmapUpdated = true
			}
		}
	} else {
		log.Printf("sequence complete: read roadmap %s: %v", roadmapPath, rerr)
	}
	// Re-derive the plan to get the new active_phase after the completion is recorded.
	newPlan, derr := s.d.derivePlan(ctx, proj, rm)
	if derr != nil {
		log.Printf("sequence complete: re-derive plan for %s: %v", proj.Slug, derr)
	}
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventSequenceGateChanged, Data: map[string]any{
		"project_id": proj.ID, "phase": p.Phase, "active_phase": newPlan.ActivePhase,
	}})
	out, _ := json.Marshal(map[string]any{
		"project_slug": proj.Slug, "phase": p.Phase,
		"active_phase": newPlan.ActivePhase, "roadmap_updated": roadmapUpdated,
	})
	return out, nil
}

// handleSequenceAdvance forces the active phase forward by flipping every
// awaiting_merge task in that phase to satisfied. This is the manual-policy
// confirmation step (user merged the PRs out-of-band) and a forced escape
// hatch for a stuck merge under any policy. The next scheduler tick then sees
// the phase's gates satisfied and advances to the next phase.
func (s *RPCServer) handleSequenceAdvance(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p sequenceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
		}
		return nil, internalErr(err)
	}
	rm, _, err := s.d.loadProjectRoadmap(proj)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	plan, err := s.d.derivePlan(ctx, proj, rm)
	if err != nil {
		return nil, internalErr(err)
	}
	advanced := 0
	for _, ph := range plan.Phases {
		if ph.Number != plan.ActivePhase {
			continue
		}
		for _, tv := range ph.Tasks {
			if tv.GateState != sequence.GateAwaitingMerge {
				continue
			}
			if err := s.d.store.UpdateTaskGateState(ctx, tv.ID, sequence.GateSatisfied); err != nil {
				return nil, internalErr(err)
			}
			s.d.refreshTaskStatus(ctx, tv.ID) // status follows the forced gate to done
			s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventSequenceGateChanged, Data: map[string]any{
				"project_id": proj.ID, "task_id": tv.ID, "gate_state": sequence.GateSatisfied,
			}})
			advanced++
		}
	}
	out, _ := json.Marshal(map[string]any{"project_slug": proj.Slug, "advanced": advanced, "active_phase": plan.ActivePhase})
	return out, nil
}
