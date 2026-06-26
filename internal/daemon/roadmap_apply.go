package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// RoadmapDecomposeApplyParams is the write-side of roadmap decompose. The CLI/TUI
// pass the approved proposals (each with optional merge_from) + the phase
// linkage; the daemon owns insertion AND the merge branches so Linear write-back
// stays server-side.
type RoadmapDecomposeApplyParams struct {
	ProjectSlug string                      `json:"project_slug"`
	RoadmapPath string                      `json:"roadmap_path"`
	Phase       string                      `json:"phase"`
	PhaseTitle  string                      `json:"phase_title"`
	SpecPath    string                      `json:"spec_path,omitempty"`
	Subtasks    []decompose.ProposedSubtask `json:"subtasks"`
}

// RoadmapDecomposeApplyResult summarises what the handler did.
type RoadmapDecomposeApplyResult struct {
	Inserted int      `json:"inserted"`
	Merged   int      `json:"merged"`
	Pulled   int      `json:"pulled"`
	Errors   []string `json:"errors,omitempty"`
}

// handleRoadmapDecomposeApply is the write side of roadmap.decompose_apply.
// It owns insertion + the three merge branches (new / rewrite-hive-task /
// pull-linear-issue), pushing merged content back to Linear when write-back
// is bound. Best-effort throughout: a Linear push failure logs but never
// fails the Hive side of the operation.
func (s *RPCServer) handleRoadmapDecomposeApply(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p RoadmapDecomposeApplyParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" || p.Phase == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug and phase required"}
	}
	if len(p.Subtasks) == 0 {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "at least one subtask required"}
	}

	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
	}

	// Build a map of already-pulled Linear issues (source_id → task) so that
	// a linear:<uuid> ref whose issue is already mirrored gets rerouted to the
	// hive-merge path instead of creating a duplicate.
	linTasks := map[string]*store.Task{}
	if existing, lerr := s.d.store.ListTasksBySource(ctx, proj.ID, "linear"); lerr == nil {
		for i := range existing {
			linTasks[existing[i].SourceID] = &existing[i]
		}
	}

	teamKey, _, writeBack := linearWriteTarget(proj)
	total := len(p.Subtasks)

	var res RoadmapDecomposeApplyResult
	// idxToID maps each subtask's 0-based proposal index to the task ID it
	// resolved to (new insert, hive-merge target, or linear-pull). Used to
	// translate depends_on indices into concrete task IDs stamped into meta.
	// Validate guarantees deps are backward refs, so idxToID[di] is always
	// populated by the time a dependent task is processed. Empty = a branch
	// couldn't determine an ID; deps referencing it are skipped (best-effort).
	idxToID := make([]string, len(p.Subtasks))
	for i, st := range p.Subtasks {
		meta := map[string]any{
			"roadmap_phase":       p.Phase,
			"roadmap_path":        p.RoadmapPath,
			"roadmap_phase_index": strconv.Itoa(i + 1),
			"roadmap_phase_total": strconv.Itoa(total),
		}
		if p.SpecPath != "" {
			meta["spec_path"] = p.SpecPath
		}
		// Resolve depends_on (0-based proposal indices) → concrete task IDs.
		if len(st.DependsOn) > 0 {
			var depIDs []string
			for _, di := range st.DependsOn {
				if di >= 0 && di < i && idxToID[di] != "" {
					depIDs = append(depIDs, idxToID[di])
				}
			}
			if len(depIDs) > 0 {
				// Store as a comma-joined STRING (not []string): a dependent task
				// that is itself a merge target has its metadata flattened by
				// MergeTaskMetadata (%v), which would turn a []string into the
				// un-parseable "[id1 id2]". A string survives flattening intact.
				meta["depends_on"] = strings.Join(depIDs, ",")
			}
		}
		if len(st.RelevantFiles) > 0 {
			meta["relevant_files"] = strings.Join(st.RelevantFiles, ",")
		}
		mf := strings.TrimSpace(st.MergeFrom)

		switch {
		case mf == "":
			// Brand-new task: insert + mirror to Linear.
			task, ierr := s.insertProjectTask(ctx, proj, st.Title, st.Body, st.Priority, defaultStr(st.Pipeline, "build"), meta)
			if ierr != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("insert %q: %v", st.Title, ierr))
				continue
			}
			idxToID[i] = task.ID
			res.Inserted++

		case strings.HasPrefix(mf, "hive:"):
			taskID := strings.TrimPrefix(mf, "hive:")
			s.applyHiveMerge(ctx, proj, teamKey, writeBack, taskID, st, meta, &res)
			// The hive-merge target is the existing task taskID itself.
			idxToID[i] = taskID

		case strings.HasPrefix(mf, "linear:"):
			uuid := strings.TrimPrefix(mf, "linear:")
			if existing, ok := linTasks[uuid]; ok {
				// Already pulled: reroute to hive-merge so we don't duplicate.
				s.applyHiveMerge(ctx, proj, teamKey, writeBack, existing.ID, st, meta, &res)
				idxToID[i] = existing.ID
				continue
			}
			idxToID[i] = s.applyLinearPull(ctx, proj, teamKey, writeBack, uuid, st, meta, &res)

		default:
			res.Errors = append(res.Errors, fmt.Sprintf("subtask %d: bad merge_from %q", i, mf))
		}
	}

	raw, _ := json.Marshal(res)
	return raw, nil
}

