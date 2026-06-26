package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohilrs/Hive/internal/adapter/claudecode"
	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
	"github.com/rohilrs/Hive/internal/worktree"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// ErrTaskNotPending is returned by dispatch when the atomic claim fails
// because another dispatcher already transitioned the task out of
// "pending" (a scheduler tick or a concurrent RunNow). Not a real error
// — callers should treat it as a no-op and stop trying to dispatch.
var ErrTaskNotPending = errors.New("task is not pending")

// branchNameFromTaskMetadata extracts the optional branch_name override
// from a task's Metadata map. Returns "" when the task is nil, metadata
// is nil/absent, the value isn't a string, or the trimmed string is
// empty. The Linear source populates this key with Linear's canonical
// branchName (e.g. "rohil/HBA-42-add-login"); the worktree provisioner
// uses it so commits land on the branch Linear's GitHub integration
// auto-links back to the issue.
func branchNameFromTaskMetadata(t *store.Task) string {
	if t == nil || t.Metadata == nil {
		return ""
	}
	v, ok := t.Metadata["branch_name"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// pendingDispatch is the result of claimAndInsertRun — a claimed task
// with a freshly-inserted pending run row, ready for the slow
// provisioning step (provisionAndLaunch).
type pendingDispatch struct {
	runID    string
	run      *store.Run
	task     *store.Task
	proj     *store.Project
	repoPath string
}

// claimAndInsertRun is the fast, synchronous prefix of a dispatch:
// load task + project, reject projects without a repo_path, atomically
// claim the task (pending -> running), and insert a pending run row.
// Returns ErrTaskNotPending if another dispatcher already won the claim.
// All operations here are quick DB calls — the slow worktree/predictor
// work lives in provisionAndLaunch, so callers get a usable run ID
// (and definitive not-found / not-pending errors) without blocking on
// the predictor.
func (s *Scheduler) claimAndInsertRun(ctx context.Context, taskID, projectID, pipelineName string) (*pendingDispatch, error) {
	task, err := s.d.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	proj, err := s.d.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, fmt.Errorf("project %s has no repo_path; cannot run pipelines", proj.Slug)
	}
	repoPath := *proj.RepoPath

	// Atomically claim the task. If ClaimTask returns false, another
	// dispatcher (scheduler tick or concurrent RunNow) already won —
	// bail with ErrTaskNotPending so the caller can decide whether to
	// surface it (RunNow -> RPC error) or skip silently (tick continues).
	// Replaces the previous non-conditional UpdateTaskStatus which left
	// a race window since two parallel UPDATEs would both succeed.
	claimed, err := s.d.store.ClaimTask(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("claim task: %w", err)
	}
	if !claimed {
		return nil, ErrTaskNotPending
	}

	runID := newID("run")
	run := &store.Run{
		ID:        runID,
		TaskID:    task.ID,
		ProjectID: proj.ID,
		Pipeline:  pipelineName,
		Status:    "pending",
	}
	if err := s.d.store.InsertRun(ctx, run); err != nil {
		return nil, err
	}
	return &pendingDispatch{runID: runID, run: run, task: task, proj: proj, repoPath: repoPath}, nil
}

// dispatch turns a (task, project, pipeline) tuple into a running
// pipeline goroutine, synchronously. Used by the scheduler tick.
// Failures before the pipeline launches mark the run needs_attention
// and propagate the error to the caller. For the RPC path that must not
// block on the predictor, see dispatchAsync.
func (s *Scheduler) dispatch(ctx context.Context, taskID, projectID, pipelineName string) (string, error) {
	pd, err := s.claimAndInsertRun(ctx, taskID, projectID, pipelineName)
	if err != nil {
		return "", err
	}
	if err := s.provisionAndLaunch(ctx, pd, pipelineName, false); err != nil {
		return pd.runID, err
	}
	return pd.runID, nil
}

// dispatchAsync claims the task + inserts the pending run synchronously
// (so not-found / not-pending errors reach the RPC caller immediately),
// then runs the slow provisioning (worktree + predictor + pipeline
// launch) in a background goroutine on the daemon context. The RPC
// returns in milliseconds; the run surfaces to subscribers via the
// run.started event once provisioning completes.
//
// Provisioning MUST use the daemon context, not the RPC ctx — the RPC
// returns right after this function, which would otherwise cancel the
// predictor + pipeline mid-flight.
func (s *Scheduler) dispatchAsync(ctx context.Context, taskID, projectID, pipelineName string) (string, error) {
	pd, err := s.claimAndInsertRun(ctx, taskID, projectID, pipelineName)
	if err != nil {
		return "", err
	}
	go func() {
		if err := s.provisionAndLaunch(s.d.ctx, pd, pipelineName, true); err != nil {
			log.Printf("dispatchAsync: provision run %s: %v", pd.runID, err)
		}
	}()
	return pd.runID, nil
}

// markRunStartedAndPublish flips the run to running in the store and
// emits the run.started event (which drives the TUI to move the task
// out of "Queued" into the active run rows). Factored out so both the
// early manual-dispatch path and the post-guard scheduler path share it.
func (s *Scheduler) markRunStartedAndPublish(ctx context.Context, pd *pendingDispatch, pipelineName string) error {
	if err := s.d.store.MarkRunStarted(ctx, pd.runID); err != nil {
		return err
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventRunStarted,
		Data: map[string]any{
			"run_id":       pd.runID,
			"task_id":      pd.task.ID,
			"task_title":   pd.task.Title,
			"project_id":   pd.proj.ID,
			"project_slug": pd.proj.Slug,
			"pipeline":     pipelineName,
		},
	})
	return nil
}

