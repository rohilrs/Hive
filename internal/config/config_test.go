package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestLoadDefaultsWhenNoFiles(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(LoadOptions{ConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxWorkers != 3 {
		t.Errorf("max_workers=%d", cfg.Concurrency.MaxWorkers)
	}
	if cfg.Pipelines.Build.MaxIterations != 3 {
		t.Error("build defaults missing")
	}
}

func TestLoadGlobalConfigOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	content := `
[concurrency]
max_workers = 5

[pipelines.build]
max_iterations = 2
`
	err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(LoadOptions{ConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxWorkers != 5 {
		t.Errorf("max_workers=%d", cfg.Concurrency.MaxWorkers)
	}
	if cfg.Pipelines.Build.MaxIterations != 2 {
		t.Errorf("max_iter=%d", cfg.Pipelines.Build.MaxIterations)
	}
	if cfg.Costs.CapPerRunUSD != 10.0 {
		t.Errorf("cost cap not preserved: %v", cfg.Costs.CapPerRunUSD)
	}
}

func TestLoadProjectOverride(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "projects", "research")
	_ = os.MkdirAll(projDir, 0755)
	// Shrink build.max_iterations alongside worker_ladder so the resulting
	// config passes Validate(): ladder length must be >= max_iterations.
	content := `
[models]
worker_ladder = ["claude-opus-4-7"]
reviewer_ladder = ["claude-opus-4-7"]

[pipelines.build]
max_iterations = 1
`
	_ = os.WriteFile(filepath.Join(projDir, "config.toml"), []byte(content), 0644)

	cfg, err := Load(LoadOptions{ConfigDir: dir, ProjectSlug: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models.WorkerLadder) != 1 || cfg.Models.WorkerLadder[0] != "claude-opus-4-7" {
		t.Errorf("worker_ladder=%v", cfg.Models.WorkerLadder)
	}
}

func TestEnvOverridesScalars(t *testing.T) {
	t.Setenv("HIVE_CONCURRENCY_MAX_WORKERS", "8")
	t.Setenv("HIVE_COSTS_CAP_PER_RUN_USD", "25.5")
	// Set build.max_iterations to 2 (within default ladder length of 3)
	// so the resulting config passes Validate().
	t.Setenv("HIVE_PIPELINES_BUILD_MAX_ITERATIONS", "2")

	dir := t.TempDir()
	cfg, err := Load(LoadOptions{ConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxWorkers != 8 {
		t.Errorf("max_workers=%d", cfg.Concurrency.MaxWorkers)
	}
	if cfg.Costs.CapPerRunUSD != 25.5 {
		t.Errorf("cap=%v", cfg.Costs.CapPerRunUSD)
	}
	if cfg.Pipelines.Build.MaxIterations != 2 {
		t.Errorf("max_iter=%d", cfg.Pipelines.Build.MaxIterations)
	}
}

func TestSkipEnvIgnoresOverrides(t *testing.T) {
	t.Setenv("HIVE_CONCURRENCY_MAX_WORKERS", "99")
	dir := t.TempDir()
	cfg, err := Load(LoadOptions{ConfigDir: dir, SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxWorkers != 3 {
		t.Errorf("env applied despite SkipEnv: %d", cfg.Concurrency.MaxWorkers)
	}
}

func TestEnvRejectsInvalidInt(t *testing.T) {
	t.Setenv("HIVE_CONCURRENCY_MAX_WORKERS", "eight")
	_, err := Load(LoadOptions{ConfigDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error for invalid int env value")
	}
	if !strings.Contains(err.Error(), "HIVE_CONCURRENCY_MAX_WORKERS") {
		t.Errorf("error doesn't mention env key: %v", err)
	}
}

func TestProjectOverridesGlobal(t *testing.T) {
	dir := t.TempDir()
	// Global config sets one value
	globalContent := `
[concurrency]
max_workers = 5
`
	_ = os.WriteFile(filepath.Join(dir, "config.toml"), []byte(globalContent), 0644)

	// Project config overrides it
	projDir := filepath.Join(dir, "projects", "research")
	_ = os.MkdirAll(projDir, 0755)
	projContent := `
[concurrency]
max_workers = 8
`
	_ = os.WriteFile(filepath.Join(projDir, "config.toml"), []byte(projContent), 0644)

	cfg, err := Load(LoadOptions{ConfigDir: dir, ProjectSlug: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxWorkers != 8 {
		t.Errorf("project should override global: got max_workers=%d want 8", cfg.Concurrency.MaxWorkers)
	}
}

func TestChatConfirmTimeoutSecondsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	src := `
[chat]
auto_confirm = ["hive_add_task"]
confirm_timeout_seconds = 45
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{ConfigDir: dir, SkipEnv: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Chat.ConfirmTimeoutSeconds; got != 45 {
		t.Errorf("ConfirmTimeoutSeconds=%d, want 45", got)
	}
	if got := cfg.Chat.AutoConfirm; len(got) != 1 || got[0] != "hive_add_task" {
		t.Errorf("AutoConfirm=%v, want [hive_add_task]", got)
	}
}

func TestEnvOverridesProjectAndGlobal(t *testing.T) {
	t.Setenv("HIVE_CONCURRENCY_MAX_WORKERS", "12")

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`[concurrency]
max_workers = 5`), 0644)
	projDir := filepath.Join(dir, "projects", "research")
	_ = os.MkdirAll(projDir, 0755)
	_ = os.WriteFile(filepath.Join(projDir, "config.toml"), []byte(`[concurrency]
max_workers = 8`), 0644)

	cfg, err := Load(LoadOptions{ConfigDir: dir, ProjectSlug: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxWorkers != 12 {
		t.Errorf("env should win over project+global: got max_workers=%d want 12", cfg.Concurrency.MaxWorkers)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"max_workers <= 0", func(c *Config) { c.Concurrency.MaxWorkers = 0 }, "max_workers"},
		{"hop_depth too big", func(c *Config) { c.Pipelines.Build.ConflictHopDepth = 10 }, "conflict_hop_depth"},
		{"ladder/iter mismatch", func(c *Config) {
			c.Pipelines.Build.MaxIterations = 3
			c.Models.WorkerLadder = []string{"claude-sonnet-4-6"}
		}, "worker_ladder"},
		{"cost cap negative", func(c *Config) { c.Costs.CapPerRunUSD = -5 }, "cap_per_run_usd"},
		{"predictor threshold > 1", func(c *Config) { c.Predictor.PrecisionKillThreshold = 1.5 }, "precision_kill_threshold"},
		{"alert_at_pct out of range", func(c *Config) { c.Costs.AlertAtPct = 150 }, "alert_at_pct"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mut(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateAcceptsDefaults(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Errorf("defaults invalid: %v", err)
	}
}

func TestScavengerConfigDefaults(t *testing.T) {
	cfg, err := Load(LoadOptions{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Scavenger.Enabled {
		t.Error("Scavenger.Enabled default should be true")
	}
	if cfg.Scavenger.Binary != "scavenger" {
		t.Errorf("Scavenger.Binary default=%q want %q", cfg.Scavenger.Binary, "scavenger")
	}
}

func TestScavengerConfigTOMLOverride(t *testing.T) {
	dir := t.TempDir()
	body := `
[scavenger]
enabled = false
binary = "/opt/scavenger/bin/scavenger"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{ConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scavenger.Enabled {
		t.Error("Enabled should be false after override")
	}
	if cfg.Scavenger.Binary != "/opt/scavenger/bin/scavenger" {
		t.Errorf("Binary=%q", cfg.Scavenger.Binary)
	}
}

func TestPredictorDefaults(t *testing.T) {
	c := Default()
	if !c.Predictor.Enabled {
		t.Errorf("Predictor.Enabled = %v, want true", c.Predictor.Enabled)
	}
	if c.Predictor.BundleTokenCap != 6000 {
		t.Errorf("Predictor.BundleTokenCap = %d, want 6000", c.Predictor.BundleTokenCap)
	}
	if c.Predictor.MaxCandidates != 10 {
		t.Errorf("Predictor.MaxCandidates = %d, want 10", c.Predictor.MaxCandidates)
	}
	if c.Predictor.PerCallTimeoutSeconds != 5 {
		t.Errorf("Predictor.PerCallTimeoutSeconds = %d, want 5", c.Predictor.PerCallTimeoutSeconds)
	}
	if c.Predictor.HaikuTimeoutSeconds != 10 {
		t.Errorf("Predictor.HaikuTimeoutSeconds = %d, want 10", c.Predictor.HaikuTimeoutSeconds)
	}
	if c.Predictor.HaikuModel != "claude-haiku-4-5" {
		t.Errorf("Predictor.HaikuModel = %q, want claude-haiku-4-5", c.Predictor.HaikuModel)
	}
}

func TestLLMDefaults(t *testing.T) {
	c := Default()
	if c.LLM.Provider != "cli" {
		t.Errorf("LLM.Provider = %q, want %q", c.LLM.Provider, "cli")
	}
}

func TestSourcesDefaults(t *testing.T) {
	c := Default()
	if c.Sources.Inbox.SyncIntervalMinutes != 5 {
		t.Errorf("Sources.Inbox.SyncIntervalMinutes = %d, want 5", c.Sources.Inbox.SyncIntervalMinutes)
	}
	if c.Sources.GitHub.SyncIntervalMinutes != 30 {
		t.Errorf("Sources.GitHub.SyncIntervalMinutes = %d, want 30", c.Sources.GitHub.SyncIntervalMinutes)
	}
	if c.Sources.Linear.SyncIntervalMinutes != 15 {
		t.Errorf("Sources.Linear.SyncIntervalMinutes = %d, want 15", c.Sources.Linear.SyncIntervalMinutes)
	}
	if c.Sources.Linear.APIKeyEnv != "LINEAR_API_KEY" {
		t.Errorf("Sources.Linear.APIKeyEnv = %q, want LINEAR_API_KEY", c.Sources.Linear.APIKeyEnv)
	}
}

func TestConflictGuardDefaults(t *testing.T) {
	c := Default()
	if !c.ConflictGuard.Enabled {
		t.Errorf("ConflictGuard.Enabled = %v, want true", c.ConflictGuard.Enabled)
	}
}

func TestChatConfirmTimeoutSecondsDefault(t *testing.T) {
	dir := t.TempDir()
	// No config.toml written → Load returns Default() (minus env overrides).
	cfg, err := Load(LoadOptions{ConfigDir: dir, SkipEnv: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Chat.ConfirmTimeoutSeconds; got != 300 {
		t.Errorf("default ConfirmTimeoutSeconds=%d, want 300", got)
	}
}

func TestScavengerPerRunDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.Scavenger.IndexWorktreeOnRun {
		t.Errorf("IndexWorktreeOnRun default = false, want true")
	}
	if cfg.Scavenger.MaxConcurrentDaemons != 8 {
		t.Errorf("MaxConcurrentDaemons default = %d, want 8", cfg.Scavenger.MaxConcurrentDaemons)
	}
	if cfg.Scavenger.IndexTimeoutSeconds != 120 {
		t.Errorf("IndexTimeoutSeconds default = %d, want 120", cfg.Scavenger.IndexTimeoutSeconds)
	}
}

func TestSchedulerDispatchModeValidation(t *testing.T) {
	// bogus value must fail Validate
	t.Run("invalid dispatch_mode fails", func(t *testing.T) {
		cfg := Default()
		cfg.Scheduler.DispatchMode = "bogus"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for dispatch_mode=bogus, got nil")
		}
		if !strings.Contains(err.Error(), "bogus") {
			t.Errorf("error should mention offending value; got: %v", err)
		}
		if !strings.Contains(err.Error(), "dispatch_mode") {
			t.Errorf("error should mention dispatch_mode; got: %v", err)
		}
	})

	// valid values must pass
	for _, mode := range []string{DispatchModeManual, DispatchModeAutoAll, DispatchModeSequenced} {
		mode := mode
		t.Run("valid dispatch_mode "+mode, func(t *testing.T) {
			cfg := Default()
			cfg.Scheduler.DispatchMode = mode
			if err := cfg.Validate(); err != nil {
				t.Errorf("dispatch_mode=%q should be valid, got: %v", mode, err)
			}
		})
	}

	// empty is also valid (falls back to AutoDispatch)
	t.Run("empty dispatch_mode passes", func(t *testing.T) {
		cfg := Default()
		cfg.Scheduler.DispatchMode = ""
		if err := cfg.Validate(); err != nil {
			t.Errorf("empty dispatch_mode should be valid, got: %v", err)
		}
	})
}

func TestSchedulerResolvedTargetBranch(t *testing.T) {
	cases := []struct {
		name string
		s    Scheduler
		want string
	}{
		{"empty resolves to main", Scheduler{TargetBranch: ""}, "main"},
		{"non-empty passthrough", Scheduler{TargetBranch: "staging"}, "staging"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.ResolvedTargetBranch(); got != c.want {
				t.Errorf("ResolvedTargetBranch() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRepoKey(t *testing.T) {
	a := RepoKey("/home/x/sidecar_ai")
	b := RepoKey("/home/x/sidecar_ai/")  // trailing slash → Clean → same key
	c := RepoKey("/home/x/./sidecar_ai") // "." → Clean → same key
	if a != b || a != c {
		t.Errorf("non-deterministic: %q %q %q", a, b, c)
	}
	if !strings.HasPrefix(a, "sidecar_ai-") {
		t.Errorf("key %q must have the basename prefix", a)
	}
	if RepoKey("/home/x/other") == a {
		t.Error("distinct paths must yield distinct keys")
	}
	if RepoKey("") != "" {
		t.Error("empty path → empty key (no repo layer)")
	}
	if RepoKey("   ") != "" {
		t.Error("whitespace-only path → empty key")
	}
}

func TestLoadRepoLayerMerge(t *testing.T) {
	dir := t.TempDir()
	repoKey := RepoKey("/repo/acme")
	mustWrite := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("config.toml", "[pipelines.build]\ntest_command = \"global-test\"\n")
	mustWrite(filepath.Join("repos", repoKey, "config.toml"), "[pipelines.build]\ntest_command = \"repo-test\"\n")
	mustWrite(filepath.Join("projects", "acme", "config.toml"), "[integration]\nfeature_branch = \"feat/x\"\n")

	cfg, err := Load(LoadOptions{ConfigDir: dir, RepoKey: repoKey, ProjectSlug: "acme", SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipelines.Build.TestCommand != "repo-test" {
		t.Errorf("repo layer must override global: test_command=%q", cfg.Pipelines.Build.TestCommand)
	}
	if cfg.Integration.FeatureBranch != "feat/x" {
		t.Errorf("project field must survive: feature_branch=%q", cfg.Integration.FeatureBranch)
	}
}

func TestLoadRepoLayerProjectWins(t *testing.T) {
	dir := t.TempDir()
	repoKey := RepoKey("/repo/acme")
	w := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w(filepath.Join("repos", repoKey, "config.toml"), "[pipelines.build]\ntest_command = \"repo-test\"\n")
	w(filepath.Join("projects", "acme", "config.toml"), "[pipelines.build]\ntest_command = \"project-test\"\n")
	cfg, err := Load(LoadOptions{ConfigDir: dir, RepoKey: repoKey, ProjectSlug: "acme", SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipelines.Build.TestCommand != "project-test" {
		t.Errorf("project must override repo: %q", cfg.Pipelines.Build.TestCommand)
	}
}

func TestLoadRepoLayerAbsentIsNoOp(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[pipelines.build]\ntest_command = \"g\"\n"), 0o644)
	cfg, err := Load(LoadOptions{ConfigDir: dir, RepoKey: "nope-00000000", ProjectSlug: "", SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipelines.Build.TestCommand != "g" {
		t.Errorf("absent repo layer must be a no-op: %q", cfg.Pipelines.Build.TestCommand)
	}
}

func TestSchedulerResolvedMode(t *testing.T) {
	cases := []struct {
		name string
		s    Scheduler
		want string
	}{
		{"explicit sequenced wins", Scheduler{DispatchMode: "sequenced", AutoDispatch: false}, "sequenced"},
		{"explicit manual overrides legacy true", Scheduler{DispatchMode: "manual", AutoDispatch: true}, "manual"},
		{"legacy true maps to auto_all", Scheduler{DispatchMode: "", AutoDispatch: true}, "auto_all"},
		{"legacy false maps to manual", Scheduler{DispatchMode: "", AutoDispatch: false}, "manual"},
		{"explicit auto_all", Scheduler{DispatchMode: "auto_all"}, "auto_all"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.ResolvedMode(); got != c.want {
				t.Errorf("ResolvedMode() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestIntegrationConfigDefaults(t *testing.T) {
	var i Integration // zero value
	if i.ResolvedMergeMethod() != "merge" {
		t.Errorf("default merge method = %q, want merge", i.ResolvedMergeMethod())
	}
	i.MergeMethod = "squash"
	if i.ResolvedMergeMethod() != "squash" {
		t.Errorf("merge method = %q, want squash", i.ResolvedMergeMethod())
	}
}

func TestIntegrationCIFixPushCommandDefault(t *testing.T) {
	var i Integration
	if i.ResolvedCIFixPushCommand() != "git push origin HEAD" {
		t.Errorf("default ci-fix push = %q, want \"git push origin HEAD\"", i.ResolvedCIFixPushCommand())
	}
	i.CIFixPushCommand = "git push fork HEAD"
	if i.ResolvedCIFixPushCommand() != "git push fork HEAD" {
		t.Errorf("override = %q", i.ResolvedCIFixPushCommand())
	}
}

func TestIntegrationConfigParsesFromTOML(t *testing.T) {
	const src = `
[integration]
feature_branch = "spec/conv-rework"
task_auto_integrate = true
merge_method = "squash"
auto_fix_ci = true
ci_fix_push_command = "git push origin HEAD"
`
	var cfg Config
	if _, err := toml.Decode(src, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Integration.FeatureBranch != "spec/conv-rework" {
		t.Errorf("FeatureBranch = %q, want spec/conv-rework", cfg.Integration.FeatureBranch)
	}
	if !cfg.Integration.TaskAutoIntegrate {
		t.Error("TaskAutoIntegrate = false, want true")
	}
	if cfg.Integration.MergeMethod != "squash" {
		t.Errorf("MergeMethod = %q, want squash", cfg.Integration.MergeMethod)
	}
	if !cfg.Integration.AutoFixCI {
		t.Error("AutoFixCI = false, want true")
	}
	if cfg.Integration.CIFixPushCommand != "git push origin HEAD" {
		t.Errorf("CIFixPushCommand = %q, want \"git push origin HEAD\"", cfg.Integration.CIFixPushCommand)
	}
}

func TestResolvePipelineDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Pipelines.Resolve.MaxIterations != 5 {
		t.Errorf("resolve max_iterations=%d want 5", cfg.Pipelines.Resolve.MaxIterations)
	}
	if cfg.Pipelines.Resolve.Auto != false {
		t.Errorf("resolve auto=%v want false (opt-in)", cfg.Pipelines.Resolve.Auto)
	}
	if cfg.Pipelines.Resolve.StageTimeoutMinutes != 20 {
		t.Errorf("resolve stage_timeout_minutes=%d want 20", cfg.Pipelines.Resolve.StageTimeoutMinutes)
	}
}

func TestResolvePipelineTOMLRoundtrip(t *testing.T) {
	const src = `
[pipelines.resolve]
auto = true
max_iterations = 9
stage_timeout_minutes = 30
`
	var cfg Config
	if _, err := toml.Decode(src, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Pipelines.Resolve.Auto {
		t.Error("Pipelines.Resolve.Auto = false, want true")
	}
	if cfg.Pipelines.Resolve.MaxIterations != 9 {
		t.Errorf("Pipelines.Resolve.MaxIterations = %d, want 9", cfg.Pipelines.Resolve.MaxIterations)
	}
	if cfg.Pipelines.Resolve.StageTimeoutMinutes != 30 {
		t.Errorf("Pipelines.Resolve.StageTimeoutMinutes = %d, want 30", cfg.Pipelines.Resolve.StageTimeoutMinutes)
	}
}

func TestGraduateConfigFieldsTOMLRoundtrip(t *testing.T) {
	const blob = `
[pipelines.finish_branch]
format_command = "pnpm exec prettier --check ."

[models]
graduate_validator_model = "claude-opus-4-8"
`
	var c Config
	if _, err := toml.Decode(blob, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Pipelines.FinishBranch.FormatCommand != "pnpm exec prettier --check ." {
		t.Errorf("FormatCommand = %q", c.Pipelines.FinishBranch.FormatCommand)
	}
	if c.Models.GraduateValidatorModel != "claude-opus-4-8" {
		t.Errorf("GraduateValidatorModel = %q", c.Models.GraduateValidatorModel)
	}
}

func TestGraduateAuditRunsDefault(t *testing.T) {
	cfg := Default()
	if got := cfg.Pipelines.Graduate.ResolvedAuditRuns(); got != 5 {
		t.Errorf("default audit_runs=%d want 5", got)
	}
}

func TestGraduateAuditRunsOverride(t *testing.T) {
	var g GraduatePipeline
	g.AuditRuns = 7
	if got := g.ResolvedAuditRuns(); got != 7 {
		t.Errorf("override=%d want 7", got)
	}
	g.AuditRuns = 0
	if got := g.ResolvedAuditRuns(); got != 5 {
		t.Errorf("zero falls back to %d want 5", got)
	}
	g.AuditRuns = -1
	if got := g.ResolvedAuditRuns(); got != 5 {
		t.Errorf("negative falls back to %d want 5", got)
	}
}

func TestGraduateSeamAuditDefault(t *testing.T) {
	cfg := Default()
	if !cfg.Pipelines.Graduate.ResolvedSeamAudit() {
		t.Error("seam_audit must default to true")
	}
}

func TestGraduateSeamAuditExplicitFalse(t *testing.T) {
	f := false
	g := GraduatePipeline{SeamAudit: &f}
	if g.ResolvedSeamAudit() {
		t.Error("explicit false must disable seam audit")
	}
}

func TestGraduateSeamAuditExplicitTrue(t *testing.T) {
	tr := true
	g := GraduatePipeline{SeamAudit: &tr}
	if !g.ResolvedSeamAudit() {
		t.Error("explicit true must enable seam audit")
	}
}

func TestGraduatePhaseAuditDefault(t *testing.T) {
	cfg := Default()
	if !cfg.Pipelines.Graduate.ResolvedPhaseAudit() {
		t.Error("phase_audit must default to true")
	}
}

func TestGraduatePhaseAuditExplicitFalse(t *testing.T) {
	f := false
	g := GraduatePipeline{PhaseAudit: &f}
	if g.ResolvedPhaseAudit() {
		t.Error("explicit false must disable phase audit")
	}
}

func TestGraduateSeamPatternsTOML(t *testing.T) {
	var c Config
	if _, err := toml.Decode(`
[pipelines.graduate.seam_patterns]
router_receivers = ["web"]
exclude_globs = ["generated/*"]
`, &c); err != nil {
		t.Fatal(err)
	}
	sp := c.Pipelines.Graduate.SeamPatterns
	if len(sp.RouterReceivers) != 1 || sp.RouterReceivers[0] != "web" {
		t.Errorf("router_receivers = %v", sp.RouterReceivers)
	}
	if len(sp.ExcludeGlobs) != 1 {
		t.Errorf("exclude_globs = %v", sp.ExcludeGlobs)
	}
}
