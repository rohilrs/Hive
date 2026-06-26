package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// Scheduler is the daemon's tick loop. Phase 1c ships a deliberately
// minimal scheduler: priority order by task row order (ListPendingTasks
// already sorts by priority + created_at), no conflict guard, no
// per-repo caps — just MaxWorkers capacity gating. Conflict + per-repo
// caps land in Phase 1d.
type Scheduler struct {
	d  *Daemon
	mu sync.Mutex // serializes tick() so two ticks can't double-dispatch a task

	// Phase 7 (doctor): lastTickAt records the wall-clock time of the
	// most recent tick() entry. doctor.health reads it via LastTickAt
	// and flags the scheduler as wedged if the age exceeds a threshold.
	// Guarded by its own mutex so LastTickAt() doesn't contend with
	// the long-running tick body (which holds s.mu).
	tickMu     sync.Mutex
	lastTickAt time.Time

	// seqStuckWarned tracks sequenced projects we've already logged as
	// "stuck" (active dispatcher but roadmap/derive failing) so the 1s tick
	// doesn't spam the log. Only touched inside tick (which holds s.mu) and
	// activePhaseForSequencedProject (called from tick). Cleared on recovery.
	seqStuckWarned map[string]bool

	// dispatchResolveFn is the seam for auto-dispatching a resolve run when an
	// auto-merge hits a content conflict. Defaults to dispatchResolveRun; tests
	// override it to observe the dispatch without provisioning a worktree.
	dispatchResolveFn func(ctx context.Context, task *store.Task, proj *store.Project, worktreePath, branchName string) error

	// reMergeInterval/reMergeTimeout bound the post-resolve "wait for GitHub to
	// recompute mergeability, then merge" loop (reMergeAfterResolve). GitHub
	// computes `mergeable` asynchronously after the resolve push, so merging
	// immediately races a stale verdict; we poll every reMergeInterval up to
	// reMergeTimeout. Defaulted in NewScheduler; tests set tiny values.
	reMergeInterval time.Duration
	reMergeTimeout  time.Duration
}

// NewScheduler constructs a Scheduler bound to the given Daemon.
func NewScheduler(d *Daemon) *Scheduler {
	s := &Scheduler{
		d:               d,
		seqStuckWarned:  map[string]bool{},
		reMergeInterval: 3 * time.Second,
		reMergeTimeout:  90 * time.Second,
	}
	s.dispatchResolveFn = s.dispatchResolveRun
	return s
}

