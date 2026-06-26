package graduate

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/config"
)

type Finding struct {
	Severity       string `json:"severity"` // Critical | High | Medium | Low
	Category       string `json:"category"` // Missing | Incomplete | Incorrect | Extra
	Title          string `json:"title"`
	Evidence       string `json:"evidence"` // file_path:line_number + spec ref
	Recommendation string `json:"recommendation"`

	// SeenCount is how many of the K ensemble runs surfaced this finding (after
	// dedup by FindingFingerprint). 0 for a finding from a non-ensemble call.
	SeenCount int `json:"seen_count,omitempty"`
	// Confirmed is the per-finding verification result for Critical/High findings:
	// nil = not verified (Medium/Low, or a single-run Audit that skipped verify),
	// non-nil = verified (true = real, false = refuted/phantom). AuditEnsemble
	// always sets this for every C/H finding before returning.
	Confirmed *bool `json:"confirmed,omitempty"`
}

// ConfirmLabel renders the verification state for display (PR body, markdown).
func (f Finding) ConfirmLabel() string {
	switch {
	case f.Confirmed == nil:
		return "unverified"
	case *f.Confirmed:
		return "confirmed"
	default:
		return "refuted"
	}
}

// GraduationVerdict is the audit result. NOTE: the gate is severity-driven —
// callers MUST decide whether to block on Blocks() (any **confirmed** Critical/High
// finding (see Blocks)), NOT on Status. Status is informational only and is not
// validated after unmarshal, so a contradictory model output (e.g. Status "COMPLETE"
// alongside a confirmed High finding) still correctly blocks via Blocks().
type GraduationVerdict struct {
	Status   string    `json:"status"` // COMPLETE | GAPS_FOUND (informational; Blocks() is authoritative)
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// Blocks reports whether the verdict blocks graduation: any CONFIRMED Critical
// or High finding. Unverified C/H (Confirmed == nil) and refuted C/H do NOT
// block. AuditEnsemble verifies every C/H finding before the gate is evaluated,
// so in the real path nil never reaches here; a defensive caller that bypasses
// the ensemble gets fail-open (does not block) rather than blocking on an
// unverified phantom.
func (v GraduationVerdict) Blocks() bool {
	for _, f := range v.Findings {
		if f.Severity != "Critical" && f.Severity != "High" {
			continue
		}
		if f.Confirmed != nil && *f.Confirmed {
			return true
		}
	}
	return false
}

// Runner is the narrow interface Audit needs (RunRoamingTool on the oneshot runner).
type Runner interface {
	RunRoamingTool(ctx context.Context, cwd, system, userPrompt string, tool anthropic.ToolDef, allowExtra []string, maxTurns int) (*anthropic.TurnOutput, error)
}

const auditMaxTurns = 40

// readOnlyTools are the built-in tools the audit agent may use. They are
// strictly read-only (Read/Grep/Glob) — NO Bash. An unrestricted Bash would let
// the audit mutate the worktree, `git push`, or `gh pr merge`, breaking the
// "graduation opens a PR, never mutates/merges" guarantee. Scoping Bash via an
// allowlist (e.g. `Bash(git log:*)`) is not viable here because the runner
// space-joins --allowedTools and those specs contain spaces, so they'd be split.
var readOnlyTools = []string{"Read", "Grep", "Glob"}

// severityRubric is the shared severity calibration injected into BOTH the audit
// and verify prompts so independent runs rate the same gap consistently. It is
// the primary lever against gate flip: the live smoke showed one borderline gap
// (an extra pipeline stage) rated Low/Medium/High by three independent audits.
const severityRubric = `Severity rubric (apply consistently; be conservative about Critical/High):
- Critical: a stated deliverable or acceptance criterion is missing or broken so the initiative's core functionality does not work, OR a correctness, data-loss, or security defect in shipped code.
- High: a stated deliverable or acceptance criterion is implemented incorrectly or incompletely such that it does not behave as the spec requires — a functional gap a user or caller would actually hit — even if a workaround exists.
- Medium: real but non-blocking — incomplete polish, missing non-critical tests, stale docs/roadmap, or a spec divergence with no user-visible runtime effect.
- Low: cosmetic or informational, or a deliberate spec divergence with a documented in-code rationale; extra elements not required by the spec that do not break a stated criterion.
Anchor rule: an EXTRA element not required by the spec is at most Medium unless it causes incorrect behavior or violates a stated acceptance criterion. A divergence WITH a documented in-code rationale is Low. Reserve Critical and High for gaps that make a stated deliverable fail to work as specified.`

const auditSystemPrompt = `You are a Senior Software Engineering Auditor verifying that an
initiative's implementation on a feature branch actually fulfills its written roadmap and phase
specs. Independently inspect the ACTUAL code in the working directory — never trust summaries.

Read the roadmap and phase specs under docs/superpowers/ (roadmaps/ and specs/). For each phase's
stated deliverables and acceptance criteria, verify the implementation exists and is complete by
reading the real files. Inspect using Read, Grep, and Glob only (no shell, git, or gh). Prioritize
functional gaps over style.

Categorize each gap as Missing | Incomplete | Incorrect | Extra, with severity
Critical | High | Medium | Low, and cite evidence as file_path:line_number plus the spec reference.

` + severityRubric + `

When done, you MUST call submit_graduation_verdict exactly once with your verdict. Status is
COMPLETE only if there are no Critical or High gaps; otherwise GAPS_FOUND.`

func verdictTool() anthropic.ToolDef {
	return anthropic.ToolDef{
		Name:        "submit_graduation_verdict",
		Description: "Submit the completion-audit verdict for the feature branch.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":  map[string]any{"type": "string", "enum": []string{"COMPLETE", "GAPS_FOUND"}},
				"summary": map[string]any{"type": "string"},
				"findings": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"severity":       map[string]any{"type": "string", "enum": []string{"Critical", "High", "Medium", "Low"}},
							"category":       map[string]any{"type": "string", "enum": []string{"Missing", "Incomplete", "Incorrect", "Extra"}},
							"title":          map[string]any{"type": "string"},
							"evidence":       map[string]any{"type": "string"},
							"recommendation": map[string]any{"type": "string"},
						},
						"required": []string{"severity", "category", "title"},
					},
				},
			},
			"required": []string{"status", "summary", "findings"},
		},
	}
}

