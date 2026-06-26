package modals

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// DeleteProjectConfirmModal is a typed-confirm modal for the destructive
// project.delete RPC. The operator must type the project's slug exactly
// before the `y` key is enabled — this prevents one-key mistake-deletes
// of a project (which cascades to all of its tasks + runs). The cascading
// counts are shown so the destructive scope is visible at the modal.
//
// Slug is the immutable identifier (unchanged from the row). TaskCount +
// RunCount are computed by the Projects tab from the snapshot before the
// modal opens so we don't have to re-query the daemon.
type DeleteProjectConfirmModal struct {
	Slug       string
	TaskCount  int
	RunCount   int
	typedSlug  textinput.Model
	submitting bool
	errMsg     string
}

// NewDeleteProjectConfirmModal builds the modal pre-focused on the slug
// confirmation input. The input starts empty; the operator must type the
// slug character-for-character to enable submission.
func NewDeleteProjectConfirmModal(slug string, taskCount, runCount int) *DeleteProjectConfirmModal {
	ti := textinput.New()
	ti.Placeholder = slug
	ti.Width = 40
	ti.Focus()
	return &DeleteProjectConfirmModal{
		Slug:      slug,
		TaskCount: taskCount,
		RunCount:  runCount,
		typedSlug: ti,
	}
}

func (m *DeleteProjectConfirmModal) Title() string { return "Delete project " + m.Slug }

func (m *DeleteProjectConfirmModal) Init() tea.Cmd { return textinput.Blink }

func (m *DeleteProjectConfirmModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// esc always cancels. enter/ctrl+s submits IF the typed slug
		// matches — using enter/ctrl+s (not 'y') lets the operator type
		// slugs that contain any letter, including 'y', without the
		// confirm key colliding with a slug character.
		switch msg.String() {
		case "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		case "enter", "ctrl+s":
			if m.submitting {
				return m, nil
			}
			if m.typedSlug.Value() != m.Slug {
				m.errMsg = "typed slug does not match — type " + m.Slug + " to confirm"
				return m, nil
			}
			m.submitting = true
			slug := m.Slug
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind:   "delete_project",
					Params: map[string]any{"slug": slug},
				}
			}
		}
	case RPCResultMsg:
		if msg.Kind == "delete_project" {
			if msg.Err != nil {
				m.submitting = false
				m.errMsg = msg.Err.Error()
				return m, nil
			}
			return m, func() tea.Msg { return CloseMsg{} }
		}
	}
	upd, cmd := m.typedSlug.Update(msg)
	m.typedSlug = upd
	// Typing toward the slug clears any stale mismatch hint so the
	// modal doesn't lie once the operator is on the right track.
	if strings.HasPrefix(m.errMsg, "typed slug does not match") {
		m.errMsg = ""
	}
	return m, cmd
}

func (m *DeleteProjectConfirmModal) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(style.Danger.Render("Delete")+" project "+m.Slug) + "\n\n")
	b.WriteString("This will permanently delete the project and " + style.NeedsAttention.Render("CASCADE") + ":\n")
	b.WriteString("  " + style.NeedsAttention.Render(strconv.Itoa(m.TaskCount)) + " task(s)\n")
	b.WriteString("  " + style.NeedsAttention.Render(strconv.Itoa(m.RunCount)) + " run(s)\n\n")
	b.WriteString("Type the slug to confirm:\n")
	b.WriteString("  " + m.typedSlug.View() + "\n\n")
	if m.errMsg != "" {
		b.WriteString(style.InlineError.Render(m.errMsg) + "\n\n")
	}
	if m.submitting {
		b.WriteString(style.Hint.Render("submitting…"))
	} else {
		b.WriteString(style.Hint.Render(
			style.Key.Render("enter/ctrl+s") + " delete (slug must match) · " +
				style.Key.Render("esc") + " cancel"))
	}
	return style.Modal.Width(64).Render(b.String())
}
