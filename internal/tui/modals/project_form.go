package modals

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// projectForm is the shared form backing both the new- and edit-project modals.
// It replaces the old per-modal slot-index arithmetic (integBase(), case 0/1/2…)
// with an ORDERED field-descriptor list (see fields()). The focus ring,
// navigation, render, and param serialization all iterate that list, so adding,
// hiding, or reordering a field can no longer desync the ring — there are no
// hardcoded indices.
//
// The two thin modals embed *projectForm and differ only in construction
// (slugEditable / canSequence / prefill), title, validation, and the
// SubmitRequest/RPCResultMsg Kind they own.
type projectForm struct {
	// Concrete field state.
	slug    textinput.Model
	name    textinput.Model
	repo    textinput.Model
	target  textinput.Model
	feature textinput.Model

	dispatchMode  int // index into dispatchModes (0=manual 1=auto_all 2=sequenced)
	policy        int // index into advancementPolicies
	mergeMethod   int // index into mergeMethods
	autoIntegrate int // index into boolChoices (0=off 1=on)
	autoFixCI     int // index into boolChoices (0=off 1=on)
	status        int // index into statusOptions (0=active 1=paused 2=archived)

	focusIdx   int
	errMsg     string
	submitting bool

	// Config flags fixed at construction.
	slugEditable bool // slug field shown+editable (new); edit shows slug in title
	canSequence  bool // when false, the dispatch-mode "sequenced" option is disabled
	showStatus   bool // status selector shown (edit only; new projects are always active)
}

// fieldKind distinguishes a labelled text input from a labelled single-choice
// selector over a fixed option set.
type fieldKind int

const (
	kindInput fieldKind = iota
	kindSelector
)

// projectField is one ordered row in the form. For kindInput, input points at
// the backing textinput. For kindSelector, options/sel/disabled/asBool describe
// the choice. disabled holds option indices that the focus ring SKIPS on cycle
// and that render greyed; the currently-selected index always renders (greyed if
// disabled) and is never auto-corrected.
type projectField struct {
	label    string
	paramKey string
	kind     fieldKind

	input *textinput.Model // kindInput

	options  []string     // kindSelector
	sel      *int         // kindSelector: index into options
	disabled map[int]bool // kindSelector: disabled option indices (skipped+greyed)
	asBool   bool         // kindSelector: serialize as bool (sel==1) not the option string
}

// projectFormOpts carries construction + prefill values. New uses
// slugEditable:true, canSequence:false; edit uses slugEditable:false plus the
// project's CanSequence. The string fields prefill the inputs; the *Mode/*Policy/
// MergeMethod strings + the two bools seed the selector indices.
type projectFormOpts struct {
	slugEditable bool
	canSequence  bool

	slug          string
	name          string
	repo          string
	target        string
	feature       string
	dispatchMode  string
	policy        string
	mergeMethod   string
	autoIntegrate bool
	autoFixCI     bool
	status        string // current lifecycle status (edit prefill); "" → active
	showStatus    bool   // reveal the status selector (edit only)
}

// newProjectForm builds a projectForm from opts, seeding inputs + selector
// indices and focusing the first visible field.
func newProjectForm(opts projectFormOpts) *projectForm {
	target := opts.target
	if strings.TrimSpace(target) == "" {
		target = "main"
	}
	f := &projectForm{
		slug:          newModalInput("slug (e.g. my-app)", opts.slug),
		name:          newModalInput("Project Name", opts.name),
		repo:          newModalInput("/abs/path/to/repo (optional)", opts.repo),
		target:        newModalInput("target branch", target),
		feature:       newModalInput("feature branch (optional)", opts.feature),
		dispatchMode:  dispatchModeFromString(opts.dispatchMode),
		policy:        policyFromString(opts.policy),
		mergeMethod:   indexOf(mergeMethods, opts.mergeMethod, 0),
		autoIntegrate: boolIdx(opts.autoIntegrate),
		autoFixCI:     boolIdx(opts.autoFixCI),
		status:        indexOf(statusOptions, opts.status, 0),
		slugEditable:  opts.slugEditable,
		canSequence:   opts.canSequence,
		showStatus:    opts.showStatus,
	}
	// NOTE: dispatchMode is NOT auto-corrected even if it points at the disabled
	// "sequenced" option (already-sequenced project whose roadmap vanished). It
	// stays selected (greyed); the operator can cycle away but not back.
	f.focusCurrent()
	return f
}

// sequenced reports whether the dispatch mode is sequenced (reveals target+policy).
func (f *projectForm) sequenced() bool { return dispatchModeChoice(f.dispatchMode) == "sequenced" }

// SetCanSequence updates whether the "sequenced" dispatch mode is selectable.
// Re-seeded live (e.g. after a project.list refresh on modal open) so a roadmap
// created while the modal was already open un-greys the option. The current
// dispatch-mode selection is never auto-corrected — flipping the flag only
// changes which options the focus ring skips and how they render (see fields()).
func (f *projectForm) SetCanSequence(v bool) { f.canSequence = v }