// applyHiveMerge rewrites an existing Hive task's title+body, stamps phase
// metadata, and pushes the merged content to Linear when the task is mirrored.
func (s *RPCServer) applyHiveMerge(ctx context.Context, proj *store.Project, teamKey string, writeBack bool, taskID string, st decompose.ProposedSubtask, meta map[string]any, res *RoadmapDecomposeApplyResult) {
	task, err := s.d.store.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			res.Errors = append(res.Errors, fmt.Sprintf("merge hive:%s: task gone, skipped", taskID))
			return
		}
		res.Errors = append(res.Errors, fmt.Sprintf("merge hive:%s: %v", taskID, err))
		return
	}
	// Stamp phase metadata FIRST: if this fails the task is left fully un-rewritten
	// (visible state unchanged), rather than rewritten-but-unlinked.
	metaStr := make(map[string]string, len(meta))
	for k, v := range meta {
		metaStr[k] = fmt.Sprintf("%v", v)
	}
	// Backfill Linear's canonical branch_name (+ external_id) when the existing
	// task lacks it. applyLinearPull stamps these at creation, but a task that
	// reaches THIS merge path without branch_name has its worktree fall back to
	// hive/run-<id>/<title> instead of Linear's rohilrshah/hba-NN branch (the
	// exact naming drift seen on the Phase-4 decompose tasks). Best-effort: a
	// fetch failure leaves the task unchanged (prior behavior).
	if task.Source == "linear" && task.SourceID != "" && s.d.linearWriter != nil {
		if _, hasBranch := task.Metadata["branch_name"]; !hasBranch {
			if ident, branch, ferr := s.d.linearWriter.FetchIssueMeta(ctx, task.SourceID); ferr != nil {
				log.Printf("roadmap-apply: fetch linear meta for hive:%s (%s): %v (branch_name not backfilled)", taskID, task.SourceID, ferr)
			} else {
				if branch != "" {
					metaStr["branch_name"] = branch
				}
				if _, hasExt := task.Metadata["external_id"]; !hasExt && ident != "" {
					metaStr["external_id"] = ident
				}
			}
		}
	}
	if err := s.d.store.MergeTaskMetadata(ctx, taskID, metaStr); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("merge hive:%s: stamp phase metadata failed (task unchanged): %v", taskID, err))
		return
	}
	if err := s.d.store.UpdateTaskContent(ctx, taskID, st.Title, st.Body); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("merge hive:%s: phase stamped but content update failed: %v", taskID, err))
		return
	}
	if writeBack && task.Source == "linear" && task.SourceID != "" && s.d.linearWriter != nil {
		if err := s.d.linearWriter.UpdateIssueContent(ctx, teamKey, task.SourceID, st.Title, st.Body); err != nil {
			log.Printf("roadmap apply: push merged content to Linear %s failed: %v (Hive updated; next sync may revert)", task.SourceID, err)
		}
	}
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskUpdated, Data: map[string]any{
		"task_id":             taskID,
		"title":               st.Title,
		"roadmap_phase":       metaString(meta, "roadmap_phase"),
		"roadmap_phase_index": metaString(meta, "roadmap_phase_index"),
	}})
	res.Merged++
}

