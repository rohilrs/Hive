package pipeline

import "testing"

func TestResolveFinishCommands(t *testing.T) {
	cfg := FinishBranchConfig{
		TypecheckCommand: "gtc", LintCommand: "glint", TestCommand: "gtest",
		CreatePRCommand: "gpr", CIMonitorCommand: "gci",
	}
	// nil run.Commands -> cfg defaults.
	got := resolveFinishCommands(cfg, &Run{})
	if got.typecheck != "gtc" || got.lint != "glint" || got.test != "gtest" || got.createPR != "gpr" || got.ciMonitor != "gci" {
		t.Errorf("nil -> %+v, want cfg defaults", got)
	}
	// override -> per-run wins.
	got = resolveFinishCommands(cfg, &Run{Commands: &RunCommands{
		Typecheck: "ntc", Lint: "nlint", FinishTest: "ntest", CreatePR: "npr", CIMonitor: "nci",
	}})
	if got.typecheck != "ntc" || got.lint != "nlint" || got.test != "ntest" || got.createPR != "npr" || got.ciMonitor != "nci" {
		t.Errorf("override -> %+v, want per-run", got)
	}
	// explicit empty -> empty (skip), not the default.
	got = resolveFinishCommands(cfg, &Run{Commands: &RunCommands{Typecheck: ""}})
	if got.typecheck != "" {
		t.Errorf("explicit-empty typecheck = %q, want \"\"", got.typecheck)
	}
}

func TestResolveFinishCommandsFormatPerRunOverride(t *testing.T) {
	cfg := FinishBranchConfig{FormatCommand: "global-fmt"}
	got := resolveFinishCommands(cfg, &Run{Commands: &RunCommands{Format: "per-run-fmt"}})
	if got.format != "per-run-fmt" {
		t.Errorf("format = %q, want per-run-fmt (per-run override)", got.format)
	}
}
