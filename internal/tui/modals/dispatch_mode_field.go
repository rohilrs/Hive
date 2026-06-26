package modals

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// Shared selector helpers for the project create/edit modals' dispatch-mode
// and advancement-policy controls. dispatch_mode (Phase 1) generalizes the old
// auto_dispatch boolean; the policy selector + target field are only shown when
// the mode is "sequenced". Both modals emit the same wire strings in
// SubmitRequest.Params so the root handler has one code path.

var dispatchModes = []string{"manual", "auto_all", "sequenced"}

var advancementPolicies = []string{"pr_opened", "human_merge", "auto_merge_on_green", "manual"}

// dispatchModeFromString seeds the selector index from a resolved mode string.
func dispatchModeFromString(s string) int {
	for i, v := range dispatchModes {
		if v == s {
			return i
		}
	}
	return 0 // manual (fail-closed, matches the daemon resolver)
}

func dispatchModeChoice(i int) string {
	if i >= 0 && i < len(dispatchModes) {
		return dispatchModes[i]
	}
	return "manual"
}

func policyFromString(s string) int {
	for i, v := range advancementPolicies {
		if v == s {
			return i
		}
	}
	return 0 // pr_opened
}

func policyChoice(i int) string {
	if i >= 0 && i < len(advancementPolicies) {
		return advancementPolicies[i]
	}
	return "pr_opened"
}

// selectorView renders an inline single-choice selector: the chosen label is
// bracketed (with a ▸ prefix when the slot is focused); the others are dimmed.
func selectorView(labels []string, sel int, focused bool) string {
	parts := make([]string, len(labels))
	for i, lbl := range labels {
		if i == sel {
			marker := "[" + lbl + "]"
			if focused {
				parts[i] = style.Key.Render("▸" + marker)
			} else {
				parts[i] = style.Key.Render(marker)
			}
		} else {
			parts[i] = style.DimText.Render(lbl)
		}
	}
	return strings.Join(parts, "  ")
}

// newModalInput builds a standard 40-wide modal text input seeded with a value.
func newModalInput(placeholder, value string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Width = 40
	ti.SetValue(value)
	return ti
}
