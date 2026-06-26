package modals

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// SourcesModal is a multi-state modal for binding / unbinding the three
// task sources (github / linear / inbox) on a project. Workflow:
//
//	[loading] ──RPC sources.list──► [list]
//	  list:
//	    cursor↑↓ navigates the 3 source rows.
//	    enter on a BOUND row    → emit SubmitRequest{Kind:"sources_unbind"}.
//	    enter on UNBOUND inbox  → emit SubmitRequest{Kind:"sources_bind"}
//	                              (no config — daemon auto-creates the dir).
//	    enter on UNBOUND github → state = configGithub  (collect "owner/repo").
//	    enter on UNBOUND linear → state = configLinear  (collect team key).
//	  configGithub:
//	    ctrl+s / enter → emit SubmitRequest{Kind:"sources_bind"}.
//	    esc           → back to list.
//	  configLinear → configLinearProject:
//	    ctrl+s / enter on team → advance to configLinearProject (project filter).
//	    ctrl+s / enter on project (optional, may be blank) → emit
//	                     SubmitRequest{Kind:"sources_bind"} with teams + projects.
//	    esc           → back to list.
//
// After each bind/unbind RPC returns, the modal re-fires sources.list
// (via a sources_list_refresh SubmitRequest) so the rows reflect the
// new daemon state.
//
// Linear note: the daemon's linear binding expects a `teams: []string`,
// not a single string. The modal collects one team key in the input and
// wraps it as `["KEY"]` at submit time. Multi-team binding is a CLI-only
// path today (cmd_sources.go --team flag is repeatable). Binding Linear is a
// three-step flow: configLinear collects the (required) team key, then
// configLinearProject collects an OPTIONAL comma-separated project filter
// (slugId/UUID, daemon-auto-resolved) that narrows ingestion to specific
// Linear projects within the team, then configLinearWriteBack toggles
// write-back (mirror Hive tasks → Linear issues + roadmap → milestones).
// Blank project step = team-only ingest.
//
// Write-back parity with the CLI (`--write-back`): the daemon requires the
// write target to be unambiguous — exactly one team AND exactly one project
// (it auto-closes mirrored issues that fall outside the ingest window). The
// modal always collects a single team, so the only client-side guard is that
// write-back demands exactly one project; the explicit wb-team/wb-project
// disambiguation for multi-team/multi-project binds stays a CLI-only path.
type SourcesModal struct {
	projectSlug     string
	state           string // "loading" | "list" | "configGithub" | "configLinear" | "configLinearProject" | "configLinearWriteBack"
	sources         []sourceRow
	cursor          int
	input           textinput.Model
	pendingTeam     string   // team key stashed between configLinear → configLinearProject
	pendingProjects []string // project filter stashed between configLinearProject → configLinearWriteBack
	writeBack       bool     // write-back toggle state in configLinearWriteBack
	submitting      bool
	errMsg          string
	noticeMsg       string // transient success line (e.g. the sync-now result summary)
}

// sourceRow is one of the three fixed source kinds, with its current
// bind status + (when bound) its binding map for context-rendering.
type sourceRow struct {
	Kind    string // "github" | "linear" | "inbox"
	Bound   bool
	Binding map[string]any // nil when unbound
}

// sourceKinds is the canonical ordered list rendered in the modal. Order
// is stable so cursor index is meaningful across refreshes.
var sourceKinds = []string{"github", "linear", "inbox"}

// NewSourcesModal constructs the modal in "loading" state. The root is
// expected to fire Client.SourcesList(projectSlug) and dispatch the
// response back as RPCResultMsg{Kind:"sources_list"} which the modal
// then parses into the list view.
func NewSourcesModal(projectSlug string) *SourcesModal {
	return &SourcesModal{
		projectSlug: projectSlug,
		state:       "loading",
	}
}

func (m *SourcesModal) Title() string { return "Sources for " + m.projectSlug }

// Init returns nil — root kicks off the initial fetch (see app.go
// case tabs.TabSourcesRequest).
func (m *SourcesModal) Init() tea.Cmd { return nil }

