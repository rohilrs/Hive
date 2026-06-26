package claudecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"
)

// ErrImplementStagnation is returned by RunStage when an implement-stage
// subprocess is killed because the worktree showed no NEW content for
// StallDiffStagnation — the agent is stuck/looping (not producing code).
var ErrImplementStagnation = errors.New("implement stage made no progress")

type implementStagnationError struct{ d time.Duration }

func (e *implementStagnationError) Error() string {
	return fmt.Sprintf("implement made no code progress for %s (the agent is likely stuck or looping; killed before the stage timeout)", e.d)
}
func (e *implementStagnationError) Unwrap() error { return ErrImplementStagnation }

// worktreeProgressHash hashes the worktree's current changes — tracked
// modifications (`git diff HEAD`) plus the contents of untracked files (the new
// files the agent created). Returns "" when there are NO changes yet (so the
// caller can distinguish "still planning / nothing written" from a stable diff).
// Best-effort: any git error returns "" (no signal). Untracked files larger than
// maxFileBytes are hashed by path+size only (cheap; avoids reading huge blobs).
func worktreeProgressHash(ctx context.Context, worktree string) string {
	const maxFileBytes = 1 << 20 // 1 MiB
	h := sha256.New()
	wrote := false
	diff, err := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "HEAD").Output()
	if err != nil {
		return ""
	}
	if len(diff) > 0 {
		h.Write(diff)
		wrote = true
	}
	others, err := exec.CommandContext(ctx, "git", "-C", worktree, "ls-files", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return ""
	}
	for _, p := range bytes.Split(others, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		h.Write(p)
		wrote = true
		full := filepath.Join(worktree, string(p))
		if fi, serr := os.Stat(full); serr == nil && fi.Size() <= maxFileBytes {
			if b, rerr := os.ReadFile(full); rerr == nil {
				h.Write(b)
			}
		} else if serr == nil {
			fmt.Fprintf(h, "|%d", fi.Size()) // large file: path+size only
		}
	}
	if !wrote {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stagnationTracker decides when the worktree has stopped progressing. observe
// is called once per poll with the current progress hash. It returns true once
// the hash has been non-empty AND unchanged for >= timeout.
type stagnationTracker struct {
	timeout    time.Duration
	lastHash   string
	lastChange time.Time
	started    bool
}

func (t *stagnationTracker) observe(hash string, now time.Time) bool {
	if hash != "" {
		t.started = true
	}
	if hash != t.lastHash {
		t.lastHash = hash
		t.lastChange = now
		return false
	}
	return t.started && now.Sub(t.lastChange) >= t.timeout
}

// watchDiffStagnation polls the worktree progress hash; on stagnation it sets
// killed and SIGTERMs the subprocess. Returns when ctx is done or it fired.
func watchDiffStagnation(ctx context.Context, worktree string, poll, timeout time.Duration, sub Signaller, killed *atomic.Bool) {
	tr := &stagnationTracker{timeout: timeout, lastChange: time.Now()}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if tr.observe(worktreeProgressHash(ctx, worktree), now) {
				killed.Store(true)
				_ = sub.Signal("SIGTERM")
				return
			}
		}
	}
}
