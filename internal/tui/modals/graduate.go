package modals

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/tui/style"
)

// graduateState is the modal's lifecycle phase.
type graduateState int

const (
	gradConfirm graduateState = iota // initial: mode selector + draft toggle
	gradRunning                      // RPC fired; streaming progress labels
	gradResult                       // terminal: verdict/done/failed rendered
)

// graduate mode options (the selector). The default selection MUST be Dry-run
// (safe — the real-PR modes are explicit, non-default choices).
const (
	gradModeDryRun = iota // dry-run: checks + audit, prints verdict + PR body, opens NO PR
	gradModeReal          // real PR
	gradModeForce         // real PR, force (bypass audit/build gate)
)

var graduateModeLabels = []string{
	"Dry-run",
	"Graduate",
	"Graduate --force",
}

// GraduateModal drives the `hive project graduate` flow from the TUI. It opens
// in a confirm state (mode selector defaulting to Dry-run + a draft toggle),
// transitions to a running state once the async project.graduate RPC has been
// kicked off by the root, and renders the streamed verdict / done / failed
// lifecycle in the result state.
//
// All graduate lifecycle events arrive from the root via RPCResultMsg with
// Kind == "project_graduate_open" and a typed payload under Data (see the
// app.go event handlers). The modal NEVER calls the client directly — it emits
// a SubmitRequest and the root owns the async start + id-tracking so concurrent
// graduations don't cross-wire.
type GraduateModal struct {
	slug, feature, target string

	state   graduateState
	modeSel int  // gradModeDryRun | gradModeReal | gradModeForce
	asDraft bool // [ ] open as draft toggle
	spinner spinner.Model

	// running state
	progress []string // accumulated "→ <label>" phase labels

	// result state
	verdict    *graduate.GraduationVerdict // set on graduate.verdict (rendered even if a failure follows)
	hasVerdict bool
	prURL      string // set on graduate.done (real PR)
	dryRun     bool   // graduate.done dry_run flag
	done       bool   // graduate.done arrived (success)
	failErr    string // set on graduate.failed

	// confirm-state "last run" (fetched on open via graduate_status)
	lastRun        *graduate.GraduateResult
	showFindings   bool
	findingsScroll int // scroll offset for the expanded findings view
	resultScroll   int // scroll offset for the result-state verdict region

	remediateStatus string // one-line outcome of an 'r' remediation, shown in gradResult
}

// NewGraduateModal constructs the modal in its confirm state. The mode selector
// defaults to Dry-run; the draft toggle defaults off.
func NewGraduateModal(slug, feature, target string) Modal {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return &GraduateModal{
		slug:    slug,
		feature: feature,
		target:  target,
		state:   gradConfirm,
		modeSel: gradModeDryRun,
		spinner: sp,
	}
}

// NewGraduateModalReattached constructs the modal directly in its running state,
// for re-attaching to an in-flight graduate after the operator detached (esc).
// Progress emitted before re-attach is not replayed; new progress + the terminal
// verdict/done land normally via the existing event forwarding.
func NewGraduateModalReattached(slug, feature, target string) *GraduateModal {
	m := NewGraduateModal(slug, feature, target).(*GraduateModal)
	m.state = gradRunning
	m.progress = []string{"re-attached — watching…"}
	return m
}

func (m *GraduateModal) Title() string { return "Graduate: " + m.slug }

func (m *GraduateModal) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			return SubmitRequest{Kind: "graduate_status_fetch", Params: map[string]any{"slug": m.slug}}
		},
	)
}

// submitParams maps the current selection to the project.graduate params. The
// real-PR modes are the explicit, non-default choices; Dry-run is the default.
func (m *GraduateModal) submitParams() map[string]any {
	force := m.modeSel == gradModeForce
	dryRun := m.modeSel == gradModeDryRun
	return map[string]any{
		"slug":    m.slug,
		"force":   force,
		"draft":   m.asDraft,
		"dry_run": dryRun,
	}
}

