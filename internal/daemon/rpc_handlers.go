package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/approval"
	"github.com/rohilrs/Hive/internal/codeintel"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// dispatch routes a parsed request envelope to the appropriate handler.
// Handlers return raw JSON results so the server can stitch them into
// a generic Response[json.RawMessage] envelope.
func (s *RPCServer) dispatch(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	switch method {
	case rpc.MethodStatus:
		return s.handleStatus(ctx)
	case rpc.MethodHealth:
		return s.handleHealth(ctx)
	case rpc.MethodAddTask:
		return s.handleAddTask(ctx, params)
	case rpc.MethodResume:
		return s.handleRunResume(ctx, params)
	case rpc.MethodRunNow:
		return s.handleRunNow(ctx, params)
	case rpc.MethodResolveNow:
		return s.handleResolveNow(ctx, params)
	case rpc.MethodMergeRetry:
		return s.handleMergeRetry(ctx, params)
	case rpc.MethodListTasks:
		return s.handleListTasks(ctx, params)
	case rpc.MethodGetRun:
		return s.handleGetRun(ctx, params)
	case rpc.MethodListProjects:
		return s.handleListProjects(ctx)
	case rpc.MethodCostSummary:
		return s.handleCostSummary(ctx)
	case rpc.MethodAddProject:
		return s.handleAddProject(ctx, params)
	case rpc.MethodEditProject:
		return s.handleEditProject(ctx, params)
	case rpc.MethodArchiveProject:
		return s.handleArchiveProject(ctx, params)
	case rpc.MethodDeleteProject:
		return s.handleDeleteProject(ctx, params)
	case rpc.MethodProjectGraduate:
		return s.handleProjectGraduate(ctx, params)
	case rpc.MethodProjectGraduateStatus:
		return s.handleProjectGraduateStatus(ctx, params)
	case rpc.MethodProjectRemediate:
		return s.handleProjectRemediate(ctx, params)
	case rpc.MethodAbandon:
		return s.handleRunAbandon(ctx, params)
	case rpc.MethodRunDocument:
		return s.handleRunDocument(ctx, params)
	case rpc.MethodDocumentationSubmit:
		return s.handleDocumentationSubmit(ctx, params)
	case rpc.MethodRunStages:
		return s.handleRunStages(ctx, params)
	case rpc.MethodGetTask:
		return s.handleGetTask(ctx, params)
	case rpc.MethodEditTask:
		return s.handleEditTask(ctx, params)
	case rpc.MethodDeleteTask:
		return s.handleDeleteTask(ctx, params)
	case rpc.MethodApprovalEvaluate:
		return s.handleApprovalEvaluate(ctx, params)
	case rpc.MethodApprovalList:
		return s.handleApprovalList(ctx, params)
	case rpc.MethodApprovalRuleAdd:
		return s.handleApprovalRuleAdd(ctx, params)
	case rpc.MethodApprovalResolve:
		return s.handleApprovalResolve(ctx, params)
	case rpc.MethodApprovalPending:
		return s.handleApprovalPending(ctx)
	case rpc.MethodSourcesSync:
		return s.handleSourcesSync(ctx, params)
	case rpc.MethodSourcesBind:
		return s.handleSourcesBind(ctx, params)
	case rpc.MethodSourcesList:
		return s.handleSourcesList(ctx, params)
	case rpc.MethodSourcesUnbind:
		return s.handleSourcesUnbind(ctx, params)
	case rpc.MethodSourcesStatus:
		return s.handleSourcesStatus(ctx, params)
	case rpc.MethodChatTool:
		return s.handleChatTool(ctx, params)
	case rpc.MethodChatConfirm:
		return s.handleChatConfirm(ctx, params)
	case rpc.MethodChatHistoryList:
		return s.handleChatHistoryList(ctx, params)
	case rpc.MethodChatHistoryGet:
		return s.handleChatHistoryGet(ctx, params)
	case rpc.MethodChatSetName:
		return s.handleChatSetName(ctx, params)
	case rpc.MethodChatDelete:
		return s.handleChatDelete(ctx, params)
	case rpc.MethodDecompose:
		return s.handleDecompose(ctx, params)
	case rpc.MethodDecomposeApply:
		return s.handleDecomposeApply(ctx, params)
	case rpc.MethodRoadmapContent:
		return s.handleRoadmapContent(ctx, params)
	case rpc.MethodRoadmapDecompose:
		return s.handleRoadmapDecompose(ctx, params)
	case rpc.MethodRoadmapDecomposeApply:
		return s.handleRoadmapDecomposeApply(ctx, params)
	case rpc.MethodRoadmapSyncLinear:
		return s.handleRoadmapSyncLinear(ctx, params)
	case rpc.MethodRoadmapPlanSetup:
		return s.handleRoadmapPlanSetup(ctx, params)
	case rpc.MethodRoadmapPlanPush:
		return s.handleRoadmapPlanPush(ctx, params)
	case rpc.MethodSequenceEnable:
		return s.handleSequenceEnable(ctx, params)
	case rpc.MethodSequenceDisable:
		return s.handleSequenceDisable(ctx, params)
	case rpc.MethodSequenceStatus:
		return s.handleSequenceStatus(ctx, params)
	case rpc.MethodSequencePause:
		return s.handleSequencePause(ctx, params)
	case rpc.MethodSequenceResume:
		return s.handleSequenceResume(ctx, params)
	case rpc.MethodSequenceSkip:
		return s.handleSequenceSkip(ctx, params)
	case rpc.MethodSequenceAdvance:
		return s.handleSequenceAdvance(ctx, params)
	case rpc.MethodSequenceComplete:
		return s.handleSequenceComplete(ctx, params)
	case rpc.MethodCleanupRun:
		return s.handleCleanupRun(ctx, params)
	case rpc.MethodTaskFinish:
		return s.handleTaskFinish(ctx, params)
	case rpc.MethodHealthRemediate:
		return s.handleHealthRemediate(ctx, params)
	default:
		return nil, &rpc.RPCError{Code: rpc.ErrMethodNotFound, Message: "unknown method: " + method}
	}
}

// GetTaskParams is the params envelope for task.get / task.delete.
type GetTaskParams struct {
	TaskID string `json:"task_id"`
}

func (s *RPCServer) handleGetTask(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p GetTaskParams
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
	out, _ := json.Marshal(taskToView(task))
	return out, nil
}

// EditTaskParams is the params envelope for task.edit.
type EditTaskParams struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

func (s *RPCServer) handleEditTask(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p EditTaskParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.TaskID == "" || p.Title == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "task_id and title required"}
	}
	if err := s.d.store.UpdateTaskContent(ctx, p.TaskID, p.Title, p.Body); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrTaskNotFound, Message: "task not found"}
		}
		return nil, internalErr(err)
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventTaskUpdated,
		Data: map[string]any{"task_id": p.TaskID, "title": p.Title},
	})
	out, _ := json.Marshal(map[string]string{"task_id": p.TaskID})
	return out, nil
}

func (s *RPCServer) handleDeleteTask(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p GetTaskParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.TaskID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "task_id required"}
	}
	// Propagate the delete to Linear FIRST, while the task row still exists.
	// Without this, deleting a mirrored task only removes the local row — the
	// open Linear issue survives and the next sync re-imports it (reconcile
	// OpInsert), so the delete "bounces back" as a fresh duplicate. Archiving
	// drops the issue out of the default issues query, closing that loop.
	// Best-effort: a Linear failure logs but never blocks local deletion.
	s.archiveLinearMirror(ctx, p.TaskID)

	if err := s.d.store.DeleteTask(ctx, p.TaskID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrTaskNotFound, Message: "task not found"}
		}
		return nil, internalErr(err)
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventTaskUpdated,
		Data: map[string]any{"task_id": p.TaskID, "deleted": true},
	})
	out, _ := json.Marshal(map[string]any{"task_id": p.TaskID, "deleted": true})
	return out, nil
}