// Audit runs the roaming completion audit in worktree and returns the parsed verdict.
// changedFiles is the `git diff --name-only target...feature` list; buildResult is a short
// human summary of the Stage-3 gate outcome. Neither file contents nor specs are embedded —
// the agent Reads them from the worktree (E2BIG-safe).
func Audit(ctx context.Context, runner Runner, worktree, target, feature string, changedFiles []string, buildResult string) (*GraduationVerdict, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Audit the feature branch %q against its roadmap and specs (it will be promoted into %q).\n\n", feature, target)
	fmt.Fprintf(&b, "Shippability gates result: %s\n\n", buildResult)
	fmt.Fprintf(&b, "Changed files (%d) since %s:\n", len(changedFiles), target)
	for _, f := range changedFiles {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	b.WriteString("\nInspect these and the surrounding code, read the roadmap + specs, then call submit_graduation_verdict.")

	out, err := runner.RunRoamingTool(ctx, worktree, auditSystemPrompt, b.String(), verdictTool(), readOnlyTools, auditMaxTurns)
	if err != nil {
		return nil, fmt.Errorf("graduate audit: %w", err)
	}
	if len(out.ToolCalls) != 1 {
		return nil, fmt.Errorf("graduate audit: expected 1 verdict tool call, got %d", len(out.ToolCalls))
	}
	var v GraduationVerdict
	if err := json.Unmarshal(out.ToolCalls[0].Input, &v); err != nil {
		return nil, fmt.Errorf("graduate audit: parse verdict: %w", err)
	}
	return &v, nil
}

// FindingFingerprint is a stable dedup/idempotency key for a finding. It keys on
// category + normalized title + the file portion of the evidence (line numbers
// drift between runs, so they are excluded). Over-merging only saves a verify
// call; under-merging only verifies twice — both harmless. Shared by the
// ensemble union (dedup) and remediation (skip-if-task-exists).
func FindingFingerprint(f Finding) string {
	norm := strings.ToLower(strings.Join(strings.Fields(f.Title), " "))
	key := strings.ToLower(f.Category) + "|" + norm + "|" + evidenceFile(f.Evidence)
	h := sha1.Sum([]byte(key))
	return hex.EncodeToString(h[:])
}

// evidenceFile extracts the path from an "path/to/file.go:123" evidence string,
// stripping a trailing ":<int>" line suffix. Returns the trimmed evidence
// unchanged when there is no numeric line suffix.
func evidenceFile(evidence string) string {
	e := strings.TrimSpace(evidence)
	if i := strings.LastIndex(e, ":"); i > 0 {
		if _, err := strconv.Atoi(strings.TrimSpace(e[i+1:])); err == nil {
			return strings.TrimSpace(e[:i])
		}
	}
	return e
}

const verifyMaxTurns = 20

const verifySystemPrompt = `You are a skeptical Senior Engineering verifier. You are given ONE claimed completion gap
from a prior audit of this feature branch, including its claimed severity. Decide, by inspecting
the ACTUAL code at this working directory (Read, Grep, Glob only — no shell, git, or gh), whether
this gap should BLOCK graduation.

Confirm (confirmed=true) ONLY if the gap BOTH:
  (1) genuinely EXISTS in the code — you can cite the specific code, or its concrete absence; AND
  (2) genuinely warrants Critical or High severity under the rubric below — i.e. it makes a stated
      deliverable or acceptance criterion fail to work as specified.

If the gap does not exist, OR it exists but is really Medium or Low (cosmetic, a divergence with a
documented in-code rationale, or a harmless extra not required by the spec), set confirmed=false and
give a one-sentence reason naming the severity you believe is actually warranted. Default to
confirmed=false whenever you cannot substantiate BOTH conditions. A vague or unverifiable claim is refuted.

` + severityRubric + `

When done you MUST call submit_finding_verification exactly once.`

func verifyTool() anthropic.ToolDef {
	return anthropic.ToolDef{
		Name:        "submit_finding_verification",
		Description: "Submit whether the claimed gap both exists in the code AND warrants Critical/High severity (i.e. should block graduation).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirmed": map[string]any{"type": "boolean"},
				"reason":    map[string]any{"type": "string"},
			},
			"required": []string{"confirmed", "reason"},
		},
	}
}