func (m *GraduateModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		if m.state == gradRunning {
			sp, cmd := m.spinner.Update(msg)
			m.spinner = sp
			return m, cmd
		}
		return m, nil

	case RPCResultMsg:
		// On-open fetch of the last persisted graduate run (additive — handled
		// before the lifecycle Kind so it never falls through to it).
		if msg.Kind == "graduate_status" {
			if js, ok := msg.Data["_graduate_status"].(string); ok && js != "" {
				var rec graduate.GraduateResult
				if json.Unmarshal([]byte(js), &rec) == nil {
					m.lastRun = &rec
				}
			}
			return m, nil
		}
		if msg.Kind == "project_remediate" {
			if msg.Err != nil {
				m.remediateStatus = "Remediate failed: " + msg.Err.Error()
				return m, nil
			}
			created := 0
			if arr, ok := msg.Data["created"].([]any); ok {
				created = len(arr)
			}
			skipped := 0
			if s, ok := msg.Data["skipped"].(float64); ok {
				skipped = int(s)
			}
			if created == 0 {
				m.remediateStatus = "Nothing to remediate"
				if skipped > 0 {
					m.remediateStatus += fmt.Sprintf(" (%d already open)", skipped)
				}
			} else {
				m.remediateStatus = fmt.Sprintf("Created %d task(s) → inbox", created)
				if skipped > 0 {
					m.remediateStatus += fmt.Sprintf(", %d skipped", skipped)
				}
			}
			return m, nil
		}
		// All graduate lifecycle (start ack + progress/verdict/done/failed) is
		// forwarded under this Kind. The root tags the payload via a "_graduate_*"
		// discriminator so we can route without a second message type.
		if msg.Kind != "project_graduate_open" {
			return m, nil
		}
		return m.handleGraduateEvent(msg)

	case tea.KeyMsg:
		switch m.state {
		case gradConfirm:
			return m.updateConfirm(msg)
		case gradRunning:
			// Graduation runs server-side; esc stops watching, others swallowed.
			if msg.String() == "esc" {
				return m, func() tea.Msg { return CloseMsg{} }
			}
			return m, nil
		case gradResult:
			// Nav scrolls the (windowed) verdict region; explicit keys close.
			switch msg.String() {
			case "up", "k":
				if m.resultScroll > 0 {
					m.resultScroll--
				}
				return m, nil
			case "down", "j":
				m.resultScroll++ // View clamps the upper bound via windowLines
				return m, nil
			case "r":
				// Remediate: create inbox tasks from the persisted result's confirmed
				// findings. Gate on "a verdict exists" — either this run's live verdict
				// (m.hasVerdict) or the on-open fetched prior run (m.lastRun). The daemon
				// persists the result before publishing the verdict/done/failed events,
				// so the file remediate reads is present whenever we're in gradResult.
				if m.hasVerdict || (m.lastRun != nil && m.lastRun.Verdict != nil) {
					slug := m.slug
					return m, func() tea.Msg {
						return SubmitRequest{Kind: "project_remediate", Params: map[string]any{"project_slug": slug}}
					}
				}
				return m, nil
			case "esc", "enter", "q":
				return m, func() tea.Msg { return CloseMsg{} }
			}
			return m, nil
		}
	}
	return m, nil
}

// updateConfirm handles keys in the confirm state: mode selection (up/down or
// j/k), draft toggle (space/d), submit (enter/ctrl+s), cancel (esc).
func (m *GraduateModal) updateConfirm(msg tea.KeyMsg) (Modal, tea.Cmd) {
	// While the findings are expanded, keys drive the scrollable findings region
	// (not the mode selector); v/esc collapse back.
	if m.showFindings {
		switch msg.String() {
		case "v", "esc":
			m.showFindings = false
			m.findingsScroll = 0
		case "up", "k":
			if m.findingsScroll > 0 {
				m.findingsScroll--
			}
		case "down", "j":
			m.findingsScroll++ // View clamps the upper bound via windowLines
		}
		return m, nil
	}
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return CloseMsg{} }
	case "up", "k":
		if m.modeSel > 0 {
			m.modeSel--
		}
		return m, nil
	case "down", "j":
		if m.modeSel < len(graduateModeLabels)-1 {
			m.modeSel++
		}
		return m, nil
	case " ", "d":
		m.asDraft = !m.asDraft
		return m, nil
	case "v":
		// Expand the last-run findings into the scrollable region — only
		// meaningful when a prior run with a verdict was fetched.
		if m.lastRun != nil && m.lastRun.Verdict != nil {
			m.showFindings = true
			m.findingsScroll = 0
		}
		return m, nil
	case "enter", "ctrl+s":
		params := m.submitParams()
		// Optimistically transition to the running state; the root fires the
		// async start and forwards the ack (or a start error) back as an
		// RPCResultMsg under "project_graduate_open".
		m.state = gradRunning
		return m, tea.Batch(
			m.spinner.Tick,
			func() tea.Msg {
				return SubmitRequest{Kind: "project_graduate_open", Params: params}
			},
		)
	}
	return m, nil
}