// archiveLinearMirror archives the Linear issue mirroring a task, so a local
// delete also removes the work item upstream and the syncer doesn't re-import
// it. No-op unless: a writer is wired, the task is Linear-sourced with a
// source_id, and its project is write-back bound (Hive owns the issue
// lifecycle). Best-effort — all failures log and return without surfacing, so
// task deletion proceeds regardless of Linear availability.
func (s *RPCServer) archiveLinearMirror(ctx context.Context, taskID string) {
	if s.d.linearWriter == nil {
		return
	}
	task, err := s.d.store.GetTask(ctx, taskID)
	if err != nil {
		return // not found / store error → let the delete path report it
	}
	if task.Source != "linear" || task.SourceID == "" {
		return // Hive-local task with no Linear mirror
	}
	proj, err := s.d.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return
	}
	if _, _, ok := linearWriteTarget(proj); !ok {
		return // pull-only binding → Hive doesn't own this issue's lifecycle
	}
	if err := s.d.linearWriter.ArchiveIssue(ctx, task.SourceID); err != nil {
		log.Printf("linear write-back: archive issue for deleted task %s failed: %v", taskID, err)
	}
}

func (s *RPCServer) handleCostSummary(ctx context.Context) (json.RawMessage, *rpc.RPCError) {
	cs, err := s.d.store.CostSummary(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	convert := func(in []store.CostBucket) []rpc.CostBucket {
		out := make([]rpc.CostBucket, len(in))
		for i, b := range in {
			out[i] = rpc.CostBucket{Key: b.Key, TotalUSD: b.TotalUSD, Count: b.Count}
		}
		return out
	}
	out := rpc.CostSummaryView{
		Daily:       convert(cs.Daily),
		Models:      convert(cs.Models),
		Pipelines:   convert(cs.Pipelines),
		Projects:    convert(cs.Projects),
		GeneratedAt: cs.GeneratedAt.Unix(),
	}
	raw, _ := json.Marshal(out)
	return raw, nil
}

// RunStagesParams is the params envelope for run.stages.
type RunStagesParams struct {
	RunID string `json:"run_id"`
}

func (s *RPCServer) handleRunStages(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p RunStagesParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.RunID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run_id required"}
	}
	stages, err := s.d.store.ListStagesForRun(ctx, p.RunID)
	if err != nil {
		return nil, internalErr(err)
	}
	out := make([]rpc.StageRow, len(stages))
	for i, st := range stages {
		out[i] = rpc.StageRow{
			ID:        st.ID,
			RunID:     st.RunID,
			Name:      st.Name,
			Iter:      st.Iter,
			Model:     st.Model,
			StartedAt: st.StartedAt,
			EndedAt:   st.EndedAt,
			Verdict:   st.Verdict,
			TokensIn:  st.TokensIn,
			TokensOut: st.TokensOut,
		}
	}
	raw, _ := json.Marshal(out)
	return raw, nil
}

// AbandonParams is the params envelope for run.abandon.
type AbandonParams struct {
	RunID string `json:"run_id"`
}

func (s *RPCServer) handleRunAbandon(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p AbandonParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.RunID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run_id required"}
	}
	// Cancel in-flight pipeline if running. Marking the run row is
	// idempotent — abandons of already-ended runs still succeed.
	cancelled := s.d.cancelRun(p.RunID)
	_ = s.d.store.MarkRunEnded(ctx, p.RunID, "abandoned", "abandoned via RPC")
	// Look up task ID for the event payload (best-effort).
	taskID := ""
	if r, err := s.d.store.GetRun(ctx, p.RunID); err == nil {
		taskID = r.TaskID
		s.d.refreshTaskStatus(ctx, taskID) // MarkRunEnded(abandoned) ran above, so the derive sees it
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventRunEnded,
		Data: map[string]any{
			"run_id":  p.RunID,
			"task_id": taskID,
			"status":  "abandoned",
			"summary": "abandoned via RPC",
		},
	})
	out, _ := json.Marshal(map[string]any{
		"cancelled": cancelled,
		"run_id":    p.RunID,
	})
	return out, nil
}

// handleRunDocument re-runs the documenter stage for a completed run
// (Phase 4.4 follow-up: `hive document <run-id>`). The documenter is
// non-blocking, so a "done" run may have skipped its docs; this lets the
// operator fill them in after the fact. Dispatched async (the documenter
// subprocess outlives the CLI's RPC read deadline); the outcome is
// persisted + emitted via run.updated so the TUI chip clears live.
func (s *RPCServer) handleRunDocument(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p AbandonParams // reuses {run_id}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.RunID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run_id required"}
	}
	run, err := s.d.store.GetRun(ctx, p.RunID)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run not found: " + p.RunID}
	}
	if run.Status == "running" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run is still in progress; wait for it to finish"}
	}
	bp, ok := s.d.Pipeline("build").(*pipeline.BuildPipeline)
	if !ok || bp == nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "build pipeline unavailable"}
	}
	task, terr := s.d.store.GetTask(ctx, run.TaskID)
	proj, perr := s.d.store.GetProject(ctx, run.ProjectID)
	if terr != nil || perr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "load run context failed"}
	}
	pr := &pipeline.Run{
		ID:           run.ID,
		Task:         task,
		Project:      proj,
		WorktreePath: filepath.Join(s.d.HiveDir(), "worktrees", run.ID),
		RuntimeDir:   filepath.Join(s.d.HiveDir(), run.ID),
		Pipeline:     run.Pipeline,
		Commands:     s.d.runCommandsForProject(proj.Slug),
	}

	s.d.goTracked(func() {
		// Use the daemon ctx, not the request ctx — the CLI connection
		// closes once it gets this dispatch ack, which would otherwise
		// cancel the documenter mid-run.
		skipped, reason, derr := bp.RunDocument(s.d.ctx, pr)
		data := map[string]any{"run_id": run.ID}
		switch {
		case derr != nil:
			data["documentation_skipped"] = true
			data["documentation_skip_reason"] = derr.Error()
			_ = s.d.store.MarkDocumentationSkipped(s.d.ctx, run.ID, derr.Error())
		case skipped:
			data["documentation_skipped"] = true
			data["documentation_skip_reason"] = reason
			_ = s.d.store.MarkDocumentationSkipped(s.d.ctx, run.ID, reason)
		default:
			data["documentation_skipped"] = false
			_ = s.d.store.ClearDocumentationSkipped(s.d.ctx, run.ID)
		}
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventRunUpdated, Data: data})
	})

	out, _ := json.Marshal(map[string]any{"dispatched": true, "run_id": run.ID})
	return out, nil
}

// DocumentationSubmitParams is the payload from the hive_submit_documentation
// MCP tool (forwarded over the daemon socket by the documenter stage).
type DocumentationSubmitParams struct {
	RunID          string   `json:"run_id"`
	Stage          string   `json:"stage"`
	Summary        string   `json:"summary"`
	FilesChanged   []string `json:"files_changed"`
	ChangelogEntry string   `json:"changelog_entry"`
}

