package modals

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

type taskDetailState int

const (
	tdLoading       taskDetailState = iota // fetching task.get
	tdView                                 // read-only view + action hints
	tdEdit                                 // editing title + body
	tdConfirmDelete                        // y/N delete prompt
	tdRunning                              // run.now dispatch in flight
	tdDecomposing                          // task.decompose (Sonnet turn) in flight
)

// RunRef is a minimal associated-run row shown in the task detail
// modal. Passed in at open time from the Projects tab's snapshot.
type RunRef struct {
	ID, Status, Summary string
}

// TaskDetail is the Enter-on-task modal: view + edit body + run +
// delete. Loads the full task (incl body) via task.get on Init.
type TaskDetail struct {
	taskID string
	state  taskDetailState

	title  string
	body   string
	status string
	errMsg string
	runs   []RunRef // associated runs (point-in-time at open)

	// phaseLine is the derived "Phase N · step i of total" header shown for
	// roadmap-decomposed tasks (built from roadmap_phase / _index / _total
	// metadata). Empty for tasks with no roadmap linkage.
	phaseLine string

	// integrationState / prURL / prNumber surface the feature-branch
	// integration pipeline status (Task 10 snapshot fields) in the modal.
	// integrationState is "" for tasks that are not currently integrating.
	integrationState string
	prURL            string
	prNumber         int

	// edit-mode widgets
	titleInput textinput.Model
	bodyArea   textarea.Model
	editFocus  int // 0=title, 1=body

	// tdView scrollable region (body + runs). Lazily initialized so it
	// gets the default keymap (a zero-value viewport has no key bindings).
	vp      viewport.Model
	vpReady bool

	// spinner animates the wait states tdLoading (task.get in flight) and
	// tdRunning (run.now blocking on the predictor). Without animation
	// the modal looks frozen during the ~10s predictor stall.
	spinner spinner.Model
}

// NewTaskDetail constructs the modal for a task. knownTitle + runs are
// the snapshot-known values shown immediately; the full body loads via
// task.get on Init. integrationState/prURL/prNumber come from the
// snapshot (Task 10) and are rendered as a status line when non-empty.
func NewTaskDetail(taskID, knownTitle string, runs []RunRef, integrationState, prURL string, prNumber int) Modal {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return &TaskDetail{
		taskID:           taskID,
		title:            knownTitle,
		runs:             runs,
		state:            tdLoading,
		spinner:          sp,
		integrationState: integrationState,
		prURL:            prURL,
		prNumber:         prNumber,
	}
}

func (m *TaskDetail) Title() string { return "Task" }

// Init fires task.get to load the body AND starts the spinner tick so
// the tdLoading state animates immediately.
func (m *TaskDetail) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			return SubmitRequest{Kind: "task_get", Params: map[string]any{"task_id": m.taskID}}
		},
		m.spinner.Tick,
	)
}

func (m *TaskDetail) Update(msg tea.Msg) (Modal, tea.Cmd) {
	// Spinner tick: advance the dots only while in a wait state.
	// Outside tdLoading/tdRunning, drop the tick so the ticker chain
	// stops naturally and we don't waste a frame redraw per beat.
	if tm, ok := msg.(spinner.TickMsg); ok {
		if m.state == tdLoading || m.state == tdRunning || m.state == tdDecomposing {
			upd, cmd := m.spinner.Update(tm)
			m.spinner = upd
			return m, cmd
		}
		return m, nil
	}
	switch msg := msg.(type) {
	case RPCResultMsg:
		return m.handleResult(msg)
	case tea.KeyMsg:
		switch m.state {
		case tdLoading, tdRunning, tdDecomposing:
			// Allow esc even while a fetch / dispatch / decompose is in flight
			// so a slow predictor or hung Sonnet turn can't trap the user. The
			// server-side work continues regardless; if a decompose result
			// later lands the root sees the modal is closed and drops it.
			if msg.String() == "esc" {
				return m, func() tea.Msg { return CloseMsg{} }
			}
		case tdView:
			return m.updateView(msg)
		case tdEdit:
			return m.updateEdit(msg)
		case tdConfirmDelete:
			return m.updateConfirmDelete(msg)
		}
	}
	// In edit mode, forward non-key msgs to the focused widget.
	if m.state == tdEdit {
		return m.forwardToWidget(msg)
	}
	return m, nil
}

