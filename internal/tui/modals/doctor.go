package modals

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/doctor"
	"github.com/rohilrs/Hive/internal/tui/style"
)

// DoctorModal is the TUI mirror of `hive doctor` — it renders a
// doctor.Report (20 read-only health/drift/config checks across the
// daemon, store, worktrees, sources, mcp, config, approvals subsystems)
// grouped by subsystem with a per-status glyph + a summary header.
//
// Flow:
//
//	[running] ──RPCResultMsg{Kind:"doctor_report"}──► [report]
//	  report:
//	    ↑/k ↓/j   scroll the (possibly long) check list
//	    r         re-run the checks (back to [running], root re-fires doctor.Run)
//	    esc       close.
//
// The actual doctor.Run is executed OFF the UI thread by the root (it does
// blocking RPC + local file/db/worktree reads); the modal opens in "running"
// and consumes the typed Report when the result lands. We carry the Report
// directly under RPCResultMsg.Data["report"] (a doctor.Report value) rather
// than round-tripping through a flat map — the report is structured data the
// modal renders field-for-field, so a typed hand-off is cleaner.
type DoctorModal struct {
	// loading is true between open / re-run (r) and the report arriving.
	loading bool

	// report is the last completed audit. nil until the first result lands;
	// hasReport disambiguates "no report yet" from "report with zero checks".
	report    doctor.Report
	hasReport bool

	// scroll is the check-list scroll offset in lines from the top of the
	// rendered (grouped) body. Update only inc/decrements (loosely bounded);
	// View does the precise clamp via windowLines so it never overflows.
	scroll int

	// errMsg surfaces a transport-level failure of the doctor run itself
	// (distinct from per-check StatusError findings, which live in the
	// report). doctor.Run rarely returns a hard error — it folds RPC
	// failures into skip/error checks — but the field exists for symmetry.
	errMsg string

	width, height int
}

// NewDoctorModal constructs the modal in its "running" (loading) state. The
// root is expected to fire doctor.Run on a background goroutine and dispatch
// the Report back as RPCResultMsg{Kind:"doctor_report", Data:{"report": rep}}.
func NewDoctorModal() *DoctorModal {
	return &DoctorModal{loading: true}
}

func (m *DoctorModal) Title() string { return "Doctor — daemon + state audit" }

// Init returns nil — the root kicks off the initial doctor.Run (see app.go
// case tabs.TabDoctorRequest).
func (m *DoctorModal) Init() tea.Cmd { return nil }

func (m *DoctorModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case RPCResultMsg:
		if msg.Kind != "doctor_report" {
			return m, nil
		}
		m.loading = false
		if msg.Err != nil {
			m.errMsg = msg.Err.Error()
			return m, nil
		}
		m.errMsg = ""
		// The Report is carried as a typed value in the Data map (see the
		// modal doc comment). A missing/ill-typed value leaves hasReport
		// false so View shows the empty state rather than a panic.
		if rep, ok := msg.Data["report"].(doctor.Report); ok {
			m.report = rep
			m.hasReport = true
			m.scroll = 0 // reset scroll on a fresh report
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		case "r":
			// Re-run the checks. Re-enter the loading state and ask the root
			// to fire doctor.Run again via a SubmitRequest the root routes
			// back through the same off-thread path. No-op while already
			// loading so a key-mash doesn't queue duplicate runs.
			if m.loading {
				return m, nil
			}
			m.loading = true
			m.errMsg = ""
			return m, func() tea.Msg {
				return SubmitRequest{Kind: "doctor_run", Params: map[string]any{}}
			}
		case "up", "k":
			if m.scroll > 0 {
				m.scroll--
			}
			return m, nil
		case "down", "j":
			// Loosely bounded by the rendered body length; View clamps
			// precisely to the visible window.
			if m.scroll < len(m.bodyLines()) {
				m.scroll++
			}
			return m, nil
		}
	}
	return m, nil
}