// provisionAndLaunch is the slow tail of a dispatch: provision the
// worktree, create the runtime dir, load per-project config, run the
// predictor, check the conflict guard, mark the run started, emit the
// run.started event, and launch executePipeline. Failures mark the run
// needs_attention.
//
// When manual is true (run.now), the run is marked started + the
// run.started event is emitted BEFORE the predictor runs, so the TUI
// reflects "in progress" immediately instead of after the ~10s
// predictor. Manual dispatch also bypasses the conflict guard — the
// user explicitly asked to run this now. The scheduler tick path
// (manual=false) keeps the guard check and marks started afterward.
func (s *Scheduler) provisionAndLaunch(ctx context.Context, pd *pendingDispatch, pipelineName string, manual bool) error {
	runID := pd.runID
	run := pd.run
	task := pd.task
	proj := pd.proj
	repoPath := pd.repoPath

	// Manual run.now: surface the run as in-progress immediately — before
	// even the worktree create (a few seconds on some filesystems) and the
	// predictor (~10s). The run.started event moves the task out of the
	// Projects "Queued" section into the active run rows. Bypasses the
	// conflict guard below (the user explicitly asked to run now). Any
	// failure after this point emits run.ended (via endRun) so the run
	// can't get stuck showing "running".
	if manual {
		if err := s.markRunStartedAndPublish(ctx, pd, pipelineName); err != nil {
			return err
		}
	}

	info, err := s.d.wtMgr.Create(ctx, worktree.CreateRequest{
		RunID:    runID,
		RepoPath: repoPath,
		// Target branch resolved here, before the per-project config.Load reload
		// further down — worktree creation must precede that reload.
		BaseBranch: s.effectiveWorktreeBaseForProject(proj.Slug),
		// When the worktree base is the integration feature branch but it
		// doesn't exist yet (plan_setup hasn't run, or it was never pushed),
		// fork from the target branch instead of hard-failing dispatch. No-op
		// when the base already IS the target (no feature branch configured).
		FallbackBase: s.effectiveTargetBranchForProject(proj.Slug),
		TaskTitle:    task.Title,
		// BranchName: when the task carries an explicit branch_name in its
		// Metadata (set by the Linear source from Linear's canonical
		// branchName field), honor it instead of auto-generating
		// hive/run-<id>/<slug>. Empty falls back to the auto-gen scheme
		// inside Manager.Create. Closes the loop with Linear's GH
		// integration: commits on Linear's branchName auto-link back to
		// the issue.
		BranchName: branchNameFromTaskMetadata(task),
	})
	if err != nil {
		s.endRun(ctx, runID, task.ID, "needs_attention", "worktree create failed: "+err.Error())
		return err
	}

	// Sequenced-dispatcher foundations: record the branch this run works on
	// so the Phase 3 merge-poller can find the PR without the worktree.
	if berr := s.d.store.SetRunBranch(ctx, runID, info.BranchName); berr != nil {
		log.Printf("dispatch: persist branch for run %s: %v", runID, berr)
	}

	runtimeDir := filepath.Join(s.d.HiveDir(), runID)
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		s.endRun(ctx, runID, task.ID, "needs_attention", "runtime dir create failed: "+err.Error())
		return fmt.Errorf("mkdir runtime: %w", err)
	}

	p := s.d.pipelines[pipelineName]
	if p == nil {
		s.endRun(ctx, runID, task.ID, "needs_attention", "unknown pipeline: "+pipelineName)
		return fmt.Errorf("unknown pipeline: %s", pipelineName)
	}
	// Phase 2b.5: re-load config with this project's slug so per-project
	// overrides in ~/.hive/projects/<slug>/config.toml take effect at
	// dispatch time. SkipEnv=true means we don't re-apply env overrides
	// (they were already applied at daemon boot to s.d.cfg.Cfg); this also
	// gives per-project TOML the final word over env vars, which is the
	// intent — operators editing project config should win over their own
	// shell env from when they started the daemon.
	effectiveCfg := s.d.effectiveConfigForProject(proj.Slug)

	// Foundational tuning-data capture: persist the effective config as
	// JSON on the run row so later analysis can attribute observed metrics
	// to the config that produced them. Best-effort — log on error, do not
	// fail dispatch. See docs/superpowers/operational-data.md.
	if payload, err := json.Marshal(effectiveCfg); err == nil {
		if err := s.d.store.PutConfigSnapshot(ctx, runID, payload); err != nil {
			log.Printf("dispatch: persist config snapshot for run %s: %v", runID, err)
		}
	} else {
		log.Printf("dispatch: marshal config snapshot for run %s: %v", runID, err)
	}

	var pred *predictor.Result
	if s.d.predictor != nil && effectiveCfg.Predictor.Enabled {
		var predErr error
		pred, predErr = s.d.predictor.Predict(ctx, task.Body, repoPath, runtimeDir)
		if predErr != nil {
			// Hard failure (e.g., bundleDir not writable). Log and continue
			// without prediction — don't block the run.
			log.Printf("dispatch: predictor failed for run %s: %v", runID, predErr)
		}
		// predErr == nil, pred == nil: graceful degrade (Haiku failure,
		// no candidates, etc.). Proceed without prediction.

		// Phase 2b.5: persist denormalized metrics for `hive predict stats`
		// AND the full Result JSON to runs.prediction (for re-dispatch
		// hydration + future ad-hoc inspection via `hive run-detail`).
		// Both best-effort — log on error, do not fail dispatch.
		//
		// 2b.5 followup: PutPredictionJSON moved out of the conflict_guard
		// branch so shadow mode (predictor on, guard off) also populates
		// runs.prediction. The cost is one extra UPDATE on the no-conflict
		// happy path, which was previously written by the conflict_guard
		// proceed branch anyway — net same write count.
		if pred != nil {
			m := &store.PredictorMetric{
				RunID:          runID,
				ProjectID:      proj.ID,
				HaikuLatencyMS: pred.Metrics.HaikuLatency.Milliseconds(),
				FetchLatencyMS: pred.Metrics.FetchLatency.Milliseconds(),
				CandidateCount: pred.Metrics.CandidateCount,
				InlineCount:    pred.Metrics.InlineCount,
				OverflowCount:  pred.Metrics.OverflowCount,
				Truncated:      pred.Metrics.Truncated,
				Error:          pred.Metrics.Error,
			}
			if err := s.d.store.InsertPredictorMetrics(ctx, m); err != nil {
				log.Printf("dispatch: persist predictor metrics for run %s: %v", runID, err)
			}
			if payload, err := json.Marshal(pred); err == nil {
				if err := s.d.store.PutPredictionJSON(ctx, runID, payload); err != nil {
					log.Printf("dispatch: persist prediction JSON for run %s: %v", runID, err)
				}
			}
		}
	}

	// Scheduler-tick path only: conflict guard + mark-started. Manual
	// run.now already marked started above and bypasses the guard.
	if !manual {
		// Phase 2b.4: conflict guard. Only runs when (a) we got a non-nil
		// prediction (no files to check otherwise) AND (b) the guard is
		// enabled in config AND (c) the guard is wired (composition root
		// constructed one).
		if pred != nil && s.d.guard != nil && effectiveCfg.ConflictGuard.Enabled {
			dec := s.d.guard.CheckAndReserve(runID, pred.Files)
			if !dec.Proceed {
				// runs.prediction was already persisted above; just record
				// the waiting_on list and leave the run in 'pending' (do
				// NOT call MarkRunStarted — the scheduler tick will re-
				// evaluate this run when blockers complete).
				_ = s.d.store.SetWaitingOn(ctx, runID, dec.WaitingOn)
				log.Printf("dispatch: run %s queued; waiting on %v", runID, dec.WaitingOn)
				return nil
			}
			// Proceed=true: nothing extra to persist (Result already saved).
		}

		if err := s.markRunStartedAndPublish(ctx, pd, pipelineName); err != nil {
			// Release the guard reservation we just acquired so a queued
			// run waiting on this one doesn't block forever.
			if s.d.guard != nil {
				s.d.guard.Release(runID)
			}
			return err
		}
	}

	s.d.goTracked(func() {
		s.executePipeline(p, run, task, proj, info.Path, info.BranchName, runtimeDir, repoPath, pred)
	})
	return nil
}

