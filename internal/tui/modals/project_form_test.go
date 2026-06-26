package modals

import (
	"strings"
	"testing"
)

// TestProjectFormFooterHintForGreyedSequenced verifies the footer explains why
// "sequenced" is greyed ONLY for an existing project (edit modal) whose gate has
// not yet passed — never for a new project (where unavailability is expected) and
// never once the gate passes.
func TestProjectFormFooterHintForGreyedSequenced(t *testing.T) {
	const needle = "hive plan"

	editGated := newProjectForm(projectFormOpts{slugEditable: false, canSequence: false})
	if !strings.Contains(editGated.footer(), needle) {
		t.Error("edit form with failing gate should show the `hive plan` hint")
	}

	editOK := newProjectForm(projectFormOpts{slugEditable: false, canSequence: true})
	if strings.Contains(editOK.footer(), needle) {
		t.Error("edit form with passing gate must NOT show the hint")
	}

	newForm := newProjectForm(projectFormOpts{slugEditable: true, canSequence: false})
	if strings.Contains(newForm.footer(), needle) {
		t.Error("new-project form must NOT show the hint (sequenced unavailable is expected)")
	}
}

// TestProjectFormSetCanSequenceUngreys verifies SetCanSequence(true) makes the
// previously-disabled "sequenced" option selectable (the focus ring lands on it).
func TestProjectFormSetCanSequenceUngreys(t *testing.T) {
	f := newProjectForm(projectFormOpts{slugEditable: false, canSequence: false})
	if f.dispatchModeDisabled() == nil {
		t.Fatal("precondition: sequenced should start disabled")
	}
	f.SetCanSequence(true)
	if f.dispatchModeDisabled() != nil {
		t.Fatal("SetCanSequence(true) should clear the disabled map")
	}
	focusDispatchMode(t, f)
	landed := false
	for i := 0; i < len(dispatchModes)+1; i++ {
		if dispatchModeChoice(f.dispatchMode) == "sequenced" {
			landed = true
			break
		}
		f.cycleSelector(1)
	}
	if !landed {
		t.Error("after SetCanSequence(true), cycling must be able to land on sequenced")
	}
}

// focusDispatchMode moves the focus ring to the dispatch-mode field for tests.
func focusDispatchMode(t *testing.T, f *projectForm) {
	t.Helper()
	for i := 0; i < len(f.fields()); i++ {
		if f.focused().paramKey == "dispatch_mode" {
			return
		}
		f.focusNext()
	}
	t.Fatal("dispatch_mode field not found in ring")
}

// TestProjectFormDisabledSequencedSkipped verifies that, in a NEW-style form
// (canSequence=false), cycling the dispatch-mode selector never lands on the
// disabled "sequenced" option.
func TestProjectFormDisabledSequencedSkipped(t *testing.T) {
	f := newProjectForm(projectFormOpts{slugEditable: true, canSequence: false})
	f.name.SetValue("My App")
	f.slug.SetValue("my-app")

	focusDispatchMode(t, f)
	seenSequenced := false
	for i := 0; i < len(dispatchModes)+2; i++ {
		if dispatchModeChoice(f.dispatchMode) == "sequenced" {
			seenSequenced = true
		}
		f.cycleSelector(1)
	}
	if seenSequenced {
		t.Error("cycling must skip the disabled sequenced option in a new form")
	}

	// Reverse direction must also skip it.
	for i := 0; i < len(dispatchModes)+2; i++ {
		if dispatchModeChoice(f.dispatchMode) == "sequenced" {
			seenSequenced = true
		}
		f.cycleSelector(-1)
	}
	if seenSequenced {
		t.Error("cycling backward must also skip the disabled sequenced option")
	}
}

// TestProjectFormKeepsAlreadySequenced verifies that a form constructed with
// dispatchMode=sequenced + canSequence=false (already-sequenced project whose
// roadmap vanished) KEEPS sequenced selected — the current index is never
// auto-corrected.
func TestProjectFormKeepsAlreadySequenced(t *testing.T) {
	f := newProjectForm(projectFormOpts{
		slugEditable: false,
		canSequence:  false,
		dispatchMode: "sequenced",
	})
	if got := dispatchModeChoice(f.dispatchMode); got != "sequenced" {
		t.Fatalf("construction auto-corrected dispatch mode to %q; want sequenced kept", got)
	}
	// The sequenced target/policy fields are therefore visible.
	if !f.sequenced() {
		t.Error("an already-sequenced form should reveal target/policy fields")
	}

	// Cycling forward off the disabled-selected option moves to an ENABLED option.
	focusDispatchMode(t, f)
	f.cycleSelector(1)
	if got := dispatchModeChoice(f.dispatchMode); got == "sequenced" {
		t.Error("cycling off sequenced should land on an enabled option")
	}
	// And cannot cycle back ONTO sequenced.
	for i := 0; i < len(dispatchModes)+2; i++ {
		f.cycleSelector(1)
		if dispatchModeChoice(f.dispatchMode) == "sequenced" {
			t.Fatal("cycling must not return to the disabled sequenced option")
		}
	}
}

