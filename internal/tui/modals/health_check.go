package modals

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/branchhealth"
	"github.com/rohilrs/Hive/internal/tui/style"
)

// healthCheckTimeout bounds the async branch-health computation so the modal
// can never stay in the loading state forever (e.g. a slow `git status` on a
// large repo on a Windows-mounted filesystem).
const healthCheckTimeout = 20 * time.Second

// healthLoadedMsg carries the async health-check result back into the loop.
type healthLoadedMsg struct {
	report branchhealth.HealthReport
	err    error
}

// HealthCheckModal shows the feature-branch health report and, when the branch
// is behind its target with a conflict-free merge, offers rebase/merge
// remediation. The report loads asynchronously (Init); remediation runs through
// the health.remediate RPC and the fresh report re-renders in place.
type HealthCheckModal struct {
	slug, repoPath, feature, target string
	loaded                          bool
	report                          branchhealth.HealthReport
	reportText                      string // rendered report (local load or RPC result)
	err                             error
	spinner                         spinner.Model

	showActions bool   // loaded, behind>0, conflict-free, feature≠target
	confirming  string // "" | "rebase" | "merge" (awaiting y/N)
	submitting  bool
	status      string // post-remediation success line
	actionErr   string // inline remediation error
}

// NewHealthCheckModal constructs the modal; git work happens in Init.
func NewHealthCheckModal(slug, repoPath, feature, target string) Modal {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return &HealthCheckModal{
		slug:     slug,
		repoPath: repoPath,
		feature:  feature,
		target:   target,
		spinner:  sp,
	}
}

func (m *HealthCheckModal) Title() string { return "Health: " + m.slug }

// Init fires the async health-check and starts the spinner. The check runs in
// its own goroutine bounded by healthCheckTimeout and guarded against panics,
// so the modal always leaves the loading state — it never hangs the UI.
func (m *HealthCheckModal) Init() tea.Cmd {
	repo, feat, tgt, slug := m.repoPath, m.feature, m.target, m.slug
	return tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			done := make(chan healthLoadedMsg, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						done <- healthLoadedMsg{err: fmt.Errorf("health check panicked: %v", r)}
					}
				}()
				roadmapRel := "docs/superpowers/roadmaps/" + slug + ".md"
				rep, err := branchhealth.CheckFeatureBranch(repo, feat, tgt, roadmapRel)
				done <- healthLoadedMsg{report: rep, err: err}
			}()
			select {
			case res := <-done:
				return res
			case <-time.After(healthCheckTimeout):
				return healthLoadedMsg{err: fmt.Errorf("health check timed out after %s", healthCheckTimeout)}
			}
		},
	)
}

// actionable reports whether rebase/merge should be offered: a resolvable,
// conflict-free "behind" against a distinct target branch.
func (m *HealthCheckModal) actionable() bool {
	return m.feature != "" && m.target != "" && m.feature != m.target &&
		m.report.Behind > 0 && len(m.report.ConflictPaths) == 0 && !m.report.Dirty
}

func (m *HealthCheckModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case healthLoadedMsg:
		m.loaded = true
		m.report = msg.report
		m.err = msg.err
		if msg.err == nil {
			m.reportText = branchhealth.RenderHealthReport(msg.report)
			m.showActions = m.actionable()
		}
		return m, nil

	case spinner.TickMsg:
		if !m.loaded {
			sp, cmd := m.spinner.Update(msg)
			m.spinner = sp
			return m, cmd
		}
		return m, nil

	case RPCResultMsg:
		if msg.Kind != "remediate_health" {
			return m, nil
		}
		m.submitting = false
		if msg.Err != nil {
			m.actionErr = msg.Err.Error()
			m.confirming = ""
			m.showActions = m.actionable()
			return m, nil
		}
		action := m.confirming
		m.confirming = ""
		m.report.Behind = healthDataInt(msg.Data, "behind")
		m.report.Ahead = healthDataInt(msg.Data, "ahead")
		m.report.ConflictPaths = healthDataStrings(msg.Data, "conflict_paths")
		if rt, ok := msg.Data["report"].(string); ok && rt != "" {
			m.reportText = rt
		}
		m.status = fmt.Sprintf("✓ %s — now %d ahead / %d behind",
			healthRemediationVerb(action), m.report.Ahead, m.report.Behind)
		m.showActions = m.actionable()
		return m, nil

	case tea.KeyMsg:
		if !m.loaded {
			// The async health check can be slow (e.g. git merge-tree on a
			// large repo on a Windows-mounted filesystem). Never trap the
			// user behind it — any key closes while still loading, matching
			// the original read-only modal.
			return m, func() tea.Msg { return CloseMsg{} }
		}
		if m.submitting {
			// Remediation RPC in flight; it continues server-side. Allow esc
			// to stop watching, swallow everything else.
			if msg.String() == "esc" {
				return m, func() tea.Msg { return CloseMsg{} }
			}
			return m, nil
		}
		if m.confirming != "" {
			switch msg.String() {
			case "y", "Y":
				action := m.confirming
				slug := m.slug
				m.submitting = true
				m.actionErr = ""
				return m, func() tea.Msg {
					return SubmitRequest{Kind: "remediate_health", Params: map[string]any{
						"project_slug": slug,
						"action":       action,
					}}
				}
			case "n", "N", "esc":
				m.confirming = ""
				return m, nil
			default:
				return m, nil
			}
		}
		if m.showActions {
			switch msg.String() {
			case "r":
				m.confirming = "rebase"
				return m, nil
			case "m":
				m.confirming = "merge"
				return m, nil
			}
		}
		// Any other key dismisses the read-only/idle modal.
		return m, func() tea.Msg { return CloseMsg{} }
	}
	return m, nil
}