// handleDocumentationSubmit records the documenter's structured output by
// emitting a documentation.submitted event. The tool-call is already
// persisted to tool_calls by the document stage, so this needs no storage.
// Non-blocking: always returns ok (a failed documenter never fails a run).
func (s *RPCServer) handleDocumentationSubmit(_ context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p DocumentationSubmitParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.RunID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run_id required"}
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventDocumentationSubmitted,
		Data: map[string]any{
			"run_id":          p.RunID,
			"stage":           p.Stage,
			"summary":         p.Summary,
			"files_changed":   p.FilesChanged,
			"changelog_entry": p.ChangelogEntry,
		},
	})
	out, _ := json.Marshal(map[string]any{"ok": true})
	return out, nil
}

// AddProjectParams is the params envelope for project.add.
type AddProjectParams struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	RepoPath string `json:"repo_path,omitempty"`

	DispatchMode      string  `json:"dispatch_mode,omitempty"` // manual|auto_all (sequenced rejected at create)
	TargetBranch      *string `json:"target_branch,omitempty"` // [scheduler] base; independent of dispatch mode
	FeatureBranch     *string `json:"feature_branch,omitempty"`
	MergeMethod       *string `json:"merge_method,omitempty"`
	TaskAutoIntegrate *bool   `json:"task_auto_integrate,omitempty"`
	AutoFixCI         *bool   `json:"auto_fix_ci,omitempty"`
}

func (s *RPCServer) handleAddProject(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p AddProjectParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Slug == "" || p.Name == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "slug and name required"}
	}
	proj := &store.Project{
		ID:     newID("proj"),
		Slug:   p.Slug,
		Name:   p.Name,
		Status: "active",
	}
	if p.RepoPath != "" {
		proj.RepoPath = &p.RepoPath
	}
	if err := s.d.store.InsertProject(ctx, proj); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "create project: " + err.Error()}
	}
	// Integration config (per-project [integration]) — only the keys the caller set.
	integ := map[string]any{}
	if p.FeatureBranch != nil {
		integ["feature_branch"] = *p.FeatureBranch
	}
	if p.MergeMethod != nil {
		integ["merge_method"] = *p.MergeMethod
	}
	if p.TaskAutoIntegrate != nil {
		integ["task_auto_integrate"] = *p.TaskAutoIntegrate
	}
	if p.AutoFixCI != nil {
		integ["auto_fix_ci"] = *p.AutoFixCI
	}
	if len(integ) > 0 {
		if err := config.SetProjectIntegration(s.d.cfg.HiveDir, proj.Slug, integ); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "write integration config: " + err.Error()}
		}
	}
	// Target branch ([scheduler] base) — independent of dispatch mode: it's the
	// grounding/health base and the worktree-fork-base fallback when no feature
	// branch is set, so it applies in manual/auto_all too.
	if p.TargetBranch != nil {
		if err := config.SetProjectScheduler(s.d.cfg.HiveDir, proj.Slug, map[string]any{"target_branch": *p.TargetBranch}); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "write target branch: " + err.Error()}
		}
	}
	// Dispatch mode (manual/auto_all only at create — sequenced needs a roadmap,
	// which can't exist on a just-created project).
	if p.DispatchMode == "sequenced" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams,
			Message: "cannot create a project as sequenced — no roadmap exists yet; create it, run `hive plan`, then enable sequenced via edit"}
	}
	if p.DispatchMode == "manual" || p.DispatchMode == "auto_all" {
		if err := s.applyDispatchMode(ctx, proj, p.DispatchMode, "", ""); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
		}
	}
	repoPath := ""
	if proj.RepoPath != nil {
		repoPath = *proj.RepoPath
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventProjectCreated,
		Data: map[string]any{
			"project_id": proj.ID,
			"slug":       proj.Slug,
			"name":       proj.Name,
			"repo_path":  repoPath,
		},
	})
	out, _ := json.Marshal(map[string]string{"project_id": proj.ID, "slug": proj.Slug})
	return out, nil
}

func (s *RPCServer) handleListProjects(ctx context.Context) (json.RawMessage, *rpc.RPCError) {
	projs, err := s.d.store.ListProjects(ctx, "")
	if err != nil {
		return nil, internalErr(err)
	}
	views := make([]rpc.ProjectView, 0, len(projs))
	for _, p := range projs {
		v := rpc.ProjectView{
			ID:        p.ID,
			Slug:      p.Slug,
			Name:      p.Name,
			Status:    p.Status,
			CreatedAt: p.CreatedAt.Unix(),
		}
		if p.RepoPath != nil {
			v.RepoPath = *p.RepoPath
		}
		v.DispatchMode = s.d.scheduler.effectiveDispatchModeForProject(p.Slug)
		v.FeatureBranch = s.d.scheduler.effectiveFeatureBranchForProject(p.Slug)
		v.TargetBranch = s.d.scheduler.effectiveTargetBranchForProject(p.Slug)
		v.AutoIntegrate = s.d.scheduler.taskAutoIntegrateForProject(p.Slug)
		eff := s.d.effectiveConfigForProject(p.Slug)
		v.MergeMethod = eff.Integration.ResolvedMergeMethod()
		v.AutoFixCI = eff.Integration.AutoFixCI
		// CanSequence drives the edit modal's greyed "sequenced" option. p is
		// already a *store.Project (ListProjects returns []*Project), so it's the
		// per-element pointer the slice holds — no &loopvar aliasing to guard.
		v.CanSequence = s.checkEnableGate(ctx, p) == nil
		views = append(views, v)
	}
	out, _ := json.Marshal(views)
	return out, nil
}

// EditProjectParams is the params envelope for project.edit. Each
// mutable field is a pointer so a nil value means "leave unchanged"
// (UpdateProject only writes the non-nil columns).
type EditProjectParams struct {
	Slug     string  `json:"slug"`
	Name     *string `json:"name"`
	RepoPath *string `json:"repo_path"`
	Status   *string `json:"status"`

	FeatureBranch     *string `json:"feature_branch"`
	TaskAutoIntegrate *bool   `json:"task_auto_integrate"`
	MergeMethod       *string `json:"merge_method"`
	AutoFixCI         *bool   `json:"auto_fix_ci"`

	// Dispatch-mode transition (nil = leave unchanged). DispatchMode is the
	// target mode (manual|auto_all|sequenced); TargetBranch + Policy apply only
	// when transitioning to sequenced. applyDispatchMode owns the full lifecycle
	// (gate check / dispatcher row / teardown), so a bad value surfaces inline.
	DispatchMode *string `json:"dispatch_mode"`
	TargetBranch *string `json:"target_branch"`
	Policy       *string `json:"policy"`
}

func (s *RPCServer) handleEditProject(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p EditProjectParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Slug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.Slug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found"}
	}
	if err := s.d.store.UpdateProject(ctx, proj.ID, p.Name, p.RepoPath, p.Status); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	integ := map[string]any{}
	if p.FeatureBranch != nil {
		integ["feature_branch"] = *p.FeatureBranch
	}
	if p.TaskAutoIntegrate != nil {
		integ["task_auto_integrate"] = *p.TaskAutoIntegrate
	}
	if p.MergeMethod != nil {
		integ["merge_method"] = *p.MergeMethod
	}
	if p.AutoFixCI != nil {
		integ["auto_fix_ci"] = *p.AutoFixCI
	}
	if len(integ) > 0 {
		if err := config.SetProjectIntegration(s.d.cfg.HiveDir, proj.Slug, integ); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "write integration config: " + err.Error()}
		}
	}
	// Target branch ([scheduler] base) — persisted independent of dispatch mode
	// (grounding/health base + worktree-fork-base fallback). applyDispatchMode
	// also writes it on a sequenced transition (same value), but writing it here
	// makes it settable in manual/auto_all too.
	if p.TargetBranch != nil {
		if err := config.SetProjectScheduler(s.d.cfg.HiveDir, proj.Slug, map[string]any{"target_branch": *p.TargetBranch}); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "write target branch: " + err.Error()}
		}
	}
	// Dispatch-mode transition. applyDispatchMode owns the full lifecycle
	// (sequenced enable-gate + dispatcher row; manual/auto_all teardown). Surface
	// its error as ErrInvalidParams so the TUI keeps the modal open inline rather
	// than treating it as an internal failure.
	if p.DispatchMode != nil {
		target, policy := "", ""
		if p.TargetBranch != nil {
			target = *p.TargetBranch
		}
		if p.Policy != nil {
			policy = *p.Policy
		}
		if err := s.applyDispatchMode(ctx, proj, *p.DispatchMode, target, policy); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
		}
	}
	data := map[string]any{
		"project_id": proj.ID,
		"slug":       proj.Slug,
	}
	if p.Name != nil {
		data["name"] = *p.Name
	}
	if p.RepoPath != nil {
		data["repo_path"] = *p.RepoPath
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventProjectUpdated,
		Data: data,
	})
	out, _ := json.Marshal(map[string]any{"ok": true})
	return out, nil
}