// launchAccuracyCompute fires the per-run accuracy compute in a
// detached goroutine. Fire-and-forget; failures are logged inside
// computeAndPersistAccuracy. The recover() catches panics so a bug
// in accuracy compute can't take down the daemon. Uses ctx (the
// daemon's ctx) so shutdown cancels the in-flight git diff.
//
// Called from executePipeline on both happy-path and error-path
// branches — errored runs may still have produced partial edits
// worth measuring.
func (s *Scheduler) launchAccuracyCompute(ctx context.Context, runID string, pred *predictor.Result, worktreePath string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("accuracy_compute: panic for run %s: %v", runID, r)
			}
		}()
		computeAndPersistAccuracy(ctx, s.d.store, runID, pred, worktreePath)
	}()
}

// dispatchExisting launches the pipeline for a run that was previously
// queued via pending+waiting_on. The worktree and runtime dir were
// created during the initial dispatch; the prediction is passed in
// from the persisted JSON. Caller has already verified the conflict
// guard reservation succeeded (CheckAndReserve returned Proceed) and
// cleared waiting_on in the store.
//
// This mirrors dispatch's post-worktree-create path without re-running
// Predict, re-creating the worktree, or re-creating the runtime dir.
func (s *Scheduler) dispatchExisting(ctx context.Context, run *store.Run, pred *predictor.Result) error {
	task, err := s.d.store.GetTask(ctx, run.TaskID)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}
	proj, err := s.d.store.GetProject(ctx, run.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return fmt.Errorf("project %s has no repo_path", proj.Slug)
	}
	repoPath := *proj.RepoPath

	// Derive worktree path + branch name from runID + task title using
	// the same deterministic scheme as worktree.Manager.Create. Honor
	// task.Metadata["branch_name"] override (Linear-ingested tasks) the
	// same way Manager.Create does, so dispatchExisting computes the
	// branch name that the worktree was actually created on.
	branchName := branchNameFromTaskMetadata(task)
	if branchName == "" {
		branchName = worktree.BranchName(run.ID, task.Title)
	}
	worktreePath := filepath.Join(s.d.HiveDir(), "worktrees", run.ID)
	runtimeDir := filepath.Join(s.d.HiveDir(), run.ID)

	p := s.d.pipelines[run.Pipeline]
	if p == nil {
		return fmt.Errorf("unknown pipeline: %s", run.Pipeline)
	}
	if err := s.d.store.MarkRunStarted(ctx, run.ID); err != nil {
		return fmt.Errorf("mark started: %w", err)
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventRunStarted,
		Data: map[string]any{
			"run_id":       run.ID,
			"task_id":      task.ID,
			"task_title":   task.Title,
			"project_id":   proj.ID,
			"project_slug": proj.Slug,
			"pipeline":     run.Pipeline,
		},
	})
	s.d.goTracked(func() {
		s.executePipeline(p, run, task, proj, worktreePath, branchName, runtimeDir, repoPath, pred)
	})
	return nil
}

