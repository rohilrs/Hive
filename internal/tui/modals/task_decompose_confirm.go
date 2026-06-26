package modals

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/tui/style"
)

// TaskDecomposeConfirmModal renders the proposed sub-tasks returned by
// task.decompose for a single task, and gates insertion behind a y/N
// confirm. On approval it emits a SubmitRequest the root translates into
// task.decompose_apply, which inserts the children of taskID.
//
// It is a deliberate sibling of DecomposeConfirmModal (the roadmap-phase
// variant) rather than a generalization of it: the roadmap modal carries
// phase/roadmap-path/spec-path metadata and emits "roadmap_insert_tasks",
// which has no analogue here. Both reuse the same package-level rendering
// helpers (leftRow / proposalDetailLines / windowLines / …), so the only
// duplication is the small two-pane Update/View scaffolding — keeping the
// proven roadmap modal untouched is the lower-risk path.
//
// Workflow:
//
//	NewTaskDecomposeConfirmModal(taskID, subtasks) ──► [list]
//	  list:
//	    y         → emit SubmitRequest{Kind:"task_decompose_apply",
//	                Params:{task_id, subtasks}}. submitting=true; modal stays
//	                open while the root runs task.decompose_apply.
//	    n / esc   → CloseMsg (no insert).
//	  RPCResultMsg{Kind:"task_decompose_apply"}:
//	    Err == nil → CloseMsg (done).
//	    Err != nil → submitting=false; render errMsg inline; operator can
//	                 retry (y) or cancel (esc).
type TaskDecomposeConfirmModal struct {
	taskID string

	// subtasks is the LLM-proposed list, in the daemon's response order
	// (typically dependency-ordered).
	subtasks []decompose.ProposedSubtask

	// Two-pane state mirrors DecomposeConfirmModal: cursor selects the
	// proposal (left pane), focusRight chooses which pane ↑/↓ act on,
	// detailScroll is the right-pane offset (reset on selection change).
	cursor       int
	focusRight   bool
	detailScroll int

	submitting bool
	errMsg     string
}

// NewTaskDecomposeConfirmModal builds the modal from a task.decompose
// response. Fields are copied so the modal's lifetime is independent of
// the caller's slice.
func NewTaskDecomposeConfirmModal(taskID string, subtasks []decompose.ProposedSubtask) *TaskDecomposeConfirmModal {
	return &TaskDecomposeConfirmModal{
		taskID:   taskID,
		subtasks: append([]decompose.ProposedSubtask(nil), subtasks...),
	}
}

func (m *TaskDecomposeConfirmModal) Title() string {
	return "Decompose " + m.taskID
}

func (m *TaskDecomposeConfirmModal) Init() tea.Cmd { return nil }

func (m *TaskDecomposeConfirmModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case RPCResultMsg:
		if msg.Kind != "task_decompose_apply" {
			return m, nil
		}
		if msg.Err != nil {
			m.submitting = false
			m.errMsg = msg.Err.Error()
			return m, nil
		}
		// Success — children inserted. Close.
		return m, func() tea.Msg { return CloseMsg{} }

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "n":
			return m, func() tea.Msg { return CloseMsg{} }
		case "tab":
			m.focusRight = !m.focusRight
			return m, nil
		case "up", "k":
			if m.focusRight {
				if m.detailScroll > 0 {
					m.detailScroll--
				}
			} else if m.cursor > 0 {
				m.cursor--
				m.detailScroll = 0
			}
			return m, nil
		case "down", "j":
			if m.focusRight {
				m.detailScroll++ // View clamps to the detail window
			} else if m.cursor < len(m.subtasks)-1 {
				m.cursor++
				m.detailScroll = 0
			}
			return m, nil
		case "y":
			if m.submitting {
				// In-flight — protect against double-y rapid-fire.
				return m, nil
			}
			if len(m.subtasks) == 0 {
				m.errMsg = "no subtasks to insert"
				return m, nil
			}
			m.submitting = true
			// Snapshot the fields the closure needs so a later modal
			// state-change can't race the cmd.
			taskID := m.taskID
			subtasks := m.subtasks
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind: "task_decompose_apply",
					Params: map[string]any{
						"task_id":  taskID,
						"subtasks": subtasks,
					},
				}
			}
		}
	}
	return m, nil
}

func (m *TaskDecomposeConfirmModal) View(width, height int) string {
	w := frameWidth(width)
	budget := w - 8 // two panel chromes (border 2 + padding 2 each)
	if budget < 24 {
		budget = 24
	}

	// Header: task heading + proposal count.
	header := []string{
		style.ModalTitle.Render(truncateRunes(m.Title(), budget)),
		style.DimText.Render(truncateRunes(formatProposalCount(len(m.subtasks)), budget)),
	}
	// Footer: error inline + key hint.
	var footer []string
	if m.errMsg != "" {
		footer = append(footer, style.InlineError.Render(truncateRunes(m.errMsg, budget)))
	}
	if m.submitting {
		footer = append(footer, style.Hint.Render("inserting…"))
	} else {
		nav := "select"
		if m.focusRight {
			nav = "scroll detail"
		}
		footer = append(footer, style.Hint.Render(
			style.Key.Render("y")+" insert all · "+
				style.Key.Render("tab")+" switch · "+
				style.Key.Render("↑↓")+" "+nav+" · "+
				style.Key.Render("n")+"/"+style.Key.Render("esc")+" cancel"))
	}

	// Pane vertical budget: total minus header, footer, the two blank
	// separators (2), the modal chrome (4), and the panel borders (2).
	bodyH := height - len(header) - len(footer) - 8
	if bodyH < 3 {
		bodyH = 3
	}
	leftContentW := budget * 2 / 5
	if leftContentW < 12 {
		leftContentW = 12
	}
	rightContentW := budget - leftContentW
	if rightContentW < 12 {
		rightContentW = 12
	}

	// Left pane: one row per proposal, cursor-marked, auto-scrolled.
	rows := make([]string, len(m.subtasks))
	for i, st := range m.subtasks {
		// -4: panel padding (2) + cursor marker (2).
		row := leftRow(i+1, st, leftContentW-4)
		if i == m.cursor {
			rows[i] = style.CursorMarker(true) + style.ModalTitle.Render(row)
		} else {
			rows[i] = style.CursorMarker(false) + row
		}
	}
	leftWin := windowLines(rows, bodyH, leftScrollFirst(m.cursor, len(rows), bodyH))

	// Right pane: the selected proposal's full detail, wrapped + scrolled.
	var rightWin []string
	if m.cursor >= 0 && m.cursor < len(m.subtasks) {
		rightWin = windowLines(proposalDetailLines(m.subtasks[m.cursor], rightContentW-2), bodyH, m.detailScroll)
	}

	leftStyle, rightStyle := style.PanelFocus, style.Panel
	if m.focusRight {
		leftStyle, rightStyle = style.Panel, style.PanelFocus
	}
	leftBox := leftStyle.Width(leftContentW).Height(bodyH).Render(strings.Join(leftWin, "\n"))
	rightBox := rightStyle.Width(rightContentW).Height(bodyH).Render(strings.Join(rightWin, "\n"))
	panes := lipgloss.JoinHorizontal(lipgloss.Top, leftBox, rightBox)

	return style.Modal.Width(w).Render(strings.Join(
		[]string{strings.Join(header, "\n"), panes, strings.Join(footer, "\n")}, "\n\n"))
}
