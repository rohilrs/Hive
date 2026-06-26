package modals

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// NewTask is the modal for adding a task to a project. The body is a
// multi-line textarea (Enter inserts newlines), so submit is ctrl+s.
type NewTask struct {
	slug     textinput.Model
	title    textinput.Model
	pipeline textinput.Model
	body     textarea.Model
	focusIdx int // 0=slug 1=title 2=body 3=pipeline
	errMsg   string
}

const ntFieldCount = 4

// NewNewTask constructs the new-task modal. If prefilledProjectSlug is
// non-empty, the slug field is pre-populated and focus starts on title.
func NewNewTask(prefilledProjectSlug string) Modal {
	m := &NewTask{}
	m.slug = textinput.New()
	m.slug.Placeholder = "project slug"
	m.slug.Width = 60
	m.title = textinput.New()
	m.title.Placeholder = "task title (required)"
	m.title.Width = 60
	m.pipeline = textinput.New()
	m.pipeline.Placeholder = "pipeline (build)"
	m.pipeline.Width = 60
	m.pipeline.SetValue("build")
	m.body = textarea.New()
	m.body.Placeholder = "task body (optional) — Enter for newlines"
	m.body.SetWidth(60)
	m.body.SetHeight(6)

	if prefilledProjectSlug != "" {
		m.slug.SetValue(prefilledProjectSlug)
		m.focusIdx = 1
		m.title.Focus()
	} else {
		m.focusIdx = 0
		m.slug.Focus()
	}
	return m
}

func (m *NewTask) Title() string { return "New task" }
func (m *NewTask) Init() tea.Cmd { return textinput.Blink }

// focusField blurs all fields and focuses index i (wrapping).
func (m *NewTask) focusField(i int) tea.Cmd {
	m.slug.Blur()
	m.title.Blur()
	m.pipeline.Blur()
	m.body.Blur()
	m.focusIdx = (i + ntFieldCount) % ntFieldCount
	switch m.focusIdx {
	case 0:
		m.slug.Focus()
	case 1:
		m.title.Focus()
	case 2:
		return m.body.Focus()
	case 3:
		m.pipeline.Focus()
	}
	return textinput.Blink
}

func (m *NewTask) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		case "tab":
			return m, m.focusField(m.focusIdx + 1)
		case "shift+tab":
			return m, m.focusField(m.focusIdx - 1)
		case "ctrl+s":
			return m.submit()
		}
	case RPCResultMsg:
		if msg.Kind == "new_task" {
			if msg.Err != nil {
				m.errMsg = msg.Err.Error()
				return m, nil
			}
			return m, func() tea.Msg { return CloseMsg{} }
		}
	}
	// Forward to the focused widget (body textarea handles Enter→newline).
	var cmd tea.Cmd
	switch m.focusIdx {
	case 0:
		m.slug, cmd = m.slug.Update(msg)
	case 1:
		m.title, cmd = m.title.Update(msg)
	case 2:
		m.body, cmd = m.body.Update(msg)
	case 3:
		m.pipeline, cmd = m.pipeline.Update(msg)
	}
	return m, cmd
}

func (m *NewTask) submit() (Modal, tea.Cmd) {
	slug := strings.TrimSpace(m.slug.Value())
	title := strings.TrimSpace(m.title.Value())
	body := strings.TrimSpace(m.body.Value())
	pipeline := strings.TrimSpace(m.pipeline.Value())
	if pipeline == "" {
		pipeline = "build"
	}
	if slug == "" || title == "" {
		m.errMsg = "project slug and title are required"
		return m, nil
	}
	return m, func() tea.Msg {
		return SubmitRequest{
			Kind:   "new_task",
			Params: map[string]any{"project_slug": slug, "title": title, "body": body, "pipeline": pipeline},
		}
	}
}

func (m *NewTask) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render("New task") + "\n\n")
	b.WriteString("Project slug:\n  " + m.slug.View() + "\n\n")
	b.WriteString("Title:\n  " + m.title.View() + "\n\n")
	b.WriteString("Body:\n" + m.body.View() + "\n\n")
	b.WriteString("Pipeline:\n  " + m.pipeline.View() + "\n\n")
	if m.errMsg != "" {
		b.WriteString(style.InlineError.Render(m.errMsg) + "\n\n")
	}
	b.WriteString(style.Hint.Render(style.Key.Render("ctrl+s") + " submit · " + style.Key.Render("tab") + " next field · " + style.Key.Render("esc") + " cancel"))
	return style.Modal.Width(70).Render(b.String())
}
