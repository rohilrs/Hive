package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	config "github.com/rohilrs/Hive/internal/config"
)

type fakeScav struct {
	mu       sync.Mutex
	calls    []string
	indexErr error
}

func (f *fakeScav) record(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeScav) IndexWorktree(_ context.Context, wt string) error {
	f.record("index:" + wt)
	return f.indexErr
}
func (f *fakeScav) InstallPlugin(_ context.Context, wt string) error { f.record("plugin:" + wt); return nil }
func (f *fakeScav) StartDaemon(_ context.Context, wt string) error   { f.record("start:" + wt); return nil }
func (f *fakeScav) StopDaemon(wt string) error                       { f.record("stop:" + wt); return nil }

func testCfgWithScav(enabled bool) Config {
	c := config.Default()
	c.Scavenger.Enabled = enabled
	c.Scavenger.IndexWorktreeOnRun = enabled
	return Config{Cfg: c} // daemon.Config wraps *config.Config in .Cfg
}

func TestPrepareScavengerWorkspaceOrder(t *testing.T) {
	f := &fakeScav{}
	s := &Scheduler{d: &Daemon{scavLifecycle: f, cfg: testCfgWithScav(true)}}
	s.prepareScavengerWorkspace(context.Background(), "/wt/run-1")
	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{"index:/wt/run-1", "plugin:/wt/run-1", "start:/wt/run-1"}
	if len(f.calls) != 3 || f.calls[0] != want[0] || f.calls[1] != want[1] || f.calls[2] != want[2] {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestPrepareScavengerWorkspaceDisabled(t *testing.T) {
	f := &fakeScav{}
	s := &Scheduler{d: &Daemon{scavLifecycle: f, cfg: testCfgWithScav(false)}}
	s.prepareScavengerWorkspace(context.Background(), "/wt/run-1")
	if len(f.calls) != 0 {
		t.Fatalf("expected no scav calls when disabled, got %v", f.calls)
	}
}

func TestPrepareScavengerWorkspaceContinuesOnIndexError(t *testing.T) {
	f := &fakeScav{indexErr: errors.New("boom")}
	s := &Scheduler{d: &Daemon{scavLifecycle: f, cfg: testCfgWithScav(true)}}
	s.prepareScavengerWorkspace(context.Background(), "/wt/run-1")
	f.mu.Lock()
	defer f.mu.Unlock()
	// Index errored, but plugin install + daemon start must STILL have run.
	want := []string{"index:/wt/run-1", "plugin:/wt/run-1", "start:/wt/run-1"}
	if len(f.calls) != 3 || f.calls[0] != want[0] || f.calls[1] != want[1] || f.calls[2] != want[2] {
		t.Fatalf("calls = %v, want %v (non-fatal continue broken)", f.calls, want)
	}
}