// executePipeline runs the pipeline FSM in a detached goroutine, then
// updates the run + task rows based on the result. We use the daemon's
// ctx (not the caller's) so an RPC client disconnecting doesn't abort
// the in-flight pipeline.
//
// Cleanup policy (Phase 1): always preserve worktree + runtime dir.
// "done" only means the reviewer approved — the code change is still
// in the worktree, unmerged, and the user needs both the diff and the
// per-stage events.jsonl to decide whether to merge it. Phase 5's
// finish-branch pipeline + an explicit `hive runs cleanup` command
// will handle teardown later; until then, eager cleanup just deletes
// data the user wants to review.
// prepareScavengerWorkspace makes the run's worktree a self-contained
// scavenger workspace: full index + local plugin + worktree-scoped daemon.
// Entirely non-fatal — any failure logs and the run proceeds with degraded
// (status-quo) capsules. No-op unless scavenger is enabled + per-run mode on.
func (s *Scheduler) prepareScavengerWorkspace(ctx context.Context, worktreePath string) {
	if s.d.scavLifecycle == nil {
		return
	}
	cfg := s.d.cfg.Cfg.Scavenger
	if !cfg.Enabled || !cfg.IndexWorktreeOnRun {
		return
	}
	if err := s.d.scavLifecycle.IndexWorktree(ctx, worktreePath); err != nil {
		log.Printf("scavenger: index worktree %s: %v", worktreePath, err)
	}
	if err := s.d.scavLifecycle.InstallPlugin(ctx, worktreePath); err != nil {
		log.Printf("scavenger: install plugin %s: %v", worktreePath, err)
	}
	if err := s.d.scavLifecycle.StartDaemon(ctx, worktreePath); err != nil {
		log.Printf("scavenger: start daemon %s: %v", worktreePath, err)
	}
}

