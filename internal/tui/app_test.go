package tui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/modals"
	"github.com/rohilrs/Hive/internal/tui/tabs"
	"github.com/rohilrs/Hive/pkg/rpc"
)

var errTest = errors.New("boom")

// stubTab implements tabs.TabModel for root-model tests.
type stubTab struct{ name string }

func (s stubTab) Name() string                              { return s.name }
func (s stubTab) Init() tea.Cmd                             { return nil }
func (s stubTab) Update(_ tea.Msg) (tabs.TabModel, tea.Cmd) { return s, nil }
func (s stubTab) View() string                              { return "stub view " + s.name }
func (s stubTab) KeyHelp() string                           { return "" }

// modalFakeForTest is a minimal Modal stub for the no-op MouseMsg gate.
type modalFakeForTest struct{}

func (modalFakeForTest) Init() tea.Cmd { return nil }
func (modalFakeForTest) Update(_ tea.Msg) (modals.Modal, tea.Cmd) {
	return modalFakeForTest{}, nil
}
func (modalFakeForTest) View(_, _ int) string { return "" }
func (modalFakeForTest) Title() string        { return "fake" }

// recordingTab captures the last message it received via Update so tests
// can assert that root forwarded an event to the active tab.
type recordingTab struct {
	name    string
	lastMsg tea.Msg
}

func (r *recordingTab) Name() string  { return r.name }
func (r *recordingTab) Init() tea.Cmd { return nil }
func (r *recordingTab) Update(msg tea.Msg) (tabs.TabModel, tea.Cmd) {
	r.lastMsg = msg
	return r, nil
}
func (r *recordingTab) View() string    { return "recording " + r.name }
func (r *recordingTab) KeyHelp() string { return "" }

func TestModelMouseWheelForwardedToActiveTab(t *testing.T) {
	// Regression: the outer switch in Update used to lack a MouseMsg
	// case, so wheel events fell through to "return m, nil" and never
	// reached the chat tab — even though ChatTab.Update handled them.
	tab := &recordingTab{name: "Chat"}
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{tab})
	wheel := tea.MouseMsg{Type: tea.MouseWheelUp}
	m.Update(wheel)
	got, ok := tab.lastMsg.(tea.MouseMsg)
	if !ok {
		t.Fatalf("active tab did not receive a MouseMsg; lastMsg=%T", tab.lastMsg)
	}
	if got.Type != tea.MouseWheelUp {
		t.Errorf("forwarded wheel type=%v, want WheelUp", got.Type)
	}
}

func TestModelMouseWheelSwallowedWhenModalOpen(t *testing.T) {
	// When a modal covers the tab, mouse events should NOT scroll the
	// invisible underlying tab.
	tab := &recordingTab{name: "Chat"}
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{tab})
	m.modal = modalFakeForTest{} // any non-nil modal triggers the gate
	m.Update(tea.MouseMsg{Type: tea.MouseWheelUp})
	if tab.lastMsg != nil {
		t.Errorf("tab received MouseMsg while modal open: %T", tab.lastMsg)
	}
}

// recordingModal captures the last message it received so tests can assert
// the root forwarded a modal's own async/animation message to it.
type recordingModal struct{ lastMsg tea.Msg }

func (r *recordingModal) Init() tea.Cmd { return nil }
func (r *recordingModal) Update(msg tea.Msg) (modals.Modal, tea.Cmd) {
	r.lastMsg = msg
	return r, nil
}
func (r *recordingModal) View(_, _ int) string { return "" }
func (r *recordingModal) Title() string        { return "rec" }

// unhandledRootMsg is a message type the root Update has no explicit case for.
type unhandledRootMsg struct{}

func TestModelForwardsUnhandledMsgToOpenModal(t *testing.T) {
	// Regression: the health modal loads via a local Cmd that returns an
	// unexported healthLoadedMsg (and animates via spinner.TickMsg). The root
	// switch had no default, so those messages were dropped and the modal
	// spun forever. The default now forwards otherwise-unhandled messages to
	// the open modal.
	rec := &recordingModal{}
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{&recordingTab{name: "Projects"}})
	m.modal = rec
	m.Update(unhandledRootMsg{})
	if _, ok := rec.lastMsg.(unhandledRootMsg); !ok {
		t.Fatalf("open modal did not receive the unhandled msg; lastMsg=%T", rec.lastMsg)
	}
}