func (s *RPCServer) handleArchiveProject(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Slug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.Slug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found"}
	}
	archived := "archived"
	if err := s.d.store.UpdateProject(ctx, proj.ID, nil, nil, &archived); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	out, _ := json.Marshal(map[string]any{"ok": true})
	return out, nil
}

func (s *RPCServer) handleDeleteProject(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Slug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "slug required"}
	}
	// Resolve slug → ID before the delete so we can emit project.updated
	// with the (now-gone) project_id. Best-effort: if the lookup fails we
	// fall through to the delete which returns the canonical not-found
	// error.
	projID := ""
	if proj, err := s.d.store.GetProjectBySlug(ctx, p.Slug); err == nil {
		projID = proj.ID
	}
	if err := s.d.store.DeleteProject(ctx, p.Slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found"}
		}
		// covers the running-run guard (refuses delete with active runs)
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if projID != "" {
		s.d.bus.Publish(rpc.EventMessage{
			Type: rpc.EventProjectUpdated,
			Data: map[string]any{"project_id": projID, "deleted": true},
		})
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "deleted": p.Slug})
	return out, nil
}

// HealthSnapshot is the daemon's live self-report for hive doctor.
// Compact, JSON-marshalable, no nested types. LastTickUnix is Unix
// seconds (0 = no tick yet) — chosen over time.Time for cross-package
// JSON consistency with internal/doctor's mirror struct.
type HealthSnapshot struct {
	ActiveRuns        int    `json:"active_runs"`
	PendingApprovals  int    `json:"pending_approvals"`
	MCPHTTPListenerOK bool   `json:"mcp_http_listener_ok"`
	MCPHTTPAddr       string `json:"mcp_http_addr"`
	SchemaVersionDB   int    `json:"schema_version_db"`
	UptimeSeconds     int64  `json:"uptime_seconds"`
	LastTickUnix      int64  `json:"last_tick_unix"`
}

