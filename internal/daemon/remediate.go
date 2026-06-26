package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/store"
)

// remediateCreated is one task created by remediation.
type remediateCreated struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// remediateResult is the outcome of one remediate call.
type remediateResult struct {
	Created []remediateCreated `json:"created"`
	Skipped int                `json:"skipped"`
}

// terminalTaskStatuses are the statuses that make a task "closed" for dedup
// purposes. A finding whose remediation task is in one of these does NOT block
// re-creation (a finding recurring after its task was completed/abandoned means
// it was not actually fixed, so it is surfaced again).
var terminalTaskStatuses = map[string]bool{
	"done": true, "abandoned": true, "source_closed": true,
}

// remediateFromGraduate reads the persisted graduate result for slug and creates
// one inbox task per CONFIRMED Critical/High finding, deduped by the finding
// fingerprint against the project's OPEN graduate tasks. Idempotent: re-running
// after a partial graduation does not duplicate still-open tasks.
func (d *Daemon) remediateFromGraduate(ctx context.Context, slug string) (remediateResult, error) {
	proj, err := d.store.GetProjectBySlug(ctx, slug)
	if err != nil {
		return remediateResult{}, fmt.Errorf("project %q: %w", slug, err)
	}

	path := filepath.Join(d.HiveDir(), "graduate-"+slug+"-result.json")
	jb, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return remediateResult{}, fmt.Errorf("no graduate result for %q; run `hive project graduate` first", slug)
		}
		return remediateResult{}, fmt.Errorf("read graduate result: %w", err)
	}
	var rec graduate.GraduateResult
	if err := json.Unmarshal(jb, &rec); err != nil {
		return remediateResult{}, fmt.Errorf("parse graduate result: %w", err)
	}
	if rec.Verdict == nil {
		return remediateResult{}, nil
	}

	existing, err := d.store.ListTasksBySource(ctx, proj.ID, "graduate")
	if err != nil {
		return remediateResult{}, fmt.Errorf("list graduate tasks: %w", err)
	}
	openFP := map[string]bool{}
	for _, t := range existing {
		if !terminalTaskStatuses[t.Status] {
			openFP[t.SourceID] = true
		}
	}

	var res remediateResult
	for _, f := range rec.Verdict.Findings {
		if f.Severity != "Critical" && f.Severity != "High" {
			continue
		}
		if f.Confirmed == nil || !*f.Confirmed {
			continue
		}
		fp := graduate.FindingFingerprint(f)
		if openFP[fp] {
			res.Skipped++
			continue
		}
		task := &store.Task{
			ID:        newID("task"),
			ProjectID: proj.ID,
			Source:    "graduate",
			SourceID:  fp,
			Title:     f.Title,
			Body:      remediateTaskBody(f, rec),
			Priority:  "P2",
			Metadata: map[string]any{
				"graduate_finding_severity": f.Severity,
				"graduate_finding_category": f.Category,
			},
		}
		if err := d.store.InsertTask(ctx, task); err != nil {
			return res, fmt.Errorf("insert remediation task: %w", err)
		}
		openFP[fp] = true
		res.Created = append(res.Created, remediateCreated{ID: task.ID, Title: task.Title})
	}
	return res, nil
}

// remediateTaskBody composes the task body from a finding plus a backlink to the
// graduate run that surfaced it.
func remediateTaskBody(f graduate.Finding, rec graduate.GraduateResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Completion-audit gap found graduating %s → %s.\n\n", rec.Feature, rec.Target)
	fmt.Fprintf(&b, "**Severity:** %s (%s)\n", f.Severity, f.Category)
	if strings.TrimSpace(f.Evidence) != "" {
		fmt.Fprintf(&b, "**Evidence:** %s\n", f.Evidence)
	}
	if strings.TrimSpace(f.Recommendation) != "" {
		fmt.Fprintf(&b, "**Recommendation:** %s\n", f.Recommendation)
	}
	fmt.Fprintf(&b, "\n_From graduate audit of %s._\n", rec.Slug)
	return b.String()
}
