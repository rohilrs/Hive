package modals

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// EditProjectModal lets the operator change a project's name + repo_path +
// per-project dispatch mode + [integration] settings. Slug is immutable (it's
// the linkage key for tasks/runs/sources) so it's shown in the title for
// context but not editable.
//
// Like NewProject, it is a thin wrapper over the shared projectForm
// (slugEditable:false, canSequence:<the project's CanSequence>): the form owns
// all field state, navigation, rendering, and serialization. The modal owns
// only the title, the "edit_project" Submit/RPCResult Kind, the immutable Slug,
// and the name-required validation.
//
// When the project's roadmap/spec gate passes (canSequence:true), the
// dispatch-mode "sequenced" option is selectable and reveals the target-branch
// + advancement-policy fields; Save runs the enable-gate validation daemon-side
// and any gate error is surfaced inline (modal STAYS OPEN). When canSequence is
// false but the project is ALREADY sequenced, the form keeps "sequenced"
// selected (greyed) rather than auto-correcting it.
type EditProjectModal struct {
	Slug string
	form *projectForm
}

// mergeMethods / boolChoices back the integration selectors (shared with the
// projectForm).
var mergeMethods = []string{"merge", "squash", "rebase"}
var boolChoices = []string{"off", "on"}

// statusOptions backs the project-lifecycle selector (edit modal only). Order
// matches the store's status vocabulary; "archived" hides the project from the
// active list (mirrors `hive project archive`).
var statusOptions = []string{"active", "paused", "archived"}

// indexOf returns the position of val in opts, or def when absent/empty.
func indexOf(opts []string, val string, def int) int {
	for i, o := range opts {
		if o == val {
			return i
		}
	}
	return def
}

// boolIdx maps a bool to a boolChoices index (false→0/off, true→1/on).
func boolIdx(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NewEditProjectModal pre-fills the form from the project's current values.
// dispatchMode/policy are resolved strings; target is the current target branch
// (empty → "main"). canSequence comes from the project's CanSequence (gate
// result) — it controls whether "sequenced" is selectable. Submit's Params
// carries dispatch_mode (+ target_branch + policy when sequenced) + the
// [integration] block for the root handler.
func NewEditProjectModal(slug, currentName, currentRepoPath, currentStatus, dispatchMode, target, policy, featureBranch, mergeMethod string, taskAutoIntegrate, autoFixCI, canSequence bool) *EditProjectModal {
	return &EditProjectModal{
		Slug: slug,
		form: newProjectForm(projectFormOpts{
			slugEditable:  false,
			canSequence:   canSequence,
			name:          currentName,
			repo:          currentRepoPath,
			status:        currentStatus,
			showStatus:    true,
			target:        target,
			feature:       featureBranch,
			dispatchMode:  dispatchMode,
			policy:        policy,
			mergeMethod:   mergeMethod,
			autoIntegrate: taskAutoIntegrate,
			autoFixCI:     autoFixCI,
		}),
	}
}

// SetCanSequence re-seeds whether "sequenced" is selectable, so a project.list
// refresh after the modal opened (or a project.updated event) can un-grey the
// option live without reopening.
func (m *EditProjectModal) SetCanSequence(v bool) { m.form.SetCanSequence(v) }

func (m *EditProjectModal) Title() string { return "Edit project " + m.Slug }

func (m *EditProjectModal) Init() tea.Cmd { return textinput.Blink }

func (m *EditProjectModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
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
			// Enter on a selector slot cycles forward; on an input slot it
			// submits (matches the other modals' enter-to-submit convention).
			if m.form.focusedIsSelector() {
				m.form.cycleSelector(1)
				return m, nil
			}
			return m.submit()
		}
	case RPCResultMsg:
		if msg.Kind == "edit_project" {
			m.form.submitting = false
			if msg.Err != nil {
				// Includes the enable-gate error when mode=sequenced — keep the
				// modal open so the operator can fix the roadmap/spec and retry.
				m.form.errMsg = msg.Err.Error()
				return m, nil
			}
			return m, func() tea.Msg { return CloseMsg{} }
		}
	}
	return m, m.form.updateInput(msg)
}

func (m *EditProjectModal) submit() (Modal, tea.Cmd) {
	if m.form.NameValue() == "" {
		m.form.errMsg = "name is required"
		return m, nil
	}
	m.form.submitting = true
	params := m.form.serialize()
	params["slug"] = m.Slug
	return m, func() tea.Msg { return SubmitRequest{Kind: "edit_project", Params: params} }
}

func (m *EditProjectModal) View(width, height int) string {
	return m.form.View("Edit project "+m.Slug, m.form.footer(), 60, height)
}