func (m *TaskDetail) handleResult(msg RPCResultMsg) (Modal, tea.Cmd) {
	switch msg.Kind {
	case "task_get":
		if msg.Err != nil {
			m.errMsg = msg.Err.Error()
			m.state = tdView
			return m, nil
		}
		if t, ok := msg.Data["title"].(string); ok {
			m.title = t
		}
		if b, ok := msg.Data["body"].(string); ok {
			m.body = b
		}
		if st, ok := msg.Data["status"].(string); ok {
			m.status = st
		}
		m.phaseLine = phaseLineFromMeta(msg.Data["metadata"])
		m.state = tdView
		return m, nil
	case "task_edit":
		if msg.Err != nil {
			m.errMsg = msg.Err.Error()
			return m, nil
		}
		// Saved → back to view with refreshed values from the inputs.
		m.title = m.titleInput.Value()
		m.body = m.bodyArea.Value()
		m.errMsg = ""
		m.state = tdView
		return m, nil
	case "task_delete", "run_now":
		if msg.Err != nil {
			// Recover to the view state so the error is visible AND
			// the user can esc out (confirm-delete trapped them before
			// the 3.7.5 fix).
			m.errMsg = msg.Err.Error()
			m.state = tdView
			return m, nil
		}
		// Both close the modal on success.
		return m, func() tea.Msg { return CloseMsg{} }
	case "task_decompose_open":
		// Only an ERROR is forwarded here: on success the root swaps the
		// whole modal out for the TaskDecomposeConfirmModal (the proposals
		// arrive as a typed taskDecomposeProposedMsg, not through Data). On
		// error, drop the spinner back to the view so it's visible + esc-able.
		if msg.Err != nil {
			m.errMsg = msg.Err.Error()
			m.state = tdView
		}
		return m, nil
	}
	return m, nil
}

func (m *TaskDetail) updateView(msg tea.KeyMsg) (Modal, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return CloseMsg{} }
	case "e":
		// Enter edit mode: seed widgets with current values.
		ti := textinput.New()
		ti.SetValue(m.title)
		ti.Width = 60
		ti.Focus()
		m.titleInput = ti
		ta := textarea.New()
		ta.SetValue(m.body)
		ta.SetWidth(60)
		ta.SetHeight(8)
		m.bodyArea = ta
		m.editFocus = 0
		m.state = tdEdit
		m.errMsg = ""
		return m, textinput.Blink
	case "r":
		// Show a dispatching state immediately — run.now blocks
		// synchronously through the predictor (~10s) before the run
		// surfaces, so without this the modal looks frozen (3.7.5).
		m.state = tdRunning
		m.errMsg = ""
		tid := m.taskID
		// Re-issue the spinner tick: the previous tick chain may have
		// halted while we were in tdView (the Update spinner.TickMsg
		// case drops the tick when state isn't a wait state). Without
		// this, tdRunning would render a static glyph.
		return m, tea.Batch(
			func() tea.Msg {
				return SubmitRequest{Kind: "run_now", Params: map[string]any{"task_id": tid}}
			},
			m.spinner.Tick,
		)
	case "d":
		m.state = tdConfirmDelete
		return m, nil
	case "D":
		// Decompose into sub-tasks (capital D — distinct from lowercase d
		// = delete). Guard against decomposing a task that's already in
		// flight: a running task is mid-dispatch and shouldn't be carved up
		// underneath itself. (status is the snapshot value loaded via
		// task.get; empty/pending/queued/needs_attention are all fair game.)
		if m.status == "running" {
			m.errMsg = "can't decompose a running task"
			return m, nil
		}
		// Show the decomposing spinner immediately — task.decompose is a
		// Sonnet tool turn (~10-60s) fired off-thread by the root. Re-issue
		// the spinner tick (the prior chain may have halted in tdView) so the
		// glyph animates, mirroring the tdRunning path.
		m.state = tdDecomposing
		m.errMsg = ""
		tid := m.taskID
		return m, tea.Batch(
			func() tea.Msg {
				return SubmitRequest{Kind: "task_decompose_open", Params: map[string]any{"task_id": tid}}
			},
			m.spinner.Tick,
		)
	}
	// Other keys (↑↓/PgUp/PgDn/j/k) scroll the body+runs viewport.
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *TaskDetail) updateEdit(msg tea.KeyMsg) (Modal, tea.Cmd) {
	switch msg.String() {
	case "ctrl+s":
		title := strings.TrimSpace(m.titleInput.Value())
		if title == "" {
			m.errMsg = "title cannot be empty"
			return m, nil
		}
		tid := m.taskID
		body := m.bodyArea.Value()
		return m, func() tea.Msg {
			return SubmitRequest{Kind: "task_edit", Params: map[string]any{
				"task_id": tid, "title": title, "body": body,
			}}
		}
	case "tab":
		if m.editFocus == 0 {
			m.titleInput.Blur()
			m.bodyArea.Focus()
			m.editFocus = 1
		} else {
			m.bodyArea.Blur()
			m.titleInput.Focus()
			m.editFocus = 0
		}
		return m, textinput.Blink
	case "esc":
		// Cancel edit → back to view (esc at modal level closes; here
		// we intercept to return to view first).
		m.state = tdView
		return m, nil
	}
	return m.forwardToWidget(msg)
}

