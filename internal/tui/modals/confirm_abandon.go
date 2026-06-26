package modals

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// ConfirmAbandon is a simple y/N modal that abandons a run on y.
type ConfirmAbandon struct {
	runID  string
	errMsg string
}

// NewConfirmAbandon constructs the confirm-abandon modal.
func NewConfirmAbandon(runID string) Modal {
	return &ConfirmAbandon{runID: runID}
}

func (m *ConfirmAbandon) Title() string { return "Abandon run" }
func (m *ConfirmAbandon) Init() tea.Cmd { return nil }

func (m *ConfirmAbandon) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch strings.ToLower(msg.String()) {
		case "y":
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind:   "abandon_run",
					Params: map[string]any{"run_id": m.runID},
				}
			}
		case "n", "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		}
	case RPCResultMsg:
		if msg.Kind == "abandon_run" {
			if msg.Err != nil {
				m.errMsg = msg.Err.Error()
				return m, nil
			}
			return m, func() tea.Msg { return CloseMsg{} }
		}
	}
	return m, nil
}

func (m *ConfirmAbandon) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(style.Danger.Render("Abandon")+" run") + "\n\n")
	b.WriteString("Run ID: " + m.runID + "\n\n")
	b.WriteString("This will cancel the in-flight pipeline and mark the run as abandoned.\n")
	b.WriteString("The worktree + runtime dir are preserved for inspection.\n\n")
	if m.errMsg != "" {
		b.WriteString(style.InlineError.Render(m.errMsg) + "\n\n")
	}
	b.WriteString(style.Hint.Render(style.Key.Render("y") + " abandon · " + style.Key.Render("n") + " / " + style.Key.Render("esc") + " cancel"))
	return style.Modal.Width(60).Render(b.String())
}
