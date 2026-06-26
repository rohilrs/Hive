package daemon

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rohilrs/Hive/internal/codeintel"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/store"
)

// effectiveConfigForProject returns the project's effective config: the global
// config overlaid with ~/.hive/projects/<slug>/config.toml (env overrides are
// NOT re-applied — they were applied at boot and per-project TOML wins). On any
// load error it falls back to the in-memory global config. This is the single
// per-project config resolver reused by dispatch + the integration helpers.
func (d *Daemon) effectiveConfigForProject(slug string) *config.Config {
	repoKey := ""
	if proj, perr := d.store.GetProjectBySlug(context.Background(), slug); perr == nil && proj.RepoPath != nil {
		repoKey = config.RepoKey(*proj.RepoPath)
	}
	if reloaded, err := config.Load(config.LoadOptions{
		ConfigDir:   d.cfg.HiveDir,
		RepoKey:     repoKey,
		ProjectSlug: slug,
		SkipEnv:     true,
	}); err == nil {
		return reloaded
	} else {
		log.Printf("config: per-project reload for %s failed: %v (using global)", slug, err)
		return d.cfg.Cfg
	}
}

// plannerGrounderFor builds the codebase Grounder for a planner/decompose
// session: search + capsules against the project's FEATURE branch when one
// exists (where an ongoing initiative's latest work lives), else the TARGET
// branch — with the grounding worktree under <hiveDir>/grounding/<slug>/.
// Returns nil when there is no repo path (grounding then degrades to
// unavailable in the tools).
func (d *Daemon) plannerGrounderFor(slug, repoPath string) *codeintel.Grounder {
	if repoPath == "" {
		return nil
	}
	ref := d.effectiveDocsRef(slug, repoPath)
	groundDir := filepath.Join(d.cfg.HiveDir, "grounding", slug)
	sc := d.cfg.Cfg.Scavenger
	return codeintel.NewGrounder(repoPath, ref, groundDir, sc.Binary, sc.Enabled,
		time.Duration(sc.IndexTimeoutSeconds)*time.Second)
}

// effectiveDocsRef resolves the git ref where a project's in-repo docs AND its
// latest code live: the feature branch when CONFIGURED and EXISTING (local or
// origin) — that's where an ongoing initiative's roadmap/specs + accumulated
// work sit — else the target branch (defaults to "main" when unset). An
// origin-only branch (fresh clone, or a shared repo whose working tree is
// checked out on ANOTHER project's branch) resolves to its remote-tracking ref,
// since a bare name only matches a local branch. Returns "" only for a repoless
// project. Used by both grounding and roadmap/spec reads so reads don't depend
// on which branch the (possibly shared) working tree happens to be on.
func (d *Daemon) effectiveDocsRef(slug, repoPath string) string {
	if repoPath == "" {
		return ""
	}
	ctx := context.Background()
	branch := d.scheduler.effectiveTargetBranchForProject(slug)
	if fb := d.scheduler.effectiveFeatureBranchForProject(slug); fb != "" {
		if refExistsGit(ctx, repoPath, fb) || refExistsGit(ctx, repoPath, "refs/remotes/origin/"+fb) {
			branch = fb
		}
	}
	ref := branch
	if !refExistsGit(ctx, repoPath, branch) && refExistsGit(ctx, repoPath, "refs/remotes/origin/"+branch) {
		// Freshen origin/<branch> (throttled, best-effort) so reads reflect the
		// latest pushed commit, not a stale remote-tracking ref.
		d.maybeFetchGroundBranch(repoPath, branch)
		ref = "origin/" + branch
	}
	return ref
}

// groundFetchThrottle bounds how often the planner grounder fetches a branch.
const groundFetchThrottle = 60 * time.Second

