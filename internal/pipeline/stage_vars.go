package pipeline

import (
	"context"
	"log"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// stageVarMaxFiles is a soft cap: above it, scoping a test gate buys nothing (the
// run touched most of the repo). We still substitute the FULL list — truncating
// would silently under-test — and just log that the gate wasn't narrowed.
const stageVarMaxFiles = 500

// renderStageCommand substitutes pipeline template variables into a shell stage
// command. Fast-path returns cmd unchanged when it has no "{{" token. Tokens:
//
//	{{target_branch}} -> the resolved target branch (empty -> "main")
//	{{changed_files}} -> shell-quoted, space-joined paths this run changed
//	{{changed_dirs}}  -> shell-quoted, space-joined deduped parent dirs of those
//
// "Changed" = the working-tree delta from HEAD (the worktree's fork point — the
// agent's edits stay uncommitted during the loop) plus untracked files. A git
// failure makes the path vars empty (fail-soft); the stage still runs.
func renderStageCommand(ctx context.Context, cmd, targetBranch, worktree string) string {
	if !strings.Contains(cmd, "{{") {
		return cmd
	}
	out := substituteTargetBranch(cmd, targetBranch)
	if strings.Contains(out, "{{changed_files}}") || strings.Contains(out, "{{changed_dirs}}") {
		files := changedFiles(ctx, worktree)
		if len(files) > stageVarMaxFiles {
			log.Printf("pipeline: %d changed files exceed scope cap %d; test gate not narrowed", len(files), stageVarMaxFiles)
		}
		out = strings.ReplaceAll(out, "{{changed_files}}", joinQuoted(files))
		out = strings.ReplaceAll(out, "{{changed_dirs}}", joinQuoted(changedDirs(files)))
	}
	return out
}

// changedFiles returns the run's changed files (modified vs HEAD + untracked),
// deduped + sorted. Empty on any git error.
//
// -z makes git emit NUL-separated raw paths, bypassing core.quotePath escaping
// (which would wrap non-ASCII names in escaped double-quotes by default).
func changedFiles(ctx context.Context, worktree string) []string {
	set := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "-z", "HEAD"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", worktree}, args...)...).Output()
		if err != nil {
			continue
		}
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				set[f] = true
			}
		}
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// changedDirs returns the deduped, sorted parent dirs of files (repo-root -> ".").
func changedDirs(files []string) []string {
	set := map[string]bool{}
	for _, f := range files {
		set[filepath.Dir(f)] = true
	}
	dirs := make([]string, 0, len(set))
	for d := range set {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// joinQuoted single-quotes each path (bash-safe) and space-joins. An embedded
// single quote is escaped via the '\” idiom.
func joinQuoted(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}
