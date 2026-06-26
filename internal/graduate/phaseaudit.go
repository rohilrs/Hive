package graduate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/roadmap"
)

// PhaseCriterion is one enumerated deliverable of a phase with its met/unmet judgment.
type PhaseCriterion struct {
	Description    string `json:"description"`
	Met            bool   `json:"met"`
	Severity       string `json:"severity"`
	Category       string `json:"category"`
	Evidence       string `json:"evidence"`
	Recommendation string `json:"recommendation"`
}

// maxPhaseBodyChars bounds the roadmap phase text embedded in the prompt argv
// (E2BIG insurance — the agent still has SpecPaths + the code to roam).
const maxPhaseBodyChars = 16000

func capBody(s string) string {
	if len(s) <= maxPhaseBodyChars {
		return s
	}
	return s[:maxPhaseBodyChars] + "\n…(phase text truncated; read the roadmap file for the full text)"
}

// PhaseSummary is the per-phase met/total rollup, surfaced in the graduate
// daemon log for diagnostics. (Rendering it into the PR body is a future
// enhancement; gating is driven by the unmet-deliverable findings, not this.)
type PhaseSummary struct {
	Phase string
	Title string
	Met   int
	Total int
}

const phaseAuditSystemPrompt = `You are a Senior Software Engineering Auditor verifying that a SINGLE
roadmap phase of an initiative is fully implemented on this feature branch. You are given the phase's
text and its linked spec paths. Read the linked specs and the ACTUAL code in the working directory
(Read, Grep, and Glob only — no shell, git, or gh). Enumerate THIS phase's concrete deliverables /
acceptance criteria, and for EACH one determine whether it is genuinely implemented, citing evidence
as file_path:line_number. Prioritize functional completeness over style.

` + severityRubric + `

When done you MUST call submit_phase_audit exactly once with one entry per deliverable you enumerated
(met=true when implemented; met=false with a severity + evidence when not).`

func phaseAuditTool() anthropic.ToolDef {
	return anthropic.ToolDef{
		Name:        "submit_phase_audit",
		Description: "Submit the per-deliverable met/unmet verdict for one roadmap phase.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"criteria": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"description":    map[string]any{"type": "string"},
							"met":            map[string]any{"type": "boolean"},
							"severity":       map[string]any{"type": "string", "enum": []string{"Critical", "High", "Medium", "Low"}},
							"category":       map[string]any{"type": "string", "enum": []string{"Missing", "Incomplete", "Incorrect", "Extra"}},
							"evidence":       map[string]any{"type": "string"},
							"recommendation": map[string]any{"type": "string"},
						},
						"required": []string{"description", "met"},
					},
				},
			},
			"required": []string{"criteria"},
		},
	}
}

// AuditPhase verifies ONE phase: a focused read-only roaming agent enumerates the
// phase's deliverables and marks each met/unmet against the actual code. Returns a
// verdict whose Findings are the UNMET criteria, plus a PhaseSummary (met/total).
//
// The returned findings have Confirmed == nil — they are candidate gaps, NOT
// gating verdicts. Callers MUST route them through unionFindings + VerifyFinding
// (as AuditEnsemble does) before evaluating GraduationVerdict.Blocks(); Blocks()
// only counts CONFIRMED Critical/High, so an unverified phase finding fails open.
func AuditPhase(ctx context.Context, runner Runner, worktree string, phase roadmap.Phase) (*GraduationVerdict, PhaseSummary, error) {
	sum := PhaseSummary{Phase: phase.Number, Title: phase.Title}

	var b strings.Builder
	fmt.Fprintf(&b, "Phase %s: %s\n\n%s\n", phase.Number, phase.Title, capBody(phase.Body))
	if len(phase.SpecPaths) > 0 {
		fmt.Fprintf(&b, "\nLinked specs (read them): %s\n", strings.Join(phase.SpecPaths, ", "))
	}
	b.WriteString("\nEnumerate this phase's deliverables, verify each against the code, then call submit_phase_audit.")

	out, err := runner.RunRoamingTool(ctx, worktree, phaseAuditSystemPrompt, b.String(), phaseAuditTool(), readOnlyTools, auditMaxTurns)
	if err != nil {
		return nil, sum, fmt.Errorf("phase %s audit: %w", phase.Number, err)
	}
	if len(out.ToolCalls) != 1 {
		return nil, sum, fmt.Errorf("phase %s audit: expected 1 tool call, got %d", phase.Number, len(out.ToolCalls))
	}
	var res struct {
		Criteria []PhaseCriterion `json:"criteria"`
	}
	if err := json.Unmarshal(out.ToolCalls[0].Input, &res); err != nil {
		return nil, sum, fmt.Errorf("phase %s audit: parse: %w", phase.Number, err)
	}

	var findings []Finding
	for _, c := range res.Criteria {
		if c.Met {
			sum.Met++
			continue
		}
		sev := c.Severity
		if sev == "" {
			sev = "High"
		}
		cat := c.Category
		if cat == "" {
			cat = "Incomplete"
		}
		findings = append(findings, Finding{
			Severity:       sev,
			Category:       cat,
			Title:          fmt.Sprintf("Phase %s deliverable not met: %s", phase.Number, c.Description),
			Evidence:       c.Evidence,
			Recommendation: c.Recommendation,
		})
	}
	if len(res.Criteria) == 0 {
		log.Printf("graduate: phase %s audit enumerated 0 criteria — treating as no gaps; verify the phase text", phase.Number)
	}
	sum.Total = len(res.Criteria)
	return &GraduationVerdict{Findings: findings}, sum, nil
}

// loadRoadmapPhases globs docs/superpowers/roadmaps/*.md under worktree and parses
// each via roadmap.Parse, returning all phases. Unparseable files (no phase
// headings) are skipped (logged). No roadmap files → empty slice (the phase lens
// then no-ops).
func loadRoadmapPhases(worktree string) []roadmap.Phase {
	matches, _ := filepath.Glob(filepath.Join(worktree, "docs", "superpowers", "roadmaps", "*.md"))
	var phases []roadmap.Phase
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			log.Printf("graduate: phase audit: read %s: %v (skipping)", m, err)
			continue
		}
		rm, perr := roadmap.Parse(data)
		if perr != nil {
			log.Printf("graduate: phase audit: parse %s: %v (skipping)", m, perr)
			continue
		}
		phases = append(phases, rm.Phases...)
	}
	return phases
}

// AuditPhases runs AuditPhase across all phases in bounded parallel. A phase that
// errors is skipped (logged) — partial coverage, never an abort. Returns one
// verdict per successful phase plus its summary.
func AuditPhases(ctx context.Context, runner Runner, worktree string, phases []roadmap.Phase) ([]*GraduationVerdict, []PhaseSummary) {
	verdicts := make([]*GraduationVerdict, len(phases))
	summaries := make([]PhaseSummary, len(phases))
	errs := make([]error, len(phases))
	runBounded(len(phases), ensembleConcurrency, func(i int) {
		verdicts[i], summaries[i], errs[i] = AuditPhase(ctx, runner, worktree, phases[i])
	})
	var okV []*GraduationVerdict
	var okS []PhaseSummary
	for i := range phases {
		if errs[i] != nil {
			log.Printf("graduate: phase %s audit skipped: %v", phases[i].Number, errs[i])
			continue
		}
		okV = append(okV, verdicts[i])
		okS = append(okS, summaries[i])
	}
	return okV, okS
}
