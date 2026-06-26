package modals

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPickerInitialState(t *testing.T) {
	m := NewChatSessionPicker()
	if !m.loading {
		t.Errorf("loading=false, want true initially")
	}
	if m.cursor != 0 {
		t.Errorf("cursor=%d, want 0", m.cursor)
	}
}

func TestPickerSetRowsClearsLoading(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "a"}})
	if m.loading {
		t.Errorf("loading still true after SetRows")
	}
	// SetRows prepends the "+ New session" sentinel, so total = 1 + 1 = 2.
	if len(m.rows) != 2 {
		t.Errorf("rows=%d, want 2 (sentinel + 1 session)", len(m.rows))
	}
	if m.rows[0].ID != "" {
		t.Errorf("row 0 should be the empty-ID sentinel, got %q", m.rows[0].ID)
	}
	if m.rows[1].ID != "a" {
		t.Errorf("row 1 should be the supplied session, got %q", m.rows[1].ID)
	}
}

func TestPickerInitialSentinelOnlyWhileLoading(t *testing.T) {
	// The sentinel is present from construction so the picker is never
	// empty/unselectable, even during the initial load.
	m := NewChatSessionPicker()
	if len(m.rows) != 1 || m.rows[0].ID != "" {
		t.Errorf("expected sentinel-only rows on construction, got %v", m.rows)
	}
}

func TestPickerDownCursor(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	// Rows now: [sentinel, a, b, c]; cursor starts at 0 on sentinel.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
	pk := updated.(*ChatSessionPicker)
	if pk.cursor != 3 {
		t.Errorf("cursor=%d, want 3 (last row) after three downs", pk.cursor)
	}
	// Bounds check
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
	pk = updated.(*ChatSessionPicker)
	if pk.cursor != 3 {
		t.Errorf("cursor went past end: %d", pk.cursor)
	}
}

func TestPickerEnterEmitsResume(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "a"}, {ID: "b"}})
	// Rows now: [sentinel, a, b]. Two downs lands on "b".
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("nil cmd on enter")
	}
	msg := cmd()
	resume, ok := msg.(SessionPickerResumeMsg)
	if !ok {
		t.Fatalf("got %T, want SessionPickerResumeMsg", msg)
	}
	if resume.SessionID != "b" {
		t.Errorf("SessionID=%q, want b", resume.SessionID)
	}
}

func TestPickerEnterOnSentinelEmitsNewSession(t *testing.T) {
	// Cursor starts on the sentinel row — Enter creates a new session
	// regardless of how many real sessions exist (including zero).
	m := NewChatSessionPicker()
	m.SetRows(nil)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("nil cmd on enter at sentinel")
	}
	if _, ok := cmd().(NewChatSessionMsg); !ok {
		t.Errorf("got %T, want NewChatSessionMsg", cmd())
	}
}

func TestPickerDeleteKeyOnRealRowEntersConfirming(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "a"}, {ID: "b"}})
	// Step cursor to a real row (index 1).
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, cmd := updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	pk := updated.(*ChatSessionPicker)
	if !pk.confirming {
		t.Errorf("expected confirming=true after d on real row")
	}
	if cmd != nil {
		t.Errorf("d should not emit a cmd yet (waits for y)")
	}
}

func TestPickerDeleteKeyOnSentinelNoOp(t *testing.T) {
	// d on the "+ New session" sentinel must not enter confirming.
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "a"}})
	// cursor starts at 0 = sentinel
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	pk := updated.(*ChatSessionPicker)
	if pk.confirming {
		t.Errorf("d on sentinel wrongly entered confirming")
	}
	if cmd != nil {
		t.Errorf("d on sentinel produced cmd: %v", cmd)
	}
}

func TestPickerConfirmYesEmitsDeleteRequest(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "doomed", Name: "test session"}})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown}) // cursor → real row
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	updated, cmd := updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	pk := updated.(*ChatSessionPicker)
	if pk.confirming {
		t.Errorf("confirming should be cleared after y")
	}
	if cmd == nil {
		t.Fatal("nil cmd on y after d")
	}
	req, ok := cmd().(ChatSessionDeleteRequestMsg)
	if !ok {
		t.Fatalf("got %T, want ChatSessionDeleteRequestMsg", cmd())
	}
	if req.SessionID != "doomed" {
		t.Errorf("SessionID=%q, want doomed", req.SessionID)
	}
}

func TestPickerConfirmNoCancels(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "kept"}})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	updated, cmd := updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	pk := updated.(*ChatSessionPicker)
	if pk.confirming {
		t.Errorf("confirming should be cleared after n")
	}
	if cmd != nil {
		t.Errorf("cancel should not emit a request: %v", cmd)
	}
}

func TestPickerConfirmEscCancels(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "kept"}})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	updated, cmd := updated.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pk := updated.(*ChatSessionPicker)
	if pk.confirming {
		t.Errorf("esc should cancel confirm, but confirming still true")
	}
	if cmd != nil {
		// esc-while-confirming must not close the modal (that's the
		// normal esc behavior); it just cancels the confirmation.
		if _, ok := cmd().(CloseMsg); ok {
			t.Errorf("esc during confirm wrongly emitted CloseMsg")
		}
	}
}

