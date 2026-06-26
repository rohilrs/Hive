// Package codeintel grounds planning and decomposition in the project's actual
// codebase: git-grep search against a ref (no checkout) plus scavenger capsules
// from a cached, indexed grounding worktree at the project's target branch.
package codeintel

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Hit is one git-grep match.
type Hit struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// SearchCode runs `git grep` for pattern (a basic regex) against ref's tree in
// repoPath WITHOUT checking ref out. Returns up to maxHits matches; optional
// globs (e.g. "*.ts") restrict the pathspec. A no-match returns (nil, nil).
func SearchCode(ctx context.Context, repoPath, ref, pattern string, maxHits int, globs ...string) ([]Hit, error) {
	if pattern == "" {
		return nil, fmt.Errorf("codeintel.SearchCode: empty pattern")
	}
	if maxHits <= 0 {
		maxHits = 50
	}
	args := []string{"-C", repoPath, "grep", "-n", "-z", "-I", "--no-color", "-e", pattern, ref}
	if len(globs) > 0 {
		args = append(args, "--")
		args = append(args, globs...)
	}
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if ok && ee.ExitCode() == 1 && len(out) == 0 {
			return nil, nil // no match
		}
		if ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git grep: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git grep: %w", err)
	}
	return parseGrep(string(out), maxHits), nil
}

// parseGrep parses `git grep -n -z <pattern> <ref>` output. Each record is
// "<ref>:<file>\x00<lineno>\x00<content>", records separated by '\n'. The ref
// cannot contain ':' (git check-ref-format), so the first ':' in the first
// field separates the ref from a possibly-colon-containing file path.
func parseGrep(out string, maxHits int) []Hit {
	var hits []Hit
	for _, rec := range strings.Split(out, "\n") {
		if rec == "" {
			continue
		}
		fields := strings.SplitN(rec, "\x00", 3)
		if len(fields) < 3 {
			continue
		}
		ci := strings.IndexByte(fields[0], ':')
		if ci < 0 {
			continue
		}
		ln, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		hits = append(hits, Hit{File: fields[0][ci+1:], Line: ln, Snippet: strings.TrimSpace(fields[2])})
		if len(hits) >= maxHits {
			break
		}
	}
	return hits
}
