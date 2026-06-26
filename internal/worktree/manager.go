// Package worktree owns git worktree lifecycle for Hive runs. Each run
// gets its own worktree at <WorktreeRoot>/run-<id>/ on a Hive-named
// branch (see BranchName).
package worktree

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// validRunID matches RunIDs that are safe to interpolate into worktree
// paths and git ref names: must start with alphanumeric, then alphanumeric
// + hyphen + underscore. Length 1-64. Rejects path traversal sequences
// (`..`, `/`) and git-ref-illegal chars (`:`, `~`, `^`, `?`, `*`, etc.).
var validRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type Config struct {
	// WorktreeRoot is the parent directory under which per-run worktrees
	// are created. Defaults to $HOME/.hive/worktrees when empty.
	WorktreeRoot string
}

type Manager struct {
	cfg Config
}

func NewManager(cfg Config) *Manager {
	if cfg.WorktreeRoot != "" {
		// Resolve symlinks once at construction so equality checks against
		// paths git reports later (which git also resolves) hold on macOS
		// where /tmp -> /private/tmp. If EvalSymlinks fails (e.g., the dir
		// doesn't exist yet), fall back to the original path; the first
		// Create will materialize it.
		if resolved, err := filepath.EvalSymlinks(cfg.WorktreeRoot); err == nil {
			cfg.WorktreeRoot = resolved
		}
	}
	return &Manager{cfg: cfg}
}

type CreateRequest struct {
	RunID      string // unique run identifier; used in directory and branch names
	RepoPath   string // absolute path to the source git repo
	BaseBranch string // branch to fork from (e.g. "main")
	TaskTitle  string // human-readable task title; slugified for branch name

	// BranchName is an optional explicit branch name for the new
	// worktree. When empty, the manager auto-generates via
	// BranchName(RunID, TaskTitle). Used by Linear-ingested tasks to
	// honor Linear's canonical branchName, closing the loop with
	// Linear's GH integration (commits on that branch auto-link back
	// to the Linear issue).
	BranchName string

	// FallbackBase is the branch to fork from when BaseBranch resolves
	// nowhere (neither origin/<base> nor a local ref). The common case is a
	// configured feature/integration branch that plan_setup hasn't created
	// yet, or one that was never pushed: forking from the target branch is
	// correct because the feature branch — if it existed — would itself be
	// based on the target, so there is no prior work to miss. Empty disables
	// the fallback (a missing BaseBranch then hard-errors).
	FallbackBase string
}

type Info struct {
	RunID      string
	Path       string // absolute path to the worktree
	BranchName string
}

// Create provisions a fresh worktree for a run. The worktree is at
// <WorktreeRoot>/run-<id>/, on a new branch named via BranchName().
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Info, error) {
	if req.RunID == "" {
		return nil, fmt.Errorf("run_id required")
	}
	if !validRunID.MatchString(req.RunID) {
		return nil, fmt.Errorf("invalid run_id %q: must match [A-Za-z0-9][A-Za-z0-9_-]{0,63}", req.RunID)
	}
	if req.RepoPath == "" {
		return nil, fmt.Errorf("repo_path required")
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}

	branch := strings.TrimSpace(req.BranchName)
	if branch == "" {
		branch = BranchName(req.RunID, req.TaskTitle)
	}
	if err := validateBranchName(branch); err != nil {
		return nil, err
	}

	// A prior run of this task may have left its branch behind: Manager.Remove
	// leaves branches for inspection, and Linear tasks reuse a FIXED branchName
	// across runs (hive/<run> names are unique per run and won't collide). Without
	// this, re-running such a task fails worktree-add with "branch already exists"
	// until the retention reclaimer eventually deletes it. Reclaim a leftover Hive
	// branch (+ its worktree, if any) so the re-run starts fresh.
	if refExists(ctx, req.RepoPath, "refs/heads/"+branch) {
		if err := m.reclaimStaleBranch(ctx, req.RepoPath, branch); err != nil {
			return nil, err
		}
	}

	wtPath := filepath.Join(m.cfg.WorktreeRoot, req.RunID)

	forkPoint, ferr := resolveForkPoint(ctx, req.RepoPath, req.BaseBranch, req.FallbackBase)
	if ferr != nil {
		return nil, ferr
	}

	cmd := exec.CommandContext(ctx, "git", "-C", req.RepoPath,
		"worktree", "add", "-b", branch, wtPath, forkPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w\n%s", err, out)
	}

	return &Info{
		RunID:      req.RunID,
		Path:       wtPath,
		BranchName: branch,
	}, nil
}