func TestPickerApplyDeletedRowRemovesAndClampsCursor(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	// Move cursor to last row.
	for i := 0; i < 3; i++ {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = updated.(*ChatSessionPicker)
	}
	if m.cursor != 3 {
		t.Fatalf("cursor=%d, expected 3 (last row 'c')", m.cursor)
	}
	if !m.ApplyDeletedRow("c") {
		t.Fatal("ApplyDeletedRow returned false for present id")
	}
	// After removing 'c', rows=[sentinel,a,b], cursor must clamp to 2.
	if m.cursor != 2 {
		t.Errorf("cursor=%d after deleting last row, want 2", m.cursor)
	}
	if len(m.rows) != 3 {
		t.Errorf("rows=%d, want 3", len(m.rows))
	}
}

func TestPickerCtrlNEmitsNewSession(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "a"}, {ID: "b"}})
	// Cursor parked on a real row.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := updated.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	if cmd == nil {
		t.Fatal("nil cmd on ctrl+n")
	}
	if _, ok := cmd().(NewChatSessionMsg); !ok {
		t.Errorf("got %T, want NewChatSessionMsg", cmd())
	}
}

func TestPickerEscEmitsClose(t *testing.T) {
	m := NewChatSessionPicker()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("nil cmd on esc")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("got %T, want CloseMsg", cmd())
	}
}

func TestPickerUpCursor(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	// Move cursor down to 2
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
	// Now decrement
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyUp})
	pk := updated.(*ChatSessionPicker)
	if pk.cursor != 1 {
		t.Errorf("cursor=%d after one up from 2, want 1", pk.cursor)
	}
	// 'k' alias
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	pk = updated.(*ChatSessionPicker)
	if pk.cursor != 0 {
		t.Errorf("cursor=%d after k from 1, want 0", pk.cursor)
	}
	// At-zero no-op
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyUp})
	pk = updated.(*ChatSessionPicker)
	if pk.cursor != 0 {
		t.Errorf("cursor went below 0: %d", pk.cursor)
	}
}

func TestPickerViewIncludesKeyHelpFooter(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "s1", Name: "a"}})
	// Cursor on sentinel — sentinel-specific footer.
	out := m.View(80, 24)
	if !strings.Contains(out, "ctrl+n new") {
		t.Errorf("footer missing ctrl+n hint on sentinel:\n%s", out)
	}
	if !strings.Contains(out, "esc close") {
		t.Errorf("footer missing esc close hint:\n%s", out)
	}
	if strings.Contains(out, "d delete") {
		t.Errorf("sentinel footer should not advertise d delete:\n%s", out)
	}
	// Move cursor to a real row — delete becomes available.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	out = updated.(*ChatSessionPicker).View(80, 24)
	if !strings.Contains(out, "d delete") {
		t.Errorf("real-row footer missing d delete hint:\n%s", out)
	}
}

func TestPickerViewSwapsFooterForDeleteConfirm(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "s1", Name: "to-go"}})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	out := updated.(*ChatSessionPicker).View(80, 24)
	if !strings.Contains(out, "delete 'to-go'") {
		t.Errorf("delete confirm footer not shown:\n%s", out)
	}
	if strings.Contains(out, "↑↓") {
		t.Errorf("nav hint should be replaced by delete-confirm prompt:\n%s", out)
	}
}

func TestPickerViewIncludesNewSessionEntry(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{{ID: "s1", Name: "an existing session"}})
	out := m.View(80, 24)
	if !strings.Contains(out, "+ New session") {
		t.Errorf("'+ New session' sentinel missing from picker view:\n%s", out)
	}
	// Sentinel must render before the existing rows.
	newIdx := strings.Index(out, "+ New session")
	exIdx := strings.Index(out, "an existing session")
	if !(newIdx < exIdx) {
		t.Errorf("sentinel should appear before existing rows; new=%d existing=%d", newIdx, exIdx)
	}
}

func TestPickerViewIncludesNameAndProvider(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{
		{ID: "s1", Name: "smoke session", Provider: "claude-code", StartedAt: 1000},
	})
	out := m.View(80, 24)
	if !strings.Contains(out, "smoke session") {
		t.Errorf("name missing from picker view: %s", out)
	}
	if !strings.Contains(out, "cc") {
		t.Errorf("provider abbreviation missing: %s", out)
	}
}

func TestPickerViewShowsUnnamedWhenNameEmpty(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{
		{ID: "s1", Surface: "cli", StartedAt: 1000},
	})
	out := m.View(80, 24)
	if !strings.Contains(out, "(unnamed)") {
		t.Errorf("'(unnamed)' fallback missing: %s", out)
	}
}

func TestPickerViewApiProviderPassesThrough(t *testing.T) {
	m := NewChatSessionPicker()
	m.SetRows([]SessionRow{
		{ID: "s1", Name: "api session", Provider: "api", StartedAt: 1000},
	})
	out := m.View(80, 24)
	if !strings.Contains(out, "api") {
		t.Errorf("api provider missing from picker view: %s", out)
	}
	// Should NOT be abbreviated to cc
	if strings.Contains(out, " cc ") {
		t.Errorf("api provider wrongly rendered as cc: %s", out)
	}
}