// handleHealth returns a HealthSnapshot for hive doctor. Pending
// approvals are an in-memory daemon registry (listPending), not a
// store query — handleStatus uses the same source.
func (s *RPCServer) handleHealth(ctx context.Context) (json.RawMessage, *rpc.RPCError) {
	// Phase 4.3.1 #3: health panel surfaces every running row including
	// child fix runs (use the all-inclusive view, not the roots-only one
	// that capacity accounting uses).
	running, err := s.d.store.ListAllRunningRuns(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	schemaV, err := s.d.store.SchemaVersion(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	addr := s.d.MCPHTTPAddr()
	lastTickUnix := int64(0)
	if t := s.d.scheduler.LastTickAt(); !t.IsZero() {
		lastTickUnix = t.Unix()
	}
	snap := HealthSnapshot{
		ActiveRuns:        len(running),
		PendingApprovals:  len(s.d.listPending()),
		MCPHTTPListenerOK: addr != "",
		MCPHTTPAddr:       addr,
		SchemaVersionDB:   schemaV,
		UptimeSeconds:     int64(time.Since(s.d.StartedAt()).Seconds()),
		LastTickUnix:      lastTickUnix,
	}
	raw, _ := json.Marshal(snap)
	return raw, nil
}

func (s *RPCServer) handleStatus(ctx context.Context) (json.RawMessage, *rpc.RPCError) {
	// Phase 4.3.1 #3: status panel shows every running row including
	// child fix runs — use the all-inclusive view.
	running, err := s.d.store.ListAllRunningRuns(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	pending, err := s.d.store.ListPendingTasks(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	recent, err := s.d.store.ListRecentRuns(ctx, 5)
	if err != nil {
		return nil, internalErr(err)
	}

	runningView := make([]map[string]any, 0, len(running))
	for _, r := range running {
		entry := map[string]any{
			"id":       r.ID,
			"task_id":  r.TaskID,
			"pipeline": r.Pipeline,
		}
		if r.StartedAt != nil {
			entry["started_at"] = r.StartedAt.Unix()
		}
		// Phase 3.7: include task title + project info so the TUI
		// snapshot can render without a separate per-task fetch
		// (task.list excludes dispatched tasks).
		if task, err := s.d.store.GetTask(ctx, r.TaskID); err == nil {
			entry["task_title"] = task.Title
			if proj, err := s.d.store.GetProject(ctx, task.ProjectID); err == nil {
				entry["project_id"] = proj.ID
				entry["project_slug"] = proj.Slug
			}
		}
		runningView = append(runningView, entry)
	}
	recentView := make([]map[string]any, 0, len(recent))
	for _, r := range recent {
		entry := map[string]any{
			"id":       r.ID,
			"task_id":  r.TaskID,
			"pipeline": r.Pipeline,
			"status":   r.Status,
			"summary":  r.Summary,
		}
		if r.EndedAt != nil {
			entry["ended_at"] = r.EndedAt.Unix()
		}
		// Enrich with task title + project (like the running view) so a
		// (re)subscribing TUI can hydrate the task — otherwise a recent
		// needs_attention run (e.g. one recovered after a daemon restart)
		// never surfaces in the Projects needs-attention lane.
		if task, err := s.d.store.GetTask(ctx, r.TaskID); err == nil {
			entry["task_title"] = task.Title
			if proj, err := s.d.store.GetProject(ctx, task.ProjectID); err == nil {
				entry["project_id"] = proj.ID
				entry["project_slug"] = proj.Slug
			}
		}
		recentView = append(recentView, entry)
	}

	doneTasks, err := s.d.store.ListRecentDoneTasks(ctx, 10)
	if err != nil {
		return nil, internalErr(err)
	}
	recentDoneView := make([]map[string]any, 0, len(doneTasks))
	for _, r := range doneTasks {
		entry := map[string]any{
			"id":       r.ID,
			"task_id":  r.TaskID,
			"pipeline": r.Pipeline,
			"status":   r.Status,
			"summary":  r.Summary,
		}
		if r.EndedAt != nil {
			entry["ended_at"] = r.EndedAt.Unix()
		}
		if task, err := s.d.store.GetTask(ctx, r.TaskID); err == nil {
			entry["task_title"] = task.Title
			if proj, err := s.d.store.GetProject(ctx, task.ProjectID); err == nil {
				entry["project_id"] = proj.ID
				entry["project_slug"] = proj.Slug
			}
		}
		recentDoneView = append(recentDoneView, entry)
	}

	pendingList := s.d.listPending()

	payload := map[string]any{
		"running_runs":           len(running),
		"pending_tasks":          len(pending),
		"pending_approvals":      len(pendingList),
		"pending_approvals_list": pendingList,
		"adapter":                s.d.adp.Name(),
		"running":                runningView,
		"recent":                 recentView,
		"recent_done":            recentDoneView,
	}
	raw, _ := json.Marshal(payload)
	return raw, nil
}

// AddTaskParams is the params envelope for task.add. Field names mirror
// what a CLI / TUI client would send. Tagged so JSON unmarshal is stable
// regardless of struct field ordering.
//
// Metadata is an opaque map persisted on the Task row (already supported
// by store.InsertTask). Phase 8.B uses it to link roadmap-decomposed
// tasks back to their source phase (roadmap_phase / roadmap_path /
// spec_path keys). Values are stringly-typed at the wire level; callers
// that need richer types can JSON-encode at the source.
type AddTaskParams struct {
	ProjectSlug string            `json:"project_slug"`
	Title       string            `json:"title"`
	Body        string            `json:"body,omitempty"`
	Priority    string            `json:"priority,omitempty"`
	Pipeline    string            `json:"pipeline,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// insertProjectTask inserts a task for a project, mirrors it to Linear when the
// project is write-back bound (best-effort; stays Hive-local on failure), and
// publishes task.created. Shared by handleAddTask and handleRoadmapDecomposeApply.
// metadata is the opaque task.metadata map (already map[string]any).
func (s *RPCServer) insertProjectTask(ctx context.Context, proj *store.Project, title, body, priority, pipeline string, metadata map[string]any) (*store.Task, error) {
	task := &store.Task{
		ID:        newID("task"),
		ProjectID: proj.ID,
		Source:    "inbox",
		Title:     title,
		Body:      body,
		Priority:  defaultStr(priority, "P3"),
		Status:    "pending",
		Pipeline:  pipeline,
		Metadata:  metadata,
	}
	// Linear write-back: mirror Hive-originated tasks (source_id=="") into the
	// bound Linear project as an issue, best-effort. On any failure we still
	// insert the task Hive-local (source_id="") — the reconciler backfills it
	// later. Never blocks task creation. (Phase 1)
	if s.d.linearWriter != nil && task.SourceID == "" {
		if teamKey, projectID, ok := linearWriteTarget(proj); ok {
			ms := milestoneForTask(proj, task.Metadata)
			issueID, identifier, _, err := s.d.linearWriter.CreateIssue(ctx, teamKey, projectID, task.Title, task.Body, ms)
			if err != nil {
				log.Printf("linear write-back: mirror task %s failed: %v (inserting Hive-local)", task.ID, err)
			} else {
				task.Source = "linear"
				task.SourceID = issueID
				if task.Metadata == nil {
					task.Metadata = map[string]any{}
				}
				task.Metadata["external_id"] = identifier
				task.LinearSyncedState = "todo"
			}
		}
	}
	if err := s.d.store.InsertTask(ctx, task); err != nil {
		return nil, err
	}
	// Phase 3.7.1c: emit task.created so subscribers (TUI snapshot)
	// see new tasks without waiting for the next initial-state fetch.
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventTaskCreated,
		Data: map[string]any{
			"task_id":             task.ID,
			"project_id":          proj.ID,
			"project_slug":        proj.Slug,
			"title":               task.Title,
			"status":              task.Status,
			"pipeline":            task.Pipeline,
			"roadmap_phase":       metaString(task.Metadata, "roadmap_phase"),
			"roadmap_phase_index": metaString(task.Metadata, "roadmap_phase_index"),
		},
	})
	return task, nil
}

func (s *RPCServer) handleAddTask(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p AddTaskParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Title == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "title required"}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	pipelineName := defaultStr(p.Pipeline, "build")
	if !s.d.HasPipeline(pipelineName) {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "unknown pipeline: " + pipelineName}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
		}
		return nil, internalErr(err)
	}
	// Thread caller-supplied metadata (Phase 8.B roadmap-decomposed tasks
	// stamp roadmap_phase / roadmap_path / spec_path here). The wire
	// shape is map[string]string but the store column is map[string]any
	// — lift each value through `any` so they round-trip identically.
	var meta map[string]any
	if len(p.Metadata) > 0 {
		meta = make(map[string]any, len(p.Metadata))
		for k, v := range p.Metadata {
			meta[k] = v
		}
	}
	task, err := s.insertProjectTask(ctx, proj, p.Title, p.Body, p.Priority, pipelineName, meta)
	if err != nil {
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(map[string]string{"task_id": task.ID})
	return out, nil
}

// RunNowParams is the params envelope for run.now.
type RunNowParams struct {
	TaskID string `json:"task_id"`
}

func (s *RPCServer) handleRunNow(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p RunNowParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.TaskID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "task_id required"}
	}
	runID, err := s.d.scheduler.RunNow(ctx, p.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrTaskNotFound, Message: "task not found"}
		}
		if errors.Is(err, ErrTaskNotPending) {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "task is not pending (already running, done, or abandoned)"}
		}
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(map[string]string{"run_id": runID})
	return out, nil
}

// ResolveNowParams is the params envelope for resolve.now.
type ResolveNowParams struct {
	TaskID string `json:"task_id"`
}

// handleResolveNow is the MANUAL conflict-resolver trigger (`hive resolve
// <task>`). It loads the task + project, then launches the manual resolve
// dispatcher — which provisions a fresh worktree on the task's PR branch, runs
// the resolve pipeline, and tears the worktree down — on a tracked goroutine.
// The resolve loop drives a bounded Claude subprocess and can run for minutes,
// so we return immediately (the CLI's RPC read deadline is short); progress
// surfaces via the normal run.started/run.ended events.
func (s *RPCServer) handleResolveNow(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ResolveNowParams
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
	if task.Status == "running" || task.Status == "done" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "task " + task.ID + " is " + task.Status + "; resolve only applies to a stuck (needs_attention/awaiting-merge) task"}
	}
	if task.GateState == sequence.GateMergeFailed {
		return nil, &rpc.RPCError{
			Code:    rpc.ErrInvalidParams,
			Message: "task " + task.ID + " is parked at merge_failed (gave up merging); run `hive merge retry " + task.ID + "` to re-arm",
		}
	}
	proj, err := s.d.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found for task"}
		}
		return nil, internalErr(err)
	}

	// Merge-queue interlock: a manual `hive resolve` must not race the auto merge
	// queue. The auto worker (runQueuedMerge) holds the per-feature-branch
	// mergeGuard while it merges/resolves; acquire it HERE (the RPC entry) so the
	// two paths are serialized. We acquire at this entry and NOT inside
	// dispatchResolveRunManual on purpose: runQueuedMerge is the other caller of
	// dispatchResolveRunManual and ALREADY holds the guard, so acquiring it inside
	// the shared callee would self-deadlock the worker. The worker path therefore
	// stays guard-free here; only this RPC entry acquires.
	branch := s.d.scheduler.effectiveWorktreeBaseForProject(proj.Slug)
	guardKey := mergeGuardKey(proj.Slug, branch)
	if branch != "" {
		if !s.d.mergeGuard.tryAcquire(guardKey) {
			// A queue merge (or another manual resolve) is in flight for this
			// (project, branch). Do NOT provision a worktree / dispatch — bail out.
			return nil, &rpc.RPCError{
				Code:    rpc.ErrInvalidParams,
				Message: "a merge is already in progress for " + branch + "; retry shortly",
			}
		}
	}

	s.d.goTracked(func() {
		// dispatchResolveRunManual is synchronous (provisions the worktree, runs
		// resolve + reMergeAfterResolve, tears down), so this defer brackets the
		// whole resolve and the branch guard is held until it terminates. When
		// branch == "" we acquired nothing, so release is a harmless no-op.
		if branch != "" {
			defer s.d.mergeGuard.release(guardKey)
		}
		if derr := s.d.scheduler.dispatchResolveRunManual(s.d.ctx, task, proj); derr != nil {
			log.Printf("resolve.now: manual resolve for task %s failed: %v", task.ID, derr)
		}
	})

	out, _ := json.Marshal(map[string]any{"ok": true, "task_id": task.ID})
	return out, nil
}

// RunResumeParams is the params envelope for the run.resume RPC.
type RunResumeParams struct {
	RunID string `json:"run_id"`
}

// handleRunResume re-launches the pipeline for an existing run via
// Scheduler.Resume. The dispatch is identical to hive_resume's chat-
// tool path; this RPC exposes it for external callers (CLI / future
// remote clients).
func (s *RPCServer) handleRunResume(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p RunResumeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.RunID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run_id required"}
	}
	if err := s.d.scheduler.Resume(ctx, p.RunID); err != nil {
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "run_id": p.RunID})
	return out, nil
}

