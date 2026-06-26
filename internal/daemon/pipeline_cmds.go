package daemon

import (
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/pipeline"
)

// runCommandsForProject resolves the per-project effective config and projects
// the build + finish-branch pipeline commands/timeouts into a
// pipeline.RunCommands attached to the per-run pipeline.Run. This is what makes
// `~/.hive/projects/<slug>/config.toml` pipeline overrides take effect at
// dispatch (the boot-built pipeline instances carry only the global defaults).
// Never returns nil — a config-load error falls back to the global config.
func (d *Daemon) runCommandsForProject(slug string) *pipeline.RunCommands {
	eff := d.effectiveConfigForProject(slug)
	b := eff.Pipelines.Build
	f := eff.Pipelines.FinishBranch
	mins := func(m int) time.Duration { return time.Duration(m) * time.Minute }
	return &pipeline.RunCommands{
		Test:            b.TestCommand,
		Validate:        b.ValidateCommand,
		TestTimeout:     mins(b.TestStageTimeoutMinutes),
		ValidateTimeout: mins(b.ValidateStageTimeoutMinutes),

		Format:             f.FormatCommand,
		Typecheck:          f.TypecheckCommand,
		Lint:               f.LintCommand,
		FinishTest:         f.TestCommand,
		Prepare:            f.PrepareCommand,
		CreatePR:           f.CreatePRCommand,
		CIMonitor:          f.CIMonitorCommand,
		FinishStageTimeout: mins(f.StageTimeoutMinutes),
		CIMonitorTimeout:   mins(f.CIMonitorTimeoutMinutes),

		AutoFixCI:        eff.Integration.AutoFixCI,
		CIFixPushCommand: eff.Integration.ResolvedCIFixPushCommand(),
	}
}

// effectiveGraduateAuditRuns resolves K (ensemble size) from the per-project
// effective config, falling back to the global default (3) on any load error.
func (d *Daemon) effectiveGraduateAuditRuns(slug string) int {
	eff := d.effectiveConfigForProject(slug)
	return eff.Pipelines.Graduate.ResolvedAuditRuns()
}

// effectiveGraduateSeam resolves the seam-audit enable flag + patterns from the
// per-project effective config, falling back to the global config on load error.
func (d *Daemon) effectiveGraduateSeam(slug string) (bool, config.SeamPatterns) {
	eff := d.effectiveConfigForProject(slug)
	return eff.Pipelines.Graduate.ResolvedSeamAudit(), eff.Pipelines.Graduate.SeamPatterns
}

// effectiveGraduatePhaseAudit resolves the phase-audit enable flag from the
// per-project effective config, falling back to the global config on load error.
func (d *Daemon) effectiveGraduatePhaseAudit(slug string) bool {
	eff := d.effectiveConfigForProject(slug)
	return eff.Pipelines.Graduate.ResolvedPhaseAudit()
}

// decomposeStackHint formats the project's per-project pipeline commands into a
// prompt fragment for decompose, so the model writes acceptance criteria using
// the project's ACTUAL toolchain (e.g. pnpm) instead of defaulting to Go's
// `go test`. Empty when the project has no resolvable build/test commands.
func (d *Daemon) decomposeStackHint(slug string) string {
	c := d.runCommandsForProject(slug)
	if c == nil {
		return ""
	}
	var lines []string
	add := func(label, cmd string) {
		if strings.TrimSpace(cmd) != "" {
			lines = append(lines, "- "+label+": "+cmd)
		}
	}
	add("tests", c.Test)
	add("build", c.Validate)
	add("typecheck", c.Typecheck)
	add("lint", c.Lint)
	if len(lines) == 0 {
		return ""
	}
	return "PROJECT PIPELINE COMMANDS (this project's real build/test toolchain — write acceptance criteria using THESE commands; do NOT assume Go / `go test`):\n" + strings.Join(lines, "\n")
}