func (m *TaskDetail) forwardToWidget(msg tea.Msg) (Modal, tea.Cmd) {
	var cmd tea.Cmd
	if m.editFocus == 0 {
		m.titleInput, cmd = m.titleInput.Update(msg)
	} else {
		m.bodyArea, cmd = m.bodyArea.Update(msg)
	}
	return m, cmd
}

func (m *TaskDetail) updateConfirmDelete(msg tea.KeyMsg) (Modal, tea.Cmd) {
	switch strings.ToLower(msg.String()) {
	case "y":
		tid := m.taskID
		return m, func() tea.Msg {
			return SubmitRequest{Kind: "task_delete", Params: map[string]any{"task_id": tid}}
		}
	case "n", "esc":
		m.state = tdView
		m.errMsg = ""
		return m, nil
	}
	return m, nil
}

// phaseLineFromMeta derives the "Phase N · step i of total" header from a
// task's metadata map (as delivered over the wire — map[string]any with
// string values). Returns "" when the task has no roadmap_phase, so
// non-roadmap tasks render no extra header row. Partial metadata degrades
// gracefully: phase-only → "Phase N"; index/total without phase → "step i
// of total".
func phaseLineFromMeta(v any) string {
	md, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	str := func(k string) string { s, _ := md[k].(string); return s }
	phase, idx, total := str("roadmap_phase"), str("roadmap_phase_index"), str("roadmap_phase_total")
	var parts []string
	if phase != "" {
		parts = append(parts, "Phase "+phase)
	}
	if idx != "" && total != "" {
		parts = append(parts, "step "+idx+" of "+total)
	}
	return strings.Join(parts, " · ")
}

