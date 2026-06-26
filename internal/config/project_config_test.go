package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func readSched(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		t.Fatal(err)
	}
	sched, _ := doc["scheduler"].(map[string]any)
	return sched
}

func TestSetProjectScheduler(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "projects", "p1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(projDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[scheduler]\nauto_dispatch = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetProjectScheduler(dir, "p1", map[string]any{
		"dispatch_mode": "sequenced",
		"target_branch": "staging",
	}); err != nil {
		t.Fatal(err)
	}
	sched := readSched(t, cfgPath)
	if sched["dispatch_mode"] != "sequenced" || sched["target_branch"] != "staging" {
		t.Fatalf("got %+v", sched)
	}
	if sched["auto_dispatch"] != true {
		t.Errorf("auto_dispatch not preserved: %+v", sched)
	}

	if err := SetProjectScheduler(dir, "p2", map[string]any{"dispatch_mode": "manual"}); err != nil {
		t.Fatal(err)
	}
	sched2 := readSched(t, filepath.Join(dir, "projects", "p2", "config.toml"))
	if sched2["dispatch_mode"] != "manual" {
		t.Fatalf("p2 got %+v", sched2)
	}
}

func TestSetProjectIntegration_MergesAndPreserves(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "projects", "slug")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(projDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[scheduler]\ndispatch_mode = \"sequenced\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetProjectIntegration(dir, "slug", map[string]any{
		"feature_branch":      "spec/x",
		"task_auto_integrate": true,
		"merge_method":        "squash",
		"auto_fix_ci":         true,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(LoadOptions{ConfigDir: dir, ProjectSlug: "slug", SkipEnv: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scheduler.DispatchMode != "sequenced" {
		t.Errorf("DispatchMode = %q, want sequenced (scheduler clobbered)", cfg.Scheduler.DispatchMode)
	}
	if cfg.Integration.FeatureBranch != "spec/x" {
		t.Errorf("FeatureBranch = %q, want spec/x", cfg.Integration.FeatureBranch)
	}
	if !cfg.Integration.TaskAutoIntegrate {
		t.Errorf("TaskAutoIntegrate = false, want true")
	}
	if cfg.Integration.MergeMethod != "squash" {
		t.Errorf("MergeMethod = %q, want squash", cfg.Integration.MergeMethod)
	}
	if !cfg.Integration.AutoFixCI {
		t.Errorf("AutoFixCI = false, want true")
	}
}
