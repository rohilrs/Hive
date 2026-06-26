package modals

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/graduate"
)

// TestGraduateModalFindingsScrollable: the expanded findings view must window to
// the modal height (never overflow + clip) and be scrollable.
func TestGraduateModalFindingsScrollable(t *testing.T) {
	m := NewGraduateModal("p", "spec/feat", "main").(*GraduateModal)
	var fs []graduate.Finding
	for i := 0; i < 25; i++ {
		fs = append(fs, graduate.Finding{Severity: "High", Category: "Missing",
			Title: fmt.Sprintf("finding number %d", i), Evidence: "apps/x/file.go:42", Recommendation: "do the thing"})
	}
	rec := graduate.GraduateResult{Mode: "dry-run", Outcome: "blocked", EndedAt: 1,
		Verdict: &graduate.GraduationVerdict{Status: "GAPS_FOUND", Summary: "lots", Findings: fs}}
	rb, _ := json.Marshal(rec)
	m.Update(RPCResultMsg{Kind: "graduate_status", Data: map[string]any{"_graduate_status": string(rb)}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}}) // expand findings

	const h = 16
	// The box height must be CONSTANT at every scroll position — never overflow
	// (footer clipped at bottom) AND never visibly grow/shrink as the ↑/↓
	// affordances appear/disappear (the two prior bugs). Also confirm scrolling
	// actually moves the window.
	wantLines := strings.Count(m.View(90, h), "\n") + 1
	if wantLines > h {
		t.Fatalf("findings view overflows height %d: %d lines:\n%s", h, wantLines, m.View(90, h))
	}
	scrolled := false
	prev := m.View(90, h)
	for step := 0; step < 8; step++ {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
		v := m.View(90, h)
		if lines := strings.Count(v, "\n") + 1; lines != wantLines {
			t.Fatalf("box height changed at scroll step %d: %d lines, want constant %d:\n%s", step, lines, wantLines, v)
		}
		if v != prev {
			scrolled = true
		}
		prev = v
	}
	if !scrolled {
		t.Errorf("down should scroll the findings window (it never changed)")
	}
	// esc/v collapses back to the mode selector.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v := m.View(90, h); !strings.Contains(v, "Dry-run") {
		t.Errorf("esc should collapse findings back to the mode selector:\n%s", v)
	}
}

func TestGraduateModalResultScrollable(t *testing.T) {
	m := NewGraduateModal("p", "spec/feat", "main").(*GraduateModal)
	// Drive into the result state with a long verdict + a PR URL outcome.
	var fs []graduate.Finding
	for i := 0; i < 25; i++ {
		fs = append(fs, graduate.Finding{Severity: "High", Title: fmt.Sprintf("finding number %d", i)})
	}
	rb, _ := json.Marshal(graduate.GraduationVerdict{Status: "GAPS_FOUND", Summary: "lots", Findings: fs})
	// forward verdict then done (the lifecycle discriminator keys used by the modal)
	m.Update(RPCResultMsg{Kind: "project_graduate_open", Data: map[string]any{"_graduate_verdict": string(rb)}})
	m.Update(RPCResultMsg{Kind: "project_graduate_open", Data: map[string]any{"_graduate_done": true, "_graduate_pr_url": "https://x/pr/1"}})

	const h = 16
	want := strings.Count(m.View(90, h), "\n") + 1
	if want > h {
		t.Fatalf("result view overflows height %d: %d lines:\n%s", h, want, m.View(90, h))
	}
	// The pinned outcome (PR URL) must be visible regardless of the long verdict.
	if !strings.Contains(m.View(90, h), "https://x/pr/1") {
		t.Errorf("PR URL outcome should be pinned + visible:\n%s", m.View(90, h))
	}
	// Constant box height while scrolling.
	scrolled := false
	prev := m.View(90, h)
	for i := 0; i < 8; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
		v := m.View(90, h)
		if got := strings.Count(v, "\n") + 1; got != want {
			t.Fatalf("box height changed at step %d: %d want %d:\n%s", i, got, want, v)
		}
		if v != prev {
			scrolled = true
		}
		prev = v
	}
	if !scrolled {
		t.Errorf("down should scroll the verdict region")
	}
}

