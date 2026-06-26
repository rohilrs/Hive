package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/branchhealth"
	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
)

// graduateGateTimeout is the fallback per-gate timeout for Stage-3 shippability
// gates when the per-project finish-branch StageTimeoutMinutes is unset/zero.
const graduateGateTimeout = 30 * time.Minute

// precheck is the result of a deterministic graduation precondition stage.
// OK is true only when every check passed; Reason explains the first failure.
// This shape is contract for the graduate pipeline's later stages.
type precheck struct {
	OK     bool
	Reason string // empty when OK
}

// graduatePreconditions runs Stage 1 (every task done + gate satisfied) and
// Stage 2 (branch health: feature ahead>0, behind==0, no conflicts vs target).
// Deterministic and never bypassed by --force: a project may not graduate while
// work is unfinished or the feature branch is stale/conflicting.
func (d *Daemon) graduatePreconditions(ctx context.Context, proj *store.Project) precheck {
	// Stage 1: completion. Every task must be done AND gate-satisfied.
	tasks, err := d.store.ListTasksByProject(ctx, proj.ID)
	if err != nil {
		return precheck{false, "list tasks: " + err.Error()}
	}
	var incomplete []string
	for _, tk := range tasks {
		if tk.Status != "done" || tk.GateState != sequence.GateSatisfied {
			incomplete = append(incomplete, tk.ID)
		}
	}
	if len(incomplete) > 0 {
		return precheck{false, fmt.Sprintf("%d task(s) not complete (done+satisfied): %s",
			len(incomplete), strings.Join(incomplete, ", "))}
	}

	// Stage 2: branch health. There must be a distinct feature branch with
	// commits ahead of the target, no commits behind, and no conflicts.
	feature := d.scheduler.effectiveWorktreeBaseForProject(proj.Slug)
	target := d.scheduler.effectiveTargetBranchForProject(proj.Slug)
	if feature == "" || feature == target {
		return precheck{false, fmt.Sprintf("no distinct feature branch to graduate (feature=%q target=%q)", feature, target)}
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return precheck{false, "project has no repo path"}
	}
	rep, err := branchhealth.CheckFeatureBranch(*proj.RepoPath, feature, target, "")
	if err != nil {
		return precheck{false, "branch health: " + err.Error()}
	}
	if rep.Ahead == 0 {
		return precheck{false, fmt.Sprintf("feature %q has no commits ahead of %q — nothing to graduate", feature, target)}
	}
	if rep.Behind > 0 || len(rep.ConflictPaths) > 0 {
		return precheck{false, fmt.Sprintf("feature %q is behind=%d / conflicts=%d vs %q — run health.remediate first",
			feature, rep.Behind, len(rep.ConflictPaths), target)}
	}
	return precheck{OK: true}
}

// graduateOpts modulates a graduation run. Force bypasses the Stage-3 build
// gates AND the Stage-4 audit Blocks() gate ONLY — never Stages 1-2
// (graduatePreconditions are always enforced). Draft opens the PR as a draft.
// DryRun composes the PR body and returns it without opening the PR.
type graduateOpts struct {
	Force  bool
	Draft  bool
	DryRun bool
}

// graduateResult is the contract returned to the RPC layer (Task 9). On success
// PRURL is set; on a blocking audit Verdict is set and Err explains the block.
// Stage names the last pipeline stage reached (preconditions | worktree |
// gate:<name> | audit | pr | complete). BuildSummary summarises Stage-3 gate
// results.
type graduateResult struct {
	PRURL        string
	Verdict      *graduate.GraduationVerdict
	Err          error
	Stage        string // preconditions | worktree | gate:<name> | audit | pr | complete
	BuildSummary string
}