func (m *DoctorModal) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(m.Title()) + "\n\n")

	if m.loading {
		b.WriteString(style.NeedsAttention.Render("⏳ running checks… (daemon + store + worktrees + sources + mcp + config)") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	if m.errMsg != "" {
		b.WriteString(style.InlineError.Render("✗ doctor failed: "+m.errMsg) + "\n\n")
		b.WriteString(style.Hint.Render(
			style.Key.Render("r") + " retry · " + style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	if !m.hasReport {
		// Defensive — root always sends a report after the running state.
		b.WriteString(style.Hint.Render("no report") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	// Summary header: ok / warn / error / skip counts, mirroring the CLI's
	// "summary:" line but as a colored header band so the operator scans the
	// overall verdict at a glance.
	s := m.report.Summary
	b.WriteString(
		style.Done.Render(fmt.Sprintf("%d ok", s.OK)) + style.DimText.Render(" · ") +
			style.NeedsAttention.Render(fmt.Sprintf("%d warn", s.Warnings)) + style.DimText.Render(" · ") +
			style.InlineError.Render(fmt.Sprintf("%d error", s.Errors)) + style.DimText.Render(" · ") +
			style.DimText.Render(fmt.Sprintf("%d skip", s.Skipped)) + "\n\n")

	// The check list can be long (20 checks + multi-line hints), so window it
	// to the available height with scroll affordances — never overflow the
	// modal. Budget: total height minus title (2), summary (2), footer (1),
	// and the modal chrome (border 2 + padding 2 = 4). Clamp to a usable min.
	bodyH := height - 9
	if bodyH < 3 {
		bodyH = 3
	}
	win := windowLines(m.bodyLines(), bodyH, m.scroll)
	b.WriteString(strings.Join(win, "\n") + "\n\n")

	b.WriteString(style.Hint.Render(
		style.Key.Render("↑↓") + " scroll · " +
			style.Key.Render("r") + " re-run · " +
			style.Key.Render("esc") + " close"))
	return m.frame(b.String(), width)
}

// bodyLines renders the report grouped by subsystem (insertion order across
// subsystems, name-sorted within), one line per check (glyph + name +
// message) followed by indented hint lines. Mirrors cmd_doctor.go's
// renderHuman but in verbose mode — the modal always shows OK checks too so
// the operator sees the full picture (unlike the CLI, where OK rows are
// hidden by default to keep piped output terse).
func (m *DoctorModal) bodyLines() []string {
	var lines []string
	groups := make(map[string][]doctor.Check)
	var order []string
	for _, c := range m.report.Checks {
		if _, seen := groups[c.Subsystem]; !seen {
			order = append(order, c.Subsystem)
		}
		groups[c.Subsystem] = append(groups[c.Subsystem], c)
	}
	for gi, sub := range order {
		if gi > 0 {
			lines = append(lines, "") // blank separator between subsystems
		}
		lines = append(lines, style.Header.Render(sub))
		sorted := append([]doctor.Check(nil), groups[sub]...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, c := range sorted {
			g := style.ForStatus(statusToRunStatus(c.Status))
			lines = append(lines, "  "+g.Render(doctorGlyph(c.Status)+" "+c.Name)+" "+c.Message)
			if c.Hint != "" {
				// Composite checks carry per-finding detail in the Hint, one
				// per line; indent each so it reads as a child of the check.
				for _, hl := range strings.Split(strings.TrimRight(c.Hint, "\n"), "\n") {
					lines = append(lines, style.DimText.Render("      — "+hl))
				}
			}
		}
	}
	return lines
}

// doctorGlyph mirrors cmd_doctor.go's unicodeGlyph — the TUI always renders to
// a real terminal so we don't need the ASCII fallback path.
func doctorGlyph(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return "✓"
	case doctor.StatusWarn:
		return "⚠"
	case doctor.StatusError:
		return "✗"
	case doctor.StatusSkip:
		return "·"
	}
	return "?"
}

// statusToRunStatus maps a doctor.Status onto the run-status vocabulary
// style.ForStatus understands, so check rows reuse the shared status palette
// (green ok / yellow warn / red error / gray skip) instead of a bespoke one.
func statusToRunStatus(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return "done"
	case doctor.StatusWarn:
		return "needs_attention"
	case doctor.StatusError:
		return "error"
	case doctor.StatusSkip:
		return "pending"
	}
	return ""
}

// frame wraps the modal body in the standard rounded-border + capped width.
// Reuses the package frameWidth helper (most-of-terminal, capped at 140) so a
// long report has room to breathe horizontally.
func (m *DoctorModal) frame(content string, width int) string {
	return style.Modal.Width(frameWidth(width)).Render(content)
}