// drainModal applies a message and returns the concrete *GraduateModal.
func asGrad(t *testing.T, m Modal) *GraduateModal {
	t.Helper()
	g, ok := m.(*GraduateModal)
	if !ok {
		t.Fatalf("modal is %T; want *GraduateModal", m)
	}
	return g
}

func TestGraduateModalDefaultsToDryRun(t *testing.T) {
	// The default selection MUST be Dry-run, and a bare enter must submit with
	// dry_run=true / force=false / draft=false.
	m := NewGraduateModal("hive", "feature/x", "staging")
	g := asGrad(t, m)
	if g.modeSel != gradModeDryRun {
		t.Fatalf("default modeSel=%d want gradModeDryRun(%d)", g.modeSel, gradModeDryRun)
	}

	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should emit a SubmitRequest cmd")
	}
	// The submit cmd is batched with the spinner tick; walk the batch.
	req := findSubmit(t, cmd)
	if req.Kind != "project_graduate_open" {
		t.Fatalf("Kind=%q want project_graduate_open", req.Kind)
	}
	if got := req.Params["dry_run"]; got != true {
		t.Errorf("dry_run=%v want true (default)", got)
	}
	if got := req.Params["force"]; got != false {
		t.Errorf("force=%v want false", got)
	}
	if got := req.Params["draft"]; got != false {
		t.Errorf("draft=%v want false", got)
	}
	if got := req.Params["slug"]; got != "hive" {
		t.Errorf("slug=%v want hive", got)
	}
	// Modal should now be in the running state.
	if asGrad(t, upd).state != gradRunning {
		t.Errorf("state=%d want gradRunning after submit", asGrad(t, upd).state)
	}
}

func TestGraduateModalForceModeSubmitsForce(t *testing.T) {
	// down+down selects Graduate --force; submit must carry force=true,
	// dry_run=false. A space toggles draft on.
	m := NewGraduateModal("hive", "feature/x", "staging")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})  // → Graduate
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})  // → Graduate --force
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace}) // toggle draft

	if g := asGrad(t, m); g.modeSel != gradModeForce {
		t.Fatalf("modeSel=%d want gradModeForce(%d)", g.modeSel, gradModeForce)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	req := findSubmit(t, cmd)
	if req.Params["force"] != true {
		t.Errorf("force=%v want true", req.Params["force"])
	}
	if req.Params["dry_run"] != false {
		t.Errorf("dry_run=%v want false", req.Params["dry_run"])
	}
	if req.Params["draft"] != true {
		t.Errorf("draft=%v want true", req.Params["draft"])
	}
}

func TestGraduateModalRealModeSubmits(t *testing.T) {
	// single down selects Graduate (real PR, no force).
	m := NewGraduateModal("hive", "feature/x", "staging")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if g := asGrad(t, m); g.modeSel != gradModeReal {
		t.Fatalf("modeSel=%d want gradModeReal(%d)", g.modeSel, gradModeReal)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	req := findSubmit(t, cmd)
	if req.Params["force"] != false {
		t.Errorf("force=%v want false", req.Params["force"])
	}
	if req.Params["dry_run"] != false {
		t.Errorf("dry_run=%v want false", req.Params["dry_run"])
	}
}

func TestGraduateModalEscCancels(t *testing.T) {
	m := NewGraduateModal("hive", "feature/x", "staging")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit a CloseMsg cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Fatalf("esc emitted %T; want CloseMsg", cmd())
	}
}

func TestGraduateModalRendersProgress(t *testing.T) {
	// After submit + a forwarded progress event, the running view lists the
	// accumulated "→ <label>" lines.
	m := NewGraduateModal("hive", "feature/x", "staging")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → running
	m, _ = m.Update(RPCResultMsg{
		Kind: "project_graduate_open",
		Data: map[string]any{"_graduate_progress": "shippability: typecheck"},
	})
	m, _ = m.Update(RPCResultMsg{
		Kind: "project_graduate_open",
		Data: map[string]any{"_graduate_progress": "audit"},
	})
	out := m.View(100, 30)
	if !strings.Contains(out, "shippability: typecheck") {
		t.Errorf("progress view missing first label:\n%s", out)
	}
	if !strings.Contains(out, "audit") {
		t.Errorf("progress view missing second label:\n%s", out)
	}
}