// VerifyFinding independently re-checks a single audit finding against the code
// in worktree. It roams read-only (Read/Grep/Glob) and is prompted to refute.
// On any error or malformed output it returns confirmed=false AND a non-nil
// error (so the caller logs it but never blocks on an unverifiable finding).
func VerifyFinding(ctx context.Context, runner Runner, worktree, target, feature string, f Finding) (bool, string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Feature branch %q (to be promoted into %q). Verify this single claimed gap:\n\n", feature, target)
	fmt.Fprintf(&b, "- Severity: %s\n- Category: %s\n- Title: %s\n", f.Severity, f.Category, f.Title)
	if strings.TrimSpace(f.Evidence) != "" {
		fmt.Fprintf(&b, "- Cited evidence: %s\n", f.Evidence)
	}
	if strings.TrimSpace(f.Recommendation) != "" {
		fmt.Fprintf(&b, "- Claimed fix: %s\n", f.Recommendation)
	}
	b.WriteString("\nInspect the actual code and call submit_finding_verification.")

	out, err := runner.RunRoamingTool(ctx, worktree, verifySystemPrompt, b.String(), verifyTool(), readOnlyTools, verifyMaxTurns)
	if err != nil {
		return false, "", fmt.Errorf("verify finding %q: %w", f.Title, err)
	}
	if len(out.ToolCalls) != 1 {
		return false, "", fmt.Errorf("verify finding %q: expected 1 tool call, got %d", f.Title, len(out.ToolCalls))
	}
	var res struct {
		Confirmed bool   `json:"confirmed"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(out.ToolCalls[0].Input, &res); err != nil {
		return false, "", fmt.Errorf("verify finding %q: parse: %w", f.Title, err)
	}
	return res.Confirmed, res.Reason, nil
}

// unionFindings merges findings from all ensemble runs, deduping by
// FindingFingerprint and accumulating SeenCount. The first occurrence's fields
// are kept; later duplicates only bump SeenCount. Order is deterministic
// (stable by first-seen across the verdicts slice).
func unionFindings(verdicts []*GraduationVerdict) []Finding {
	order := []string{}
	byFP := map[string]*Finding{}
	for _, v := range verdicts {
		if v == nil {
			continue
		}
		for _, f := range v.Findings {
			fp := FindingFingerprint(f)
			if existing, ok := byFP[fp]; ok {
				existing.SeenCount++
				continue
			}
			cp := f
			cp.SeenCount = 1
			cp.Confirmed = nil
			byFP[fp] = &cp
			order = append(order, fp)
		}
	}
	out := make([]Finding, 0, len(order))
	for _, fp := range order {
		out = append(out, *byFP[fp])
	}
	return out
}

// ensembleConcurrency bounds parallel audit runs; verifyConcurrency bounds
// parallel finding verifications. Both roam the same worktree read-only, so the
// only real limit is subprocess fan-out, not correctness.
const (
	ensembleConcurrency = 3
	verifyConcurrency   = 4
)

// EnsembleOptions configures AuditEnsemble's lenses. K is the roaming-generalist
// count; SeamAudit/SeamPatterns drive the code-seam extractor; PhaseAudit drives
// the per-roadmap-phase deliverable auditor (wired in a later task).
type EnsembleOptions struct {
	K            int
	SeamAudit    bool
	SeamPatterns config.SeamPatterns
	PhaseAudit   bool
}

// AuditEnsemble runs the completion audit k times in parallel, unions the
// findings, verifies each unique Critical/High, and returns one merged verdict
// whose C/H findings all carry a non-nil Confirmed. Status is recomputed:
// COMPLETE iff !Blocks(), else GAPS_FOUND. k<=1 degrades to a single audit
// (still verified). Returns an error only if EVERY audit run fails; a partial
// ensemble proceeds with the successful verdicts (still conservative).
func AuditEnsemble(ctx context.Context, runner Runner, worktree, target, feature string, changedFiles []string, buildResult string, opts EnsembleOptions) (*GraduationVerdict, error) {
	k := opts.K
	if k < 1 {
		k = 1
	}

	// 1. Fan out k audits (bounded).
	verdicts := make([]*GraduationVerdict, k)
	errs := make([]error, k)
	runBounded(k, ensembleConcurrency, func(i int) {
		verdicts[i], errs[i] = Audit(ctx, runner, worktree, target, feature, changedFiles, buildResult)
	})

	ok := verdicts[:0]
	var lastErr error
	for i := range verdicts {
		if errs[i] != nil {
			lastErr = errs[i]
			continue
		}
		if verdicts[i] != nil {
			ok = append(ok, verdicts[i])
		}
	}
	if len(ok) == 0 {
		return nil, fmt.Errorf("graduate audit ensemble: all %d runs failed: %w", k, lastErr)
	}

	// 2. Inject the deterministic seam suspects as a pseudo-verdict BEFORE the
	// union, so they dedup + flow through the same C/H verification + gate as the
	// roaming findings. Placed after the all-failed guard, so ok has >=1 entry.
	if opts.SeamAudit {
		if scan, serr := ExtractSeamSuspects(worktree, opts.SeamPatterns); serr != nil {
			log.Printf("graduate: seam extractor failed: %v (continuing with roaming audits)", serr)
		} else {
			log.Printf("graduate: seam scan: %d files, %d calls, %d regs → %d suspects",
				scan.FilesScanned, scan.CallsSeen, scan.RegsSeen, len(scan.Suspects))
			if sf := synthesizeSeamFindings(scan.Suspects); len(sf) > 0 {
				ok = append(ok, &GraduationVerdict{Findings: sf})
			}
		}
	}

	// 3. Inject per-roadmap-phase deliverable findings as pseudo-verdicts BEFORE
	// the union, so they dedup + flow through the same C/H verification + gate as
	// the roaming and seam findings.
	if opts.PhaseAudit {
		phases := loadRoadmapPhases(worktree)
		if len(phases) == 0 {
			log.Printf("graduate: phase audit: no roadmap under docs/superpowers/roadmaps/ — skipping")
		} else {
			phaseVerdicts, summaries := AuditPhases(ctx, runner, worktree, phases)
			var rb strings.Builder
			for i, s := range summaries {
				if i > 0 {
					rb.WriteString(", ")
				}
				fmt.Fprintf(&rb, "P%s %d/%d", s.Phase, s.Met, s.Total)
			}
			log.Printf("graduate: phase audit: %d phases → %d audited [%s]", len(phases), len(summaries), rb.String())
			ok = append(ok, phaseVerdicts...)
		}
	}

	// 4. Union + dedup, accumulating SeenCount.
	merged := &GraduationVerdict{
		Summary:  ok[0].Summary,
		Findings: unionFindings(ok),
	}

	// 5. Verify each unique Critical/High (bounded). Collect the indices first so
	// the bounded runner indexes a contiguous job list.
	var idx []int
	for i, f := range merged.Findings {
		if f.Severity == "Critical" || f.Severity == "High" {
			idx = append(idx, i)
		}
	}
	runBounded(len(idx), verifyConcurrency, func(j int) {
		i := idx[j]
		confirmed, _, verr := VerifyFinding(ctx, runner, worktree, target, feature, merged.Findings[i])
		if verr != nil {
			// Fail toward not-confirmed; the finding stays out of the gate.
			confirmed = false
		}
		c := confirmed
		merged.Findings[i].Confirmed = &c
	})

	// 6. Recompute status from the confirmed-only gate.
	if merged.Blocks() {
		merged.Status = "GAPS_FOUND"
	} else {
		merged.Status = "COMPLETE"
	}
	return merged, nil
}

// synthesizeSeamFindings maps deterministic seam suspects into audit findings.
// Severity High (an unwired call is a functional break; VerifyFinding refutes
// false positives). The Note rides in the evidence so the verifier can refute
// real-but-dynamic registrations.
func synthesizeSeamFindings(suspects []SeamSuspect) []Finding {
	out := make([]Finding, 0, len(suspects))
	for _, s := range suspects {
		var ev strings.Builder
		ev.WriteString("Called but no handler/route registered. Call sites: ")
		for i, c := range s.CallSites {
			if i > 0 {
				ev.WriteString(", ")
			}
			fmt.Fprintf(&ev, "%s:%d", c.File, c.Line)
		}
		if s.Note != "" {
			fmt.Fprintf(&ev, ". Note: %s", s.Note)
		}
		out = append(out, Finding{
			Severity:       "High",
			Category:       "Missing",
			Title:          fmt.Sprintf("Unwired %s seam: %q is called but no handler is registered", s.Kind, s.Key),
			Evidence:       ev.String(),
			Recommendation: fmt.Sprintf("Register a handler for %q (or remove the dead call), and add a boundary-crossing test.", s.Key),
		})
	}
	return out
}

// runBounded runs fn(0..n-1) across at most `limit` concurrent goroutines and
// waits for all to finish. n<=0 is a no-op. fn must be safe for concurrent calls
// on distinct indices (callers write only to out[i]).
func runBounded(n, limit int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