// applyLinearPull creates a Hive task bound to an existing (un-pulled) Linear
// issue using the merged content + phase metadata, then pushes the merged
// content to Linear so both sides agree.
// applyLinearPull returns the ID of the created Hive task, or "" if creation
// failed (so dependents referencing it are skipped — best-effort).
func (s *RPCServer) applyLinearPull(ctx context.Context, proj *store.Project, teamKey string, writeBack bool, uuid string, st decompose.ProposedSubtask, meta map[string]any, res *RoadmapDecomposeApplyResult) string {
	// Enrich with the Linear metadata the syncer's OpInsert would stamp:
	// external_id ("HBA-42" — used in future decompose prompts) and branch_name
	// (Linear's canonical branch; the worktree provisioner honors it so commits
	// auto-link back to the issue). The bare linear:<uuid> ref reaches this
	// handler, so re-fetch by UUID. Best-effort: a fetch failure leaves the task
	// with roadmap metadata only (prior behavior) rather than aborting the pull.
	if s.d.linearWriter != nil {
		if ident, branch, ferr := s.d.linearWriter.FetchIssueMeta(ctx, uuid); ferr != nil {
			log.Printf("roadmap-apply: fetch linear meta for %s: %v (task created without external_id/branch_name)", uuid, ferr)
		} else {
			if ident != "" {
				meta["external_id"] = ident
			}
			if branch != "" {
				meta["branch_name"] = branch
			}
		}
	}
	task := &store.Task{
		ID:                newID("task"),
		ProjectID:         proj.ID,
		Source:            "linear",
		SourceID:          uuid,
		Title:             st.Title,
		Body:              st.Body,
		Priority:          defaultStr(st.Priority, "P3"),
		Status:            "pending",
		Pipeline:          defaultStr(st.Pipeline, "build"),
		Metadata:          meta,
		LinearSyncedState: "todo",
	}
	if err := s.d.store.InsertTask(ctx, task); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("pull linear:%s: %v", uuid, err))
		return ""
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventTaskCreated,
		Data: map[string]any{
			"task_id":             task.ID,
			"project_id":          proj.ID,
			"project_slug":        proj.Slug,
			"title":               task.Title,
			"status":              task.Status,
			"pipeline":            task.Pipeline,
			"source":              "linear",
			"roadmap_phase":       metaString(meta, "roadmap_phase"),
			"roadmap_phase_index": metaString(meta, "roadmap_phase_index"),
		},
	})
	if writeBack && s.d.linearWriter != nil {
		if err := s.d.linearWriter.UpdateIssueContent(ctx, teamKey, uuid, st.Title, st.Body); err != nil {
			log.Printf("roadmap apply: push pulled content to Linear %s failed: %v (Hive updated; next sync may revert)", uuid, err)
		}
	}
	// Link the pulled issue to its phase's milestone if the roadmap is mirrored.
	if ms := milestoneForTask(proj, meta); ms != "" && s.d.linearWriter != nil {
		if err := s.d.linearWriter.SetIssueMilestone(ctx, uuid, ms); err != nil {
			log.Printf("roadmap apply: link pulled issue %s to milestone failed: %v", uuid, err)
		}
	}
	res.Pulled++
	return task.ID
}