// handleGraduateEvent routes a forwarded graduate lifecycle payload. The root
// sets exactly one discriminator key per event:
//
//   - "_graduate_started" bool   → async start acked (or msg.Err set on start failure)
//   - "_graduate_progress" string → a phase label to accumulate
//   - "_graduate_verdict" string  → the GraduationVerdict JSON
//   - "_graduate_done" bool       → terminal success (with "_graduate_pr_url" + "_graduate_dry_run")
//   - "_graduate_failed" string   → terminal failure error
func (m *GraduateModal) handleGraduateEvent(msg RPCResultMsg) (Modal, tea.Cmd) {
	d := msg.Data

	// Async start ack / start error.
	if _, ok := d["_graduate_started"]; ok {
		if msg.Err != nil {
			// The start RPC failed before any daemon-side job — drop back so the
			// operator sees the error and can retry/cancel.
			m.state = gradResult
			m.failErr = msg.Err.Error()
		}
		return m, nil
	}

	if label, ok := d["_graduate_progress"].(string); ok && label != "" {
		m.progress = append(m.progress, label)
		// Stay in the running state; keep the spinner ticking.
		if m.state != gradResult {
			m.state = gradRunning
		}
		return m, m.spinner.Tick
	}

	if vj, ok := d["_graduate_verdict"].(string); ok && vj != "" {
		var v graduate.GraduationVerdict
		if err := json.Unmarshal([]byte(vj), &v); err == nil {
			m.verdict = &v
			m.hasVerdict = true
		}
		// A verdict may precede a graduate.failed (blocking-verdict case) — keep
		// it visible. Move to result so the verdict renders immediately.
		m.state = gradResult
		return m, nil
	}

	if _, ok := d["_graduate_done"]; ok {
		m.done = true
		m.dryRun, _ = d["_graduate_dry_run"].(bool)
		m.prURL, _ = d["_graduate_pr_url"].(string)
		m.state = gradResult
		return m, nil
	}

	if e, ok := d["_graduate_failed"].(string); ok {
		m.failErr = e
		m.state = gradResult // any already-rendered verdict stays visible
		return m, nil
	}

	return m, nil
}

func (m *GraduateModal) View(width, height int) string {
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
	// Inner content width for wrapping (modal padding is 2 each side).
	innerW := bw - 4
	if innerW < 10 {
		innerW = 10
	}

	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(m.Title()) + "\n\n")
	b.WriteString(style.Hint.Render(m.feature+" → "+m.target) + "\n\n")

	switch m.state {
	case gradConfirm:
		m.viewConfirm(&b, innerW, height)
	case gradRunning:
		m.viewRunning(&b)
	case gradResult:
		m.viewResult(&b, innerW, height)
	}
	// MaxHeight is the hard backstop: a long body (e.g. expanded findings) is
	// windowed below, but this guarantees the modal box never exceeds the
	// terminal height and so is never top-clipped by the centering placement.
	return style.Modal.Width(bw).MaxHeight(height).Render(b.String())
}

