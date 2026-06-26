package modals

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// NewProject is the modal for adding a new project. It is a thin wrapper over
// the shared projectForm (slugEditable:true, canSequence:false): the form owns
// all field state, focus navigation, rendering, and param serialization. The
// modal only owns the title, the "new_project" Submit/RPCResult Kind, and the
// slug+name required validation.
//
// canSequence is false because a brand-new project has no roadmap yet, so the
// dispatch-mode "sequenced" option is disabled (the form greys + skips it on
// cycle). Sequenced is enabled later via Edit once a roadmap exists.
type NewProject struct {
	form *projectForm
}

// NewNewProject constructs the new-project modal.
func NewNewProject() *NewProject {
	return &NewProject{
		form: newProjectForm(projectFormOpts{slugEditable: true, canSequence: false}),
	}
}

func (m *NewProject) Title() string { return "New project" }

func (m *NewProject) Init() tea.Cmd { return textinput.Blink }

func (m *NewProject) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		case "tab", "down":
			return m, m.form.focusNext()
		case "shift+tab", "up":
			return m, m.form.focusPrev()
		case "left":
			if m.form.focusedIsSelector() {
				m.form.cycleSelector(-1)
				return m, nil
			}
		case "right", " ":
			if m.form.focusedIsSelector() {
				m.form.cycleSelector(1)
				return m, nil
			}
		case "ctrl+s":
			return m.submit()
		case "enter":
			if m.form.focusedIsSelector() {
				m.form.cycleSelector(1)
				return m, nil
			}
			return m.submit()
		}
	case RPCResultMsg:
		if msg.Kind == "new_project" {
			m.form.submitting = false
			if msg.Err != nil {
				m.form.errMsg = msg.Err.Error()
				return m, nil
			}
			return m, func() tea.Msg { return CloseMsg{} }
		}
	}
	return m, m.form.updateInput(msg)
}

func (m *NewProject) submit() (Modal, tea.Cmd) {
	if m.form.SlugValue() == "" || m.form.NameValue() == "" {
		m.form.errMsg = "slug and name are required"
		return m, nil
	}
	m.form.submitting = true
	params := m.form.serialize()
	return m, func() tea.Msg { return SubmitRequest{Kind: "new_project", Params: params} }
}

func (m *NewProject) View(width, height int) string {
	return m.form.View("New project", m.form.footer(), 60, height)
}