func (s *RPCServer) handleListTasks(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	// Optional "statuses" filter. Default (absent/empty) = pending only — the
	// historical behavior the CLI `hive task list` + the roadmap phase-guard
	// rely on. The TUI passes ["pending","needs_attention"] so its initial state
	// seeds the needs-attention lane (a task that failed before the TUI
	// subscribed otherwise never enters the snapshot).
	var p struct {
		Statuses []string `json:"statuses"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	var tasks []*store.Task
	var err error
	if len(p.Statuses) > 0 {
		tasks, err = s.d.store.ListTasksByStatuses(ctx, p.Statuses)
	} else {
		tasks, err = s.d.store.ListPendingTasks(ctx)
	}
	if err != nil {
		return nil, internalErr(err)
	}
	views := make([]rpc.TaskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, taskToView(t))
	}
	out, _ := json.Marshal(views)
	return out, nil
}

// GetRunParams is the params envelope for run.get.
type GetRunParams struct {
	RunID string `json:"run_id"`
}

func (s *RPCServer) handleGetRun(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p GetRunParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.RunID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run_id required"}
	}
	r, err := s.d.store.GetRun(ctx, p.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrRunNotFound, Message: "run not found"}
		}
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(runToView(r))
	return out, nil
}

func internalErr(err error) *rpc.RPCError {
	return &rpc.RPCError{Code: rpc.ErrInternal, Message: err.Error()}
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// taskToView converts the store-shaped Task row into its RPC projection.
// PredictConfidence is *int on the view (so omitempty can distinguish
// "no confidence reported" from a real 0) but int on the store row;
// we lift it via a local copy.
func taskToView(t *store.Task) rpc.TaskView {
	conf := t.PredictConfidence
	return rpc.TaskView{
		ID:                t.ID,
		ProjectID:         t.ProjectID,
		Source:            t.Source,
		SourceID:          t.SourceID,
		Title:             t.Title,
		Body:              t.Body,
		Priority:          t.Priority,
		Status:            rpc.TaskStatus(t.Status),
		Pipeline:          t.Pipeline,
		PredictedFiles:    t.PredictedFiles,
		ConflictSet:       t.ConflictSet,
		PredictConfidence: &conf,
		Metadata:          t.Metadata,
		CreatedAt:         t.CreatedAt,
		UpdatedAt:         t.UpdatedAt,
	}
}

// runToView converts the store-shaped Run row into its RPC projection.
// store.Run.StartedAt is *time.Time (NULL when the run hasn't started);
// rpc.RunView.StartedAt is time.Time (non-pointer), so we dereference
// when present and leave the zero value otherwise.
func runToView(r *store.Run) rpc.RunView {
	v := rpc.RunView{
		ID:                      r.ID,
		TaskID:                  r.TaskID,
		ProjectID:               r.ProjectID,
		Pipeline:                r.Pipeline,
		Status:                  rpc.RunStatus(r.Status),
		TotalCostUSD:            r.TotalCostUSD,
		Summary:                 r.Summary,
		DocumentationSkipped:    r.DocumentationSkipped,
		DocumentationSkipReason: r.DocumentationSkipReason,
	}
	if r.StartedAt != nil {
		v.StartedAt = *r.StartedAt
	}
	if r.EndedAt != nil {
		v.EndedAt = r.EndedAt
	}
	return v
}

// ApprovalEvaluateParams is the request from the hive_permission_check
// MCP tool (via the daemon socket) for a single tool-use decision.
type ApprovalEvaluateParams struct {
	RunID    string         `json:"run_id"`
	Stage    string         `json:"stage"`
	ToolName string         `json:"tool_name"`
	Input    map[string]any `json:"input"`
}

func (s *RPCServer) handleApprovalEvaluate(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ApprovalEvaluateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	req := approval.ToolUseRequest{
		RunID: p.RunID, Stage: p.Stage, ToolName: p.ToolName, ToolInput: p.Input,
	}
	// Resolve project slug for project-scoped rules (best-effort).
	if r, err := s.d.store.GetRun(ctx, p.RunID); err == nil {
		if t, err := s.d.store.GetTask(ctx, r.TaskID); err == nil {
			if proj, err := s.d.store.GetProject(ctx, t.ProjectID); err == nil {
				req.Project = proj.Slug
			}
		}
	}
	requested := time.Now().Unix()
	dec, _ := s.d.approver.Evaluate(ctx, req) // engine is fail-closed internally; never errors

	// Phase 4.6 ask mode: an unmatched tool (fail_closed) doesn't deny
	// outright — it becomes a pending approval the operator resolves live
	// in the TUI. Explicit allow/deny rules still resolve instantly.
	if dec.RuleID == "fail_closed" && s.d.cfg.Cfg.Approvals.Mode != "deny" {
		approvalID := fmt.Sprintf("ap-%d", time.Now().UnixNano())
		ch := s.d.registerPending(approvalID, p.RunID, p.Stage, p.ToolName, p.Input)
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventApprovalRequested, Data: map[string]any{
			"approval_id": approvalID, "run_id": p.RunID, "stage": p.Stage,
			"tool_name": p.ToolName, "tool_input": p.Input,
		}})
		timeout := time.Duration(s.d.cfg.Cfg.Approvals.HookTimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 300 * time.Second
		}
		select {
		case resolved := <-ch:
			dec = resolved
		case <-time.After(timeout):
			s.d.clearPending(approvalID)
			dec = approval.Decision{Kind: approval.DecisionDeny, Reason: "approval timeout (fail-closed)", RuleID: "timeout"}
		case <-ctx.Done():
			s.d.clearPending(approvalID)
			dec = approval.Decision{Kind: approval.DecisionDeny, Reason: "request cancelled", RuleID: "cancelled"}
		}
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventApprovalResolved, Data: map[string]any{
			"approval_id": approvalID, "decision": string(dec.Kind), "reason": dec.Reason,
		}})
	}

	inputJSON, _ := json.Marshal(p.Input)
	_ = s.d.store.InsertApproval(ctx, store.ApprovalAudit{
		RunID: p.RunID, Stage: p.Stage, ToolName: p.ToolName, ToolInputJSON: string(inputJSON),
		Decision: string(dec.Kind), ResolvedBy: dec.RuleID, Reason: dec.Reason,
		RequestedAt: requested, ResolvedAt: time.Now().Unix(),
	})
	// Live per-tool activity for the Events tab + drill-in tail (stage
	// events alone are sparse while a worker grinds through a stage).
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventToolDecision, Data: map[string]any{
		"run_id": p.RunID, "stage": p.Stage, "tool_name": p.ToolName,
		"decision": string(dec.Kind), "arg": approvalArgPreview(p.ToolName, p.Input),
	}})
	out, _ := json.Marshal(map[string]any{"decision": string(dec.Kind), "reason": dec.Reason})
	return out, nil
}

// approvalArgPreview returns a short display arg (Bash command / file
// path) for the tool.decision event.
func approvalArgPreview(toolName string, in map[string]any) string {
	var s string
	switch toolName {
	case "Bash":
		s, _ = in["command"].(string)
	case "Edit", "Write", "Read", "MultiEdit":
		s, _ = in["file_path"].(string)
	}
	if len(s) > 60 {
		s = s[:59] + "…"
	}
	return s
}

// handleApprovalPending lists in-flight pending approvals (ask mode) so
// the CLI can resolve them headlessly without the TUI.
func (s *RPCServer) handleApprovalPending(_ context.Context) (json.RawMessage, *rpc.RPCError) {
	out, _ := json.Marshal(s.d.listPending())
	return out, nil
}

// ApprovalResolveParams resolves a pending approval (from the TUI).
type ApprovalResolveParams struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"` // "approve" | "deny"
	Remember   bool   `json:"remember,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ArgMatcher string `json:"arg_matcher,omitempty"`
}

func (s *RPCServer) handleApprovalResolve(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ApprovalResolveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	kind := approval.DecisionApprove
	if p.Decision == "deny" {
		kind = approval.DecisionDeny
	}
	if p.Remember && p.ToolName != "" {
		ruleDecision := "allow"
		if kind == approval.DecisionDeny {
			ruleDecision = "deny"
		}
		_, _ = s.d.store.InsertApprovalRule(ctx, store.ApprovalRule{
			Scope: "global", ToolName: p.ToolName, ArgMatcher: p.ArgMatcher, Decision: ruleDecision, Source: "user",
		})
	}
	found := s.d.resolvePending(p.ApprovalID, approval.Decision{Kind: kind, Reason: "resolved by operator", RuleID: "operator"})
	out, _ := json.Marshal(map[string]any{"resolved": found})
	return out, nil
}

// ApprovalListParams optionally filters the audit by run.
type ApprovalListParams struct {
	RunID string `json:"run_id,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func (s *RPCServer) handleApprovalList(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ApprovalListParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	rows, err := s.d.store.ListApprovals(ctx, p.RunID, p.Limit)
	if err != nil {
		return nil, internalErr(err)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, map[string]any{
			"run_id": a.RunID, "stage": a.Stage, "tool_name": a.ToolName,
			"decision": a.Decision, "resolved_by": a.ResolvedBy, "reason": a.Reason,
			"tool_input": a.ToolInputJSON, "resolved_at": a.ResolvedAt,
		})
	}
	raw, _ := json.Marshal(out)
	return raw, nil
}