// dispatchModeDisabled returns the disabled-option map for the dispatch-mode
// selector: "sequenced" is disabled when !canSequence. nil when nothing disabled.
func (f *projectForm) dispatchModeDisabled() map[int]bool {
	if f.canSequence {
		return nil
	}
	seqIdx := dispatchModeFromString("sequenced")
	return map[int]bool{seqIdx: true}
}

// fields returns the ordered, currently-VISIBLE field descriptors. Order:
// slug (only if slugEditable), name, repo, dispatch-mode, target+policy (only if
// sequenced), feature_branch, merge_method, task_auto_integrate, auto_fix_ci.
func (f *projectForm) fields() []projectField {
	var fs []projectField
	if f.slugEditable {
		fs = append(fs, projectField{label: "Slug:", paramKey: "slug", kind: kindInput, input: &f.slug})
	}
	fs = append(fs,
		projectField{label: "Name:", paramKey: "name", kind: kindInput, input: &f.name},
		projectField{label: "Repo path:", paramKey: "repo_path", kind: kindInput, input: &f.repo},
	)
	// Lifecycle status (edit only) — active/paused/archived. Sits between the
	// identity fields and the operational block; new projects are always active
	// so the selector is hidden there.
	if f.showStatus {
		fs = append(fs,
			projectField{label: "Status:", paramKey: "status", kind: kindSelector, options: statusOptions, sel: &f.status},
		)
	}
	fs = append(fs,
		projectField{
			label: "Dispatch mode:", paramKey: "dispatch_mode", kind: kindSelector,
			options: dispatchModes, sel: &f.dispatchMode, disabled: f.dispatchModeDisabled(),
		},
	)
	// Advancement policy is sequenced-only (it governs how sequenced tasks
	// advance through the gate). Target branch is NOT sequenced-only — it's the
	// project's base for grounding/health checks AND the worktree-fork-base
	// fallback when no feature branch is set — so it's always shown (below, with
	// the other branch settings).
	if f.sequenced() {
		fs = append(fs,
			projectField{label: "Advancement policy:", paramKey: "policy", kind: kindSelector, options: advancementPolicies, sel: &f.policy},
		)
	}
	fs = append(fs,
		projectField{label: "Feature branch (integration):", paramKey: "feature_branch", kind: kindInput, input: &f.feature},
		projectField{label: "Target branch (base):", paramKey: "target_branch", kind: kindInput, input: &f.target},
		projectField{label: "Merge method:", paramKey: "merge_method", kind: kindSelector, options: mergeMethods, sel: &f.mergeMethod},
		projectField{label: "Auto-integrate tasks:", paramKey: "task_auto_integrate", kind: kindSelector, options: boolChoices, sel: &f.autoIntegrate, asBool: true},
		projectField{label: "Auto-fix CI:", paramKey: "auto_fix_ci", kind: kindSelector, options: boolChoices, sel: &f.autoFixCI, asBool: true},
	)
	return fs
}

// focused returns the descriptor at the current focus index, clamping to keep
// it valid if the visible set shrank (e.g. sequenced→manual hid two fields).
func (f *projectForm) focused() projectField {
	fs := f.fields()
	if f.focusIdx >= len(fs) {
		f.focusIdx = len(fs) - 1
	}
	if f.focusIdx < 0 {
		f.focusIdx = 0
	}
	return fs[f.focusIdx]
}

// focusedIsInput reports whether the focused field is a text input.
func (f *projectForm) focusedIsInput() bool { return f.focused().kind == kindInput }

// focusedIsSelector reports whether the focused field is a selector.
func (f *projectForm) focusedIsSelector() bool { return f.focused().kind == kindSelector }

// blurFocused blurs the focused input (no-op for selectors).
func (f *projectForm) blurFocused() {
	if fld := f.focused(); fld.kind == kindInput {
		fld.input.Blur()
	}
}

// focusCurrent focuses the focused input and returns the blink cmd (nil for
// selectors).
func (f *projectForm) focusCurrent() tea.Cmd {
	if fld := f.focused(); fld.kind == kindInput {
		fld.input.Focus()
		return textinput.Blink
	}
	return nil
}

// focusNext / focusPrev move the focus ring by one over the visible fields.
func (f *projectForm) focusNext() tea.Cmd {
	n := len(f.fields())
	f.blurFocused()
	f.focusIdx = (f.focusIdx + 1) % n
	return f.focusCurrent()
}

func (f *projectForm) focusPrev() tea.Cmd {
	n := len(f.fields())
	f.blurFocused()
	f.focusIdx = (f.focusIdx - 1 + n) % n
	return f.focusCurrent()
}

// cycleSelector advances (dir=+1) or rewinds (dir=-1) the focused selector,
// SKIPPING disabled option indices — so you can move OFF a disabled option but
// never ONTO one. The currently-selected index is never auto-corrected; if it
// starts on a disabled option, the first cycle moves to the next ENABLED option
// in the given direction. No-op when the focused field isn't a selector.
func (f *projectForm) cycleSelector(dir int) {
	fld := f.focused()
	if fld.kind != kindSelector {
		return
	}
	n := len(fld.options)
	if n == 0 {
		return
	}
	next := *fld.sel
	for i := 0; i < n; i++ {
		next = (next + dir + n) % n
		if !fld.disabled[next] {
			*fld.sel = next
			return
		}
	}
	// All other options disabled — leave the selection unchanged.
}