func TestModelDropsUnhandledMsgWhenNoModal(t *testing.T) {
	// With no modal open, an unhandled message must NOT leak to the active
	// tab (tabs receive only their explicit message types).
	tab := &recordingTab{name: "Projects"}
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{tab})
	m.Update(unhandledRootMsg{})
	if tab.lastMsg != nil {
		t.Errorf("tab received an unhandled msg with no modal open: %T", tab.lastMsg)
	}
}

func TestRootHandlesPlannerOpenSwitchesToChat(t *testing.T) {
	// TabPlannerOpenRequest must (1) reset the chat tab and (2) switch
	// m.currentTab to the chat tab's index. The actual Client.StreamPlannerChat
	// fires in a tea.Cmd; the test uses a NewClient with no socket — the
	// goroutine returns immediately because its program is nil (not Bound),
	// so the handler's cmd evaluation is safe.
	chat := tabs.NewChat()
	chat.SetSessionID("session-pre-reset")
	// Stub tab before chat so the chat tab is at index 1 — proves
	// currentTab moves to 1, not "always 0".
	m := NewModel(
		NewClient("/tmp/hive-nosock.sock"),
		NewSnapshot(),
		[]tabs.TabModel{stubTab{name: "Projects"}, chat},
	)
	if m.currentTab != 0 {
		t.Fatalf("precondition: currentTab=%d want 0", m.currentTab)
	}
	if chat.SessionID() != "session-pre-reset" {
		t.Fatalf("precondition: chat session not set; got %q", chat.SessionID())
	}

	// Dispatch the request. The handler returns a cmd that calls
	// StreamPlannerChat — invoke it to exercise the no-op goroutine path
	// (proves the cmd is wired without panicking when Client.program is nil).
	_, cmd := m.Update(tabs.TabPlannerOpenRequest{ProjectSlug: "x"})
	if m.currentTab != 1 {
		t.Errorf("after planner-open currentTab=%d want 1", m.currentTab)
	}
	if chat.SessionID() != "" {
		t.Errorf("chat tab not reset; sessionID=%q want empty", chat.SessionID())
	}
	if cmd == nil {
		t.Fatal("planner-open handler should return a cmd that fires StreamPlannerChat")
	}
	// Evaluate the cmd — should return nil without panicking. (The
	// underlying StreamPlannerChat spawns a goroutine that no-ops on
	// program==nil; the cmd itself returns nil.)
	if got := cmd(); got != nil {
		t.Errorf("planner-open cmd returned %v; want nil", got)
	}
}

func TestRootHandlesPlannerOpenNoopWhenNoChatTab(t *testing.T) {
	// If there's no chat tab in m.tabs, the handler is a no-op. Guards
	// against panics in tests / minimal builds without a chat tab.
	m := NewModel(
		NewClient("/tmp/hive-nosock.sock"),
		NewSnapshot(),
		[]tabs.TabModel{stubTab{name: "Projects"}},
	)
	_, cmd := m.Update(tabs.TabPlannerOpenRequest{ProjectSlug: "x"})
	if m.currentTab != 0 {
		t.Errorf("no-chat-tab path should not move currentTab; got %d", m.currentTab)
	}
	if cmd != nil {
		t.Errorf("no-chat-tab path should not return a cmd; got %T", cmd)
	}
}

func TestModelTabSwitchingViaTab(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}, stubTab{name: "B"}})
	if m.currentTab != 0 {
		t.Fatalf("currentTab=%d want 0", m.currentTab)
	}
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m2.(*Model).currentTab != 1 {
		t.Errorf("after tab currentTab=%d want 1", m2.(*Model).currentTab)
	}
	m3, _ := m2.(*Model).Update(tea.KeyMsg{Type: tea.KeyTab})
	if m3.(*Model).currentTab != 0 {
		t.Errorf("after second tab currentTab=%d want 0 (wrap)", m3.(*Model).currentTab)
	}
}

func TestModelEventAppliesToSnapshot(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	m.Update(eventMsg{Event: rpc.EventMessage{
		Type: rpc.EventRunStarted,
		Data: map[string]any{"run_id": "r1", "task_id": "t1"},
	}})
	if _, ok := m.Snapshot().Runs["r1"]; !ok {
		t.Error("event not applied to snapshot")
	}
}

func TestModelDaemonDownToggle(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	if m.daemonDown {
		t.Fatal("daemonDown should start false")
	}
	m.Update(daemonDownMsg{})
	if !m.daemonDown {
		t.Error("daemonDown not set after daemonDownMsg")
	}
	m.Update(daemonReconnectedMsg{})
	if m.daemonDown {
		t.Error("daemonDown still set after reconnect")
	}
}