// runGraduate executes Stages 1-5 synchronously, emitting human-readable
// progress lines. Stage 1-2 are the always-enforced preconditions; Stage 3 runs
// the per-project finish-branch shippability gates in a feature-branch worktree;
// Stage 4 runs the completion audit; Stage 5 opens the PR feature → target.
func (d *Daemon) runGraduate(ctx context.Context, proj *store.Project, opts graduateOpts, progress func(string)) (res graduateResult) {
	stage := "preconditions"
	buildSummary := ""
	defer func() {
		res.Stage = stage
		res.BuildSummary = buildSummary
	}()

	progress("checking completion + branch health")
	if pc := d.graduatePreconditions(ctx, proj); !pc.OK {
		return graduateResult{Err: fmt.Errorf("preconditions: %s", pc.Reason)}
	}

	feature := d.scheduler.effectiveWorktreeBaseForProject(proj.Slug)
	target := d.scheduler.effectiveTargetBranchForProject(proj.Slug)
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return graduateResult{Err: fmt.Errorf("project has no repo path")}
	}
	repo := *proj.RepoPath

	stage = "worktree"

	// Provision a DETACHED worktree at the feature-branch tip (Stage 3 + 4 share
	// it). Graduate only builds + audits the tree — it never commits or pushes —
	// so it needs no named branch, and a detached checkout avoids the
	// "<feature> is already used by worktree" error that a named checkout hits
	// when the canonical repo (or a prior worktree) is itself sitting on the
	// feature branch (the common case: `hive plan` leaves the repo on it).
	// Run-scope the worktree path with a unique suffix so two concurrent
	// graduations of the same project don't RemoveAll/provision the same dir.
	wt := filepath.Join(d.HiveDir(), fmt.Sprintf("graduate-%s-%d", proj.Slug, time.Now().UnixNano()))
	_ = os.RemoveAll(wt)
	if err := provisionGraduateWorktree(ctx, repo, wt, feature, target); err != nil {
		return graduateResult{Err: fmt.Errorf("provision worktree: %w", err)}
	}
	defer func() { _, _ = gitC(repo, "worktree", "remove", "--force", wt) }()

	// Stage 3: shippability gates (format → typecheck → lint → test), fail-fast.
	// Commands come from the per-project effective finish-branch config (the same
	// source the finish-branch pipeline uses), so per-project overrides apply.
	fb := d.runCommandsForProject(proj.Slug)
	// Source the per-project finish-branch stage timeout (config-driven) instead
	// of a hardcoded value; fall back to 30m when unset/zero.
	gateTimeout := fb.FinishStageTimeout
	if gateTimeout <= 0 {
		gateTimeout = graduateGateTimeout
	}
	// prepare (install) → build-validate (compile) → format → typecheck → lint
	// → test. Order matters in a monorepo: the build/compile step generates the
	// workspace packages' type declarations (.d.ts), which the typecheck gate of
	// DEPENDENT packages needs — so build-validate MUST precede typecheck (the
	// per-task flow gets this for free because its build stage runs before the
	// finish-branch gates; graduate has no build stage, so it runs the build
	// pipeline's validate_command here). Each skips when its command is empty;
	// all fail-fast.
	gates := []struct{ name, cmd string }{
		{"prepare", fb.Prepare},
		{"build-validate", fb.Validate},
		{"format", fb.Format}, {"typecheck", fb.Typecheck},
		{"lint", fb.Lint}, {"test", fb.FinishTest},
	}
	buildSummary = "all configured gates passed"
	var failedGates []string
	for _, g := range gates {
		if strings.TrimSpace(g.cmd) == "" {
			continue
		}
		stage = "gate:" + g.name
		progress("shippability: " + g.name)
		out, ok, err := pipeline.RunShellStage(ctx, g.cmd, wt, gateTimeout, 16384)
		if err != nil || !ok {
			if !opts.Force {
				return graduateResult{Err: fmt.Errorf("shippability %s failed:\n%s", g.name, out)}
			}
			// Forced graduation: keep evaluating so the summary reports EVERY
			// broken gate, not just the first.
			failedGates = append(failedGates, g.name)
			continue
		}
	}
	if len(failedGates) > 0 {
		buildSummary = "gates FAILED: " + strings.Join(failedGates, ", ")
	}

	stage = "audit"

	// Stage 4: completion audit. Diff the target base against the worktree HEAD
	// (the exact feature checkout the audit agent inspects) — this matches the
	// files the graduation actually ships and avoids the stale-origin divergence
	// window the prior origin/<feature> form had. Prefer origin/<target> base;
	// fall back to local <target> when origin is absent (test repos, offline).
	// changed feeds the audit prompt as PATHS only (E2BIG-safe — the agent Reads
	// the files itself).
	progress("running completion audit (opus)")
	changed, err := gitCtxOut(ctx, wt, "diff", "--name-only", "origin/"+target+"...HEAD")
	if err != nil {
		changed, _ = gitCtxOut(ctx, wt, "diff", "--name-only", target+"...HEAD")
	}
	seamAudit, seamPatterns := d.effectiveGraduateSeam(proj.Slug)
	ensembleOpts := graduate.EnsembleOptions{
		K:            d.effectiveGraduateAuditRuns(proj.Slug),
		SeamAudit:    seamAudit,
		SeamPatterns: seamPatterns,
		PhaseAudit:   d.effectiveGraduatePhaseAudit(proj.Slug),
	}
	verdict, err := graduate.AuditEnsemble(ctx, d.graduateRunner, wt, target, feature, splitLines(changed), buildSummary, ensembleOpts)
	var auditNote string
	if err != nil {
		// An audit-infrastructure failure (timeout, E2BIG, crash — claude never
		// produced a verdict). Without --force this aborts the run. Under --force
		// the spec says proceed to open the PR, noting in the body that the audit
		// did not run.
		if !opts.Force {
			return graduateResult{Err: fmt.Errorf("audit: %w", err)}
		}
		verdict = nil
		auditNote = "completion audit DID NOT RUN (forced past): " + err.Error()
		log.Printf("graduate: %s: %s — proceeding under --force", proj.Slug, auditNote)
	}
	if verdict != nil && verdict.Blocks() {
		if !opts.Force {
			return graduateResult{Verdict: verdict, Err: fmt.Errorf("completion audit found %s", blockingSummary(verdict))}
		}
	}

	body := composeGraduateBody(verdict, auditNote, buildSummary, feature, target)
	if opts.DryRun {
		stage = "complete"
		progress("dry-run: would open PR\n" + body)
		return graduateResult{Verdict: verdict}
	}

	stage = "pr"

	// Stage 5: open PR.
	progress("opening PR " + feature + " -> " + target)
	title := fmt.Sprintf("%s: graduate %s → %s", proj.Name, feature, target)
	url, err := d.prGateway.OpenPR(ctx, repo, feature, target, title, body, opts.Draft)
	if err != nil {
		return graduateResult{Verdict: verdict, Err: fmt.Errorf("open PR: %w", err)}
	}
	stage = "complete"
	return graduateResult{PRURL: url, Verdict: verdict}
}