// Loop runs the scheduler tick until ctx is canceled. 1-second cadence;
// this is intentionally chatty because Phase 1c has no event-driven
// wakeup yet (Phase 1d adds run completion -> scheduler kick via channel).
//
// Phase 2b.4: runs reEvalQueuedRuns once at startup for daemon-restart
// recovery. Stale "running" runs from the previous daemon are not in the
// in-memory Guard, so queued pending+waiting_on runs will all proceed
// immediately (they were blocked on runs that no longer hold any files).
func (s *Scheduler) Loop(ctx context.Context) {
	// Phase 7 restart-recovery: kill leftover worker subprocesses
	// before any other state reconciliation. Must run first so
	// runs.worker_pid is still readable — recoverStaleRuns doesn't
	// touch worker_pid, but keeping the kill at step zero makes the
	// ordering invariant explicit at the call site.
	s.recoverOrphanedWorkers(ctx)
	s.recoverStaleRuns(ctx)
	s.mu.Lock()
	s.reEvalQueuedRuns(ctx)
	s.mu.Unlock()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	s.tickMu.Lock()
	s.lastTickAt = time.Now()
	s.tickMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.reEvalQueuedRuns(ctx)

	// Per-project dispatch mode resolution. The global setting is the
	// default, but a project's ~/.hive/projects/<slug>/config.toml can
	// override it with its own [scheduler] dispatch_mode (or the legacy
	// auto_dispatch). Resolution happens per pending task; results cached
	// per-tick by project ID so multiple pending tasks for the same
	// project don't re-load the same TOML file.
	capacity, err := s.capacity(ctx)
	if err != nil || capacity <= 0 {
		return
	}
	pending, err := s.d.store.ListPendingTasks(ctx)
	if err != nil {
		return
	}
	effectiveMode := map[string]string{} // projectID → resolved dispatch mode
	seqActive := map[string]string{}     // projectID → active phase ("" = none/paused/not-ready)
	for _, t := range pending {
		if capacity <= 0 {
			break
		}
		mode, cached := effectiveMode[t.ProjectID]
		if !cached {
			proj, perr := s.d.store.GetProject(ctx, t.ProjectID)
			if perr != nil {
				// Fail closed if we can't read the project row.
				effectiveMode[t.ProjectID] = config.DispatchModeManual
				continue
			}
			mode = s.effectiveDispatchModeForProject(proj.Slug)
			effectiveMode[t.ProjectID] = mode
		}
		switch mode {
		case config.DispatchModeAutoAll:
			// no phase filter — dispatch every pending task (shared tail below)
		case config.DispatchModeSequenced:
			active, ok := seqActive[t.ProjectID]
			if !ok {
				active = s.activePhaseForSequencedProject(ctx, t.ProjectID)
				seqActive[t.ProjectID] = active
			}
			phase, _ := t.Metadata["roadmap_phase"].(string)
			// Dispatch only fresh tasks in the active phase. Later-phase tasks
			// (phase != active), already-progressed tasks (gate != none), and
			// unsequenced tasks (phase == "") are skipped.
			if active == "" || phase != active || t.GateState != sequence.GateNone {
				continue
			}
			if !s.dependenciesSatisfied(ctx, t) {
				continue // a declared dependency hasn't merged yet — hold this task
			}
		default: // manual or unknown
			continue
		}
		pipelineName := t.Pipeline
		if pipelineName == "" {
			pipelineName = "build"
		}
		if _, err := s.dispatch(ctx, t.ID, t.ProjectID, pipelineName); err != nil {
			if errors.Is(err, ErrTaskNotPending) {
				continue // RunNow or another tick won the race — expected
			}
			log.Printf("scheduler: dispatch %s failed: %v", t.ID, err)
			continue
		}
		capacity--
	}
}

// dependenciesSatisfied reports whether every task listed in t's "depends_on"
// metadata has merged (gate=satisfied). Tasks with no deps are always ready.
// Fails CLOSED (returns false) if a dependency can't be read, so a transient
// store error holds the task rather than dispatching it onto an incomplete base.
func (s *Scheduler) dependenciesSatisfied(ctx context.Context, t *store.Task) bool {
	// depends_on is stored as a comma-joined string of task IDs (survives the
	// MergeTaskMetadata %v flattening when a dependent is itself a merge target).
	// Also accept the legacy []any form defensively.
	var depIDs []string
	switch v := t.Metadata["depends_on"].(type) {
	case string:
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				depIDs = append(depIDs, id)
			}
		}
	case []any:
		for _, d := range v {
			if id, _ := d.(string); id != "" {
				depIDs = append(depIDs, id)
			}
		}
	}
	if len(depIDs) == 0 {
		return true
	}
	for _, depID := range depIDs {
		dep, err := s.d.store.GetTask(ctx, depID)
		if err != nil {
			return false // fail closed: hold rather than dispatch onto an incomplete base
		}
		// A dependency is met once its work is integrated: gate satisfied (the
		// normal build→merge path), gate skipped (operator unblocked it), OR the
		// task reached terminal "done" without a merge gate at all — e.g. an
		// audit/plan-pipeline task that committed a doc to the feature branch but
		// opened no PR, so its gate stays GateNone. Matching sequence.resolved().
		if dep.GateState != sequence.GateSatisfied &&
			dep.GateState != sequence.GateSkipped &&
			dep.Status != "done" {
			return false
		}
	}
	return true
}

// loadEffectiveScheduler resolves the effective [scheduler] config for a
// project by overlaying its per-project ~/.hive/projects/<slug>/config.toml
// on top of the in-memory global config (same TOML-merge semantics as
// config.Load). Returns (Scheduler, ok). ok=false means a read/parse
// failure occurred and the caller should fail closed.
func (s *Scheduler) loadEffectiveScheduler(slug string) (config.Scheduler, bool) {
	baseline := s.d.cfg.Cfg.Scheduler
	if slug == "" {
		return baseline, true
	}
	projPath := filepath.Join(s.d.cfg.HiveDir, "projects", slug, "config.toml")
	if _, err := os.Stat(projPath); err != nil {
		if os.IsNotExist(err) {
			return baseline, true
		}
		return baseline, false // permission/IO error -> caller fails closed
	}
	cfg := *s.d.cfg.Cfg
	if _, err := toml.DecodeFile(projPath, &cfg); err != nil {
		return baseline, false // malformed -> caller fails closed
	}
	return cfg.Scheduler, true
}

