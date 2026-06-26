package config

import (
	"testing"
	"time"
)

func TestCleanupResolvers(t *testing.T) {
	var c Cleanup // zero value
	if !c.ResolvedAutoSweep() {
		t.Error("auto_sweep should default true")
	}
	if c.ResolvedKeepLastRuns() != 20 {
		t.Errorf("keep_last_runs default = %d, want 20", c.ResolvedKeepLastRuns())
	}
	if c.ResolvedOrphanGrace() != 60*time.Minute {
		t.Errorf("orphan grace default = %v, want 60m", c.ResolvedOrphanGrace())
	}
	if !c.ResolvedCleanBranches() {
		t.Error("clean_branches should default true")
	}
	f := false
	c = Cleanup{AutoSweep: &f, KeepLastRuns: 5, OrphanGraceMinutes: 10, CleanBranches: &f}
	if c.ResolvedAutoSweep() || c.ResolvedKeepLastRuns() != 5 || c.ResolvedOrphanGrace() != 10*time.Minute || c.ResolvedCleanBranches() {
		t.Errorf("explicit values not honored: %+v", c)
	}
}

func TestResolvedSweepInterval(t *testing.T) {
	if got := (Cleanup{}).ResolvedSweepInterval(); got != 30*time.Minute {
		t.Errorf("default sweep interval = %v, want 30m", got)
	}
	if got := (Cleanup{SweepIntervalMinutes: 5}).ResolvedSweepInterval(); got != 5*time.Minute {
		t.Errorf("explicit sweep interval = %v, want 5m", got)
	}
	if got := (Cleanup{SweepIntervalMinutes: -1}).ResolvedSweepInterval(); got != 0 {
		t.Errorf("negative sweep interval (disabled) = %v, want 0", got)
	}
}
