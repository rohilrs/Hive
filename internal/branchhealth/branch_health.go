package branchhealth

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// HealthReport is the result of comparing a feature branch against its target.
// All counts/paths are advisory; Clean is true only when nothing was flagged.
type HealthReport struct {
	Feature       string
	Target        string
	Behind        int      // commits target is ahead of feature (stale)
	Ahead         int      // commits feature is ahead of target (in-flight)
	ConflictPaths []string // paths that would conflict on a feature↔target merge
	OriginState   string   // "synced" | "absent" | "ahead" | "behind" | "diverged" | "unknown" | "no-remote"
	Dirty         bool     // uncommitted changes in the working tree
	Clean         bool     // nothing flagged
}

// CheckFeatureBranch inspects a feature branch against its target — read-only,
// never mutates the repo. repoPath is a git worktree/checkout; feature/target
// are local branch refs. ignoreDirtyPath is a repo-relative path (forward
// slashes, e.g. "docs/superpowers/roadmaps/conv-rework.md") that is excluded
// from the Dirty/Clean computation; pass "" to ignore nothing (preserves
// prior behavior).
func CheckFeatureBranch(repoPath, feature, target, ignoreDirtyPath string) (HealthReport, error) {
	rep := HealthReport{Feature: feature, Target: target, OriginState: "unknown"}
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	count := func(rangeSpec string) (int, error) {
		s, err := git("rev-list", "--count", rangeSpec)
		if err != nil {
			return 0, fmt.Errorf("rev-list %s: %v (%s)", rangeSpec, err, s)
		}
		return strconv.Atoi(s)
	}

	var err error
	if rep.Behind, err = count(feature + ".." + target); err != nil {
		return rep, err
	}
	if rep.Ahead, err = count(target + ".." + feature); err != nil {
		return rep, err
	}

	rep.ConflictPaths = mergeConflictPaths(repoPath, feature, target)

	rep.OriginState = originState(git, feature)

	if st, derr := git("status", "--porcelain"); derr == nil && st != "" {
		// Parse the porcelain output into paths so we can exclude ignoreDirtyPath.
		// gitC (and the local git helper) does strings.TrimSpace on the combined
		// output, which can strip the leading space of a " M path" first line. We
		// therefore TrimLeft spaces first, then split on the first space to
		// isolate the status token and extract the path — robust to "M path",
		// "?? path", "M  path", etc. For rename/copy lines ("old -> new") we take
		// the destination (new) path. git may double-quote paths with special
		// chars; we strip surrounding quotes.
		for _, line := range strings.Split(st, "\n") {
			if line == "" {
				continue
			}
			line = strings.TrimLeft(line, " ")
			sp := strings.IndexByte(line, ' ')
			if sp < 0 {
				continue
			}
			path := strings.TrimSpace(line[sp+1:])
			if idx := strings.Index(path, " -> "); idx >= 0 {
				path = path[idx+len(" -> "):]
			}
			path = strings.Trim(path, "\"")
			if path == "" {
				continue
			}
			if ignoreDirtyPath != "" && path == ignoreDirtyPath {
				continue
			}
			rep.Dirty = true
			break
		}
	}

	originOK := rep.OriginState == "synced" || rep.OriginState == "ahead" || rep.OriginState == "no-remote"
	rep.Clean = rep.Behind == 0 && len(rep.ConflictPaths) == 0 &&
		!rep.Dirty && originOK
	return rep, nil
}

// mergeConflictPaths uses the modern git merge-tree --write-tree form (git 2.38+)
// to detect conflicts without touching the working tree. It returns the conflicting
// file paths, or nil if the merge would be clean or if merge-tree itself errors.
func mergeConflictPaths(repoPath, feature, target string) []string {
	cmd := exec.Command("git", "merge-tree", "--write-tree", "--name-only", feature, target)
	cmd.Dir = repoPath
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err == nil {
		// Exit 0 = clean merge, no conflicts.
		return nil
	}

	// Check if this is a conflict (exit code 1) vs a genuine error (exit code > 1).
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		// Unexpected error — best-effort, leave ConflictPaths empty.
		return nil
	}

	// Exit code 1 = conflicts detected.
	// stdout format:
	//   <tree-OID>
	//   <conflicting-file-1>
	//   <conflicting-file-2>
	//   ...
	//   (blank line)
	//   informational messages...
	lines := strings.Split(stdout.String(), "\n")
	if len(lines) < 2 {
		return nil
	}

	// Skip the first line (tree OID), collect until first blank line.
	var paths []string
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		paths = append(paths, trimmed)
	}
	return paths
}

// originState returns the sync state of a branch relative to its origin remote.
// If the repo has no "origin" remote at all, returns "no-remote".
// If origin/<branch> doesn't exist (but origin does), returns "absent".
func originState(git func(...string) (string, error), branch string) string {
	// Check if the repo has an "origin" remote at all.
	remotes, err := git("remote")
	if err != nil {
		return "unknown"
	}
	hasOrigin := false
	for _, r := range strings.Split(remotes, "\n") {
		if strings.TrimSpace(r) == "origin" {
			hasOrigin = true
			break
		}
	}
	if !hasOrigin {
		return "no-remote"
	}

	// origin exists — check if the branch is tracked there.
	if _, err := git("rev-parse", "--verify", "origin/"+branch); err != nil {
		return "absent"
	}

	ahead, _ := git("rev-list", "--count", "origin/"+branch+".."+branch)
	behind, _ := git("rev-list", "--count", branch+"..origin/"+branch)
	a, b := ahead != "0" && ahead != "", behind != "0" && behind != ""
	switch {
	case a && b:
		return "diverged"
	case a:
		return "ahead"
	case b:
		return "behind"
	default:
		return "synced"
	}
}

// RenderHealthReport formats a HealthReport as operator-facing text.
func RenderHealthReport(r HealthReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Feature branch %q vs target %q:\n", r.Feature, r.Target)
	if r.Behind > 0 {
		fmt.Fprintf(&b, "  ⚠ %d commit(s) behind target (stale)\n", r.Behind)
	}
	if r.Ahead > 0 {
		fmt.Fprintf(&b, "  ℹ %d commit(s) ahead of target (in-flight work)\n", r.Ahead)
	}
	if len(r.ConflictPaths) > 0 {
		fmt.Fprintf(&b, "  ⚠ likely conflicts on promotion: %s\n", strings.Join(r.ConflictPaths, ", "))
	}
	if r.OriginState != "synced" && r.OriginState != "ahead" && r.OriginState != "no-remote" {
		fmt.Fprintf(&b, "  ⚠ origin: %s\n", r.OriginState)
	}
	if r.Dirty {
		b.WriteString("  ⚠ uncommitted changes in the working tree\n")
	}
	if r.Clean {
		b.WriteString("  ✓ clean\n")
	}
	return b.String()
}
