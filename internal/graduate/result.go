package graduate

import (
	"fmt"
	"strings"
	"time"
)

// GraduateResult is the durable record of one `hive project graduate` run,
// written on every terminal outcome (see internal/daemon persistGraduateResult).
// It lives here (not in internal/daemon) so the CLI render path can reuse it
// without importing the daemon package.
type GraduateResult struct {
	Slug         string             `json:"slug"`
	Mode         string             `json:"mode"`    // dry-run | graduate | graduate-force
	Outcome      string             `json:"outcome"` // done | blocked | failed
	Stage        string             `json:"stage"`   // preconditions | worktree | gate:<name> | audit | pr | complete
	Feature      string             `json:"feature"`
	Target       string             `json:"target"`
	StartedAt    int64              `json:"started_at"`
	EndedAt      int64              `json:"ended_at"`
	Verdict      *GraduationVerdict `json:"verdict,omitempty"`
	PRURL        string             `json:"pr_url,omitempty"`
	BuildSummary string             `json:"build_summary,omitempty"`
	Error        string             `json:"error,omitempty"`
}

// RenderResultMarkdown formats a GraduateResult as human-readable markdown. Used
// both by the persisted .md file and (potentially) the CLI.
func RenderResultMarkdown(r GraduateResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Graduate result: %s\n\n", r.Slug)
	fmt.Fprintf(&b, "- When: %s\n", time.Unix(r.EndedAt, 0).Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- Mode: %s\n", r.Mode)
	fmt.Fprintf(&b, "- Outcome: %s (stage: %s)\n", r.Outcome, r.Stage)
	if r.Feature != "" || r.Target != "" {
		fmt.Fprintf(&b, "- Branch: %s → %s\n", r.Feature, r.Target)
	}
	if r.PRURL != "" {
		fmt.Fprintf(&b, "- PR: %s\n", r.PRURL)
	}
	if r.BuildSummary != "" {
		fmt.Fprintf(&b, "- Shippability gates: %s\n", r.BuildSummary)
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "- Error: %s\n", r.Error)
	}
	b.WriteString("\n")
	if r.Verdict != nil {
		fmt.Fprintf(&b, "## Completion audit: %s\n\n", r.Verdict.Status)
		if strings.TrimSpace(r.Verdict.Summary) != "" {
			fmt.Fprintf(&b, "%s\n\n", r.Verdict.Summary)
		}
		for _, f := range r.Verdict.Findings {
			seen := ""
			if f.SeenCount > 0 {
				seen = fmt.Sprintf(", seen %d", f.SeenCount)
			}
			fmt.Fprintf(&b, "### [%s/%s] %s (%s%s)\n", f.Severity, f.Category, f.Title, f.ConfirmLabel(), seen)
			if strings.TrimSpace(f.Evidence) != "" {
				fmt.Fprintf(&b, "- Evidence: %s\n", f.Evidence)
			}
			if strings.TrimSpace(f.Recommendation) != "" {
				fmt.Fprintf(&b, "- Recommendation: %s\n", f.Recommendation)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