// effectiveDispatchModeForProject resolves the dispatch mode for a project,
// honoring per-project overrides and the auto_dispatch back-compat
// fallback. Fails closed to "manual" on any read/parse error or
// unrecognized mode value.
func (s *Scheduler) effectiveDispatchModeForProject(slug string) string {
	sched, ok := s.loadEffectiveScheduler(slug)
	if !ok {
		return config.DispatchModeManual
	}
	switch m := sched.ResolvedMode(); m {
	case config.DispatchModeManual, config.DispatchModeAutoAll, config.DispatchModeSequenced:
		return m
	default:
		// Unrecognized per-project dispatch_mode (the overlay path skips
		// config.Validate); fail closed to manual rather than misroute.
		return config.DispatchModeManual
	}
}

// activePhaseForSequencedProject returns the roadmap phase number the
// sequenced dispatcher should currently be working, or "" if the project
// has no active dispatcher (absent/paused/completed), its roadmap can't be
// read, or all phases are complete. Fail-closed: any error returns "" so the
// tick simply doesn't dispatch.
func (s *Scheduler) activePhaseForSequencedProject(ctx context.Context, projectID string) string {
	// NOTE (perf debt): this reads + parses the roadmap markdown from disk and
	// does a full task scan per sequenced project per tick (every 1s), under
	// s.mu. Acceptable at current scale (<10 sequenced projects). If it grows,
	// cache ActivePhase in the sequence_dispatchers row and update it from the
	// gate-advancement path instead of re-deriving here.
	disp, err := s.d.store.GetSequenceDispatcher(ctx, projectID)
	if err != nil || disp.Status != "active" {
		return ""
	}
	proj, err := s.d.store.GetProject(ctx, projectID)
	if err != nil {
		return ""
	}
	rm, _, err := s.d.loadProjectRoadmap(proj)
	if err != nil {
		s.warnSeqStuck(proj.Slug, "roadmap unavailable: "+err.Error())
		return ""
	}
	plan, err := s.d.derivePlan(ctx, proj, rm)
	if err != nil {
		s.warnSeqStuck(proj.Slug, "derive plan failed: "+err.Error())
		return ""
	}
	// Recovered (or never stuck): clear any prior warning so a future breakage
	// logs again.
	delete(s.seqStuckWarned, proj.Slug)
	return plan.ActivePhase
}

// warnSeqStuck logs a stuck sequenced project once until it recovers. Called
// only from the tick path (s.mu held), so map access is safe.
func (s *Scheduler) warnSeqStuck(slug, reason string) {
	if s.seqStuckWarned[slug] {
		return
	}
	s.seqStuckWarned[slug] = true
	log.Printf("scheduler: sequenced project %s stuck — %s; not dispatching until resolved", slug, reason)
}

// effectiveTargetBranchForProject resolves the integration/target branch
// for a project (worktree base + PR base). Fails closed to "main".
func (s *Scheduler) effectiveTargetBranchForProject(slug string) string {
	sched, ok := s.loadEffectiveScheduler(slug)
	if !ok {
		return "main"
	}
	return sched.ResolvedTargetBranch()
}

func (s *Scheduler) capacity(ctx context.Context) (int, error) {
	running, err := s.d.store.ListRunningRuns(ctx)
	if err != nil {
		return 0, err
	}
	return s.d.cfg.Cfg.Concurrency.MaxWorkers - len(running), nil
}