// refExists reports whether a git ref resolves in the repo (used to confirm
// origin/<base> is present after a fetch before forking from it).
func refExists(ctx context.Context, repoPath, ref string) bool {
	return exec.CommandContext(ctx, "git", "-C", repoPath,
		"rev-parse", "--verify", "--quiet", ref).Run() == nil
}

// resolveForkPoint picks the git ref a new worktree forks from. It prefers
// `base`, falling back to `fallback` (the target branch) only when `base`
// resolves NOWHERE — neither on origin nor locally. The previous code logged
// "forking from local base" on a failed fetch but left the fork point as the
// (still nonexistent) base name, so a missing/typo'd/unpushed feature branch
// died with a cryptic `git worktree add: invalid reference`. Returns an
// actionable error only when neither base nor fallback resolves.
func resolveForkPoint(ctx context.Context, repoPath, base, fallback string) (string, error) {
	if ref := resolveBranchRef(ctx, repoPath, base); ref != "" {
		return ref, nil
	}
	if fallback != "" && fallback != base {
		if ref := resolveBranchRef(ctx, repoPath, fallback); ref != "" {
			log.Printf("worktree: base branch %q resolves neither on origin nor locally; forking from fallback target %q instead",
				base, fallback)
			return ref, nil
		}
	}
	return "", fmt.Errorf("base branch %q does not exist on origin or locally (fallback %q also missing) — create or push it (e.g. via `hive plan`) before dispatching",
		base, fallback)
}

// resolveBranchRef returns the ref to fork from for a single branch: the
// up-to-date origin tip when present (best-effort fetch first — task PRs
// squash-merge into origin/<base>, advancing origin but never the local ref, so
// forking from local would start from a stale base and conflict on integration),
// else the local branch, else "" when the branch is unknown to this repo.
func resolveBranchRef(ctx context.Context, repoPath, branch string) string {
	if branch == "" {
		return ""
	}
	fetch := exec.CommandContext(ctx, "git", "-C", repoPath, "fetch", "--quiet", "origin", branch)
	if out, err := fetch.CombinedOutput(); err != nil {
		log.Printf("worktree: fetch origin/%s failed: %v (%s); trying local ref",
			branch, err, strings.TrimSpace(string(out)))
	}
	if originRef := "origin/" + branch; refExists(ctx, repoPath, originRef) {
		return originRef
	}
	if refExists(ctx, repoPath, "refs/heads/"+branch) {
		return branch
	}
	return ""
}

// ReclaimBranch removes the worktree holding `branch` (if any, and only when it's
// a Hive-owned worktree) and deletes the local branch. Used for post-merge
// cleanup so a merged task's worktree + local branch don't accumulate. Safe to
// call when nothing holds the branch (it just deletes the branch) or the branch
// is already gone (the branch -D error is the caller's to treat as best-effort).
func (m *Manager) ReclaimBranch(ctx context.Context, repoPath, branch string) error {
	return m.reclaimStaleBranch(ctx, repoPath, branch)
}

// reclaimStaleBranch removes a leftover local branch (and its worktree, if any)
// so a re-run of the same task can recreate it. SAFETY: if the branch is checked
// out by a worktree OUTSIDE this Manager's WorktreeRoot (e.g. the operator's main
// checkout, or a hand-made worktree), it refuses — that's not Hive's to clobber.
func (m *Manager) reclaimStaleBranch(ctx context.Context, repoPath, branch string) error {
	if wt := worktreeForBranch(ctx, repoPath, branch); wt != "" {
		resolved, wtRoot := wt, m.cfg.WorktreeRoot
		if r, err := filepath.EvalSymlinks(wt); err == nil {
			resolved = r
		}
		if r, err := filepath.EvalSymlinks(wtRoot); err == nil {
			wtRoot = r
		}
		if filepath.Dir(resolved) != wtRoot {
			return fmt.Errorf("branch %q is checked out by a non-Hive worktree at %s; refusing to clobber", branch, wt)
		}
		if out, err := exec.CommandContext(ctx, "git", "-C", repoPath,
			"worktree", "remove", "--force", wt).CombinedOutput(); err != nil {
			return fmt.Errorf("remove stale worktree %s: %w\n%s", wt, err, out)
		}
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", repoPath,
		"branch", "-D", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("delete stale branch %s: %w\n%s", branch, err, out)
	}
	return nil
}

