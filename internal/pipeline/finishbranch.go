package pipeline

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rohilrs/Hive/pkg/rpc"
)

// FinishBranchConfig configures the finish-branch pipeline. Empty command
// skips that stage.
type FinishBranchConfig struct {
	FormatCommand       string
	TypecheckCommand    string
	LintCommand         string
	TestCommand         string
	CreatePRCommand     string
	CIMonitorCommand    string
	StageTimeout        time.Duration
	CIMonitorTimeout    time.Duration
	ShellOutputMaxBytes int
	MaxFixAttempts      int // per-gate auto-fix attempts (typecheck/lint/test); 0 disables
}

// SubRunner spawns a child Build run to fix a finish-branch gate failure
// and blocks until it completes. The daemon provides the implementation
// (it has store + pipeline access the detached pipeline goroutine lacks).
type SubRunner interface {
	// RunChildFix runs a child Build pass on the parent's worktree to fix
	// the named gate, returning the child's terminal Result. err is only
	// for unrecoverable dispatch failures (not a failed-but-completed child).
	RunChildFix(ctx context.Context, parent *Run, gate, failureOutput string) (*Result, error)
}

// FinishBranchPipeline gates a completed branch then opens + watches a PR:
//
//	format -> typecheck -> lint -> test -> create-pr -> ci-monitor
//
// Local gate (typecheck/lint/test) failures trigger an auto-fix: a child
// Build run is spawned via Fixer to repair the failure, then the SAME gate
// re-runs, up to Cfg.MaxFixAttempts. create-pr/ci-monitor are not fixable.
// With no Fixer wired (nil), any gate failure stops with needs_attention.
// Pure shell + stage observability; runs no worker subprocess itself.
type FinishBranchPipeline struct {
	Stages StageStore
	Events EventPublisher
	Fixer  SubRunner // nil = no auto-fix (gate failure -> needs_attention)
	Cfg    FinishBranchConfig
}

func (*FinishBranchPipeline) Name() string { return "finish-branch" }

// finishCmds is the resolved set of finish-branch stage commands + timeouts.
type finishCmds struct {
	format, typecheck, lint, test, createPR, ciMonitor string
	stageTimeout, ciMonitorTimeout                     time.Duration
	autoFixCI                                          bool
	ciFixPush                                          string
}

// resolveFinishCommands returns the per-run command overrides when
// run.Commands is set, else the pipeline's boot Cfg defaults. Empty strings
// (skip) are preserved.
func resolveFinishCommands(cfg FinishBranchConfig, run *Run) finishCmds {
	c := finishCmds{
		format:    cfg.FormatCommand,
		typecheck: cfg.TypecheckCommand, lint: cfg.LintCommand, test: cfg.TestCommand,
		createPR: cfg.CreatePRCommand, ciMonitor: cfg.CIMonitorCommand,
		stageTimeout: cfg.StageTimeout, ciMonitorTimeout: cfg.CIMonitorTimeout,
	}
	if run.Commands != nil {
		c.format = run.Commands.Format
		c.typecheck, c.lint, c.test = run.Commands.Typecheck, run.Commands.Lint, run.Commands.FinishTest
		c.createPR, c.ciMonitor = run.Commands.CreatePR, run.Commands.CIMonitor
		c.stageTimeout, c.ciMonitorTimeout = run.Commands.FinishStageTimeout, run.Commands.CIMonitorTimeout
		c.autoFixCI = run.Commands.AutoFixCI
		c.ciFixPush = run.Commands.CIFixPushCommand
	}
	return c
}

