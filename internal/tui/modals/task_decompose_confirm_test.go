package modals

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/decompose"
)

// taskSampleSubtasks builds a 3-proposal fixture matching the wire shape
// task.decompose returns.
func taskSampleSubtasks() []decompose.ProposedSubtask {
	return []decompose.ProposedSubtask{
		{Title: "alpha task", Body: "do alpha", Priority: "P1", Pipeline: "build"},
		{Title: "beta task", Body: "do beta", Priority: "P2", Pipeline: "debug"},
		{Title: "gamma task", Body: "do gamma"}, // priority/pipeline default
	}
}

// TestTaskDecomposeConfirmRendersProposals: the fed proposals + the task id
// heading + the footer keybinds all render.
func TestTaskDecomposeConfirmRendersProposals(t *testing.T) {
	m := NewTaskDecomposeConfirmModal("t-42", taskSampleSubtasks())
	view := m.View(120, 40)
	for _, title := range []string{"alpha task", "beta task", "gamma task"} {
		if !strings.Contains(view, title) {
			t.Errorf("view missing proposal title %q; got:\n%s", title, view)
		}
	}
	if !strings.Contains(view, "t-42") {
		t.Errorf("view missing task id heading; got:\n%s", view)
	}
	if !strings.Contains(view, "insert all") || !strings.Contains(view, "cancel") {
		t.Errorf("view missing footer hint; got:\n%s", view)
	}
}

// TestTaskDecomposeConfirmYEmitsApply: y emits the task_decompose_apply
// SubmitRequest carrying the task_id + the typed subtasks slice, and flips
// the modal into the submitting state.
func TestTaskDecomposeConfirmYEmitsApply(t *testing.T) {
	m := NewTaskDecomposeConfirmModal("t-42", taskSampleSubtasks())
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("y should emit a cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg %T want SubmitRequest", cmd())
	}
	if req.Kind != "task_decompose_apply" {
		t.Errorf("Kind=%q want task_decompose_apply", req.Kind)
	}
	if req.Params["task_id"] != "t-42" {
		t.Errorf("task_id=%v want t-42", req.Params["task_id"])
	}
	subs, ok := req.Params["subtasks"].([]decompose.ProposedSubtask)
	if !ok {
		t.Fatalf("subtasks param type=%T want []decompose.ProposedSubtask", req.Params["subtasks"])
	}
	if len(subs) != 3 {
		t.Errorf("subtasks len=%d want 3", len(subs))
	}
	if !m.submitting {
		t.Errorf("submitting should flip to true on y")
	}
}

// TestTaskDecomposeConfirmEscCancels: esc emits CloseMsg without applying.
func TestTaskDecomposeConfirmEscCancels(t *testing.T) {
	m := NewTaskDecomposeConfirmModal("t-1", taskSampleSubtasks())
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc emitted %T want CloseMsg", cmd())
	}
}

// TestTaskDecomposeConfirmNCancels: n is the "no" half of [y/N].
func TestTaskDecomposeConfirmNCancels(t *testing.T) {
	m := NewTaskDecomposeConfirmModal("t-1", taskSampleSubtasks())
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd == nil {
		t.Fatal("n should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("n emitted %T want CloseMsg", cmd())
	}
}

// TestTaskDecomposeConfirmApplySuccessCloses: a successful apply result
// closes the modal.
func TestTaskDecomposeConfirmApplySuccessCloses(t *testing.T) {
	m := NewTaskDecomposeConfirmModal("t-1", taskSampleSubtasks())
	m.submitting = true
	_, cmd := m.Update(RPCResultMsg{
		Kind: "task_decompose_apply",
		Data: map[string]any{"inserted_task_ids": []any{"c1", "c2", "c3"}},
	})
	if cmd == nil {
		t.Fatal("success result should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("success emitted %T want CloseMsg", cmd())
	}
}

// TestTaskDecomposeConfirmApplyErrorStaysOpen: an apply error drops the
// submitting flag, renders inline, and keeps the modal open (no CloseMsg)
// so the operator can retry or cancel.
func TestTaskDecomposeConfirmApplyErrorStaysOpen(t *testing.T) {
	m := NewTaskDecomposeConfirmModal("t-1", taskSampleSubtasks())
	m.submitting = true
	_, cmd := m.Update(RPCResultMsg{
		Kind: "task_decompose_apply",
		Err:  errors.New("boom"),
	})
	if cmd != nil {
		if _, ok := cmd().(CloseMsg); ok {
			t.Fatal("error result should NOT close the modal")
		}
	}
	if m.submitting {
		t.Error("submitting should reset to false on error")
	}
	if !strings.Contains(m.View(120, 40), "boom") {
		t.Errorf("error should render inline; got:\n%s", m.View(120, 40))
	}
}

// TestTaskDecomposeConfirmNeverExceedsHeight: the list scrolls within a
// height-bounded modal — a long proposal list can't overflow + clip it.
func TestTaskDecomposeConfirmNeverExceedsHeight(t *testing.T) {
	m := NewTaskDecomposeConfirmModal("t-1", manySubtasks(25))
	for _, wd := range []int{80, 120} {
		for _, h := range []int{16, 24, 40} {
			for _, cur := range []int{0, 12, 24} {
				m.cursor = cur
				for _, fr := range []bool{false, true} {
					m.focusRight = fr
					for _, sc := range []int{0, 5, 100} {
						m.detailScroll = sc
						rows := strings.Count(m.View(wd, h), "\n") + 1
						if rows > h {
							t.Errorf("w=%d h=%d cur=%d focusRight=%v scroll=%d: View %d rows > height", wd, h, cur, fr, sc, rows)
						}
					}
				}
			}
		}
	}
}