func (m *HealthCheckModal) View(width, height int) string {
	// bw is the modal's content+padding width; lipgloss adds the border (+2),
	// so the rendered block is bw+2 cols. Cap bw so that bw+2 never exceeds the
	// terminal width — otherwise the floor below would push the modal off the
	// right edge on a narrow terminal and clip the action/footer line.
	bw := width - 6
	if bw < 50 {
		bw = 50
	}
	if bw > width-2 {
		bw = width - 2
	}
	if bw < 10 {
		bw = 10
	}
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(m.Title()) + "\n\n")

	if !m.loaded {
		b.WriteString(style.Hint.Render(
			m.spinner.View()+" checking "+m.feature+" vs "+m.target+"…") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("any key") + " to close"))
		return style.Modal.Width(bw).Render(b.String())
	}
	if m.err != nil {
		b.WriteString(style.InlineError.Render("error: "+m.err.Error()) + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("any key") + " to close"))
		return style.Modal.Width(bw).Render(b.String())
	}

	// Build the action/status zone and footer first so we can measure their
	// row count before budgeting the report body.
	//
	// Layout inside style.Modal.Render (chrome = RoundedBorder top+bottom + Padding(1,2) top+bottom = 4):
	//
	//   title line           ← 1 row
	//   blank                ← 1 row  (the "\n\n" after the title)
	//   <body lines>         ← bodyBudget rows
	//   blank separator      ← 1 row  (written as "\n" between body and action)
	//   action zone line     ← 1 row  (one line: the keys / status / warning)
	//   footer               ← 1 row  ("any key to close")
	//
	// Fixed overhead (excluding body) = 5.
	// bodyBudget = (height - 4) - 5 = height - 9. Guard ≥ 1.
	//
	// Action zone is rendered as a single line (no trailing blank) so the
	// separator "\n" between body and action is exactly 1 row and the blank
	// line between action and footer is gone — keeping the total predictable.

	// Early-return paths (submitting, confirming) are handled inside the switch
	// below after windowing the report body with the same budget.

	// Main path: build action zone as a single-line string (no trailing \n\n).
	var actionZone string
	switch {
	case m.actionErr != "":
		actionZone = style.InlineError.Render(m.actionErr)
	case m.status != "":
		actionZone = style.Hint.Render(m.status)
	case m.showActions:
		actionZone = style.Key.Render("[r]") + " rebase onto target   " +
			style.Key.Render("[m]") + " merge target in"
	case m.report.Dirty:
		actionZone = style.Hint.Render("⚠ uncommitted changes — commit or stash before remediating")
	case len(m.report.ConflictPaths) > 0:
		actionZone = style.Hint.Render("⚠ conflicts predicted — resolve manually")
	}
	footer := style.Hint.Render(style.Key.Render("any key") + " to close")

	// bodyBudget: (height minus modal chrome 4) minus fixed overhead (5 rows).
	// windowLines needs height ≥ 2 to produce at most height rows when
	// truncation is required (it reserves 1 row for the scroll hint). Clamp
	// to 2 so the returned slice is always ≤ bodyBudget in length, which
	// keeps total rendered rows ≤ chrome(4) + fixed(5) + bodyBudget = height.
	// The minimum usable modal height is therefore 4+5+2 = 11 rows.
	bodyBudget := height - 9
	if bodyBudget < 2 {
		bodyBudget = 2
	}
	reportLines := strings.Split(strings.TrimRight(m.reportText, "\n"), "\n")

	switch {
	case m.submitting:
		// submitting: progress line replaces the normal action zone; no footer.
		// Fixed overhead: title(1) + blank(1) + blank_sep(1) + progress(1) + working(1) = 5.
		winLines := windowLines(reportLines, bodyBudget, 0)
		for _, line := range winLines {
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
		b.WriteString(style.Hint.Render("⏳ "+healthRemediationProgressVerb(m.confirming)+"…") + "\n")
		b.WriteString(style.Hint.Render("working…"))
		return style.Modal.Width(bw).Render(b.String())
	case m.confirming != "":
		// confirming: two-line prompt (verb line + key line), no footer.
		// Fixed overhead: title(1) + blank(1) + blank_sep(1) + verb(1) + keys(1) = 5.
		winLines := windowLines(reportLines, bodyBudget, 0)
		for _, line := range winLines {
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
		verb, note := "Rebase feature onto target", "this rewrites history"
		if m.confirming == "merge" {
			verb, note = "Merge target into feature", "this adds a merge commit"
		}
		b.WriteString(verb + " — " + note + ".\n")
		b.WriteString(style.Key.Render("[y]") + " confirm   " + style.Key.Render("[n/esc]") + " cancel")
		return style.Modal.Width(bw).Render(b.String())
	}

	// Normal (idle) path: window the report body, then append action zone + footer.
	// Layout: title\n\n <body>\n \n<action>\n<footer>
	winLines := windowLines(reportLines, bodyBudget, 0)
	for _, line := range winLines {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	if actionZone != "" {
		b.WriteString(actionZone + "\n")
	}
	b.WriteString(footer)
	return style.Modal.Width(bw).Render(b.String())
}

func healthRemediationVerb(action string) string {
	if action == "merge" {
		return "merged"
	}
	return "rebased"
}

func healthRemediationProgressVerb(action string) string {
	if action == "merge" {
		return "merging"
	}
	return "rebasing"
}

func healthDataInt(d map[string]any, k string) int {
	if v, ok := d[k].(float64); ok {
		return int(v)
	}
	return 0
}

func healthDataStrings(d map[string]any, k string) []string {
	raw, ok := d[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