// teardownScavengerWorkspace stops the run's worktree-scoped daemon. The
// worktree-local .scavenger/ is removed with the worktree itself elsewhere.
func (s *Scheduler) teardownScavengerWorkspace(worktreePath string) {
	if s.d.scavLifecycle == nil {
		return
	}
	if err := s.d.scavLifecycle.StopDaemon(worktreePath); err != nil {
		log.Printf("scavenger: stop daemon %s: %v", worktreePath, err)
	}
}

func (s *Scheduler) executePipeline(
	p pipeline.Pipeline,
	run *store.Run, task *store.Task, proj *store.Project,
	worktreePath, branchName, runtimeDir, repoPath string,
	pred *predictor.Result,
) {
	// Phase 3.7: per-run ctx so run.abandon can cancel just this run
	// without taking down sibling runs or the daemon itself. Cleanup
	// on every exit path via defer.
	ctx, cancel := context.WithCancel(s.d.ctx)
	s.d.registerRunCancel(run.ID, cancel)
	defer func() {
		s.d.unregisterRunCancel(run.ID)
		cancel()
	}()
	// Per-run scavenger workspace: full index + plugin + worktree daemon,
	// torn down on exit. Non-fatal throughout. Covers both dispatch and
	// dispatchExisting since both funnel through executePipeline.
	s.prepareScavengerWorkspace(ctx, worktreePath)
	scavengerTransferred := false
	defer func() {
		if !scavengerTransferred {
			s.teardownScavengerWorkspace(worktreePath)
		}
	}()
	pr := &pipeline.Run{
		ID:           run.ID,
		Task:         task,
		Project:      proj,
		WorktreePath: worktreePath,
		RuntimeDir:   runtimeDir,
		BranchName:   branchName,
		Pipeline:     p.Name(),
		Prediction:   pred,
		TargetBranch: s.effectiveWorktreeBaseForProject(proj.Slug),
		Commands:     s.d.runCommandsForProject(proj.Slug),
	}
	// Phase 2b.4: free the run's file reservation regardless of
	// outcome (success, error, or panic). Tick loop will re-evaluate
	// any pending+waiting_on runs on the next cycle.
	if s.d.guard != nil {
		defer s.d.guard.Release(run.ID)
	}
	result, err := p.Run(ctx, pr)
	if err != nil {
		// Phase 3.7: if the run's context was cancelled, the run was
		// abandoned via run.abandon — the handler already marked the row
		// "abandoned" + emitted run.ended. Don't overwrite it with
		// needs_attention. We check ctx.Err() (not just errors.Is on the
		// returned err) because a killed worker subprocess often surfaces
		// as an exec/"signal: killed" error that doesn't wrap
		// context.Canceled, which previously caused the run to flicker
		// from "abandoned" back to "needs_attention".
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			s.launchAccuracyCompute(s.d.ctx, run.ID, pred, worktreePath)
			return
		}
		var status, summary string
		switch {
		case errors.Is(err, verdict.ErrFileRefsMissing):
			status = "error"
			summary = "REVIEW_FEEDBACK_MISSING: reviewer emitted CHANGES_REQUESTED with no file_refs"
		case errors.Is(err, claudecode.ErrToolCallStall):
			tool, _ := claudecode.StallToolFromError(err)
			status = "needs_attention"
			summary = "tool_call_stall: subprocess SIGTERM'd"
			if tool != "" {
				summary = "tool_call_stall: " + tool
			}
		case errors.Is(err, claudecode.ErrImplementStagnation):
			// The implement stage was killed early because the worktree showed no
			// NEW content for the configured window — the agent is stuck/looping
			// (e.g. deps weren't merged, so it has nothing to build on). Surface
			// the typed message directly.
			status = "needs_attention"
			summary = err.Error()
		case errors.Is(err, claudecode.ErrStageTimeout):
			// A stage hit its time budget (the subprocess was SIGKILLed by the
			// timeout context). err.Error() already reads e.g.
			// "iter 0 implement: implement timed out after 20m0s (...)" — surface
			// it directly instead of the cryptic raw "signal: killed".
			status = "needs_attention"
			summary = err.Error()
		default:
			status = "needs_attention"
			summary = "pipeline error: " + err.Error()
		}
		s.endRun(ctx, run.ID, task.ID, status, summary)
		// Phase 2c.1: even errored runs may have produced edits worth
		// measuring. Same fire-and-forget pattern.
		// Use daemon ctx so accuracy compute outlives this run's
		// cancelled per-run ctx (Phase 3.7 abandon-cancellation).
		s.launchAccuracyCompute(s.d.ctx, run.ID, pred, worktreePath)
		return
	}
	_ = s.d.store.MarkRunEnded(ctx, run.ID, result.Status, result.Summary)
	if result.PRURL != "" {
		if perr := s.d.store.SetRunPR(ctx, run.ID, result.PRURL, result.PRNumber); perr != nil {
			log.Printf("dispatch: persist PR for run %s: %v", run.ID, perr)
		}
	}
	endData := map[string]any{"run_id": run.ID, "task_id": task.ID, "status": result.Status, "summary": result.Summary}
	if result.DocumentationSkipped {
		endData["documentation_skipped"] = true
		endData["documentation_skip_reason"] = result.DocumentationSkipReason
	}
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventRunEnded, Data: endData})

	// Persist or clear the task's last-failure feedback for the build
	// pipeline only: on needs_attention, write the final feedback blob so
	// the next run's iter-0 implement prompt can inject it; on done, clear
	// any prior failure. Both are best-effort — log on error, do not block
	// the normal status/refresh path.
	if pr.Pipeline == "build" {
		switch result.Status {
		case "needs_attention":
			if result.FinalFeedback != nil {
				blob, merr := json.Marshal(struct {
					Summary       string            `json:"summary"`
					FileRefs      []verdict.FileRef `json:"file_refs"`
					ExhaustReason string            `json:"exhaust_reason"`
				}{
					Summary:       result.FinalFeedback.Summary,
					FileRefs:      result.FinalFeedback.FileRefs,
					ExhaustReason: result.ExhaustReason,
				})
				if merr != nil {
					log.Printf("dispatch: marshal last_failure_feedback for task %s: %v", task.ID, merr)
				} else if serr := s.d.store.SetTaskLastFailureFeedback(ctx, task.ID, string(blob)); serr != nil {
					log.Printf("dispatch: persist last_failure_feedback for task %s: %v", task.ID, serr)
				}
			}
		case "done":
			if cerr := s.d.store.ClearTaskLastFailureFeedback(ctx, task.ID); cerr != nil {
				log.Printf("dispatch: clear last_failure_feedback for task %s: %v", task.ID, cerr)
			}
		}
	}

	if !s.handleSequencedBuildEnd(ctx, run, task, proj, result, worktreePath, branchName, &scavengerTransferred) {
		if !s.maybeAutoIntegrate(ctx, run, task, proj, result, worktreePath, branchName, &scavengerTransferred) {
			s.markNonSeqPRGate(ctx, task, result.PRURL)
			s.d.refreshTaskStatus(ctx, task.ID)
		}
	}
	if result.DocumentationSkipped {
		_ = s.d.store.MarkDocumentationSkipped(ctx, run.ID, result.DocumentationSkipReason)
	}

	// Phase 2c.1: compute accuracy asynchronously. Fire-and-forget;
	// failures are logged inside computeAndPersistAccuracy. Uses the
	// daemon ctx (not per-run) so cancellation from Phase 3.7
	// run.abandon doesn't kill the accuracy goroutine — accuracy
	// outlives the pipeline.
	s.launchAccuracyCompute(s.d.ctx, run.ID, pred, worktreePath)
}