// TestGraduateReconnectClearsPendingAndStopsModal verifies a daemon reconnect
// mid-graduation clears pendingGraduate (so it can't leak) and drives the open
// GraduateModal to its terminal result state (the spinner stops, the
// interruption renders) — the daemon dropped the in-memory job, so the done/
// failed event would otherwise never arrive and the spinner would hang forever.
func TestGraduateReconnectClearsPendingAndStopsModal(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	id := "graduate-abc"
	m.pendingGraduate = map[string]string{id: "demo"}
	m.modal = modals.NewGraduateModal("demo", "spec/feat", "main")
	// Drive the modal into its running state (mode confirm → submit).
	gm, _ := m.modal.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.modal = gm

	updated, _ := m.Update(daemonReconnectedMsg{})
	mm := updated.(*Model)
	if len(mm.pendingGraduate) != 0 {
		t.Errorf("pendingGraduate should be cleared on reconnect; got %v", mm.pendingGraduate)
	}
	if mm.modal == nil {
		t.Fatal("modal should still be open showing the interruption")
	}
	if v := mm.modal.View(80, 24); !strings.Contains(v, "interrupted") {
		t.Errorf("modal should render the interruption after reconnect; view:\n%s", v)
	}
}

// TestGraduateGReattachesWhenInFlight verifies that pressing G for a project
// that already has an in-flight graduate (its id→slug is recorded in
// pendingGraduate) re-attaches to it — re-opening the modal directly in its
// running state — instead of starting a fresh confirm.
func TestGraduateGReattachesWhenInFlight(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	m.pendingGraduate = map[string]string{"graduate-1": "demo"}
	updated, _ := m.Update(tabs.TabGraduateRequest{Slug: "demo", Feature: "spec/feat", Target: "main"})
	mm := updated.(*Model)
	gm, ok := mm.modal.(*modals.GraduateModal)
	if !ok {
		t.Fatalf("expected a GraduateModal, got %T", mm.modal)
	}
	// Re-attach opens directly in the running state — NOT the confirm/mode-selector view.
	v := gm.View(80, 24)
	if strings.Contains(v, "Dry-run") {
		t.Errorf("re-attached modal must NOT show the mode selector (should be running):\n%s", v)
	}
	if !strings.Contains(v, "re-attached") {
		t.Errorf("re-attached modal should indicate it is watching an in-flight run:\n%s", v)
	}
}

func TestDecomposeProposedEventBuildsConfirmModal(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	id := "decompose-xyz"
	m.pendingDecompose = map[string]map[string]any{
		id: {"__project_slug": "demo", "__phase_title": "P", "__roadmap_path": "/r.md"},
	}
	ev := rpc.EventMessage{Type: rpc.EventDecomposeProposed, Data: map[string]any{
		"decompose_id": id,
		"result": map[string]any{
			"phase_number": "1", "roadmap_path": "/r.md",
			"subtasks": []any{map[string]any{"title": "t1", "body": "b", "priority": "P3", "pipeline": "build"}},
		},
	}}
	updated, _ := m.Update(eventMsg{Event: ev})
	mm := updated.(*Model)
	if _, still := mm.pendingDecompose[id]; still {
		t.Error("pendingDecompose entry should be cleared after proposed")
	}
	if mm.modal == nil {
		t.Error("expected a confirm modal after decompose.proposed")
	}
}

// TestDecomposeFailedEventSurfacesError verifies a decompose.failed event for a
// pending id clears the pending entry and forwards the error to the open viewer.
func TestDecomposeFailedEventSurfacesError(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	id := "decompose-fail"
	m.pendingDecompose = map[string]map[string]any{id: {"__project_slug": "demo"}}
	m.modal = modals.NewRoadmapViewerModal("demo", t.TempDir()) // load fails (no roadmap) but modal is non-nil
	ev := rpc.EventMessage{Type: rpc.EventDecomposeFailed, Data: map[string]any{
		"decompose_id": id, "error": "model boom",
	}}
	updated, _ := m.Update(eventMsg{Event: ev})
	mm := updated.(*Model)
	if _, still := mm.pendingDecompose[id]; still {
		t.Error("pendingDecompose entry should be cleared after failed")
	}
	if mm.modal == nil {
		t.Error("modal should remain open to surface the error")
	}
}