// updateInput forwards a message to the focused input (no-op for selectors).
func (f *projectForm) updateInput(msg tea.Msg) tea.Cmd {
	fld := f.focused()
	if fld.kind != kindInput {
		return nil
	}
	upd, cmd := fld.input.Update(msg)
	*fld.input = upd
	return cmd
}

// selectorValue renders a selector's inline value, greying disabled options
// (including the selected one if it is disabled). Reuses selectorView for the
// common no-disabled case so styling stays consistent.
func (f *projectForm) selectorValue(fld projectField, focused bool) string {
	if len(fld.disabled) == 0 {
		return selectorView(fld.options, *fld.sel, focused)
	}
	parts := make([]string, len(fld.options))
	for i, lbl := range fld.options {
		switch {
		case fld.disabled[i] && i == *fld.sel:
			// Selected but disabled: show bracketed yet greyed.
			parts[i] = style.DimText.Render("[" + lbl + "]")
		case fld.disabled[i]:
			parts[i] = style.DimText.Render(lbl)
		case i == *fld.sel:
			marker := "[" + lbl + "]"
			if focused {
				parts[i] = style.Key.Render("▸" + marker)
			} else {
				parts[i] = style.Key.Render(marker)
			}
		default:
			parts[i] = style.DimText.Render(lbl)
		}
	}
	return strings.Join(parts, "  ")
}

// View renders the form body (title + windowed fields + footer) via the shared
// framedModalBody helper. Each visible field becomes one formField; selectors
// with disabled options grey those options in their value string.
func (f *projectForm) View(title, footer string, width, height int) string {
	fs := f.fields()
	formFields := make([]formField, 0, len(fs))
	for i, fld := range fs {
		var val string
		switch fld.kind {
		case kindInput:
			val = fld.input.View()
		case kindSelector:
			val = f.selectorValue(fld, f.focusIdx == i)
		}
		formFields = append(formFields, formField{slot: i, label: fld.label, value: val})
	}
	fieldLines, focusedLine := buildFieldLines(formFields, f.focusIdx)
	return framedModalBody(style.ModalTitle.Render(title), fieldLines, focusedLine, footer, width, height)
}

// footer is the pinned-bottom region: the inline error (when present) above the
// key-hint / submitting line. Mirrors the existing modals' footer.
func (f *projectForm) footer() string {
	var hint string
	if f.submitting {
		hint = style.Hint.Render("submitting…")
	} else {
		hint = style.Hint.Render(
			style.Key.Render("ctrl+s") + " submit · " +
				style.Key.Render("tab") + " next · " +
				style.Key.Render("←/→") + " cycle · " +
				style.Key.Render("esc") + " cancel")
	}
	// When "sequenced" is greyed on an existing project (edit modal, gate not yet
	// satisfied), explain why + how to fix it so the disabled option isn't a
	// silent dead end. Suppressed for new projects (slugEditable), where the
	// option is expected to be unavailable until the project exists + is planned.
	var lead string
	if !f.canSequence && !f.slugEditable {
		lead = style.DimText.Render("sequenced needs a roadmap + active-phase spec — run `hive plan` first") + "\n\n"
	}
	if f.errMsg != "" {
		return lead + style.InlineError.Render(f.errMsg) + "\n\n" + hint
	}
	return lead + hint
}

// SlugValue / NameValue expose the trimmed input values the modals validate.
func (f *projectForm) SlugValue() string { return strings.TrimSpace(f.slug.Value()) }
func (f *projectForm) NameValue() string { return strings.TrimSpace(f.name.Value()) }

// serialize produces the SubmitRequest params: every visible/applicable field by
// its param key. Integration params + target_branch ALWAYS serialize (so
// create+edit both round-trip); policy only when sequenced; slug only when
// slugEditable; bool selectors serialize as bool (sel==1).
func (f *projectForm) serialize() map[string]any {
	params := map[string]any{
		"name":          f.NameValue(),
		"repo_path":     strings.TrimSpace(f.repo.Value()),
		"dispatch_mode": dispatchModeChoice(f.dispatchMode),
		"target_branch": strings.TrimSpace(f.target.Value()),
		// [integration] — ALWAYS sent so it round-trips on save.
		"feature_branch":      strings.TrimSpace(f.feature.Value()),
		"merge_method":        mergeMethods[f.mergeMethod],
		"task_auto_integrate": f.autoIntegrate == 1,
		"auto_fix_ci":         f.autoFixCI == 1,
	}
	if f.slugEditable {
		params["slug"] = f.SlugValue()
	}
	if f.showStatus {
		params["status"] = statusOptions[f.status]
	}
	if f.sequenced() {
		params["policy"] = policyChoice(f.policy)
	}
	return params
}
