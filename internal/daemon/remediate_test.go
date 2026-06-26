package daemon

import (
	"context"
	"testing"

	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/store"
)

func remediateTestDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	d := newTestDaemon(t)
	slug := "remedy"
	if err := d.store.InsertProject(context.Background(), &store.Project{
		ID: slug, Slug: slug, Name: "R", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	return d, slug
}

func confirmedHigh(title string) graduate.Finding {
	tru := true
	return graduate.Finding{Severity: "High", Category: "Missing", Title: title, Evidence: "a.go:1",
		Recommendation: "do " + title, Confirmed: &tru}
}

func writeResult(t *testing.T, d *Daemon, slug string, findings []graduate.Finding) {
	t.Helper()
	d.persistGraduateResult(graduate.GraduateResult{
		Slug: slug, Outcome: "blocked", Stage: "audit", Feature: "feat", Target: "main",
		Verdict: &graduate.GraduationVerdict{Status: "GAPS_FOUND", Findings: findings},
	})
}

func TestRemediateCreatesTasksForConfirmedCH(t *testing.T) {
	d, slug := remediateTestDaemon(t)
	tru := true
	fls := false
	writeResult(t, d, slug, []graduate.Finding{
		confirmedHigh("alpha"),
		{Severity: "High", Category: "Missing", Title: "refuted", Confirmed: &fls},
		{Severity: "Medium", Category: "Incomplete", Title: "medium", Confirmed: nil},
		{Severity: "Critical", Category: "Missing", Title: "crit", Confirmed: &tru},
	})
	res, err := d.remediateFromGraduate(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 { // alpha (High) + crit (Critical) only
		t.Fatalf("created %d want 2: %+v", len(res.Created), res.Created)
	}
	tasks, _ := d.store.ListTasksBySource(context.Background(), slug, "graduate")
	if len(tasks) != 2 {
		t.Fatalf("store has %d graduate tasks want 2", len(tasks))
	}
	if tasks[0].Body == "" {
		t.Error("empty task body")
	}
}

func TestRemediateIdempotentOnOpenTask(t *testing.T) {
	d, slug := remediateTestDaemon(t)
	writeResult(t, d, slug, []graduate.Finding{confirmedHigh("alpha")})
	if _, err := d.remediateFromGraduate(context.Background(), slug); err != nil {
		t.Fatal(err)
	}
	res, err := d.remediateFromGraduate(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 0 || res.Skipped != 1 {
		t.Fatalf("created=%d skipped=%d want 0/1", len(res.Created), res.Skipped)
	}
}

func TestRemediateRecreatesAfterCompletion(t *testing.T) {
	d, slug := remediateTestDaemon(t)
	writeResult(t, d, slug, []graduate.Finding{confirmedHigh("alpha")})
	r1, _ := d.remediateFromGraduate(context.Background(), slug)
	if err := d.store.UpdateTaskStatus(context.Background(), r1.Created[0].ID, "done"); err != nil {
		t.Fatal(err)
	}
	res, err := d.remediateFromGraduate(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created=%d want 1 (recurrence re-creates)", len(res.Created))
	}
}

func TestRemediateNoResultErrors(t *testing.T) {
	d, slug := remediateTestDaemon(t)
	if _, err := d.remediateFromGraduate(context.Background(), slug); err == nil {
		t.Error("missing persisted result must error")
	}
}

func TestRemediateDedupsSameFingerprintWithinOneCall(t *testing.T) {
	d, slug := remediateTestDaemon(t)
	tru := true
	// Two findings with identical category+title, evidence differing only by line
	// number → SAME fingerprint (line numbers are stripped). Only one task created.
	writeResult(t, d, slug, []graduate.Finding{
		{Severity: "High", Category: "Missing", Title: "dup gap", Evidence: "a.go:1", Confirmed: &tru},
		{Severity: "High", Category: "Missing", Title: "dup gap", Evidence: "a.go:42", Confirmed: &tru},
	})
	res, err := d.remediateFromGraduate(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || res.Skipped != 1 {
		t.Fatalf("created=%d skipped=%d want 1/1 (same fingerprint within one call)", len(res.Created), res.Skipped)
	}
}

func TestRemediateExcludesUnverifiedHigh(t *testing.T) {
	d, slug := remediateTestDaemon(t)
	// A Critical/High with Confirmed==nil (unverified) must NOT create a task —
	// the gate is confirmed-only, not severity-only.
	writeResult(t, d, slug, []graduate.Finding{
		{Severity: "Critical", Category: "Missing", Title: "unverified crit", Evidence: "a.go:1", Confirmed: nil},
		{Severity: "High", Category: "Missing", Title: "unverified high", Evidence: "b.go:1", Confirmed: nil},
	})
	res, err := d.remediateFromGraduate(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 0 {
		t.Fatalf("created=%d want 0 (unverified C/H must be excluded)", len(res.Created))
	}
}