// RunNow is the dispatch entrypoint used by the run.now RPC. Bypasses
// the tick loop and capacity check — the user has explicitly requested
// this run. Dispatches asynchronously (dispatchAsync): the task is
// claimed and a pending run row inserted synchronously (so not-found /
// not-pending errors reach the caller), then the slow worktree +
// predictor + pipeline provisioning runs in the background on the daemon
// context. This keeps run.now fast — previously it blocked ~10s in the
// predictor and blew past the TUI client's read deadline, surfacing as a
// spurious daemon.sock timeout even though the dispatch succeeded.
func (s *Scheduler) RunNow(ctx context.Context, taskID string) (string, error) {
	task, err := s.d.store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	// Re-run support: a task whose previous run was abandoned or failed
	// sits in needs_attention (or done). Reset it to pending so the
	// atomic claim in dispatchAsync succeeds — this starts a fresh run.
	// A task that's currently running stays as-is so the claim fails
	// with ErrTaskNotPending (can't double-dispatch a live run).
	if task.Status != "pending" && task.Status != "running" {
		if err := s.d.store.UpdateTaskStatus(ctx, task.ID, "pending"); err != nil {
			return "", err
		}
	}
	pipelineName := task.Pipeline
	if pipelineName == "" {
		pipelineName = "build"
	}
	return s.dispatchAsync(ctx, task.ID, task.ProjectID, pipelineName)
}

// recoverOrphanedWorkers kills claude subprocesses left over from a
// crashed daemon. Reads runs.worker_pid (which survives daemon death
// because it lives in SQLite, not memory), SIGKILLs each process group,
// then nulls the column.
//
// Runs BEFORE recoverStaleRuns so the PID is still readable when we
// kill — recoverStaleRuns flips status to needs_attention but doesn't
// touch worker_pid, so the order is correctness, not coupling.
//
// After a clean shutdown the OnExited subprocess callback already
// cleared every row's worker_pid; this is a no-op in that case. After
// a crash, ListRunsWithWorkerPID returns the survivor set and each
// gets killed exactly once. Publishes a worker.orphan_killed event per
// PID so the TUI / Events tab surfaces the recovery action.
func (s *Scheduler) recoverOrphanedWorkers(ctx context.Context) {
	rows, err := s.d.store.ListRunsWithWorkerPID(ctx)
	if err != nil {
		log.Printf("scheduler: list runs with worker_pid: %v", err)
		return
	}
	for _, r := range rows {
		if !r.WorkerPID.Valid {
			continue
		}
		pid := int(r.WorkerPID.Int64)
		wasAlive, killErr := killProcessGroup(pid)
		if killErr != nil {
			if errors.Is(killErr, syscall.EPERM) {
				// PID is owned by another user — almost certainly a recycle
				// after long downtime. We don't own the process, so we can't
				// kill it; leaving worker_pid set means next boot retries
				// (which is benign — at worst we hit EPERM again on a
				// foreign process). Skip the event so we don't lie about
				// having reaped it.
				log.Printf("scheduler: orphan worker pid=%d run=%s permission denied (recycled to another user?); leaving worker_pid set", pid, r.ID)
				continue
			}
			// Unexpected error (not ESRCH, not EPERM). Log it but still
			// proceed to clear + publish — better to clean up DB state than
			// leak the row forever on a transient kernel error.
			log.Printf("scheduler: kill orphan worker pid=%d run=%s: %v", pid, r.ID, killErr)
		}
		log.Printf("scheduler: orphan worker pid=%d run=%s wasAlive=%v", pid, r.ID, wasAlive)
		if err := s.d.store.ClearRunWorkerPID(ctx, r.ID); err != nil {
			log.Printf("scheduler: clear worker pid for run %s: %v", r.ID, err)
		}
		s.d.bus.Publish(rpc.EventMessage{
			Type: rpc.EventWorkerOrphanKilled,
			Data: map[string]any{
				"run_id":    r.ID,
				"pid":       pid,
				"was_alive": wasAlive,
				"timestamp": time.Now().Unix(),
			},
		})
	}
}