// gitCtxOut runs a git command in dir context-aware (cancellable), returning the
// combined output. Unlike the shared gitC it honors ctx for cancellation.
func gitCtxOut(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

// provisionGraduateWorktree checks out the feature-branch tip into wt as a
// DETACHED worktree. Graduate only builds + audits the tree (never commits or
// pushes), so it needs no named branch — and a detached checkout sidesteps the
// "<feature> is already used by worktree at <path>" git error that a named
// checkout (git worktree add -B <feature>) hits whenever the feature branch is
// already checked out elsewhere (the canonical repo after `hive plan`, or a
// leaked prior worktree). Prefers origin/<feature>; falls back to the local
// <feature> ref when origin is absent. Fetches origin/<feature> + origin/<target>
// best-effort first so the worktree tip and the Stage-4 diff base are current.
func provisionGraduateWorktree(ctx context.Context, repo, wt, feature, target string) error {
	if out, err := exec.CommandContext(ctx, "git", "-C", repo,
		"fetch", "--quiet", "origin", feature, target).CombinedOutput(); err != nil {
		log.Printf("graduate: fetch origin %s %s: %v\n%s (continuing with local refs)",
			feature, target, err, out)
	}
	startPoint := feature
	if refExistsGit(ctx, repo, "refs/remotes/origin/"+feature) {
		startPoint = "origin/" + feature
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", repo,
		"worktree", "add", "--detach", wt, startPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add --detach %s on %s: %w\n%s", wt, startPoint, err, out)
	}
	return nil
}

// splitLines splits on "\n", trims each line, and drops empties.
func splitLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// blockingSummary counts the CONFIRMED blocking severities (Critical/High) for
// the error message, e.g. "2 Critical, 1 High finding(s)". Mirrors Blocks():
// refuted/unverified C/H are not counted.
func blockingSummary(v *graduate.GraduationVerdict) string {
	var crit, high int
	for _, f := range v.Findings {
		if f.Confirmed == nil || !*f.Confirmed {
			continue
		}
		switch f.Severity {
		case "Critical":
			crit++
		case "High":
			high++
		}
	}
	return fmt.Sprintf("%d Critical, %d High finding(s)", crit, high)
}

// composeGraduateBody renders the PR body: a completion-audit section with the
// verdict summary + a findings table, the build-gate summary, and the
// feature→target note.
func composeGraduateBody(v *graduate.GraduationVerdict, auditNote, buildSummary, feature, target string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Completion audit\n\n")
	if strings.TrimSpace(auditNote) != "" {
		fmt.Fprintf(&b, "⚠️ %s\n\n", auditNote)
	}
	if v != nil {
		status := v.Status
		if status == "" {
			status = "unknown"
		}
		fmt.Fprintf(&b, "**Status:** %s\n\n", status)
		if strings.TrimSpace(v.Summary) != "" {
			fmt.Fprintf(&b, "%s\n\n", v.Summary)
		}
		if len(v.Findings) > 0 {
			b.WriteString("| Severity | Category | Title | Verified | Seen |\n")
			b.WriteString("| --- | --- | --- | --- | --- |\n")
			for _, f := range v.Findings {
				seen := ""
				if f.SeenCount > 0 {
					seen = fmt.Sprintf("%d", f.SeenCount)
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
					escapeCell(f.Severity), escapeCell(f.Category), escapeCell(f.Title),
					f.ConfirmLabel(), seen)
			}
			b.WriteString("\n")
		} else {
			b.WriteString("No findings.\n\n")
		}
	}
	fmt.Fprintf(&b, "**Shippability gates:** %s\n\n", buildSummary)
	fmt.Fprintf(&b, "Graduating `%s` → `%s`.\n", feature, target)
	return b.String()
}

// escapeCell neutralizes "|" and newlines so a finding field can't break the
// markdown table layout.
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// persistGraduateResult writes the run record to graduate-<slug>-result.{json,md}
// atomically (temp+rename), overwriting the prior run. Best-effort: write
// failures are logged, never propagated (the run's terminal event still fires).
func (d *Daemon) persistGraduateResult(rec graduate.GraduateResult) {
	base := filepath.Join(d.HiveDir(), "graduate-"+rec.Slug+"-result")
	if j, err := json.MarshalIndent(rec, "", "  "); err != nil {
		log.Printf("graduate: marshal result for %s: %v", rec.Slug, err)
	} else if werr := atomicWriteFile(base+".json", j, 0o644); werr != nil {
		log.Printf("graduate: persist result json for %s: %v", rec.Slug, werr)
	}
	if werr := atomicWriteFile(base+".md", []byte(graduate.RenderResultMarkdown(rec)), 0o644); werr != nil {
		log.Printf("graduate: persist result md for %s: %v", rec.Slug, werr)
	}
}
