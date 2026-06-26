package daemon

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/cleanup"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// graduateWorktreeMaxAge bounds how old a graduate worktree DIR must be before
// the periodic sweep reaps it. Set well past the WORST-CASE runGraduate — 6
// Stage-3 gates × graduateGateTimeout (30m) = 3h, plus the audit — so the
// periodic sweep can never clobber a (pathologically long) live run. The boot
// reap (minAge 0) catches leaks on the next restart regardless.
const graduateWorktreeMaxAge = 6 * time.Hour

// reapGraduateWorktrees removes leaked graduate worktree DIRECTORIES
// (<HiveDir>/graduate-<slug>-<unixnano>). A graduate worktree is created + removed
// within one runGraduate, so any that survives is a leak (a failed deferred
// `git worktree remove`). With minAge=0 (boot: none can be live) it reaps all; a
// positive minAge (periodic) reaps only dirs older than that, well past any real
// run. The persisted graduate-<slug>-result.{json,md} are FILES, not dirs, so the
// IsDir() check already excludes them.
func (d *Daemon) reapGraduateWorktrees(minAge time.Duration) {
	entries, err := os.ReadDir(d.HiveDir())
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "graduate-") {
			continue
		}
		if minAge > 0 {
			if info, ierr := e.Info(); ierr == nil && now.Sub(info.ModTime()) < minAge {
				continue
			}
		}
		dir := filepath.Join(d.HiveDir(), e.Name())
		// Best-effort: remove the registered worktree via its owning repo (derived
		// from the worktree's .git file), else just rm the directory.
		removed := false
		if repo := graduateWorktreeRepo(dir); repo != "" {
			if out, rerr := exec.Command("git", "-C", repo, "worktree", "remove", "--force", dir).CombinedOutput(); rerr != nil {
				log.Printf("cleanup: graduate worktree remove %s: %v (%s); rm fallback", dir, rerr, strings.TrimSpace(string(out)))
				removed = os.RemoveAll(dir) == nil
			} else {
				removed = true
			}
		} else {
			removed = os.RemoveAll(dir) == nil
		}
		if removed {
			log.Printf("cleanup: reaped leaked graduate worktree %s", dir)
		}
	}
}

// graduateWorktreeRepo derives the owning main repo from a linked worktree's
// `.git` FILE, which contains `gitdir: <repo>/.git/worktrees/<name>`. Returns ""
// when the dir isn't a git worktree (then the caller just rm's it).
func graduateWorktreeRepo(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	// line = <repo>/.git/worktrees/<name>  → strip "/.git/worktrees/<name>"
	if i := strings.Index(line, "/.git/worktrees/"); i > 0 {
		return line[:i]
	}
	return ""
}

// collectRunInfos projects every run into cleanup.RunInfo, resolving each
// run's source repo (for `git worktree remove` + `branch -D`) via its project.
func (d *Daemon) collectRunInfos(ctx context.Context) ([]cleanup.RunInfo, error) {
	runs, err := d.store.ListAllRuns(ctx)
	if err != nil {
		return nil, err
	}
	repoCache := map[string]string{}
	out := make([]cleanup.RunInfo, 0, len(runs))
	for _, r := range runs {
		repo, ok := repoCache[r.ProjectID]
		if !ok {
			if p, perr := d.store.GetProject(ctx, r.ProjectID); perr == nil && p.RepoPath != nil {
				repo = *p.RepoPath
			}
			repoCache[r.ProjectID] = repo
		}
		out = append(out, cleanup.RunInfo{
			ID: r.ID, Status: r.Status, CreatedAt: r.CreatedAt,
			ParentRunID: r.ParentRunID, RepoPath: repo, BranchName: r.BranchName,
		})
	}
	return out, nil
}

// runCleanupWith assembles the planner + reclaimer for a given retention.
func (d *Daemon) runCleanupWith(ctx context.Context, ret cleanup.Retention, dryRun bool) (cleanup.Result, cleanup.Plan, error) {
	runs, err := d.collectRunInfos(ctx)
	if err != nil {
		return cleanup.Result{}, cleanup.Plan{}, err
	}
	dirs, err := cleanup.EnumerateDirs(d.HiveDir())
	if err != nil {
		return cleanup.Result{}, cleanup.Plan{}, err
	}
	plan := cleanup.BuildPlan(runs, dirs, ret, time.Now())
	rec := &cleanup.Reclaimer{WT: d.wtMgr, Log: log.Printf}
	res := rec.Reclaim(ctx, plan, dryRun, ret.Branches)
	return res, plan, nil
}

// configRetention builds the Retention from [cleanup] config.
func (d *Daemon) configRetention() cleanup.Retention {
	c := d.cfg.Cfg.Cleanup
	return cleanup.Retention{
		KeepLastRuns: c.ResolvedKeepLastRuns(),
		OrphanGrace:  c.ResolvedOrphanGrace(),
		Branches:     c.ResolvedCleanBranches(),
	}
}

// sweepLoop periodically reclaims old terminal runs' worktrees + scratch so
// they don't accumulate over the daemon's whole lifetime (the boot sweep runs
// once). Same retention + safety as the boot sweep: respects keep_last_runs,
// never touches running runs. Disabled (boot-only) when sweep_interval_minutes
// is negative. Wired in Start alongside the boot sweep, under the same
// auto_sweep gate.
func (d *Daemon) sweepLoop(ctx context.Context) {
	iv := d.cfg.Cfg.Cleanup.ResolvedSweepInterval()
	if iv <= 0 {
		return
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.sweepRunArtifactsLabeled(ctx, "periodic sweep")
			d.reapGraduateWorktrees(graduateWorktreeMaxAge)
		}
	}
}

// sweepRunArtifacts is the best-effort boot sweep (wired in Start by Task 6).
func (d *Daemon) sweepRunArtifacts(ctx context.Context) {
	d.sweepRunArtifactsLabeled(ctx, "boot sweep")
	// At boot no graduate run can be live, so reap every leaked graduate worktree.
	d.reapGraduateWorktrees(0)
}

func (d *Daemon) sweepRunArtifactsLabeled(ctx context.Context, label string) {
	res, _, err := d.runCleanupWith(ctx, d.configRetention(), false)
	if err != nil {
		log.Printf("cleanup: %s failed: %v", label, err)
		return
	}
	if res.Runs > 0 {
		log.Printf("cleanup: %s reclaimed %d run(s), %d bytes", label, res.Runs, res.Bytes)
	}
	for _, e := range res.Errors {
		log.Printf("cleanup: %s: %s", label, e)
	}
}

type cleanupParams struct {
	DryRun   bool  `json:"dry_run"`
	KeepLast *int  `json:"keep_last"`
	Branches *bool `json:"branches"`
}

func (s *RPCServer) handleCleanupRun(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p cleanupParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
		}
	}
	ret := s.d.configRetention()
	if p.KeepLast != nil && *p.KeepLast >= 0 {
		ret.KeepLastRuns = *p.KeepLast
	}
	if p.Branches != nil {
		ret.Branches = *p.Branches
	}
	res, plan, err := s.d.runCleanupWith(ctx, ret, p.DryRun)
	if err != nil {
		return nil, internalErr(err)
	}
	items := make([]map[string]any, 0, len(plan.Reclaim))
	for _, it := range plan.Reclaim {
		items = append(items, map[string]any{"run_id": it.RunID, "reason": it.Reason})
	}
	out, _ := json.Marshal(map[string]any{
		"runs": res.Runs, "bytes": res.Bytes, "dry_run": p.DryRun,
		"kept": plan.Kept, "items": items, "errors": res.Errors,
	})
	return out, nil
}