// markNonSeqPRGate puts a non-sequenced task that opened a PR into the
// awaiting_merge gate so the unified detector tracks it to merge and Linear
// shows In Review until then. Guarded on gate==none so a sequenced task whose
// gate is managed by chainFinishBranch is never overridden. (Phase 2)
func (s *Scheduler) markNonSeqPRGate(ctx context.Context, task *store.Task, prURL string) {
	if prURL == "" {
		return
	}
	if task.GateState != "" && task.GateState != sequence.GateNone {
		return
	}
	if err := s.d.store.UpdateTaskGateState(ctx, task.ID, sequence.GateAwaitingMerge); err != nil {
		log.Printf("phase2: set awaiting_merge for %s: %v", task.ID, err)
	}
	// FRESH entry into awaiting_merge after a successful build opened the PR —
	// reset the circuit-breaker counter so a re-queued task starts clean.
	s.d.mergeAttempts.reset(task.ID)
}

// endRun marks a run ended and refreshes the owning task's derived status.
// (No more skipTaskUpdate: DeriveTaskStatus inspects all of a task's runs,
// so parent/child rows unify — the parent no longer needs to "own" the update.)
func (s *Scheduler) endRun(ctx context.Context, runID, taskID, status, summary string) {
	_ = s.d.store.MarkRunEnded(ctx, runID, status, summary)
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventRunEnded,
		Data: map[string]any{"run_id": runID, "task_id": taskID, "status": status, "summary": summary},
	})
	s.d.refreshTaskStatus(ctx, taskID)
}