func TestGraduateModalRendersVerdictThenFailed(t *testing.T) {
	// A blocking verdict fires verdict-then-failed; the modal MUST keep the
	// verdict visible AND render the failure beneath it.
	m := NewGraduateModal("hive", "feature/x", "staging")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	v := graduate.GraduationVerdict{
		Status:  "GAPS_FOUND",
		Summary: "two findings block graduation",
		Findings: []graduate.Finding{
			{Severity: "High", Title: "missing migration"},
			{Severity: "Low", Title: "stale comment"},
		},
	}
	vj, _ := json.Marshal(v)
	m, _ = m.Update(RPCResultMsg{
		Kind: "project_graduate_open",
		Data: map[string]any{"_graduate_verdict": string(vj)},
	})
	m, _ = m.Update(RPCResultMsg{
		Kind: "project_graduate_open",
		Data: map[string]any{"_graduate_failed": "blocking findings present"},
	})

	out := m.View(100, 30)
	if !strings.Contains(out, "GAPS_FOUND") {
		t.Errorf("verdict status missing:\n%s", out)
	}
	if !strings.Contains(out, "two findings block graduation") {
		t.Errorf("verdict summary missing:\n%s", out)
	}
	if !strings.Contains(out, "missing migration") {
		t.Errorf("finding title missing:\n%s", out)
	}
	if !strings.Contains(out, "blocking findings present") {
		t.Errorf("failure error missing (must render beneath verdict):\n%s", out)
	}
}

func TestGraduateModalRendersDryRunDone(t *testing.T) {
	m := NewGraduateModal("hive", "feature/x", "staging")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = m.Update(RPCResultMsg{
		Kind: "project_graduate_open",
		Data: map[string]any{"_graduate_done": true, "_graduate_dry_run": true},
	})
	out := m.View(100, 30)
	if !strings.Contains(out, "dry-run complete") {
		t.Errorf("dry-run done view missing 'dry-run complete':\n%s", out)
	}
}

func TestGraduateModalRendersPRDone(t *testing.T) {
	m := NewGraduateModal("hive", "feature/x", "staging")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = m.Update(RPCResultMsg{
		Kind: "project_graduate_open",
		Data: map[string]any{
			"_graduate_done":    true,
			"_graduate_dry_run": false,
			"_graduate_pr_url":  "https://github.com/o/r/pull/42",
		},
	})
	out := m.View(100, 30)
	if !strings.Contains(out, "https://github.com/o/r/pull/42") {
		t.Errorf("done view missing PR URL:\n%s", out)
	}
}

func TestGraduateModalStartErrorRendersFailure(t *testing.T) {
	// A failed async start arrives as a "_graduate_started" ack with Err set;
	// the modal drops to the result state and shows the error.
	m := NewGraduateModal("hive", "feature/x", "staging")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = m.Update(RPCResultMsg{
		Kind: "project_graduate_open",
		Err:  errors.New("daemon did not return a graduate_id"),
		Data: map[string]any{"_graduate_started": true},
	})
	out := m.View(100, 30)
	if !strings.Contains(out, "daemon did not return a graduate_id") {
		t.Errorf("start-error view missing the error:\n%s", out)
	}
}

// findSubmit walks a (possibly batched) tea.Cmd and returns the SubmitRequest
// it produces. The submit cmd is batched with the spinner tick.
func findSubmit(t *testing.T, cmd tea.Cmd) SubmitRequest {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil cmd; want one producing a SubmitRequest")
	}
	msg := cmd()
	if req, ok := msg.(SubmitRequest); ok {
		return req
	}
	// Batched: BatchMsg is a []tea.Cmd; execute each to find the SubmitRequest.
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c == nil {
				continue
			}
			if req, ok := c().(SubmitRequest); ok {
				return req
			}
		}
	}
	t.Fatalf("cmd did not produce a SubmitRequest; got %T", msg)
	return SubmitRequest{}
}