// killProcessGroup SIGKILLs the process group of pid. Returns
// (wasAlive bool, err error):
//
//   - (true, nil)           — process was alive, signal delivered
//   - (false, nil)          — process already dead (ESRCH); treated as
//     success because the orphan we wanted gone is already gone
//   - (false, syscall.EPERM) — PID is owned by another user (likely a
//     recycle after long daemon downtime); caller must NOT treat this
//     as a successful reap and must NOT clear the column — next boot
//     will retry
//   - (false, otherErr)     — some other kill failure (caller decides
//     whether to proceed with DB cleanup)
//
// Tries the process-group form (Kill(-pid, ...)) first because workers
// are spawned with SysProcAttr.Setpgid=true so the worker IS its own
// pgid leader; killing the group also kills any child it spawned (e.g.
// claude → MCP server). Falls back to plain Kill(pid, ...) defensively
// in case a future code path spawns a worker without Setpgid.
//
// EPERM precedence: returned only when BOTH forms fail with EPERM (or
// the first fails with EPERM and the fallback fails with ESRCH —
// neither outcome means we successfully killed). The fallback's EPERM
// shadows the pgroup form's EPERM, which is fine — they signal the
// same condition.
func killProcessGroup(pid int) (bool, error) {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	// Fallback for processes that weren't started as process-group leaders.
	// Also lets us distinguish "pgid -pid didn't exist but the bare pid
	// might" from "we lack permission on this pid entirely."
	err2 := syscall.Kill(pid, syscall.SIGKILL)
	if err2 == nil {
		return true, nil
	}
	if errors.Is(err2, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err2, syscall.EPERM) || errors.Is(err, syscall.EPERM) {
		return false, syscall.EPERM
	}
	return false, err2
}

// recoverStaleRuns reconciles DB state at daemon startup. Any run still
// marked "running" has no live pipeline goroutine — the daemon that
// owned it is gone — so it's a zombie. Mark it needs_attention (which
// also moves its task to the re-runnable needs-attention lane) instead
// of leaving it "running" forever. This is why a run dispatched before a
// daemon restart showed "stuck at implement, no events". Full restart
// recovery (resume/worktree cleanup, killing orphaned worker subprocesses)
// is Phase 7; this is the minimal state-reconcile.
func (s *Scheduler) recoverStaleRuns(ctx context.Context) {
	// Phase 4.3.1 #3: recovery sweeps need every running row including
	// children — ListRunningRuns now excludes parent_run_id != NULL, so
	// use ListAllRunningRuns to mark BOTH parent and child needs_attention.
	running, err := s.d.store.ListAllRunningRuns(ctx)
	if err != nil {
		log.Printf("scheduler: recover stale runs: %v", err)
		return
	}
	for _, r := range running {
		log.Printf("scheduler: stale run %s orphaned by daemon restart → needs_attention", r.ID)
		// Mark the orphaned run needs_attention; endRun refreshes the task's
		// derived status over all its runs (a still-running parent keeps the
		// task "running" until it too is swept).
		s.endRun(ctx, r.ID, r.TaskID, "needs_attention", "orphaned by daemon restart")
	}
}

// reEvalQueuedRuns scans pending runs with non-NULL waiting_on and
// re-checks the conflict guard for each. Eligible ones get their
// waiting_on cleared and are launched via dispatchExisting.
//
// Called from tick() (every 1s, before new-task dispatch) and once at
// Loop startup for daemon-restart recovery (stale "running" runs from
// the previous daemon aren't in the in-memory Guard, so queued runs
// all proceed immediately).
//
// Callers must hold s.mu.
func (s *Scheduler) reEvalQueuedRuns(ctx context.Context) {
	if s.d.guard == nil || !s.d.cfg.Cfg.ConflictGuard.Enabled {
		return
	}
	queued, err := s.d.store.ListPendingWithWaitingOn(ctx)
	if err != nil {
		log.Printf("scheduler: list pending+waiting_on: %v", err)
		return
	}
	for _, run := range queued {
		predJSON, err := s.d.store.GetPredictionJSON(ctx, run.ID)
		if err != nil {
			log.Printf("scheduler: get prediction %s: %v", run.ID, err)
			continue
		}
		var pred predictor.Result
		if jerr := json.Unmarshal(predJSON, &pred); jerr != nil {
			log.Printf("scheduler: unmarshal prediction %s: %v", run.ID, jerr)
			continue
		}
		dec := s.d.guard.CheckAndReserve(run.ID, pred.Files)
		if !dec.Proceed {
			// Update waiting_on to the current blockers (may have
			// changed since last check — e.g. rA finished but rC
			// started and touches the same file).
			if err := s.d.store.SetWaitingOn(ctx, run.ID, dec.WaitingOn); err != nil {
				log.Printf("scheduler: update waiting_on %s: %v", run.ID, err)
			}
			continue
		}
		// Eligible: clear waiting_on then launch via dispatchExisting.
		if err := s.d.store.SetWaitingOn(ctx, run.ID, nil); err != nil {
			log.Printf("scheduler: clear waiting_on %s: %v", run.ID, err)
			s.d.guard.Release(run.ID) // roll back reservation
			continue
		}
		if err := s.dispatchExisting(ctx, run, &pred); err != nil {
			log.Printf("scheduler: dispatchExisting %s: %v", run.ID, err)
			s.d.guard.Release(run.ID)
			// Re-set waiting_on so we'll try again next tick.
			_ = s.d.store.SetWaitingOn(ctx, run.ID, dec.WaitingOn)
		}
	}
}

