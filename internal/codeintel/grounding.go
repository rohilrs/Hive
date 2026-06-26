package codeintel

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ensureGrounding makes a read-only detached worktree of ref at groundDir,
// scavenger-indexed via indexFn, and returns groundDir. It re-indexes only when
// ref's SHA differs from the last index (tracked in a sibling "<groundDir>.sha"
// marker so the worktree tree stays clean). repoPath is the canonical repo.
func ensureGrounding(ctx context.Context, repoPath, ref, groundDir string, indexFn func(context.Context, string) error) (string, error) {
	sha, err := gitOut(ctx, repoPath, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	if err := os.MkdirAll(filepath.Dir(groundDir), 0o755); err != nil {
		return "", err
	}
	if _, statErr := os.Stat(filepath.Join(groundDir, ".git")); statErr != nil {
		if out, e := gitOut(ctx, repoPath, "worktree", "add", "--detach", groundDir, sha); e != nil {
			return "", fmt.Errorf("worktree add: %v (%s)", e, out)
		}
	} else {
		// Worktree exists — only checkout if HEAD isn't already at sha (avoids a
		// no-op git subprocess on the common unchanged-ref path).
		if head, _ := gitOut(ctx, groundDir, "rev-parse", "HEAD"); head != sha {
			if out, e := gitOut(ctx, groundDir, "checkout", "--detach", sha); e != nil {
				return "", fmt.Errorf("worktree checkout %s: %v (%s)", sha, e, out)
			}
		}
	}
	marker := groundDir + ".sha"
	if prev, _ := os.ReadFile(marker); strings.TrimSpace(string(prev)) == sha {
		return groundDir, nil
	}
	if err := indexFn(ctx, groundDir); err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	if werr := os.WriteFile(marker, []byte(sha+"\n"), 0o644); werr != nil {
		log.Printf("codeintel: write grounding marker %s: %v (will re-index next call)", marker, werr)
	}
	return groundDir, nil
}

// gitOut runs a git subcommand in dir, returning trimmed combined output.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// scavengerIndex shells `scavenger index` in dir, bounded by timeout.
func scavengerIndex(binary string, timeout time.Duration) func(context.Context, string) error {
	return func(ctx context.Context, dir string) error {
		bin := binary
		if bin == "" {
			bin = "scavenger"
		}
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		cmd := exec.CommandContext(ctx, bin, "index")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("scavenger index: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}
