package modals

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func sampleRows() []ChatToolResultRow {
	return []ChatToolResultRow{
		{Tool: "hive_list_projects", Result: `[{"id":"p-hive-smoke","name":"Hive"}]`, IsError: false},
		{Tool: "hive_status", Result: `{"pending_tasks":3}`, IsError: false},
		{Tool: "hive_get_task", Result: `{"error":"task not found"}`, IsError: true},
	}
}

func TestNewChatToolResultPickerInitialState(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	if m.state != statePicking {
		t.Errorf("state=%v, want statePicking", m.state)
	}
	if m.cursor != 0 {
		t.Errorf("cursor=%d, want 0", m.cursor)
	}
	if len(m.rows) != 3 {
		t.Errorf("rows=%d, want 3", len(m.rows))
	}
}

func TestChatToolResultPickerUpDownNavigation(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	// Down twice → cursor at 2
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
	pk := updated.(*ChatToolResultPicker)
	if pk.cursor != 2 {
		t.Errorf("cursor=%d after two downs, want 2", pk.cursor)
	}
	// Down again → clamp at len-1 = 2
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
	pk = updated.(*ChatToolResultPicker)
	if pk.cursor != 2 {
		t.Errorf("cursor=%d past end, want 2 (clamped)", pk.cursor)
	}
	// Up → cursor at 1
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyUp})
	pk = updated.(*ChatToolResultPicker)
	if pk.cursor != 1 {
		t.Errorf("cursor=%d after up, want 1", pk.cursor)
	}
	// 'k' alias for up → cursor at 0
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	pk = updated.(*ChatToolResultPicker)
	if pk.cursor != 0 {
		t.Errorf("cursor=%d after k, want 0", pk.cursor)
	}
	// Up at 0 → clamp at 0
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyUp})
	pk = updated.(*ChatToolResultPicker)
	if pk.cursor != 0 {
		t.Errorf("cursor=%d below 0, want 0 (clamped)", pk.cursor)
	}
}

func TestChatToolResultPickerEnterTransitionsToViewing(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pk := updated.(*ChatToolResultPicker)
	if pk.state != stateViewing {
		t.Errorf("state=%v after Enter, want stateViewing", pk.state)
	}
	// Viewport should contain the first row's Result.
	if !strings.Contains(pk.viewport.View(), "hive_list_projects") &&
		!strings.Contains(pk.viewport.View(), "p-hive-smoke") {
		t.Errorf("viewport content doesn't show selected row body: %q", pk.viewport.View())
	}
}

func TestChatToolResultPickerEscFromViewingReturnsToPicking(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pk := updated.(*ChatToolResultPicker)
	if pk.state != stateViewing {
		t.Fatalf("setup: expected stateViewing after Enter, got %v", pk.state)
	}
	// Esc → back to picking with cursor preserved.
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pk = updated.(*ChatToolResultPicker)
	if pk.state != statePicking {
		t.Errorf("state=%v after esc in viewing, want statePicking", pk.state)
	}
	if pk.cursor != 1 {
		t.Errorf("cursor=%d after esc in viewing, want 1 (preserved)", pk.cursor)
	}
}

func TestChatToolResultPickerEscFromPickingEmitsCloseMsg(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected CloseMsg cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("got %T, want CloseMsg", cmd())
	}
}

func TestChatToolResultPickerQFromViewingEmitsCloseMsg(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, cmd := updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected CloseMsg cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("got %T, want CloseMsg", cmd())
	}
}

func TestChatToolResultPickerEnterWithEmptyRowsIsNoOp(t *testing.T) {
	m := NewChatToolResultPicker(nil, 80, 24)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pk := updated.(*ChatToolResultPicker)
	if pk.state != statePicking {
		t.Errorf("state=%v after Enter on empty, want statePicking", pk.state)
	}
}

