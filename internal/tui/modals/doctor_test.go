package modals

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/doctor"
)

// fakeReport builds a small doctor.Report spanning two subsystems with a
// mix of statuses, including a hint, so tests exercise grouping + glyphs.
func fakeReport() doctor.Report {
	rep := doctor.Report{
		Checks: []doctor.Check{
			{Name: "socket", Subsystem: "daemon", Status: doctor.StatusOK, Message: "reachable"},
			{Name: "schema", Subsystem: "store", Status: doctor.StatusWarn, Message: "drift detected", Hint: "run migrate"},
			{Name: "orphans", Subsystem: "worktrees", Status: doctor.StatusError, Message: "2 orphan worktrees", Hint: "/a\n/b"},
		},
	}
	rep.Summary = doctor.Summary{OK: 1, Warnings: 1, Errors: 1, Skipped: 0}
	return rep
}

func TestDoctorModalInitialStateIsRunning(t *testing.T) {
	m := NewDoctorModal()
	if !m.loading {
		t.Fatal("modal should open in the loading/running state")
	}
	out := m.View(100, 40)
	if !strings.Contains(out, "running checks") {
		t.Errorf("running-state view should show a progress line; got:\n%s", out)
	}
}

func TestDoctorModalRendersReportChecksAndSummary(t *testing.T) {
	m := NewDoctorModal()
	m.Update(RPCResultMsg{Kind: "doctor_report", Data: map[string]any{"report": fakeReport()}})
	if m.loading {
		t.Fatal("loading should clear once the report arrives")
	}
	out := m.View(120, 40)
	// Summary counts.
	for _, want := range []string{"1 ok", "1 warn", "1 error", "0 skip"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary should contain %q; got:\n%s", want, out)
		}
	}
	// Subsystem groups + check names/messages.
	for _, want := range []string{"daemon", "store", "worktrees", "socket", "drift detected", "2 orphan worktrees"} {
		if !strings.Contains(out, want) {
			t.Errorf("report view should contain %q; got:\n%s", want, out)
		}
	}
	// Composite-check hint detail (orphan worktree paths).
	if !strings.Contains(out, "/a") || !strings.Contains(out, "/b") {
		t.Errorf("hint lines should render; got:\n%s", out)
	}
}

// TestDoctorModalRReRuns: pressing r from the report state re-enters loading
// and emits the doctor_run SubmitRequest so the root re-fires doctor.Run.
func TestDoctorModalRReRuns(t *testing.T) {
	m := NewDoctorModal()
	m.Update(RPCResultMsg{Kind: "doctor_report", Data: map[string]any{"report": fakeReport()}})
	if m.loading {
		t.Fatal("precondition: report should be loaded (not loading)")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if !m.loading {
		t.Error("r should re-enter the loading state")
	}
	if cmd == nil {
		t.Fatal("r should emit a re-run command")
	}
	msg := cmd()
	sr, ok := msg.(SubmitRequest)
	if !ok {
		t.Fatalf("r should emit a SubmitRequest; got %T", msg)
	}
	if sr.Kind != "doctor_run" {
		t.Errorf("re-run Kind=%q want doctor_run", sr.Kind)
	}
}

// TestDoctorModalErrorSurfaced: a transport error on the report result shows
// inline and keeps esc/retry available.
func TestDoctorModalErrorSurfaced(t *testing.T) {
	m := NewDoctorModal()
	m.Update(RPCResultMsg{Kind: "doctor_report", Err: errors.New("daemon down")})
	out := m.View(100, 40)
	if !strings.Contains(out, "daemon down") {
		t.Errorf("error should surface inline; got:\n%s", out)
	}
}

// TestDoctorModalEscCloses: esc emits CloseMsg from the report state.
func TestDoctorModalEscCloses(t *testing.T) {
	m := NewDoctorModal()
	m.Update(RPCResultMsg{Kind: "doctor_report", Data: map[string]any{"report": fakeReport()}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit a command")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc should emit CloseMsg; got %T", cmd())
	}
}
