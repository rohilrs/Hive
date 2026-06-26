package graduate

import (
	"strings"
	"testing"
)

func TestRenderResultMarkdown(t *testing.T) {
	r := GraduateResult{
		Slug: "conv-rework", Mode: "dry-run", Outcome: "blocked", Stage: "audit",
		Feature: "spec/feat", Target: "main", StartedAt: 1717000000, EndedAt: 1717000600,
		BuildSummary: "all configured gates passed",
		Verdict: &GraduationVerdict{
			Status: "GAPS_FOUND", Summary: "two gaps",
			Findings: []Finding{{Severity: "High", Category: "Missing", Title: "no sink", Evidence: "emit.ts:61", Recommendation: "add sink"}},
		},
	}
	md := RenderResultMarkdown(r)
	for _, want := range []string{"conv-rework", "dry-run", "blocked", "GAPS_FOUND", "two gaps", "[High/Missing] no sink", "emit.ts:61", "add sink", "all configured gates passed"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestRenderResultMarkdownFailedNoVerdict(t *testing.T) {
	r := GraduateResult{Slug: "p", Mode: "graduate", Outcome: "failed", Stage: "gate:typecheck", Error: "typecheck gate FAILED"}
	md := RenderResultMarkdown(r)
	if !strings.Contains(md, "failed") || !strings.Contains(md, "gate:typecheck") || !strings.Contains(md, "typecheck gate FAILED") {
		t.Errorf("failed-render missing fields:\n%s", md)
	}
}

func TestRenderResultMarkdownShowsVerification(t *testing.T) {
	tru := true
	r := GraduateResult{
		Slug: "demo", Outcome: "blocked", Stage: "audit",
		Verdict: &GraduationVerdict{Status: "GAPS_FOUND", Findings: []Finding{
			{Severity: "High", Category: "Missing", Title: "gap", SeenCount: 2, Confirmed: &tru},
		}},
	}
	md := RenderResultMarkdown(r)
	if !strings.Contains(md, "confirmed") || !strings.Contains(md, "seen 2") {
		t.Errorf("markdown missing verification badge:\n%s", md)
	}
}