// ApprovalRuleAddParams adds an allow/deny rule.
type ApprovalRuleAddParams struct {
	Scope      string `json:"scope"`
	ToolName   string `json:"tool_name"`
	ArgMatcher string `json:"arg_matcher,omitempty"`
	Decision   string `json:"decision"`
}

func (s *RPCServer) handleApprovalRuleAdd(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ApprovalRuleAddParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ToolName == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "tool_name required"}
	}
	if p.Decision != "allow" && p.Decision != "deny" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "decision must be allow or deny"}
	}
	if p.Scope == "" {
		p.Scope = "global"
	}
	id, err := s.d.store.InsertApprovalRule(ctx, store.ApprovalRule{
		Scope: p.Scope, ToolName: p.ToolName, ArgMatcher: p.ArgMatcher, Decision: p.Decision, Source: "user",
	})
	if err != nil {
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(map[string]any{"rule_id": id})
	return out, nil
}

// SourcesSyncParams optionally restricts the sync to a single source.
type SourcesSyncParams struct {
	Source string `json:"source"`
}

func (s *RPCServer) handleSourcesSync(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p SourcesSyncParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
		}
	}
	rep := s.d.Sync(ctx, p.Source)
	out, _ := json.Marshal(rep)
	return out, nil
}

// SourcesBindParams binds a source to a project with a source-specific binding.
type SourcesBindParams struct {
	Slug    string         `json:"slug"`
	Source  string         `json:"source"`
	Binding map[string]any `json:"binding"`
}

func (s *RPCServer) handleSourcesBind(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p SourcesBindParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Slug == "" || p.Source == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "slug and source required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.Slug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found"}
	}
	if proj.Sources == nil {
		proj.Sources = map[string]any{}
	}
	binding := p.Binding
	if binding == nil {
		binding = map[string]any{}
	}
	// Linear write-back: reject an ambiguous create target so the operator
	// fixes it now rather than at first mirror. (Phase 1)
	if p.Source == "linear" {
		var lb struct {
			Teams     []string `json:"teams"`
			Projects  []string `json:"projects"`
			WriteBack bool     `json:"write_back"`
			WBTeam    string   `json:"wb_team"`
			WBProject string   `json:"wb_project"`
		}
		bj, _ := json.Marshal(binding)
		_ = json.Unmarshal(bj, &lb)
		if lb.WriteBack {
			if lb.WBTeam == "" && len(lb.Teams) != 1 {
				return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "write_back needs exactly one --team or an explicit --wb-team"}
			}
			if lb.WBProject == "" && len(lb.Projects) != 1 {
				return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "write_back needs exactly one --project or an explicit --wb-project"}
			}
			// The write target MUST be inside the read (ingest) filter, or every
			// mirrored issue would be fetched-absent on the next Linear poll and
			// auto-closed by Reconcile. Assert membership when an explicit target
			// is given (the single-team/single-project default is in-window by
			// construction).
			if lb.WBTeam != "" && !slices.Contains(lb.Teams, lb.WBTeam) {
				return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "wb_team must be one of the bound --team values"}
			}
			if lb.WBProject != "" && !slices.Contains(lb.Projects, lb.WBProject) {
				return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "wb_project must be one of the bound --project values"}
			}
		}
	}
	proj.Sources[p.Source] = binding
	if err := s.d.store.UpdateProjectSources(ctx, proj.ID, proj.Sources); err != nil {
		return nil, internalErr(err)
	}
	if p.Source == "inbox" {
		// 0o700 to match the rest of the ~/.hive tree.
		if err := os.MkdirAll(filepath.Join(s.d.HiveDir(), "inbox", p.Slug), 0o700); err != nil {
			return nil, internalErr(err)
		}
	}
	out, _ := json.Marshal(map[string]any{"ok": true})
	return out, nil
}

// SourcesListParams selects a project to list bound sources for.
type SourcesListParams struct {
	Slug string `json:"slug"`
}

func (s *RPCServer) handleSourcesList(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p SourcesListParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Slug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.Slug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found"}
	}
	out, _ := json.Marshal(proj.Sources)
	return out, nil
}

// SourcesUnbindParams removes a bound source from a project.
type SourcesUnbindParams struct {
	Slug   string `json:"slug"`
	Source string `json:"source"`
}

func (s *RPCServer) handleSourcesUnbind(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p SourcesUnbindParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Slug == "" || p.Source == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "slug and source required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.Slug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found"}
	}
	delete(proj.Sources, p.Source)
	if err := s.d.store.UpdateProjectSources(ctx, proj.ID, proj.Sources); err != nil {
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(map[string]any{"ok": true})
	return out, nil
}

func (s *RPCServer) handleSourcesStatus(_ context.Context, _ json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	out, _ := json.Marshal(s.d.SourceStatus())
	return out, nil
}

// DecomposeParams is the params envelope for task.decompose.
type DecomposeParams struct {
	TaskID      string `json:"task_id"`
	MaxSubtasks int    `json:"max_subtasks,omitempty"`
}