// TestDecomposeEventIgnoredWhenNotPending verifies a proposed event for an id we
// did NOT start (concurrent CLI/other-TUI decompose) is a no-op — no modal swap.
func TestDecomposeEventIgnoredWhenNotPending(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	ev := rpc.EventMessage{Type: rpc.EventDecomposeProposed, Data: map[string]any{
		"decompose_id": "someone-elses-id",
		"result":       map[string]any{"phase_number": "1", "subtasks": []any{map[string]any{"title": "t"}}},
	}}
	updated, _ := m.Update(eventMsg{Event: ev})
	mm := updated.(*Model)
	if mm.modal != nil {
		t.Error("a proposed event for a non-pending id must not open a modal")
	}
}

func TestModelDrillInRequest(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	m.Update(tabs.DrillInRequest{RunID: "run-42"})
	if m.drillInRunID != "run-42" {
		t.Errorf("drillInRunID=%q want run-42", m.drillInRunID)
	}
	// Esc clears.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.drillInRunID != "" {
		t.Errorf("drillInRunID=%q want empty after esc", m.drillInRunID)
	}
}

func TestModelDrillInXOpensAbandon(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	// x with no drill-in active must NOT open the modal (falls through).
	m.Update(tea.KeyMsg{Runes: []rune{'x'}, Type: tea.KeyRunes})
	if m.modal != nil {
		t.Fatal("x outside drill-in should not open a modal")
	}
	// In drill-in, x opens the confirm-abandon modal for that run.
	m.Update(tabs.DrillInRequest{RunID: "run-42"})
	m.Update(tea.KeyMsg{Runes: []rune{'x'}, Type: tea.KeyRunes})
	if m.modal == nil {
		t.Fatal("x in drill-in should open the confirm-abandon modal")
	}
	if m.modal.Title() != "Abandon run" {
		t.Errorf("modal title=%q want \"Abandon run\"", m.modal.Title())
	}
}

func TestModelAbandonExitsDrillIn(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	m.Update(tabs.DrillInRequest{RunID: "run-42"})
	m.Update(tea.KeyMsg{Runes: []rune{'x'}, Type: tea.KeyRunes}) // open confirm-abandon
	if m.drillInRunID == "" {
		t.Fatal("precondition: should be in drill-in")
	}
	// A successful abandon result should drop the drill-in overlay so we
	// return to the originating tab.
	m.Update(rpcResultMsg{Kind: "abandon_run"})
	if m.drillInRunID != "" {
		t.Errorf("drillInRunID=%q want empty after successful abandon", m.drillInRunID)
	}
}

func TestModelAbandonErrorStaysInDrillIn(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	m.Update(tabs.DrillInRequest{RunID: "run-42"})
	m.Update(tea.KeyMsg{Runes: []rune{'x'}, Type: tea.KeyRunes})
	m.Update(rpcResultMsg{Kind: "abandon_run", Err: errTest})
	if m.drillInRunID != "run-42" {
		t.Errorf("drillInRunID=%q want run-42 (stay on error)", m.drillInRunID)
	}
}

func TestModelDrillInModalRendersOverDrillIn(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(tabs.DrillInRequest{RunID: "run-42"})
	m.Update(tea.KeyMsg{Runes: []rune{'x'}, Type: tea.KeyRunes})
	// With the abandon modal open, View must show the modal prompt, not
	// the drill-in (regression: drill-in returned early before the modal).
	view := m.View()
	if !strings.Contains(view, "y abandon") {
		t.Errorf("View should show the confirm-abandon modal; got:\n%s", view)
	}
}

func TestResolveDrillApproval(t *testing.T) {
	m := NewModel(NewClient("/tmp/hive-none.sock"), NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	m.drillInRunID = "r1"
	// No pending approval for the run → no-op (nil cmd).
	if cmd := m.resolveDrillApproval("a"); cmd != nil {
		t.Error("no pending approval should yield nil cmd")
	}
	// With a pending approval, a/A/d/D yield a resolve cmd.
	m.snapshot.ApplyPendingApproval(map[string]any{
		"approval_id": "ap-1", "run_id": "r1", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "make all"},
	})
	if cmd := m.resolveDrillApproval("a"); cmd == nil {
		t.Error("pending approval should yield a resolve cmd")
	}
}