func TestGraduateModalRendersLastRun(t *testing.T) {
	m := NewGraduateModal("conv-rework", "spec/feat", "main").(*GraduateModal)
	rec := graduate.GraduateResult{Mode: "dry-run", Outcome: "blocked", EndedAt: 1717000600,
		Verdict: &graduate.GraduationVerdict{Status: "GAPS_FOUND", Findings: []graduate.Finding{{Severity: "High", Title: "no sink", Evidence: "emit.ts:61", Recommendation: "add sink"}}}}
	rb, _ := json.Marshal(rec)
	m.Update(RPCResultMsg{Kind: "graduate_status", Data: map[string]any{"_graduate_status": string(rb)}})
	v := m.View(80, 24)
	if !strings.Contains(v, "Last run") || !strings.Contains(v, "dry-run") || !strings.Contains(v, "blocked") {
		t.Errorf("confirm view should show the last-run header:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	v = m.View(80, 24)
	if !strings.Contains(v, "no sink") || !strings.Contains(v, "emit.ts:61") {
		t.Errorf("v should expand findings:\n%s", v)
	}
}

func TestGraduateRemediateResultStatus(t *testing.T) {
	// created > 0 with skips
	m := NewGraduateModal("p", "spec/feat", "main").(*GraduateModal)
	m.Update(RPCResultMsg{Kind: "project_remediate", Data: map[string]any{
		"created": []any{map[string]any{"id": "t1", "title": "x"}},
		"skipped": float64(2),
	}})
	if !strings.Contains(m.remediateStatus, "Created 1") {
		t.Errorf("created status = %q", m.remediateStatus)
	}
	if !strings.Contains(m.remediateStatus, "2 skipped") {
		t.Errorf("created status missing skipped count = %q", m.remediateStatus)
	}

	// created == 0
	m2 := NewGraduateModal("p", "spec/feat", "main").(*GraduateModal)
	m2.Update(RPCResultMsg{Kind: "project_remediate", Data: map[string]any{
		"created": []any{},
		"skipped": float64(0),
	}})
	if !strings.Contains(m2.remediateStatus, "Nothing to remediate") {
		t.Errorf("empty status = %q", m2.remediateStatus)
	}

	// created == 0 with already-open skips
	m4 := NewGraduateModal("p", "spec/feat", "main").(*GraduateModal)
	m4.Update(RPCResultMsg{Kind: "project_remediate", Data: map[string]any{
		"created": []any{},
		"skipped": float64(3),
	}})
	if !strings.Contains(m4.remediateStatus, "Nothing to remediate") {
		t.Errorf("already-open status = %q", m4.remediateStatus)
	}
	if !strings.Contains(m4.remediateStatus, "3 already open") {
		t.Errorf("already-open status missing count = %q", m4.remediateStatus)
	}

	// error
	m3 := NewGraduateModal("p", "spec/feat", "main").(*GraduateModal)
	m3.Update(RPCResultMsg{Kind: "project_remediate", Err: fmt.Errorf("boom")})
	if !strings.Contains(m3.remediateStatus, "Remediate failed") {
		t.Errorf("error status = %q", m3.remediateStatus)
	}
	if !strings.Contains(m3.remediateStatus, "boom") {
		t.Errorf("error status missing message = %q", m3.remediateStatus)
	}
}

func TestGraduateResultRemediateKey(t *testing.T) {
	m := NewGraduateModal("p", "spec/feat", "main").(*GraduateModal)
	m.state = gradResult
	tru := true
	m.lastRun = &graduate.GraduateResult{Verdict: &graduate.GraduationVerdict{
		Findings: []graduate.Finding{{Severity: "High", Title: "x", Confirmed: &tru}},
	}}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("r should emit a SubmitRequest cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok || req.Kind != "project_remediate" {
		t.Fatalf("got %#v want SubmitRequest{project_remediate}", cmd())
	}
	if req.Params["project_slug"] != "p" {
		t.Errorf("project_slug=%v want p", req.Params["project_slug"])
	}
}