func (p *FinishBranchPipeline) Run(ctx context.Context, run *Run) (*Result, error) {
	res := &Result{}
	fc := resolveFinishCommands(p.Cfg, run)
	gates := []struct {
		name, cmd string
		timeout   time.Duration
		fixable   bool
	}{
		// fixable=true: formatters (e.g. prettier --write, gofmt -w) can repair
		// their own failures, so a fix iteration is cheap and consistent with typecheck/lint.
		{"format", fc.format, fc.stageTimeout, true},
		{"typecheck", fc.typecheck, fc.stageTimeout, true},
		{"lint", fc.lint, fc.stageTimeout, true},
		{"test", fc.test, fc.stageTimeout, true},
		{"create-pr", fc.createPR, fc.stageTimeout, false},
		{"ci-monitor", fc.ciMonitor, fc.ciMonitorTimeout, fc.autoFixCI},
	}
	var ran []string
	for _, g := range gates {
		if g.cmd == "" {
			continue // empty command skips this gate (no row, no claim)
		}
		attempts := 0
		cmd := g.cmd
		if g.name == "create-pr" {
			cmd = substituteTargetBranch(g.cmd, run.TargetBranch)
			// Surface a likely-misconfiguration: a custom create_pr_command
			// that drops the {{target_branch}} token will open PRs against
			// the repo's default base, ignoring a non-default target branch.
			if cmd == g.cmd && run.TargetBranch != "" && run.TargetBranch != "main" {
				log.Printf("finishbranch: create_pr_command has no {{target_branch}} token; PR base will not target %q", run.TargetBranch)
			}
		}
		budget := p.maxFixAttempts()
		if g.name == "ci-monitor" {
			budget = 1 // CI auto-fix is a single attempt, independent of MaxFixAttempts
		}
		for {
			ok, output, err := p.runShellStage(ctx, run, g.name, cmd, attempts, g.timeout)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", g.name, err)
			}
			if ok {
				if g.name == "create-pr" {
					if url, num := parsePRURL(output); url != "" {
						res.PRURL = url
						res.PRNumber = num
					}
				}
				ran = append(ran, g.name)
				break
			}
			// Gate failed. Auto-fix only local gates, only when a Fixer is
			// wired, only within the attempt budget.
			if !g.fixable || p.Fixer == nil || attempts >= budget {
				res.Status = "needs_attention"
				res.Summary = g.name + " failed: " + firstLine(output)
				res.EndedAt = time.Now()
				return res, nil
			}
			attempts++
			child, ferr := p.Fixer.RunChildFix(ctx, run, g.name, output)
			if ferr != nil {
				res.Status = "needs_attention"
				res.Summary = g.name + " auto-fix error: " + ferr.Error()
				res.EndedAt = time.Now()
				return res, nil
			}
			if child == nil || child.Status != "done" {
				cs := "nil"
				if child != nil {
					cs = child.Status
				}
				res.Status = "needs_attention"
				res.Summary = g.name + " auto-fix did not converge (child: " + cs + ")"
				res.EndedAt = time.Now()
				return res, nil
			}
			// Child produced a passing build; loop re-runs the SAME gate.
			// ci-monitor is a REMOTE gate: the fix must be pushed to the PR
			// branch so CI re-runs against it before we re-check.
			if g.name == "ci-monitor" {
				pushCmd := fc.ciFixPush
				if pushCmd == "" {
					pushCmd = "git push origin HEAD"
				}
				ok, pout, perr := p.runShellStage(ctx, run, "ci-fix-push", pushCmd, attempts, fc.stageTimeout)
				if perr != nil {
					return nil, fmt.Errorf("ci-fix-push: %w", perr)
				}
				if !ok {
					res.Status = "needs_attention"
					res.Summary = "ci-monitor auto-fix push failed: " + firstLine(pout)
					res.EndedAt = time.Now()
					return res, nil
				}
			}
		}
	}
	res.Status = "done"
	// Report what actually ran rather than claiming a PR/CI when those
	// gates were skipped (empty command).
	res.Summary = "branch finished: " + strings.Join(ran, ", ") + " passed"
	res.EndedAt = time.Now()
	return res, nil
}

// runShellStage runs one finish-branch stage with stage-row + event
// observability. Empty command skips (ok=true, no row). iter is the
// gate-loop retry counter (0 on first attempt, 1 after first auto-fix,
// ...) so re-runs of the SAME gate produce distinct stage rows rather
// than supplanting the failed one. Returns (ok, output, err): ok=false
// means the command exited non-zero; err is only for unrecoverable
// setup failures.
func (p *FinishBranchPipeline) runShellStage(ctx context.Context, run *Run, name, command string, iter int, timeout time.Duration) (bool, string, error) {
	if command == "" {
		return true, "", nil
	}
	maxBytes := p.Cfg.ShellOutputMaxBytes
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	stageID := int64(0)
	if p.Stages != nil {
		id, berr := p.Stages.BeginStage(ctx, run.ID, name, iter, "")
		if berr != nil {
			log.Printf("finishbranch: BeginStage %s: %v", name, berr)
		} else {
			stageID = id
		}
	}
	p.emit(rpc.EventStageStarted, map[string]any{"run_id": run.ID, "stage_id": stageID, "name": name, "iter": iter})
	output, ok, runErr := RunShellStage(ctx, command, run.WorktreePath, timeout, maxBytes)
	if runErr != nil {
		if p.Stages != nil && stageID != 0 {
			_ = p.Stages.EndStage(ctx, stageID, "", nil, 0, 0, 0, nil)
		}
		return false, output, fmt.Errorf("shell stage %s: %w", name, runErr)
	}
	verdict := "APPROVE"
	if !ok {
		verdict = "CHANGES_REQUESTED"
	}
	if p.Stages != nil && stageID != 0 {
		_ = p.Stages.EndStage(ctx, stageID, verdict, nil, 0, 0, 0, nil)
	}
	p.emit(rpc.EventStageEnded, map[string]any{"run_id": run.ID, "stage_id": stageID, "name": name, "iter": iter, "verdict": verdict})
	return ok, output, nil
}

func (p *FinishBranchPipeline) emit(t rpc.EventType, data map[string]any) {
	if p.Events == nil {
		return
	}
	p.Events.Publish(rpc.EventMessage{Type: t, Data: data})
}

func (p *FinishBranchPipeline) maxFixAttempts() int {
	if p.Cfg.MaxFixAttempts <= 0 {
		return 0
	}
	return p.Cfg.MaxFixAttempts
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// prURLRe matches a GitHub PR URL and captures its number.
var prURLRe = regexp.MustCompile(`https://github\.com/[^/\s]+/[^/\s]+/pull/(\d+)`)

// substituteTargetBranch replaces the {{target_branch}} token in a shell
// command with the resolved target branch (empty -> "main"). Commands
// without the token are returned unchanged, so custom create_pr_command
// values keep working.
func substituteTargetBranch(command, target string) string {
	if target == "" {
		target = "main"
	}
	return strings.ReplaceAll(command, "{{target_branch}}", target)
}

// parsePRURL extracts the first GitHub PR URL and its number from command
// output. Returns ("", 0) when no PR URL is present.
func parsePRURL(output string) (string, int) {
	m := prURLRe.FindStringSubmatch(output)
	if m == nil {
		return "", 0
	}
	n, _ := strconv.Atoi(m[1]) // err impossible: prURLRe captures \d+
	return m[0], n
}