func TestChatToolResultPickerViewPickingShowsAllRows(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	out := m.View(80, 24)
	if !strings.Contains(out, "hive_list_projects") {
		t.Errorf("picking view missing 'hive_list_projects': %s", out)
	}
	if !strings.Contains(out, "hive_status") {
		t.Errorf("picking view missing 'hive_status': %s", out)
	}
	if !strings.Contains(out, "hive_get_task") {
		t.Errorf("picking view missing 'hive_get_task': %s", out)
	}
}

func TestChatToolResultPickerViewViewingShowsBody(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pk := updated.(*ChatToolResultPicker)
	out := pk.View(80, 24)
	// The body of row 0 should be in the rendered output.
	if !strings.Contains(out, "p-hive-smoke") {
		t.Errorf("viewing view missing body content: %s", out)
	}
}

// Resizing after Enter must not wedge the inspector at stale dims —
// pre-fix the viewport was sized once at Enter time and never updated,
// so a terminal resize left the body clipped or over-wide.
func TestChatToolResultPickerWindowResizeUpdatesViewport(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pk := updated.(*ChatToolResultPicker)
	if pk.state != stateViewing {
		t.Fatalf("setup: expected stateViewing after Enter, got %v", pk.state)
	}
	// Capture the pre-resize viewport width so the assertion below can
	// prove it actually changed.
	preW := pk.viewport.Width
	updated, _ = updated.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	pk = updated.(*ChatToolResultPicker)
	if pk.width != 120 {
		t.Errorf("width=%d after resize, want 120", pk.width)
	}
	if pk.height != 40 {
		t.Errorf("height=%d after resize, want 40", pk.height)
	}
	wantW := 120 - pickerVpWPad
	if pk.viewport.Width != wantW {
		t.Errorf("viewport.Width=%d, want %d (120 - pickerVpWPad)", pk.viewport.Width, wantW)
	}
	if pk.viewport.Width == preW {
		t.Errorf("viewport.Width didn't change from pre-resize value %d", preW)
	}
	wantH := 40 - pickerVpHPad
	if pk.viewport.Height != wantH {
		t.Errorf("viewport.Height=%d, want %d (40 - pickerVpHPad)", pk.viewport.Height, wantH)
	}
}

// Cursor must survive a round-trip through the inspector — Enter, Esc,
// Enter again should reopen on the row the user originally picked, not
// fall back to row 0.
func TestChatToolResultPickerCursorPreservedAcrossEnterEscEnter(t *testing.T) {
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	// Down → cursor at row 1 (hive_status).
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	// Enter → viewing row 1.
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pk := updated.(*ChatToolResultPicker)
	if pk.state != stateViewing {
		t.Fatalf("setup: expected stateViewing after first Enter, got %v", pk.state)
	}
	// Esc → back to picking, cursor preserved.
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pk = updated.(*ChatToolResultPicker)
	if pk.state != statePicking {
		t.Fatalf("expected statePicking after esc, got %v", pk.state)
	}
	if pk.cursor != 1 {
		t.Fatalf("cursor=%d after esc-back, want 1", pk.cursor)
	}
	// Enter again → back to viewing the SAME row's body.
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pk = updated.(*ChatToolResultPicker)
	if pk.state != stateViewing {
		t.Fatalf("expected stateViewing after re-Enter, got %v", pk.state)
	}
	if !strings.Contains(pk.viewport.View(), "pending_tasks") {
		t.Errorf("viewport after re-Enter doesn't show row 1 body (want substring 'pending_tasks'): %q",
			pk.viewport.View())
	}
}

// The error glyph (✗) must render in the picking list for rows where
// IsError is true. Pre-fix this was only covered indirectly by the
// content tests.
func TestChatToolResultPickerViewShowsErrorGlyph(t *testing.T) {
	// Row 2 of sampleRows() has IsError=true.
	m := NewChatToolResultPicker(sampleRows(), 80, 24)
	out := m.View(80, 24)
	if !strings.Contains(out, "✗") {
		t.Errorf("picking view missing ✗ for error row: %s", out)
	}
}
