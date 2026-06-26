package modals

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// SequenceModal is the control surface for a sequenced project (opened with `q`
// on the Projects tab). It renders the live phase panel — status · policy ·
// target, the active phase, and its tasks with gate glyphs + a cursor — and
// drives pause/resume/advance/skip. State refreshes live: the root folds each
// sequence.* event's refetched status in via SetView, so an open modal tracks
// the dispatcher without manual reload.
//
// Actions go through the standard SubmitRequest → root → Client.Sequence*
// round-trip; the modal stays open on success (the subsequent event refreshes
// it) and shows errors inline.
type SequenceModal struct {
	slug      string
	projectID string
	view      *rpc.SeqStatusView
	cursor    int // index into the active phase's tasks (for skip)
	errMsg    string
}

func NewSequenceModal(slug, projectID string, view *rpc.SeqStatusView) *SequenceModal {
	return &SequenceModal{slug: slug, projectID: projectID, view: view}
}

func (m *SequenceModal) Title() string { return "Sequence — " + m.slug }
func (m *SequenceModal) Init() tea.Cmd { return nil }

// ProjectID lets the root match a refreshed status to this open modal.
func (m *SequenceModal) ProjectID() string { return m.projectID }

// SetView swaps in a freshly-fetched status (called by the root on a
// sequence.* event) and clamps the cursor.
func (m *SequenceModal) SetView(v *rpc.SeqStatusView) {
	m.view = v
	if n := len(m.activeTasks()); m.cursor >= n {
		m.cursor = 0
	}
}

// activeTasks returns the tasks in the active phase (cursor + skip target).
func (m *SequenceModal) activeTasks() []rpc.SeqTaskView {
	if m.view == nil {
		return nil
	}
	for _, ph := range m.view.Phases {
		if ph.Number == m.view.ActivePhase {
			return ph.Tasks
		}
	}
	return nil
}

func (m *SequenceModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			return m, func() tea.Msg { return CloseMsg{} }
		case "j", "up": // j=up (repo convention)
			if m.cursor > 0 {
				m.cursor--
			}
		case "k", "down":
			if m.cursor < len(m.activeTasks())-1 {
				m.cursor++
			}
		case "p":
			return m, m.action("sequence_pause", map[string]any{"project_slug": m.slug})
		case "r":
			return m, m.action("sequence_resume", map[string]any{"project_slug": m.slug})
		case "a":
			return m, m.action("sequence_advance", map[string]any{"project_slug": m.slug})
		case "s":
			tasks := m.activeTasks()
			if m.cursor < len(tasks) {
				return m, m.action("sequence_skip", map[string]any{"task_id": tasks[m.cursor].ID})
			}
		case "c":
			// Mark the active phase operator-complete so the dispatcher advances.
			// The daemon rejects it unless every task in the phase is satisfied/
			// skipped; that error surfaces inline (modal stays open).
			if m.view != nil && m.view.ActivePhase != "" && !m.view.Complete {
				return m, m.action("sequence_complete", map[string]any{
					"project_slug": m.slug, "phase": m.view.ActivePhase,
				})
			}
		}
	case RPCResultMsg:
		switch msg.Kind {
		case "sequence_pause", "sequence_resume", "sequence_advance", "sequence_skip", "sequence_complete":
			// Stay open either way — a successful action triggers a sequence.*
			// event that refreshes the view via SetView; a failure shows here.
			if msg.Err != nil {
				m.errMsg = msg.Err.Error()
			} else {
				m.errMsg = ""
			}
			return m, nil
		}
	}
	return m, nil
}

func (m *SequenceModal) action(kind string, params map[string]any) tea.Cmd {
	return func() tea.Msg { return SubmitRequest{Kind: kind, Params: params} }
}

func (m *SequenceModal) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render("Sequence — "+m.slug) + "\n\n")

	if m.view == nil {
		b.WriteString(style.Hint.Render("loading sequence status…") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return style.Modal.Width(60).Render(b.String())
	}

	// Header: status · policy · →target.
	status := m.view.Status
	if status == "" {
		status = "active"
	}
	statusStr := status
	if status == "paused" {
		statusStr = style.Danger.Render("paused")
	}
	header := statusStr
	if m.view.Policy != "" {
		header += style.DimText.Render(" · " + m.view.Policy)
	}
	if m.view.Target != "" {
		header += style.DimText.Render(" · →" + m.view.Target)
	}
	b.WriteString(header + "\n\n")

	if m.view.Complete {
		b.WriteString(style.Done.Render("✓ all phases complete") + "\n\n")
	} else {
		// Active phase header.
		title := ""
		for _, ph := range m.view.Phases {
			if ph.Number == m.view.ActivePhase {
				title = ph.Title
				break
			}
		}
		b.WriteString(style.Header.Render("Phase "+m.view.ActivePhase+": "+title) + "\n")
		tasks := m.activeTasks()
		if len(tasks) == 0 {
			b.WriteString(style.DimText.Render("  (no tasks in this phase yet)") + "\n")
		}
		for i, tk := range tasks {
			cur := style.CursorMarker(i == m.cursor)
			line := cur + style.GateGlyph(tk.GateState) + " " + tk.Title + " " + style.DimText.Render("["+tk.GateState+"]")
			if tk.Status == "needs_attention" {
				line = cur + style.GateGlyph(tk.GateState) + " " + style.Danger.Render(tk.Title+" ⚠ blocked")
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	if m.errMsg != "" {
		b.WriteString(style.InlineError.Render(m.errMsg) + "\n\n")
	}
	b.WriteString(style.Hint.Render(
		style.Key.Render("p") + " pause · " +
			style.Key.Render("r") + " resume · " +
			style.Key.Render("a") + " advance · " +
			style.Key.Render("s") + " skip · " +
			style.Key.Render("c") + " complete · " +
			style.Key.Render("esc") + " close"))
	return style.Modal.Width(64).Render(b.String())
}