// TestAltDigitJumpsToTab verifies that Alt+1..6 jump to the corresponding
// tab (Alt is used because terminals reliably distinguish Alt+digit from
// bare digit; Ctrl+digit is also handled but many emulators don't transmit
// it distinctly from the bare digit).
func TestAltDigitJumpsToTab(t *testing.T) {
	tabs6 := []tabs.TabModel{
		stubTab{name: "t1"}, stubTab{name: "t2"}, stubTab{name: "t3"},
		stubTab{name: "t4"}, stubTab{name: "t5"}, stubTab{name: "t6"},
	}
	m := NewModel(nil, NewSnapshot(), tabs6)
	for i, digit := range []rune{'1', '2', '3', '4', '5', '6'} {
		// Alt: true + KeyRunes → msg.String() == "alt+<digit>"
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{digit}, Alt: true})
		m = updated.(*Model)
		if m.currentTab != i {
			t.Errorf("after alt+%c, currentTab=%d, want %d", digit, m.currentTab, i)
		}
	}
}

// TestAltDigitBeyondTabCountIsNoop verifies that an out-of-range Alt+digit
// does not change the current tab.
func TestAltDigitBeyondTabCountIsNoop(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "only"}})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}, Alt: true})
	m = updated.(*Model)
	if m.currentTab != 0 {
		t.Errorf("currentTab=%d after alt+5 with only 1 tab; want 0 (no-op)", m.currentTab)
	}
}

// TestBareDigitDoesNotJumpTabs verifies that pressing a bare digit (without
// modifier) no longer switches tabs — naked digits now flow to the active
// tab's textinput so users can type numbers in chat without switching tabs.
func TestBareDigitDoesNotJumpTabs(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{
		stubTab{name: "t1"}, stubTab{name: "t2"},
	})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = updated.(*Model)
	if m.currentTab != 0 {
		t.Errorf("currentTab=%d after bare '2'; want 0 (bare digits must not jump tabs)", m.currentTab)
	}
}

// TestModelChatEditArgsSubmitMarksFrameApproved is a regression test for the
// interface assertion bug: when ChatEditArgsSubmitMsg arrives at root.Update,
// it must call ResolveByEdit on the chat tab so the frame transitions from
// pending to approved. A type-signature mismatch on the interface assertion
// ([]byte vs json.RawMessage) would silently skip the call, leaving the
// tool_proposed frame stuck in pendingConfirms.
func TestModelChatEditArgsSubmitMarksFrameApproved(t *testing.T) {
	chatTab := tabs.NewChat()
	chatTab.SetSessionID("s1")
	// Seed a pending tool_proposed so there is something to resolve.
	chatTab.SeedPendingConfirmForTest("tc1")

	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{chatTab})

	// Drive the submit message through root.Update — this exercises the full
	// interface assertion path in the ChatEditArgsSubmitMsg case.
	m.Update(modals.ChatEditArgsSubmitMsg{
		ToolCallID: "tc1",
		EditedArgs: json.RawMessage(`{"k":"v"}`),
	})

	if !chatTab.IsFrameResolvedForTest("tc1") {
		t.Error("ResolveByEdit was not called via root.Update interface assertion — frame not marked resolved")
	}
}

// TestRootOpenChatToolResultPickerMsgConstructsModal verifies that the root's
// Update method handles tabs.OpenChatToolResultPickerMsg by converting the
// rows to modals.ChatToolResultRow and constructing a ChatToolResultPicker
// modal. The two ChatToolResultRow types are intentionally separate to
// preserve the "tabs doesn't import modals" architectural boundary — the
// root is the seam that converts.
func TestRootOpenChatToolResultPickerMsgConstructsModal(t *testing.T) {
	chatTab := tabs.NewChat()
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{chatTab})
	rows := []tabs.ChatToolResultRow{
		{Tool: "hive_status", Result: `{"ok":true}`, IsError: false},
	}
	updated, _ := m.Update(tabs.OpenChatToolResultPickerMsg{Rows: rows})
	mm := updated.(*Model)
	if mm.modal == nil {
		t.Fatal("modal not set on root after OpenChatToolResultPickerMsg")
	}
	if _, ok := mm.modal.(*modals.ChatToolResultPicker); !ok {
		t.Errorf("modal type %T, want *modals.ChatToolResultPicker", mm.modal)
	}
	// Verify rows actually made it into the modal (catches a bug where
	// the conversion loop is dropped but the modal is still constructed
	// with an empty slice).
	if v := mm.modal.View(80, 24); !strings.Contains(v, "hive_status") {
		t.Errorf("modal View missing 'hive_status' — rows not propagated: %q", v)
	}
}