func (m *TaskDetail) View(width, height int) string {
	bw := width - 6
	if bw < 50 {
		bw = 50
	}

	var b strings.Builder
	b.WriteString(style.ModalTitle.Render("Task "+m.taskID) + "\n\n")

	switch m.state {
	case tdLoading:
		b.WriteString(dim(m.spinner.View() + " loading task…"))
	case tdView:
		// Fixed header. Truncate to the frame's content area (bw-4: the modal
		// padding) so a long title/status can't soft-wrap and add a row that
		// pushes the modal past its height budget.
		cw := bw - 4
		b.WriteString(truncateRunes("Title: "+m.title, cw) + "\n")
		if m.phaseLine != "" {
			b.WriteString(dim(truncateRunes(m.phaseLine, cw)) + "\n")
		}
		b.WriteString(dim(truncateRunes("status: "+m.status, cw)) + "\n")
		if m.integrationState != "" {
			var s string
			switch m.integrationState {
			case "integrating":
				s = "Integration: finishing…"
			case "pr_open":
				s = fmt.Sprintf("Integration: PR #%d (open)", m.prNumber)
			case "ci":
				s = fmt.Sprintf("Integration: PR #%d · CI running", m.prNumber)
			case "merged":
				s = fmt.Sprintf("Integration: PR #%d · ✓ merged", m.prNumber)
			case "blocked":
				s = fmt.Sprintf("Integration: PR #%d · ✗ blocked", m.prNumber)
			default:
				s = "Integration: " + m.integrationState
			}
			if m.prURL != "" {
				s += "  " + m.prURL
			}
			var line string
			switch m.integrationState {
			case "merged":
				line = style.Done.Render(s)
			case "blocked":
				line = style.Danger.Render(s)
			default:
				line = dim(s)
			}
			b.WriteString(truncateRunes(line, cw) + "\n")
		}
		b.WriteString("\n")

		// Scrollable region: body + runs (+ any error). Long tasks/run
		// lists no longer overflow the modal — ↑↓/PgUp/PgDn scroll.
		var c strings.Builder
		c.WriteString("Body:\n")
		if strings.TrimSpace(m.body) == "" {
			c.WriteString(dim("  (empty)") + "\n")
		} else {
			for _, line := range strings.Split(m.body, "\n") {
				c.WriteString("  " + line + "\n")
			}
		}
		c.WriteString("\nRuns:\n")
		if len(m.runs) == 0 {
			c.WriteString(dim("  (none yet — press r to dispatch)") + "\n")
		} else {
			for _, r := range m.runs {
				c.WriteString(fmt.Sprintf("  %s  [%s]  %s\n", r.ID, r.Status, r.Summary))
			}
		}
		if m.errMsg != "" {
			c.WriteString("\n" + errStyle(m.errMsg) + "\n")
		}

		// Reserve rows for the chrome around the viewport so the modal never
		// overflows + clips at the top: header (5: "Task" + blank + Title +
		// status + blank), the blank after the viewport (1), the footer (1),
		// and the modal frame's border+padding (4) = 11. A roadmap phase line
		// adds one more header row → 12. An integration state line adds one
		// more → 13 (or 12 when no phaseLine).
		// -4 = the modal frame's horizontal padding (style.Modal has Padding(1,2)).
		// If the viewport is wider than the frame's content area, EVERY viewport
		// line soft-wraps inside the frame, doubling the row count and pushing
		// the modal off-screen (the "scrolling resizes the modal" cutoff bug).
		chrome := 11
		if m.phaseLine != "" {
			chrome++
		}
		if m.integrationState != "" {
			chrome++
		}
		vpW, vpH := bw-4, height-chrome
		if vpH < 4 {
			vpH = 4
		}
		if vpW < 10 {
			vpW = 10
		}
		if !m.vpReady {
			m.vp = viewport.New(vpW, vpH)
			m.vpReady = true
		} else {
			m.vp.Width, m.vp.Height = vpW, vpH
		}
		m.vp.SetContent(c.String())
		b.WriteString(m.vp.View() + "\n\n")

		hint := style.Key.Render("e") + " edit · " + style.Key.Render("r") + " run · " + style.Key.Render("D") + " decompose · " + style.Key.Render("d") + " delete · " + style.Key.Render("esc") + " close"
		if m.vp.TotalLineCount() > vpH {
			hint += " · " + style.Key.Render("↑↓") + " scroll"
		}
		b.WriteString(style.Hint.Render(hint))
	case tdRunning:
		b.WriteString(dim(m.spinner.View()+" dispatching run… (predictor may take a few seconds)") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " to close — the run continues and will appear in Active"))
	case tdDecomposing:
		b.WriteString(dim(m.spinner.View()+" decomposing task… (Sonnet may take a few seconds)") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " to cancel — proposals open in a confirm step"))
	case tdEdit:
		b.WriteString("Title:\n  " + m.titleInput.View() + "\n\n")
		b.WriteString("Body:\n" + m.bodyArea.View() + "\n\n")
		if m.errMsg != "" {
			b.WriteString(errStyle(m.errMsg) + "\n\n")
		}
		b.WriteString(style.Hint.Render(style.Key.Render("ctrl+s") + " save · " + style.Key.Render("tab") + " switch field · " + style.Key.Render("esc") + " cancel"))
	case tdConfirmDelete:
		b.WriteString(style.Danger.Render("Delete") + " task " + m.taskID + "?\n")
		b.WriteString("Title: " + m.title + "\n\n")
		if m.errMsg != "" {
			b.WriteString(errStyle(m.errMsg) + "\n\n")
		}
		b.WriteString(style.Hint.Render(style.Key.Render("y") + " delete · " + style.Key.Render("n") + " cancel"))
	}

	return style.Modal.Width(bw).Render(b.String())
}

// dim is a legacy wrapper around style.Hint; new code should use style.Hint.Render directly.
func dim(s string) string {
	return style.Hint.Render(s)
}

// errStyle is a legacy wrapper around style.InlineError; new code should use style.InlineError.Render directly.
func errStyle(s string) string {
	return style.InlineError.Render(fmt.Sprintf("error: %s", s))
}