// parseSourcesList expands the daemon's sources.list response into the
// 3-row sourceRow slice. The daemon returns the project's Sources map
// directly (e.g. {"github":{"repo":"x/y"}, "inbox":{}}). A key present
// means bound; absent means unbound.
func parseSourcesList(data map[string]any) []sourceRow {
	rows := make([]sourceRow, 0, len(sourceKinds))
	for _, k := range sourceKinds {
		row := sourceRow{Kind: k}
		if v, ok := data[k]; ok {
			row.Bound = true
			if b, ok := v.(map[string]any); ok {
				row.Binding = b
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func (m *SourcesModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case RPCResultMsg:
		switch msg.Kind {
		case "sources_list":
			if msg.Err != nil {
				m.errMsg = msg.Err.Error()
				return m, nil
			}
			m.sources = parseSourcesList(msg.Data)
			m.state = "list"
			m.submitting = false
			m.errMsg = ""
			// Clamp cursor in case the list shrank (it shouldn't —
			// always 3 entries — but defensive anyway).
			if m.cursor >= len(m.sources) {
				m.cursor = 0
			}
			return m, nil
		case "sources_sync":
			// Sync-now result: stay in the list state and render a one-line
			// summary (inserted/updated/closed summed across sources) or the
			// error. No list refresh — the rows reflect bindings, not task counts.
			m.submitting = false
			if msg.Err != nil {
				m.errMsg = msg.Err.Error()
				return m, nil
			}
			m.errMsg = ""
			m.noticeMsg = summarizeSyncReport(msg.Data)
			return m, nil
		case "sources_bind", "sources_unbind":
			m.submitting = false
			if msg.Err != nil {
				m.errMsg = msg.Err.Error()
				// State is preserved so the operator can retry / esc back.
				return m, nil
			}
			// Success → return to list state and re-fetch so the row
			// flips bound/unbound visually.
			m.state = "list"
			m.errMsg = ""
			slug := m.projectSlug
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind:   "sources_list_refresh",
					Params: map[string]any{"slug": slug},
				}
			}
		}
		return m, nil

	case tea.KeyMsg:
		switch m.state {
		case "list":
			return m.updateList(msg)
		case "configGithub", "configLinear", "configLinearProject", "configLinearWriteBack":
			return m.updateConfig(msg)
		}
	}
	return m, nil
}

// updateList handles key input in the "list" state — navigation + enter
// dispatches to bind / unbind / sub-state transition.
func (m *SourcesModal) updateList(msg tea.KeyMsg) (Modal, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return CloseMsg{} }
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "j":
		if m.cursor < len(m.sources)-1 {
			m.cursor++
		}
		return m, nil
	case "y":
		// Sync-now: pull every bound source into tasks. This is GLOBAL (by
		// source kind, not just this project — mirrors `hive sync`); the result
		// summary renders inline. Disabled while another op is in flight.
		if m.submitting {
			return m, nil
		}
		m.submitting = true
		m.errMsg = ""
		m.noticeMsg = ""
		return m, func() tea.Msg {
			return SubmitRequest{Kind: "sources_sync", Params: map[string]any{}}
		}
	case "enter":
		if m.submitting || m.cursor < 0 || m.cursor >= len(m.sources) {
			return m, nil
		}
		m.noticeMsg = "" // clear any stale sync summary on a new bind/unbind action
		row := m.sources[m.cursor]
		slug := m.projectSlug
		if row.Bound {
			m.submitting = true
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind: "sources_unbind",
					Params: map[string]any{
						"slug":   slug,
						"source": row.Kind,
					},
				}
			}
		}
		// Unbound — kind-dependent action.
		switch row.Kind {
		case "inbox":
			// Inbox has no config — bind immediately.
			m.submitting = true
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind: "sources_bind",
					Params: map[string]any{
						"slug":    slug,
						"source":  "inbox",
						"binding": map[string]any{},
					},
				}
			}
		case "github":
			m.state = "configGithub"
			m.input = newConfigInput("owner/repo")
			m.errMsg = ""
			return m, textinput.Blink
		case "linear":
			m.state = "configLinear"
			m.input = newConfigInput("team key (e.g. HBA)")
			m.errMsg = ""
			return m, textinput.Blink
		}
	}
	return m, nil
}

