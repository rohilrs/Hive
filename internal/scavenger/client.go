package scavenger

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// ErrDaemonCapReached is returned by StartDaemon when MaxConcurrentDaemons
// would be exceeded. Callers treat it as non-fatal (skip the daemon).
var ErrDaemonCapReached = errors.New("scavenger: max concurrent daemons reached")

// daemonProc tracks one worktree-scoped `scavenger daemon` subprocess.
type daemonProc struct {
	cmd     *exec.Cmd
	logFile *os.File
}

// Client manages N worktree-scoped daemons keyed by worktree path.
// Safe for concurrent use after construction.
type Client struct {
	cfg Config

	mu      sync.Mutex
	daemons map[string]*daemonProc
}

// NewClient constructs a Client. Does not spawn anything.
func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg, daemons: make(map[string]*daemonProc)}
}

// IndexWorktree runs `scavenger index` with cwd = wtPath, building the
// full graph into the worktree's per-branch DB. Bounded by IndexTimeout.
func (c *Client) IndexWorktree(ctx context.Context, wtPath string) error {
	if c.cfg.IndexTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.IndexTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.cfg.Binary, "index")
	cmd.Dir = wtPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scavenger index (%s): %w\n%s", wtPath, err, out)
	}
	return nil
}

// InstallPlugin runs `scavenger init --plugin-only` with cwd = wtPath,
// creating <wtPath>/.scavenger/claude-plugin/ with no other side effects.
func (c *Client) InstallPlugin(ctx context.Context, wtPath string) error {
	cmd := exec.CommandContext(ctx, c.cfg.Binary, "init", "--plugin-only")
	cmd.Dir = wtPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scavenger init --plugin-only (%s): %w\n%s", wtPath, err, out)
	}
	// Self-ignore the .scavenger state dir. A ".gitignore" of "*" ignores every
	// file in the dir — including itself — so git treats the whole .scavenger/
	// as nonexistent. Without this it shows as an untracked dir in the worktree,
	// which code reviewers (running `git status`) flag as "internal state that
	// shouldn't be committed", wasting build iterations.
	gi := filepath.Join(wtPath, ".scavenger", ".gitignore")
	if err := os.WriteFile(gi, []byte("*\n"), 0o644); err != nil {
		return fmt.Errorf("scavenger self-ignore (%s): %w", wtPath, err)
	}
	return nil
}

// StartDaemon spawns `scavenger daemon start` with cwd = wtPath so the
// post-tool-use hook's reindex (which needs daemon.sock) works. Tracks the
// proc for StopDaemon. Returns ErrDaemonCapReached if MaxConcurrentDaemons
// would be exceeded. Idempotent per worktree.
func (c *Client) StartDaemon(ctx context.Context, wtPath string) error {
	c.mu.Lock()
	if _, ok := c.daemons[wtPath]; ok {
		c.mu.Unlock()
		return nil
	}
	if limit := c.cfg.MaxConcurrentDaemons; limit > 0 && len(c.daemons) >= limit {
		c.mu.Unlock()
		return ErrDaemonCapReached
	}
	cmd := exec.Command(c.cfg.Binary, "daemon", "start")
	cmd.Dir = wtPath
	var logFile *os.File
	if lp := c.daemonLogPath(wtPath); lp != "" {
		if err := os.MkdirAll(filepath.Dir(lp), 0o700); err == nil {
			if f, ferr := os.OpenFile(lp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
				logFile = f
				cmd.Stdout = f
				cmd.Stderr = f
			} else {
				log.Printf("scavenger: open daemon log %s: %v (output discarded)", lp, ferr)
			}
		}
	}
	if err := cmd.Start(); err != nil {
		c.mu.Unlock()
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("scavenger daemon start (%s): %w", wtPath, err)
	}
	go func() { _ = cmd.Wait() }()
	c.daemons[wtPath] = &daemonProc{cmd: cmd, logFile: logFile}
	c.mu.Unlock()

	// Readiness poll WITHOUT the lock (best-effort; non-fatal on timeout).
	socketWait := c.cfg.SocketWaitTimeout
	if socketWait <= 0 {
		socketWait = 10 * time.Second
	}
	healthCtx, cancel := context.WithTimeout(ctx, socketWait)
	defer cancel()
	for {
		if _, err := os.Stat(filepath.Join(wtPath, ".scavenger", "daemon.sock")); err == nil {
			return nil
		}
		select {
		case <-healthCtx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// StopDaemon stops the worktree-scoped daemon. Tries clean shutdown, then
// kills the tracked proc. Idempotent / safe if never started.
func (c *Client) StopDaemon(wtPath string) error {
	c.mu.Lock()
	dp := c.daemons[wtPath]
	delete(c.daemons, wtPath)
	c.mu.Unlock()
	if dp == nil {
		return nil
	}
	stop := exec.Command(c.cfg.Binary, "daemon", "stop")
	stop.Dir = wtPath
	stopErr := stop.Run()
	if dp.cmd != nil && dp.cmd.Process != nil {
		_ = dp.cmd.Process.Kill()
	}
	if dp.logFile != nil {
		_ = dp.logFile.Close()
	}
	if stopErr != nil {
		return fmt.Errorf("scavenger daemon stop (%s): %w", wtPath, stopErr)
	}
	return nil
}

// activeDaemonCount reports currently-tracked daemons (tests + doctor).
func (c *Client) activeDaemonCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.daemons)
}

// daemonLogPath derives a per-worktree log path. Worktrees live at
// <hiveDir>/worktrees/<run-id>/, so log to <hiveDir>/<run-id>/scavenger.log.
// Returns "" (discard) if the layout doesn't match.
func (c *Client) daemonLogPath(wtPath string) string {
	runID := filepath.Base(wtPath)
	wtParent := filepath.Dir(wtPath)
	hiveDir := filepath.Dir(wtParent)
	if filepath.Base(wtParent) != "worktrees" {
		return ""
	}
	return filepath.Join(hiveDir, runID, "scavenger.log")
}

// HealthCheck runs `scavenger doctor --format json` and reports any error.
// Used by tests and by `hive doctor` (Phase 7) to verify the integration.
func (c *Client) HealthCheck(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.cfg.Binary, "doctor", "--format", "json")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scavenger doctor: %w (%s)", err, out)
	}
	return nil
}