// Resume re-launches the pipeline against the existing worktree of a
// run in {needs_attention, error, abandoned}. The pipeline runs from
// stage 0 (dumb resume — pipeline-agnostic). State preserved: cost,
// stalls, runs.prediction, prior stages. Idempotent if the caller
// retries on a transient error; not idempotent if the run completes
// between calls (second call returns "not resumable").
//
// Skips ClaimTask + InsertRun + worktree create + Predictor + conflict
// guard. The user explicitly asked to retry; manual override semantics
// apply.
func (s *Scheduler) Resume(ctx context.Context, runID string) error {
	run, err := s.d.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("run not found")
		}
		return err
	}
	if run.ParentRunID != "" {
		return fmt.Errorf("child run; resume the parent instead")
	}
	switch run.Status {
	case "needs_attention", "error", "abandoned":
		// resumable
	default:
		return fmt.Errorf("run not resumable in state %s; use hive_run_now for a fresh attempt", run.Status)
	}

	worktreePath := filepath.Join(s.d.HiveDir(), "worktrees", runID)
	if _, err := os.Stat(worktreePath); err != nil {
		return fmt.Errorf("worktree missing for run %s (expected at %s); use hive_run_now", runID, worktreePath)
	}
	runtimeDir := filepath.Join(s.d.HiveDir(), runID)

	task, err := s.d.store.GetTask(ctx, run.TaskID)
	if err != nil {
		return fmt.Errorf("task missing: %w", err)
	}
	proj, err := s.d.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return fmt.Errorf("project lookup failed: %w", err)
	}
	// proj == nil is defensive — GetProject always returns ErrNotFound, never (nil, nil)
	if proj == nil {
		return fmt.Errorf("project %s not found", task.ProjectID)
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return fmt.Errorf("project %s has no repo_path configured", proj.ID)
	}
	repoPath := *proj.RepoPath

	branchName := ""
	if out, gitErr := exec.CommandContext(ctx, "git", "-C", worktreePath, "symbolic-ref", "--short", "HEAD").CombinedOutput(); gitErr == nil {
		branchName = strings.TrimSpace(string(out))
	}

	var pred *predictor.Result
	if payload, predErr := s.d.store.GetPredictionJSON(ctx, runID); predErr == nil && len(payload) > 0 {
		var p predictor.Result
		if err := json.Unmarshal(payload, &p); err == nil {
			pred = &p
		}
	}

	pipelineName := run.Pipeline
	pipe, ok := s.d.pipelines[pipelineName]
	if !ok {
		return fmt.Errorf("unknown pipeline %q (run was originally dispatched with it; daemon may have been upgraded)", pipelineName)
	}

	pd := &pendingDispatch{runID: runID, run: run, task: task, proj: proj, repoPath: repoPath}
	if err := s.markRunStartedAndPublish(ctx, pd, pipelineName); err != nil {
		return err
	}

	s.d.goTracked(func() {
		s.executePipeline(pipe, run, task, proj, worktreePath, branchName, runtimeDir, repoPath, pred)
	})

	return nil
}

// LastTickAt returns the timestamp of the most recent scheduler tick.
// Zero if no tick has run yet. Used by doctor.health to detect a
// wedged scheduler loop.
func (s *Scheduler) LastTickAt() time.Time {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	return s.lastTickAt
}