// maybeFetchGroundBranch best-effort fetches origin/<branch> so the grounder's
// remote-tracking ref is current, throttled to at most once per groundFetchThrottle
// per (repo,branch) — plannerGrounderFor runs per planner tool-call, so an
// unthrottled fetch would hit the network every turn. Returns true when it did
// NOT throttle (i.e. attempted a fetch). Failures are logged, never propagated:
// grounding proceeds against the cached ref. Capped at 10s so a slow/offline
// remote can't hang the planner.
func (d *Daemon) maybeFetchGroundBranch(repo, branch string) (attempted bool) {
	key := repo + "\x00" + branch
	if v, ok := d.groundFetchAt.Load(key); ok {
		if last, ok := v.(time.Time); ok && time.Since(last) < groundFetchThrottle {
			return false
		}
	}
	d.groundFetchAt.Store(key, time.Now()) // store before fetch so a slow/concurrent call still throttles
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "--quiet", "origin", branch).CombinedOutput(); err != nil {
		log.Printf("planner-ground: fetch origin %s: %v (%s) — grounding the cached ref", branch, err, strings.TrimSpace(string(out)))
	}
	return true
}

// effectiveFeatureBranchForProject resolves the integration feature branch for a
// project. "" when unset — the feature is disabled and the project keeps its
// current target-branch behavior.
func (s *Scheduler) effectiveFeatureBranchForProject(slug string) string {
	return s.d.effectiveConfigForProject(slug).Integration.FeatureBranch
}

// effectiveWorktreeBaseForProject is the branch a task's worktree forks from
// AND the base its finish-branch PR targets: the integration feature branch when
// set, else the target branch (current behavior). One helper means dispatch and
// finish-branch agree on the fork/PR-base point.
func (s *Scheduler) effectiveWorktreeBaseForProject(slug string) string {
	if fb := s.effectiveFeatureBranchForProject(slug); fb != "" {
		return fb
	}
	return s.effectiveTargetBranchForProject(slug)
}

// taskAutoIntegrateForProject reports whether a project opts into auto-chaining
// finish-branch → PR → CI → merge after each successful build run.
func (s *Scheduler) taskAutoIntegrateForProject(slug string) bool {
	return s.d.effectiveConfigForProject(slug).Integration.TaskAutoIntegrate
}

// resolveAutoForProject reports whether a project opts into auto-dispatching a
// resolve run when an auto-merge hits a content conflict ([pipelines.resolve]
// auto). Read through the per-project effective config so a project override
// takes precedence over the global default.
func (s *Scheduler) resolveAutoForProject(slug string) bool {
	return s.d.effectiveConfigForProject(slug).Pipelines.Resolve.Auto
}

// isMergeConflictErr reports whether a prGateway.Merge error is a content
// conflict — the case the resolve pipeline can fix — as opposed to branch
// protection, required-checks, auth, or network failures.
//
// The ghPRGateway.Merge error format is:
//
//	"gh pr merge <url>: exit status 1: <gh stderr>"
//
// On a conflicting / DIRTY PR, gh stderr contains one of:
//   - "Pull Request is not mergeable"
//   - "merge conflict"
//   - "merge commit cannot be created"
//
// Non-conflict failures (branch protection, required checks not green, auth,
// network) have very different messages and do NOT contain any of these.
func isMergeConflictErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "conflict") ||
		strings.Contains(s, "not mergeable") ||
		strings.Contains(s, "merge commit cannot be created")
}

// isAlreadyMergedErr reports whether a prGateway.Merge error means the PR was
// ALREADY merged/closed (e.g. a human or external merge-queue merged it, or a
// duplicate merge attempt). This is success-equivalent for the queue — it must
// NOT trigger the conflict resolver.
func isAlreadyMergedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// "is not open" (not bare "not open") — anchored to gh's PR phrasing
	// ("Pull request #N is not open") so an unrelated error that happens to
	// contain "not open" (e.g. a connection/socket error) can't be misread as
	// success-equivalent and silently swallow a real merge failure.
	return strings.Contains(s, "already merged") ||
		strings.Contains(s, "is not open") ||
		strings.Contains(s, "pull request is closed")
}