// worktreeForBranch returns the absolute worktree path currently checked out on
// `branch`, or "" if none holds it. Parses `git worktree list --porcelain`
// (the `branch refs/heads/<name>` line follows each `worktree <path>` line).
func worktreeForBranch(ctx context.Context, repoPath, branch string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath,
		"worktree", "list", "--porcelain").Output()
	if err != nil {
		return ""
	}
	target := "refs/heads/" + branch
	var curPath string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			curPath = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "branch "):
			if strings.TrimSpace(strings.TrimPrefix(line, "branch ")) == target {
				return curPath
			}
		}
	}
	return ""
}

// Remove tears down the worktree for runID. repoPath is the source repo
// the worktree was created from. The branch is left behind for inspection;
// callers can `git branch -D` it separately if desired.
func (m *Manager) Remove(ctx context.Context, repoPath, runID string) error {
	if !validRunID.MatchString(runID) {
		return fmt.Errorf("invalid run_id %q", runID)
	}
	wtPath := filepath.Join(m.cfg.WorktreeRoot, runID)
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath,
		"worktree", "remove", "--force", wtPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w\n%s", err, out)
	}
	return nil
}

// List returns every Hive-managed worktree currently registered with the
// given repo (filters by `hive/` branch prefix and the configured root).
func (m *Manager) List(ctx context.Context, repoPath string) ([]*Info, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath,
		"worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelain(out, m.cfg.WorktreeRoot), nil
}

// parseWorktreePorcelain parses `git worktree list --porcelain` output and
// returns Hive-managed entries only (filtered by branch prefix + root prefix).
func parseWorktreePorcelain(out []byte, wtRoot string) []*Info {
	var infos []*Info
	var cur Info
	flush := func() {
		if cur.Path != "" && cur.BranchName != "" {
			// Resolve symlinks in the worktree path so equality holds on macOS
			// where /tmp -> /private/tmp. Falls back to the raw path if the
			// dir is gone (stale registration); the filepath.Dir comparison
			// below will then likely reject it, which is fine.
			resolvedPath := cur.Path
			if r, err := filepath.EvalSymlinks(cur.Path); err == nil {
				resolvedPath = r
			}
			if filepath.Dir(resolvedPath) == wtRoot &&
				strings.HasPrefix(cur.BranchName, "hive/") &&
				len(cur.BranchName) > len("hive/") {
				cur.RunID = runIDFromPath(resolvedPath)
				infos = append(infos, &Info{
					RunID: cur.RunID, Path: resolvedPath, BranchName: cur.BranchName,
				})
			}
		}
		cur = Info{}
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r") // CRLF tolerance
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			cur.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			cur.BranchName = strings.TrimPrefix(ref, "refs/heads/")
		}
	}
	flush()
	return infos
}

func runIDFromPath(p string) string {
	return filepath.Base(p)
}

// validateBranchName rejects names that would fail git or look surprising.
// Empty is rejected — callers should fall back to BranchName() BEFORE
// validation, so we should never see an empty string here.
//
// Reference: git-check-ref-format(1). We don't try to enforce every rule
// (refs/heads/ implied, etc.) — just the high-leverage ones for the
// strings that realistically reach us: operator-pasted values, Linear's
// branchName field, and our own slugifier output.
func validateBranchName(s string) error {
	if s == "" {
		return fmt.Errorf("branch name is empty")
	}
	if strings.Contains(s, "..") || strings.HasPrefix(s, "-") || strings.HasSuffix(s, ".") {
		return fmt.Errorf("invalid branch name %q", s)
	}
	if strings.ContainsAny(s, " \t\n:?*[]^~\\") {
		return fmt.Errorf("invalid branch name %q (contains forbidden character)", s)
	}
	return nil
}