// updateConfig handles key input in the configGithub / configLinear
// sub-states — single textinput, ctrl+s/enter submits, esc back to list.
func (m *SourcesModal) updateConfig(msg tea.KeyMsg) (Modal, tea.Cmd) {
	// The write-back step is a toggle, not a text field — route it to its own
	// handler so keystrokes don't fall through to the (now-detached) input.
	if m.state == "configLinearWriteBack" {
		return m.updateWriteBack(msg)
	}
	switch msg.String() {
	case "esc":
		m.state = "list"
		m.errMsg = ""
		return m, nil
	case "ctrl+s", "enter":
		value := strings.TrimSpace(m.input.Value())

		// The project-filter step is OPTIONAL: a blank value ingests team-wide.
		// Rather than submit here, stash the (optional) project filter and
		// advance to the write-back toggle. Handle it before the
		// required-value guard below.
		if m.state == "configLinearProject" {
			m.pendingProjects = splitCSV(value)
			m.state = "configLinearWriteBack"
			m.writeBack = false
			m.errMsg = ""
			return m, nil
		}

		if value == "" {
			m.errMsg = "value is required"
			return m, nil
		}
		switch m.state {
		case "configGithub":
			m.submitting = true
			slug := m.projectSlug
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind: "sources_bind",
					Params: map[string]any{
						"slug":    slug,
						"source":  "github",
						"binding": map[string]any{"repo": value},
					},
				}
			}
		case "configLinear":
			// Stash the (required) team key and advance to the optional
			// project-filter step rather than submitting immediately.
			m.pendingTeam = value
			m.state = "configLinearProject"
			m.input = newConfigInput("project id(s), comma-separated — optional, blank = all")
			m.errMsg = ""
			return m, textinput.Blink
		}
	}
	upd, cmd := m.input.Update(msg)
	m.input = upd
	return m, cmd
}

// updateWriteBack handles the configLinearWriteBack toggle step: space/w
// flips write-back, ctrl+s/enter submits the linear bind, esc backs to list.
// On submit it assembles the binding (teams + optional projects + optional
// write_back) and mirrors the daemon's write-target guard client-side so the
// operator gets an immediate, specific error instead of a round-trip reject.
func (m *SourcesModal) updateWriteBack(msg tea.KeyMsg) (Modal, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.state = "list"
		m.errMsg = ""
		return m, nil
	case " ", "w":
		m.writeBack = !m.writeBack
		m.errMsg = ""
		return m, nil
	case "ctrl+s", "enter":
		binding := map[string]any{"teams": []string{m.pendingTeam}}
		if len(m.pendingProjects) > 0 {
			binding["projects"] = m.pendingProjects
		}
		if m.writeBack {
			// The daemon rejects write-back unless the write target is
			// unambiguous: exactly one project (the single team is the target
			// by construction). Guard here so the user fixes it before the RPC.
			if len(m.pendingProjects) != 1 {
				m.errMsg = fmt.Sprintf(
					"write-back needs exactly one project (you have %d) — esc back to set one",
					len(m.pendingProjects))
				return m, nil
			}
			binding["write_back"] = true
		}
		slug := m.projectSlug
		m.submitting = true
		return m, func() tea.Msg {
			return SubmitRequest{
				Kind: "sources_bind",
				Params: map[string]any{
					"slug":    slug,
					"source":  "linear",
					"binding": binding,
				},
			}
		}
	}
	return m, nil
}

// splitCSV splits a comma-separated value into trimmed, non-empty parts.
// Returns nil for an all-blank input (so callers can omit the field entirely).
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newConfigInput is a small constructor so list→config transitions
// always produce a freshly-focused input with consistent width.
func newConfigInput(placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Width = 40
	ti.Focus()
	return ti
}

func (m *SourcesModal) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(m.Title()) + "\n\n")

	switch m.state {
	case "loading":
		b.WriteString(style.Hint.Render("loading sources…") + "\n\n")
	case "list":
		b.WriteString(m.renderList())
	case "configGithub":
		b.WriteString("GitHub repo (owner/name):\n")
		b.WriteString("  " + m.input.View() + "\n\n")
	case "configLinear":
		b.WriteString("Linear team key:\n")
		b.WriteString("  " + m.input.View() + "\n\n")
	case "configLinearProject":
		b.WriteString("Linear project filter for team " + style.Key.Render(m.pendingTeam) + ":\n")
		b.WriteString("  " + m.input.View() + "\n")
		b.WriteString(style.Hint.Render("  optional — comma-separate ids/slugs; blank ingests the whole team") + "\n\n")
	case "configLinearWriteBack":
		box := "[ ]"
		if m.writeBack {
			box = "[x]"
		}
		b.WriteString("Write-back to Linear for team " + style.Key.Render(m.pendingTeam) + ":\n")
		b.WriteString("  " + style.Key.Render(box) + " mirror Hive tasks → Linear issues + roadmap → milestones\n")
		b.WriteString(style.Hint.Render("  needs exactly one project filter; blank/multi = ingest-only") + "\n\n")
	}

	if m.errMsg != "" {
		b.WriteString(style.InlineError.Render(m.errMsg) + "\n\n")
	}
	if m.noticeMsg != "" {
		b.WriteString(style.Done.Render(m.noticeMsg) + "\n\n")
	}
	if m.submitting {
		b.WriteString(style.Hint.Render("submitting…"))
	} else {
		b.WriteString(m.footerFor(m.state))
	}
	return style.Modal.Width(72).Render(b.String())
}