// maybeAutoIntegrate chains finish-branch → PR → CI → auto-merge for a completed
// build run when the project opts into task_auto_integrate and has a feature
// branch — independent of dispatch mode. It is the NON-sequenced counterpart to
// handleSequencedBuildEnd: the caller invokes it only when the sequenced path
// did not take ownership, so the two never double-chain. Returns true if it
// chained (caller then skips the plain done/needs_attention status write,
// mirroring the sequenced path). Sets *transferred so the worktree teardown is
// handed to the finish-branch goroutine.
func (s *Scheduler) maybeAutoIntegrate(ctx context.Context, run *store.Run, task *store.Task, proj *store.Project, result *pipeline.Result, worktreePath, branchName string, transferred *bool) bool {
	if run.Pipeline != "build" || result.Status != "done" {
		return false
	}
	if s.effectiveFeatureBranchForProject(proj.Slug) == "" || !s.taskAutoIntegrateForProject(proj.Slug) {
		return false
	}
	_ = s.d.store.UpdateTaskStatus(ctx, task.ID, "running")
	*transferred = true
	s.chainFinishBranch(run, task, proj, worktreePath, branchName)
	return true
}

// gitC runs a git command in repoPath, returning trimmed combined output.
func gitC(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// fetchForHealth refreshes origin refs so a branch-health check reflects LIVE
// origin state rather than the last-fetched snapshot: it updates the
// origin/<feature> + origin/<target> remote-tracking refs (so the local-feature
// vs origin-feature comparison is current) and fast-forwards the local target
// to origin/target (so the feature..target "behind" count is current too).
// Best-effort: a fetch failure (offline, no remote, target checked out or
// diverged) leaves the check on cached refs rather than erroring.
func fetchForHealth(repo, feature, target string) {
	_, _ = gitC(repo, "fetch", "origin", feature, target)
	_, _ = gitC(repo, "fetch", "origin", target+":"+target)
}

// dirtyPathsExcluding returns the working-tree paths reported dirty by
// `git status --porcelain`, EXCLUDING excludeRel. Porcelain v1 lines are
// "XY <path>" (X/Y status columns, a space, then the path); renames/copies are
// "R  old -> new" (or "C  old -> new") for which we take the NEW path (the
// destination is what's present in the tree). On a git error it returns nil
// (treated as "no other dirty paths" — the caller's git ops will fail loudly if
// the repo is truly broken).
func dirtyPathsExcluding(repo, excludeRel string) []string {
	out, err := gitC(repo, "status", "--porcelain")
	if err != nil || out == "" {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		// Porcelain v1 is "XY <path>": two status columns, a space, then the
		// path. We can't rely on a fixed column offset because gitC trims the
		// combined output, dropping the leading space of a " M path" first line.
		// Strip leading spaces (a blank X column), then drop the status token up
		// to its trailing space, then trim — robust to 1- or 2-char status.
		line = strings.TrimLeft(line, " ")
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		path := strings.TrimSpace(line[sp+1:])
		// Rename/copy: "old -> new" — the destination path is what's in the tree.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+len(" -> "):]
		}
		// git may quote paths containing special chars; trim surrounding quotes.
		path = strings.Trim(path, "\"")
		if path == "" || path == excludeRel {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// syncLocalBaseAfterMerge fast-forwards the local <base> branch to origin/<base>
// after a task PR merged there, so the local checkout doesn't drift behind
// origin. Best-effort + SAFE: serialized under the repo git lock; skips if the
// branch has DIVERGED (local commits not on origin) or if the working tree has
// dirty files OTHER than the reconciler-owned roadmap doc (never disturbs the
// user's real work). The reconciler roadmap file is stashed (it regenerates),
// so its perpetual dirtiness doesn't block the FF.
func (d *Daemon) syncLocalBaseAfterMerge(repo, base, slug string) {
	unlock := lockRepoGit(repo)
	defer unlock()

	// 1. Refresh origin/<base> (best-effort; bail quietly on error).
	if _, err := gitC(repo, "fetch", "origin", base); err != nil {
		log.Printf("merge-detect: sync local %s: fetch origin %s failed: %v", base, base, err)
		return
	}

	// 2. behind := count <base>..origin/<base>; 0 => already synced.
	behindOut, err := gitC(repo, "rev-list", "--count", base+"..origin/"+base)
	if err != nil {
		log.Printf("merge-detect: sync local %s: behind count failed: %v", base, err)
		return
	}
	if strings.TrimSpace(behindOut) == "0" {
		return // already synced
	}

	// 3. ahead := count origin/<base>..<base>; >0 => diverged, never FF over it.
	aheadOut, err := gitC(repo, "rev-list", "--count", "origin/"+base+".."+base)
	if err != nil {
		log.Printf("merge-detect: sync local %s: ahead count failed: %v", base, err)
		return
	}
	if strings.TrimSpace(aheadOut) != "0" {
		log.Printf("merge-detect: local %s diverged from origin (%s local-only commit(s)); skipping FF", base, strings.TrimSpace(aheadOut))
		return
	}

	// 4. Refuse to disturb the user's real work: any dirty file OTHER than the
	//    reconciler-owned roadmap doc aborts the FF.
	roadmapRel := "docs/superpowers/roadmaps/" + slug + ".md"
	if other := dirtyPathsExcluding(repo, roadmapRel); len(other) > 0 {
		log.Printf("merge-detect: skip FF of local %s: uncommitted work (%s)", base, strings.Join(other, ", "))
		return
	}

	// 5. Fast-forward. If <base> is the checked-out branch we must move the
	//    working tree, which means clearing the reconciler roadmap dirtiness
	//    first; otherwise we can FF the ref directly without touching the tree.
	cur, err := gitC(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		log.Printf("merge-detect: sync local %s: rev-parse HEAD failed: %v", base, err)
		return
	}
	behind := strings.TrimSpace(behindOut)

	if strings.TrimSpace(cur) == base {
		// Is the reconciler roadmap dirty in the tree? (It's the only allowed
		// dirty path at this point — checked in step 4.)
		roadmapDirty := isPathDirty(repo, roadmapRel)
		stashed := false
		if roadmapDirty {
			if _, serr := gitC(repo, "stash", "push", "--", roadmapRel); serr != nil {
				log.Printf("merge-detect: sync local %s: stash roadmap failed: %v", base, serr)
				return
			}
			stashed = true
		}
		if out, merr := gitC(repo, "merge", "--ff-only", "origin/"+base); merr != nil {
			log.Printf("merge-detect: sync local %s: ff-only merge failed: %v (%s)", base, merr, out)
			if stashed {
				if _, perr := gitC(repo, "stash", "pop"); perr != nil {
					log.Printf("merge-detect: sync local %s: stash pop (restore) failed: %v", base, perr)
				}
			}
			return
		}
		if stashed {
			// Don't pop — origin's roadmap is now in place and the reconciler
			// regenerates it; popping the stale stash would conflict.
			if _, derr := gitC(repo, "stash", "drop"); derr != nil {
				log.Printf("merge-detect: sync local %s: stash drop failed: %v", base, derr)
			}
		}
	} else {
		// FF the local ref directly — no checkout, no working tree involved.
		if out, ferr := gitC(repo, "fetch", "origin", base+":"+base); ferr != nil {
			log.Printf("merge-detect: sync local %s: direct ref FF failed: %v (%s)", base, ferr, out)
			return
		}
	}

	log.Printf("merge-detect: fast-forwarded local %s to origin (%s commit(s))", base, behind)
}

// isPathDirty reports whether rel appears as a dirty path in `git status
// --porcelain` (modified, staged, or untracked).
func isPathDirty(repo, rel string) bool {
	out, err := gitC(repo, "status", "--porcelain", "--", rel)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// ensureFeatureBranch creates the feature branch from base when it doesn't exist
// (locally or on origin), or reports it already exists (adopt path).
// Returns (created bool, err). repoPath is the local checkout.
func ensureFeatureBranch(repoPath, feature, base string) (created bool, err error) {
	if _, e := gitC(repoPath, "rev-parse", "--verify", feature); e == nil {
		return false, nil // exists locally → adopt
	}
	if _, e := gitC(repoPath, "rev-parse", "--verify", "origin/"+feature); e == nil {
		if out, ce := gitC(repoPath, "branch", feature, "origin/"+feature); ce != nil {
			return false, fmt.Errorf("track origin/%s: %v (%s)", feature, ce, out)
		}
		return false, nil
	}
	if out, ce := gitC(repoPath, "branch", feature, base); ce != nil {
		return false, fmt.Errorf("create %s from %s: %v (%s)", feature, base, ce, out)
	}
	return true, nil
}

// commitDocToFeatureBranch stages + commits a single doc path on the feature
// branch (the checkout is assumed to be ON the feature branch). Best-effort:
// returns the error for logging; callers do not abort on failure. An identical
// re-save (nothing staged) is a no-op, not an error.
func commitDocToFeatureBranch(repoPath, relPath, message string) error {
	if out, err := gitC(repoPath, "add", relPath); err != nil {
		return fmt.Errorf("git add %s: %v (%s)", relPath, err, out)
	}
	if diff, _ := gitC(repoPath, "diff", "--cached", "--name-only"); diff == "" {
		return nil
	}
	if out, err := gitC(repoPath, "commit", "-m", message); err != nil {
		return fmt.Errorf("git commit: %v (%s)", err, out)
	}
	return nil
}

// pushFeatureBranch pushes the feature branch to origin, setting upstream.
func pushFeatureBranch(repoPath, feature string) error {
	if out, err := gitC(repoPath, "push", "-u", "origin", feature); err != nil {
		return fmt.Errorf("git push origin %s: %v (%s)", feature, err, out)
	}
	return nil
}

// repoGitLocks serializes git mutations per repo path. Callers MUST acquire this
// lock before any git operation that mutates HEAD or the working tree of the
// canonical checkout; it only serializes call sites that take it. Acquired by:
// the health.remediate RPC handler and roadmap plan-setup.
var repoGitLocks sync.Map // repoPath -> *sync.Mutex

// lockRepoGit acquires the per-repo git mutex and returns its unlock func.
func lockRepoGit(repoPath string) func() {
	mu, _ := repoGitLocks.LoadOrStore(repoPath, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// rebaseFeatureBranch checks out feature and rebases it onto target. On any
// failure it runs `git rebase --abort` so the repo is never left mid-rebase.
func rebaseFeatureBranch(repoPath, feature, target string) error {
	if out, err := gitC(repoPath, "checkout", feature); err != nil {
		return fmt.Errorf("checkout %s: %v (%s)", feature, err, out)
	}
	if out, err := gitC(repoPath, "rebase", target); err != nil {
		if aout, aerr := gitC(repoPath, "rebase", "--abort"); aerr != nil {
			return fmt.Errorf("rebase %s onto %s: %v (%s); abort also failed: %v (%s)", feature, target, err, out, aerr, aout)
		}
		return fmt.Errorf("rebase %s onto %s: %v (%s)", feature, target, err, out)
	}
	return nil
}

// mergeTargetIntoFeature checks out feature and merges target into it
// (--no-edit: non-interactive, accept the default merge message). On any
// failure it runs `git merge --abort`.
func mergeTargetIntoFeature(repoPath, feature, target string) error {
	if out, err := gitC(repoPath, "checkout", feature); err != nil {
		return fmt.Errorf("checkout %s: %v (%s)", feature, err, out)
	}
	if out, err := gitC(repoPath, "merge", "--no-edit", target); err != nil {
		if aout, aerr := gitC(repoPath, "merge", "--abort"); aerr != nil {
			return fmt.Errorf("merge %s into %s: %v (%s); abort also failed: %v (%s)", target, feature, err, out, aerr, aout)
		}
		return fmt.Errorf("merge %s into %s: %v (%s)", target, feature, err, out)
	}
	return nil
}