// TestProjectFormSerialize verifies serialize() emits the right params:
// integration always, target/policy only when sequenced, slug only when
// slugEditable, bools as bool.
func TestProjectFormSerialize(t *testing.T) {
	// New form, manual: slug present, no target/policy, integration present.
	f := newProjectForm(projectFormOpts{slugEditable: true, canSequence: false})
	f.slug.SetValue("my-app")
	f.name.SetValue("My App")
	f.repo.SetValue("/repo")
	p := f.serialize()

	if p["slug"] != "my-app" || p["name"] != "My App" || p["repo_path"] != "/repo" {
		t.Errorf("serialize slug/name/repo wrong: %v", p)
	}
	if p["dispatch_mode"] != "manual" {
		t.Errorf("dispatch_mode = %v, want manual", p["dispatch_mode"])
	}
	// target_branch + integration params always serialize (target_branch is the
	// [scheduler] base, not sequenced-only).
	for _, k := range []string{"target_branch", "feature_branch", "merge_method", "task_auto_integrate", "auto_fix_ci"} {
		if _, ok := p[k]; !ok {
			t.Errorf("param %q must always serialize: %v", k, p)
		}
	}
	if _, ok := p["policy"]; ok {
		t.Error("policy must NOT serialize when not sequenced")
	}
	// Bool selectors serialize as bool.
	if _, ok := p["task_auto_integrate"].(bool); !ok {
		t.Errorf("task_auto_integrate must be a bool, got %T", p["task_auto_integrate"])
	}
	if _, ok := p["auto_fix_ci"].(bool); !ok {
		t.Errorf("auto_fix_ci must be a bool, got %T", p["auto_fix_ci"])
	}

	// Sequenced form: target/policy present; slug absent when !slugEditable.
	fs := newProjectForm(projectFormOpts{
		slugEditable: false,
		canSequence:  true,
		dispatchMode: "sequenced",
		target:       "release",
		policy:       "human_merge",
	})
	ps := fs.serialize()
	if _, ok := ps["slug"]; ok {
		t.Error("slug must NOT serialize when !slugEditable")
	}
	if ps["target_branch"] != "release" {
		t.Errorf("target_branch = %v, want release", ps["target_branch"])
	}
	if ps["policy"] != "human_merge" {
		t.Errorf("policy = %v, want human_merge", ps["policy"])
	}
}

// TestProjectFormSerializeBoolValues verifies the on/off selectors serialize the
// actual selected bool value.
func TestProjectFormSerializeBoolValues(t *testing.T) {
	f := newProjectForm(projectFormOpts{
		slugEditable:  true,
		autoIntegrate: true,
		autoFixCI:     false,
	})
	p := f.serialize()
	if p["task_auto_integrate"] != true {
		t.Errorf("task_auto_integrate = %v, want true", p["task_auto_integrate"])
	}
	if p["auto_fix_ci"] != false {
		t.Errorf("auto_fix_ci = %v, want false", p["auto_fix_ci"])
	}
}

// TestProjectFormFocusRingLength verifies the visible field count for the
// sequenced vs non-sequenced rings, and that slug toggles the count.
func TestProjectFormFocusRingLength(t *testing.T) {
	// New (slug editable), manual: slug,name,repo,dispatch + 5 (feature,target,
	// merge,auto-integrate,auto-fix) = 9. (target_branch is always shown now.)
	newManual := newProjectForm(projectFormOpts{slugEditable: true, canSequence: false})
	if got := len(newManual.fields()); got != 9 {
		t.Errorf("new+manual ring length = %d, want 9", got)
	}

	// Edit (no slug), manual: name,repo,dispatch + 5 = 8.
	editManual := newProjectForm(projectFormOpts{slugEditable: false, canSequence: true})
	if got := len(editManual.fields()); got != 8 {
		t.Errorf("edit+manual ring length = %d, want 8", got)
	}

	// Edit, sequenced: adds the policy selector → 9 (target is already always-on).
	editSeq := newProjectForm(projectFormOpts{slugEditable: false, canSequence: true, dispatchMode: "sequenced"})
	if got := len(editSeq.fields()); got != 9 {
		t.Errorf("edit+sequenced ring length = %d, want 9", got)
	}
}

// TestProjectFormFocusNextWraps verifies the focus ring wraps modulo the visible
// field count and that focusCurrent/blur don't panic across kinds.
func TestProjectFormFocusNextWraps(t *testing.T) {
	f := newProjectForm(projectFormOpts{slugEditable: true})
	n := len(f.fields())
	start := f.focusIdx
	for i := 0; i < n; i++ {
		f.focusNext()
	}
	if f.focusIdx != start {
		t.Errorf("focus ring did not wrap: after %d focusNext got %d, want %d", n, f.focusIdx, start)
	}
}