// TestRootForwardsWindowSizeMsgToActiveModal verifies that when a modal is
// open and the terminal resizes, the WindowSizeMsg reaches the modal's
// Update so its viewport can refresh. Without this forward, every modal's
// resize handler is dead code: modals snapshot dims at construct time and
// stay stuck at stale sizes forever. Uses ChatToolResultPicker because it
// stores width/height as observable state.
func TestRootForwardsWindowSizeMsgToActiveModal(t *testing.T) {
	chatTab := tabs.NewChat()
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{chatTab})
	rows := []tabs.ChatToolResultRow{
		{Tool: "hive_status", Result: `{"ok":true}`, IsError: false},
	}
	// Open the picker via the normal route.
	updated, _ := m.Update(tabs.OpenChatToolResultPickerMsg{Rows: rows})
	mm := updated.(*Model)
	picker, ok := mm.modal.(*modals.ChatToolResultPicker)
	if !ok {
		t.Fatalf("setup: modal not ChatToolResultPicker; got %T", mm.modal)
	}
	initialW, initialH := picker.Width(), picker.Height()

	// Resize the terminal.
	updated, _ = mm.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	mm = updated.(*Model)
	picker = mm.modal.(*modals.ChatToolResultPicker)

	// Modal should reflect the new dims (adjusted for root chrome).
	// rootChromeRowsFor(60) = 6 (legend renders at h >= 24), so expected
	// height = 60 - 6 = 54.
	if picker.Width() != 200 {
		t.Errorf("modal Width=%d, want 200 (was %d before resize)", picker.Width(), initialW)
	}
	if picker.Height() != 54 {
		t.Errorf("modal Height=%d, want 54 (60 - rootChromeRowsFor(60)=6); was %d before resize", picker.Height(), initialH)
	}
}

func TestFirstBashGlob(t *testing.T) {
	if g := firstBashGlob("make all"); g != "make *" {
		t.Errorf("firstBashGlob(make all)=%q want 'make *'", g)
	}
	if g := firstBashGlob("ls"); g != "ls *" {
		t.Errorf("firstBashGlob(ls)=%q want 'ls *'", g)
	}
}

// isQuit executes a cmd and reports whether it is tea.Quit (returns tea.QuitMsg).
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// TestModelQTypesInChatTabNotQuit: on the chat tab, bare `q` must type into the
// input, not quit the app (text inputs need printable keys). Dogfood fix.
func TestModelQTypesInChatTabNotQuit(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{tabs.NewChat()}) // currentTab=0=Chat
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if isQuit(cmd) {
		t.Error("q on the chat tab should type into the input, not quit")
	}
}

// TestModelQQuitsOnNonChatTab: away from the chat tab, bare `q` still quits.
func TestModelQQuitsOnNonChatTab(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "Projects"}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if !isQuit(cmd) {
		t.Error("q on a non-chat tab should quit")
	}
}

// TestModelCtrlCAlwaysQuits: ctrl+c is the universal quit, even on the chat tab.
func TestModelCtrlCAlwaysQuits(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{tabs.NewChat()})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !isQuit(cmd) {
		t.Error("ctrl+c should always quit, even on the chat tab")
	}
}

func TestParseProposedSubtasksDecodesDepsAndFiles(t *testing.T) {
	raw := []any{
		map[string]any{
			"title": "t0", "body": "b", "priority": "P1",
			"relevant_files": []any{"a.go", "b.ts"},
		},
		map[string]any{
			"title": "t1", "body": "b", "priority": "P1",
			"depends_on": []any{float64(0)}, "relevant_files": []any{"c.go"},
		},
	}
	out := parseProposedSubtasks(raw)
	if len(out) != 2 {
		t.Fatalf("got %d subtasks", len(out))
	}
	if len(out[0].RelevantFiles) != 2 || out[0].RelevantFiles[0] != "a.go" {
		t.Errorf("t0 relevant_files = %v", out[0].RelevantFiles)
	}
	if len(out[1].DependsOn) != 1 || out[1].DependsOn[0] != 0 {
		t.Errorf("t1 depends_on = %v, want [0]", out[1].DependsOn)
	}
	if len(out[1].RelevantFiles) != 1 || out[1].RelevantFiles[0] != "c.go" {
		t.Errorf("t1 relevant_files = %v", out[1].RelevantFiles)
	}
}
