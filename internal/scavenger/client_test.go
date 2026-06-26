package scavenger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
)

// buildFakeScavenger builds the fake-scavenger stub once per test run and
// returns the cached binary path. The temp dir is intentionally NOT a
// t.TempDir() because it must outlive a single test; the OS reclaims it.
func buildFakeScavenger(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fake-scavenger")
		if err != nil {
			fakeErr = err
			return
		}
		bin := filepath.Join(dir, "fake-scavenger")
		if out, berr := exec.Command("go", "build", "-o", bin, "../../scripts/fake-scavenger").CombinedOutput(); berr != nil {
			fakeErr = fmt.Errorf("build fake-scavenger: %v\n%s", berr, out)
			return
		}
		fakePath = bin
	})
	if fakeErr != nil {
		t.Fatalf("%v", fakeErr)
	}
	return fakePath
}

func TestClientHealthCheckWithFakeBinary(t *testing.T) {
	fake := buildFakeScavenger(t)
	c := NewClient(Config{Binary: fake})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}
}

func TestIndexWorktreeRunsInCwd(t *testing.T) {
	fake := buildFakeScavenger(t)
	wt := t.TempDir()
	c := NewClient(Config{Binary: fake})
	if err := c.IndexWorktree(context.Background(), wt); err != nil {
		t.Fatalf("IndexWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".scavenger", "indexes")); err != nil {
		t.Errorf("expected .scavenger/indexes created in worktree: %v", err)
	}
}

func TestIndexWorktreeFailureIsReturned(t *testing.T) {
	fake := buildFakeScavenger(t)
	wt := t.TempDir()
	t.Setenv("FAKE_SCAVENGER_INDEX_FAIL", "1")
	c := NewClient(Config{Binary: fake})
	if err := c.IndexWorktree(context.Background(), wt); err == nil {
		t.Fatal("expected error on forced index failure")
	}
}

func TestInstallPluginCreatesWorktreePlugin(t *testing.T) {
	fake := buildFakeScavenger(t)
	wt := t.TempDir()
	c := NewClient(Config{Binary: fake})
	if err := c.InstallPlugin(context.Background(), wt); err != nil {
		t.Fatalf("InstallPlugin: %v", err)
	}
	marker := filepath.Join(wt, ".scavenger", "claude-plugin", ".claude-plugin", "plugin.json")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("expected plugin marker created: %v", err)
	}
}

func TestStartStopDaemonTracksPerWorktree(t *testing.T) {
	fake := buildFakeScavenger(t)
	wt := t.TempDir()
	c := NewClient(Config{Binary: fake, SocketWaitTimeout: 50 * time.Millisecond})
	if err := c.StartDaemon(context.Background(), wt); err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}
	if n := c.activeDaemonCount(); n != 1 {
		t.Errorf("active daemons = %d, want 1", n)
	}
	if err := c.StopDaemon(wt); err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if n := c.activeDaemonCount(); n != 0 {
		t.Errorf("active daemons after stop = %d, want 0", n)
	}
}

func TestStartDaemonRespectsCap(t *testing.T) {
	fake := buildFakeScavenger(t)
	c := NewClient(Config{Binary: fake, MaxConcurrentDaemons: 1, SocketWaitTimeout: 50 * time.Millisecond})
	wt1, wt2 := t.TempDir(), t.TempDir()
	if err := c.StartDaemon(context.Background(), wt1); err != nil {
		t.Fatalf("StartDaemon wt1: %v", err)
	}
	err := c.StartDaemon(context.Background(), wt2)
	if !errors.Is(err, ErrDaemonCapReached) {
		t.Fatalf("StartDaemon wt2 = %v, want ErrDaemonCapReached", err)
	}
	_ = c.StopDaemon(wt1)
}

// TestInstallPluginSelfIgnoresScavengerDir verifies InstallPlugin leaves the
// .scavenger/ state dir self-ignored (a ".gitignore" of "*") so git — and code
// reviewers running `git status` — never surface it as an untracked dir in the
// worktree.
func TestInstallPluginSelfIgnoresScavengerDir(t *testing.T) {
	fake := buildFakeScavenger(t)
	wt := t.TempDir()
	c := NewClient(Config{Binary: fake})
	if err := c.InstallPlugin(context.Background(), wt); err != nil {
		t.Fatalf("InstallPlugin: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(wt, ".scavenger", ".gitignore"))
	if err != nil {
		t.Fatalf("expected .scavenger/.gitignore: %v", err)
	}
	if string(b) != "*\n" {
		t.Errorf(".scavenger/.gitignore = %q, want %q", string(b), "*\n")
	}
}