// DecomposeResult is the read-side proposal returned to the CLI/chat.
type DecomposeResult struct {
	Subtasks     []decompose.ProposedSubtask `json:"subtasks"`
	Model        string                      `json:"model"`
	CostUSD      float64                     `json:"cost_usd"`
	InputTokens  int                         `json:"input_tokens"`
	OutputTokens int                         `json:"output_tokens"`
}

// DecomposeApplyParams is the params envelope for task.decompose_apply.
type DecomposeApplyParams struct {
	ParentTaskID string                      `json:"parent_task_id"`
	Subtasks     []decompose.ProposedSubtask `json:"subtasks"`
}

// DecomposeApplyResult is the write-side result.
type DecomposeApplyResult struct {
	InsertedTaskIDs []string `json:"inserted_task_ids"`
}

// handleDecompose runs the read-side decomposition: validates the task,
// invokes decompose.Decompose against the wired Runner, and returns the
// validated proposal + cost + token counts. Pure read; only side effect
// is the LLM call.
func (s *RPCServer) handleDecompose(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p DecomposeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.TaskID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "task_id required"}
	}
	if s.d.decomposeRunner == nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "decompose runner not configured (ANTHROPIC_API_KEY not set?)"}
	}
	task, err := s.d.store.GetTask(ctx, p.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrTaskNotFound, Message: "task not found"}
		}
		return nil, internalErr(err)
	}
	project, err := s.d.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return nil, internalErr(err)
	}
	repoPath := ""
	if project.RepoPath != nil {
		repoPath = *project.RepoPath
	}
	codebaseContext := codeintel.BuildContext(ctx, s.d.plannerGrounderFor(project.Slug, repoPath), task.Title+"\n"+task.Body)

	// nil: single-task decompose has no project-wide existing-work context
	res, err := decompose.Decompose(ctx, s.d.decomposeRunner, *task, *project, p.MaxSubtasks, s.d.decomposeStackHint(project.Slug), nil, codebaseContext)
	if err != nil {
		return nil, internalErr(err)
	}
	out := DecomposeResult{
		Subtasks:     res.Subtasks,
		Model:        res.Model,
		CostUSD:      res.CostUSD,
		InputTokens:  res.InputTokens,
		OutputTokens: res.OutputTokens,
	}
	raw, _ := json.Marshal(out)
	return raw, nil
}

// handleDecomposeApply is the write-side: re-validates the subtask
// payload (defense against client tampering), transactionally inserts
// the children, and publishes one task.created event per inserted row.
func (s *RPCServer) handleDecomposeApply(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p DecomposeApplyParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ParentTaskID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "parent_task_id required"}
	}
	if len(p.Subtasks) == 0 {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "at least one subtask required"}
	}

	// Defense-in-depth re-validation: enforce the same rules decompose
	// already ran (title length cap, max-count ceiling, dedup, priority/
	// pipeline enums), in case the client tampered with the payload
	// between the propose call and apply call. HardMaxSubtasks is the
	// absolute ceiling — the per-call cap was already applied at propose
	// time; apply only enforces the bound a valid propose could produce.
	clean, vErr := decompose.Validate(p.Subtasks, decompose.HardMaxSubtasks)
	if vErr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: vErr.Error()}
	}
	items := make([]store.SubtaskInput, 0, len(clean))
	for _, st := range clean {
		items = append(items, store.SubtaskInput{
			Title: st.Title, Body: st.Body, Priority: st.Priority, Pipeline: st.Pipeline,
		})
	}

	ids, err := s.d.store.InsertSubtasks(ctx, p.ParentTaskID, items)
	if err != nil {
		// Distinguish parent-not-found from other store errors. The
		// store wraps the sentinel via fmt.Errorf("get parent: %w", err),
		// so errors.Is unwraps it correctly.
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrTaskNotFound, Message: err.Error()}
		}
		return nil, internalErr(err)
	}

	// Persist per-subtask metadata that SubtaskInput doesn't carry: depends_on
	// (backward indices → the just-inserted IDs) and relevant_files. ids are in
	// insertion order, matching clean.
	for i, id := range ids {
		if i >= len(clean) {
			break
		}
		st := clean[i]
		kv := map[string]string{}
		if len(st.DependsOn) > 0 {
			var depIDs []string
			for _, di := range st.DependsOn {
				// di < i guards backward refs; ids[di] != id guards the rare
				// case where Validate's title-dedup shifted indices so a dep
				// resolves to the task itself (would otherwise block it forever).
				if di >= 0 && di < i && ids[di] != "" && ids[di] != id {
					depIDs = append(depIDs, ids[di])
				}
			}
			if len(depIDs) > 0 {
				kv["depends_on"] = strings.Join(depIDs, ",")
			}
		}
		if len(st.RelevantFiles) > 0 {
			kv["relevant_files"] = strings.Join(st.RelevantFiles, ",")
		}
		if len(kv) > 0 {
			if merr := s.d.store.MergeTaskMetadata(ctx, id, kv); merr != nil {
				log.Printf("decompose_apply: metadata for %s: %v", id, merr)
			}
		}
	}

	// Look up the parent's project once so we can attach project_slug to
	// the task.created event payload (matching handleAddTask's shape).
	// All children share the parent's project, so this is a single lookup.
	var projectSlug string
	if parent, gerr := s.d.store.GetTask(ctx, p.ParentTaskID); gerr == nil {
		if proj, perr := s.d.store.GetProject(ctx, parent.ProjectID); perr == nil {
			projectSlug = proj.Slug
		}
	}

	// Publish task.created per inserted row so TUI snapshots see the
	// new children without a re-subscribe roundtrip.
	for _, id := range ids {
		if t, gerr := s.d.store.GetTask(ctx, id); gerr == nil {
			s.d.bus.Publish(rpc.EventMessage{
				Type: rpc.EventTaskCreated,
				Data: map[string]any{
					"task_id":      t.ID,
					"project_id":   t.ProjectID,
					"project_slug": projectSlug,
					"title":        t.Title,
					"status":       t.Status,
					"pipeline":     t.Pipeline,
				},
			})
		}
	}

	raw, _ := json.Marshal(DecomposeApplyResult{InsertedTaskIDs: ids})
	return raw, nil
}

// TaskFinishParams is the params envelope for task.finish.
type TaskFinishParams struct {
	TaskID string `json:"task_id"`
}

func (s *RPCServer) handleTaskFinish(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p TaskFinishParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.TaskID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "task_id required"}
	}
	task, err := s.d.store.GetTask(ctx, p.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrTaskNotFound, Message: "task not found: " + p.TaskID}
		}
		return nil, internalErr(err)
	}
	run, err := s.d.store.LatestDoneBuildRunForTask(ctx, task.ID)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: err.Error()}
	}
	if run == nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "no completed build run to finish for task " + task.ID}
	}
	if run.BranchName == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "build run " + run.ID + " has no recorded branch"}
	}
	worktreePath := filepath.Join(s.d.HiveDir(), "worktrees", run.ID)
	if _, serr := os.Stat(worktreePath); serr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "run worktree was reclaimed; re-run the build before finishing (" + worktreePath + ")"}
	}
	proj, err := s.d.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "load project failed"}
	}
	if s.d.scheduler.effectiveFeatureBranchForProject(proj.Slug) == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project has no feature_branch configured (set [integration].feature_branch)"}
	}
	s.d.scheduler.chainFinishBranch(run, task, proj, worktreePath, run.BranchName)
	out, _ := json.Marshal(map[string]any{"task_id": task.ID, "run_id": run.ID, "finishing": true})
	return out, nil
}