func (m *GraduateModal) viewConfirm(b *strings.Builder, innerW, height int) {
	// Expanded findings take over the confirm body as a scrollable region.
	if m.showFindings && m.lastRun != nil && m.lastRun.Verdict != nil {
		m.viewFindings(b, innerW, height)
		return
	}
	m.viewLastRun(b)
	b.WriteString(style.Key.Render("MODE") + "\n")
	for i, label := range graduateModeLabels {
		marker := style.CursorMarker(i == m.modeSel)
		line := marker + label
		if i == m.modeSel {
			line = marker + style.Key.Render(label)
		}
		// Annotate the real-PR modes so their gravity is visible.
		switch i {
		case gradModeDryRun:
			line += style.Hint.Render("  (checks + audit, no PR)")
		case gradModeReal:
			line += style.Hint.Render("  (opens the PR)")
		case gradModeForce:
			line += style.Danger.Render("  (bypass gate, opens PR)")
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	box := "[ ]"
	if m.asDraft {
		box = "[x]"
	}
	b.WriteString(box + " open as draft\n\n")
	b.WriteString(style.Hint.Render(
		style.Key.Render("↑↓/jk") + " mode   " +
			style.Key.Render("space/d") + " draft   " +
			style.Key.Render("enter") + " run   " +
			style.Key.Render("esc") + " cancel"))
}

func (m *GraduateModal) viewRunning(b *strings.Builder) {
	if len(m.progress) == 0 {
		b.WriteString(style.Hint.Render(m.spinner.View()+" starting graduation…") + "\n")
	} else {
		for _, label := range m.progress {
			b.WriteString(style.Hint.Render("→ "+label) + "\n")
		}
		b.WriteString("\n")
		b.WriteString(style.Hint.Render(m.spinner.View()+" running…") + "\n")
	}
	b.WriteString("\n")
	b.WriteString(style.Hint.Render(style.Key.Render("esc") + " stop watching"))
}

func (m *GraduateModal) viewResult(b *strings.Builder, innerW, height int) {
	wrap := lipgloss.NewStyle().Width(innerW)

	// Build the verdict region (status + wrapped summary + per-finding lines) into
	// a flat []string, then window it to a CONSTANT height so a long findings list
	// neither overflows the modal nor pushes the pinned outcome/footer off-screen.
	if m.hasVerdict && m.verdict != nil {
		v := m.verdict
		statusStyle := style.Done
		if v.Blocks() || v.Status == "GAPS_FOUND" {
			statusStyle = style.NeedsAttention
		}
		var lines []string
		lines = append(lines, style.Key.Render("Verdict: ")+statusStyle.Render(v.Status))
		if v.Summary != "" {
			lines = append(lines, strings.Split(wrap.Render(v.Summary), "\n")...)
		}
		if len(v.Findings) > 0 {
			lines = append(lines, "")
			for _, f := range v.Findings {
				sev := graduateSeverityStyle(f.Severity).Render("[" + f.Severity + "]")
				lines = append(lines, sev+" "+f.Title)
			}
		}
		// Budget = modal height minus ALL chrome so the box never exceeds `height`
		// even at mid-scroll (where windowLines emits both ↑ and ↓ affordance rows).
		// Chrome = 12: border (2) + padding (2) + title+blank (2) + branch+blank (2)
		// + blank-before-outcome + outcome (2) + blank+footer (2). Kept at a CONSTANT
		// `budget` rows (pad on overflow) so the box neither overflows nor visibly
		// grows/shrinks as the ↑/↓ affordances appear/disappear across scroll
		// positions (windowLines emits budget-1 rows at the ends, budget mid-scroll).
		// When remediateStatus is non-empty, it adds one additional row below the
		// outcome line; that extra row is absorbed by the verdict window shedding a
		// line, and the overall box remains bounded by MaxHeight — no arithmetic
		// change needed here.
		budget := height - 12
		if budget < 3 {
			budget = 3
		}
		win := windowLines(lines, budget, m.resultScroll)
		for _, ln := range win {
			b.WriteString(ln + "\n")
		}
		if len(lines) > budget {
			for i := len(win); i < budget; i++ {
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}

	// Pinned outcome line — always rendered below the windowed verdict so the PR
	// URL / error (the most important info) is never scrolled away or clipped.
	switch {
	case m.failErr != "":
		b.WriteString(style.InlineError.Render("✗ "+m.failErr) + "\n")
	case m.done && m.dryRun:
		b.WriteString(style.Done.Render("✓ dry-run complete — no PR opened") + "\n")
	case m.done && m.prURL != "":
		b.WriteString(style.Done.Render("✓ PR opened: ") + m.prURL + "\n")
	case m.done:
		b.WriteString(style.Done.Render("✓ graduation complete") + "\n")
	}

	// Pinned remediation status (set by an 'r' press) — sits below the outcome line
	// and above the footer; never inside the windowed verdict region.
	if m.remediateStatus != "" {
		b.WriteString(m.remediateStatus + "\n")
	}

	b.WriteString("\n")
	b.WriteString(style.Hint.Render(
		style.Key.Render("↑↓/jk") + " scroll   " + style.Key.Render("r") + " remediate   " + style.Key.Render("esc") + " close"))
}

// viewLastRun renders the "Last run" header above the mode selector in the
// confirm state: a one-line summary (when · mode · outcome) + severity tally +
// a `v` hint, and — when toggled — the expanded verdict findings.
func (m *GraduateModal) viewLastRun(b *strings.Builder) {
	if m.lastRun == nil {
		b.WriteString(style.Key.Render("Last run: ") + style.Hint.Render("none yet") + "\n\n")
		return
	}
	r := m.lastRun
	when := time.Unix(r.EndedAt, 0).Format("2006-01-02 15:04")
	b.WriteString(style.Key.Render("Last run: ") +
		style.Hint.Render(when+" · "+r.Mode+" · "+r.Outcome) + "\n")
	if r.Verdict != nil {
		if tally := graduateSeverityTally(r.Verdict.Findings); tally != "" {
			b.WriteString(style.Hint.Render(tally) + "   ")
		}
		b.WriteString(style.Hint.Render(style.Key.Render("v")+" view findings") + "\n")
	}
	b.WriteString("\n")
}

// viewFindings renders the expanded last-run verdict as a SCROLLABLE region
// windowed to the modal height, so a long findings list never overflows + clips
// the modal. ↑↓/jk scroll; v/esc collapses back to the mode selector.
func (m *GraduateModal) viewFindings(b *strings.Builder, innerW, height int) {
	r := m.lastRun
	when := time.Unix(r.EndedAt, 0).Format("2006-01-02 15:04")
	b.WriteString(style.Key.Render("Last run: ") +
		style.Hint.Render(when+" · "+r.Mode+" · "+r.Outcome) + "\n\n")

	// Wrap the full findings markdown to the inner width, then window/scroll it.
	wrapped := lipgloss.NewStyle().Width(innerW).Render(strings.TrimRight(graduate.RenderResultMarkdown(*r), "\n"))
	lines := strings.Split(wrapped, "\n")
	// Budget = modal height minus ALL chrome so the box never exceeds `height`
	// even at mid-scroll (where windowLines emits both ↑ and ↓ affordance rows).
	// Chrome = 12: modal border (2) + padding (2) + title+blank (2) + branch+blank
	// (2) + last-run header+blank (2) + blank+footer (2). The region is kept at a
	// CONSTANT `budget` rows so the modal box neither overflows nor visibly
	// grows/shrinks as the ↑/↓ affordances appear/disappear across scroll
	// positions (windowLines emits budget-1 rows at the ends, budget mid-scroll).
	budget := height - 12
	if budget < 3 {
		budget = 3
	}
	win := windowLines(lines, budget, m.findingsScroll)
	for _, ln := range win {
		b.WriteString(ln + "\n")
	}
	// Pad to the constant budget height when windowing is active (more content
	// than fits), so the box size is fixed regardless of the scroll position.
	if len(lines) > budget {
		for i := len(win); i < budget; i++ {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(style.Hint.Render(
		style.Key.Render("↑↓/jk") + " scroll   " + style.Key.Render("v/esc") + " back"))
}

// graduateSeverityTally summarizes findings by severity, e.g. "2 High, 1 Medium".
func graduateSeverityTally(findings []graduate.Finding) string {
	order := []string{"Critical", "High", "Medium", "Low"}
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.Severity]++
	}
	var parts []string
	for _, sev := range order {
		if c := counts[sev]; c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c, sev))
		}
	}
	return strings.Join(parts, ", ")
}

// graduateSeverityStyle colors a finding's severity glyph.
func graduateSeverityStyle(sev string) lipgloss.Style {
	switch sev {
	case "Critical", "High":
		return style.Danger
	case "Medium":
		return style.NeedsAttention
	default: // Low / unknown
		return style.Hint
	}
}

// compile-time sanity: GraduateModal satisfies Modal.
var _ Modal = (*GraduateModal)(nil)
