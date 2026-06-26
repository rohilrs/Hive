package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// GraduateParams is the params envelope for project.graduate. The CLI/TUI
// passes a project slug plus the same flags runGraduate's graduateOpts
// understands. Force bypasses the Stage-3 shippability gate; Draft opens the
// PR as a draft; DryRun runs the full pipeline (build + audit) but stops short
// of opening a PR.
type GraduateParams struct {
	ProjectSlug string `json:"project_slug"`
	Force       bool   `json:"force"`
	Draft       bool   `json:"draft"`
	DryRun      bool   `json:"dry_run"`
}

// handleProjectGraduate STARTS an async graduation and returns a graduate_id
// immediately. The full pipeline (preconditions → shippability → completion
// audit → PR) can take minutes, so it runs off the RPC's read deadline; the
// outcome is delivered via the event bus (graduate.verdict / graduate.done /
// graduate.failed), with graduate.progress streaming coarse phase labels.
// Mirrors handleRoadmapDecompose. Check order is param-validate → project
// lookup (ErrProjectNotFound) → runner-configured → spawn.
func (s *RPCServer) handleProjectGraduate(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p GraduateParams
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
	if s.d.graduateRunner == nil {
		return nil, &rpc.RPCError{
			Code:    rpc.ErrInternal,
			Message: "graduate runner not configured (composition root must wire SetGraduateRunner; see cmd/hive/cmd_daemon.go)",
		}
	}
	// One graduation per project at a time: a second concurrent run collides on
	// the feature-branch worktree (`git worktree add -B <feature>` fails while the
	// branch is checked out by the first run). The async runner defers release.
	if !s.d.graduateInFlight.tryAcquire(proj.Slug) {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "graduate already in progress for " + proj.Slug}
	}
	id := newID("graduate")
	go s.runGraduateAsync(id, p, proj)
	raw, _ := json.Marshal(map[string]string{"graduate_id": id})
	return raw, nil
}

// runGraduateAsync runs the graduation pipeline under the DAEMON context (so a
// client disconnect doesn't cancel it) and publishes lifecycle events. The
// verdict event fires whenever a verdict exists — including the blocking-verdict
// case where runGraduate returns both a Verdict and an Err — before the
// terminal failed/done event.
func (s *RPCServer) runGraduateAsync(id string, p GraduateParams, proj *store.Project) {
	// Release the per-project in-flight guard the RPC handler acquired before
	// spawning this goroutine. Deferred first so it runs LAST (after the
	// panic-recover defer), guaranteeing the guard frees on success, failure, or
	// panic — otherwise a panicked run would wedge the project forever.
	defer s.d.graduateInFlight.release(proj.Slug)
	defer func() {
		if r := recover(); r != nil {
			s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventGraduateFailed, Data: map[string]any{
				"graduate_id": id, "project_slug": p.ProjectSlug,
				"error": fmt.Sprintf("graduate panicked: %v", r),
			}})
		}
	}()

	started := time.Now().Unix()

	progress := func(label string) {
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventGraduateProgress, Data: map[string]any{
			"graduate_id": id, "project_slug": p.ProjectSlug, "phase_label": label,
		}})
	}

	res := s.d.runGraduate(s.d.ctx, proj, graduateOpts{Force: p.Force, Draft: p.Draft, DryRun: p.DryRun}, progress)

	mode := "graduate"
	if p.DryRun {
		mode = "dry-run"
	} else if p.Force {
		mode = "graduate-force"
	}
	rec := graduate.GraduateResult{
		Slug:         p.ProjectSlug,
		Mode:         mode,
		Feature:      s.d.scheduler.effectiveWorktreeBaseForProject(proj.Slug),
		Target:       s.d.scheduler.effectiveTargetBranchForProject(proj.Slug),
		StartedAt:    started,
		EndedAt:      time.Now().Unix(),
		Stage:        res.Stage,
		BuildSummary: res.BuildSummary,
		Verdict:      res.Verdict,
		PRURL:        res.PRURL,
	}
	switch {
	case res.Err == nil:
		rec.Outcome = "done"
	case res.Verdict != nil && res.Verdict.Blocks():
		rec.Outcome = "blocked"
		rec.Error = res.Err.Error()
	default:
		rec.Outcome = "failed"
		rec.Error = res.Err.Error()
	}
	s.d.persistGraduateResult(rec)

	if res.Verdict != nil {
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventGraduateVerdict, Data: map[string]any{
			"graduate_id": id, "project_slug": p.ProjectSlug, "verdict": res.Verdict,
		}})
	}
	if res.Err != nil {
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventGraduateFailed, Data: map[string]any{
			"graduate_id": id, "project_slug": p.ProjectSlug, "error": res.Err.Error(),
		}})
		return
	}
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventGraduateDone, Data: map[string]any{
		"graduate_id": id, "project_slug": p.ProjectSlug, "pr_url": res.PRURL, "dry_run": p.DryRun,
	}})
}

// handleProjectGraduateStatus reads the persisted graduate result for a
// project and returns it. This is a read-only RPC; it does not start or
// interact with a running graduation. Absent file → {exists:false}. Present
// and parseable → {exists:true, result:{...}}.
func (s *RPCServer) handleProjectGraduateStatus(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p struct {
		ProjectSlug string `json:"project_slug"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	path := filepath.Join(s.d.HiveDir(), "graduate-"+p.ProjectSlug+"-result.json")
	jb, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			raw, _ := json.Marshal(map[string]any{"exists": false})
			return raw, nil
		}
		return nil, internalErr(err)
	}
	var rec graduate.GraduateResult
	if err := json.Unmarshal(jb, &rec); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "parse graduate result: " + err.Error()}
	}
	raw, _ := json.Marshal(map[string]any{"exists": true, "result": rec})
	return raw, nil
}

// handleProjectRemediate creates inbox tasks from the persisted graduate
// result's confirmed Critical/High findings (idempotent). Read-the-result →
// create-tasks; does not start a graduation.
func (s *RPCServer) handleProjectRemediate(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p struct {
		ProjectSlug string `json:"project_slug"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	res, err := s.d.remediateFromGraduate(ctx, p.ProjectSlug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	raw, _ := json.Marshal(res)
	return raw, nil
}