// renderList renders the 3-row list with cursor + bound/unbound state +
// optional binding-summary tail. Each row uses style.CursorMarker so the
// active glyph is consistent with other list-based UIs.
func (m *SourcesModal) renderList() string {
	var b strings.Builder
	for i, row := range m.sources {
		marker := style.CursorMarker(i == m.cursor)
		// Pad the kind name to 8 cells so the status column lines up.
		kind := fmt.Sprintf("%-8s", row.Kind)
		var status, detail string
		if row.Bound {
			status = style.Done.Render("[bound]  ")
			detail = bindingSummary(row.Kind, row.Binding)
		} else {
			status = style.Hint.Render("[unbound]")
		}
		line := marker + kind + " " + status
		if detail != "" {
			line += " " + style.DimText.Render("→ "+detail)
		}
		b.WriteString(line + "\n")
	}
	return b.String() + "\n"
}

// bindingSummary returns a short human-readable description of a bound
// source's binding map — e.g. "owner/name" for github, "HBA, FOO" for
// linear. Empty when no useful summary is available.
func bindingSummary(kind string, binding map[string]any) string {
	if binding == nil {
		return ""
	}
	switch kind {
	case "github":
		if repo, ok := binding["repo"].(string); ok && repo != "" {
			return repo
		}
	case "linear":
		// Wire-shape: teams/projects are []any (json.Unmarshal default for
		// arrays into map[string]any). Join teams, and append a project
		// filter when one is bound: "HBA · projects: Bug Bash".
		teams := joinAnyStrings(binding["teams"])
		if teams == "" {
			return ""
		}
		if projects := joinAnyStrings(binding["projects"]); projects != "" {
			return teams + " · projects: " + projects
		}
		return teams
	case "inbox":
		// Inbox binding is intentionally empty — no useful summary.
		return ""
	}
	return ""
}

// summarizeSyncReport reduces the daemon's SyncReport ({per_source:{<kind>:
// {inserted,updated,closed,error}}}) into a one-line summary for the modal:
// totals across sources plus a count of sources that errored. JSON numbers
// decode as float64. Returns a friendly line for an empty/no-op sync.
func summarizeSyncReport(data map[string]any) string {
	per, _ := data["per_source"].(map[string]any)
	if len(per) == 0 {
		return "sync complete — no bound sources"
	}
	var inserted, updated, closed, errored int
	for _, v := range per {
		res, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if n, ok := res["inserted"].(float64); ok {
			inserted += int(n)
		}
		if n, ok := res["updated"].(float64); ok {
			updated += int(n)
		}
		if n, ok := res["closed"].(float64); ok {
			closed += int(n)
		}
		if e, ok := res["error"].(string); ok && e != "" {
			errored++
		}
	}
	summary := fmt.Sprintf("✓ synced: %d new · %d updated · %d closed", inserted, updated, closed)
	if errored > 0 {
		summary += fmt.Sprintf(" · %d source(s) errored", errored)
	}
	return summary
}

// joinAnyStrings comma-joins the string elements of a JSON-decoded array
// (map[string]any unmarshals arrays as []any). Non-string / nil → "".
func joinAnyStrings(v any) string {
	raw, ok := v.([]any)
	if !ok || len(raw) == 0 {
		return ""
	}
	parts := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// footerFor returns the keybind-hint footer for the given state.
func (m *SourcesModal) footerFor(state string) string {
	switch state {
	case "loading":
		return style.Hint.Render(style.Key.Render("esc") + " close")
	case "list":
		return style.Hint.Render(
			style.Key.Render("↑↓") + " select · " +
				style.Key.Render("enter") + " bind/unbind · " +
				style.Key.Render("y") + " sync now · " +
				style.Key.Render("esc") + " close")
	case "configGithub":
		return style.Hint.Render(
			style.Key.Render("ctrl+s/enter") + " submit · " +
				style.Key.Render("esc") + " back")
	case "configLinear":
		return style.Hint.Render(
			style.Key.Render("ctrl+s/enter") + " next · " +
				style.Key.Render("esc") + " back")
	case "configLinearProject":
		return style.Hint.Render(
			style.Key.Render("ctrl+s/enter") + " next · " +
				style.Key.Render("esc") + " back")
	case "configLinearWriteBack":
		return style.Hint.Render(
			style.Key.Render("space/w") + " toggle · " +
				style.Key.Render("ctrl+s/enter") + " submit · " +
				style.Key.Render("esc") + " back")
	}
	return ""
}
