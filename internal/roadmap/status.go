package roadmap

import (
	"regexp"
	"strings"
)

// statusLine matches a phase-body "Status:" line, tolerant of bold/italic
// markers in the common forms: "**Status:**", "*Status:*", "Status:".
// The colon is mandatory and positioned right after "status" (or its closing
// bold marker), so prose like "Status of the migration:" does NOT match.
// Group 1 is the full label prefix (preserved on rewrite).
//
// Supported forms and their group-1 capture:
//
//	**Status:** <value>   →  "**Status:** "
//	*Status:* <value>     →  "*Status:* "
//	Status: <value>       →  "Status: "
var statusLine = regexp.MustCompile(`(?i)^(\s*(\*{1,2})?\s*status\s*:\s*(\*{1,2})?\s*).*$`)

// doneMarker / notDoneMarker classify a Status line's value. A value is "done"
// if it has a ✅ or the word done/complete AND no negation; a negation
// (not started / not done / incomplete) wins regardless.
var doneMarker = regexp.MustCompile(`(?i)\b(done|completed?)\b`)
var notDoneMarker = regexp.MustCompile(`(?i)not\s+(started|done|complete)|\bincomplete\b`)

// PhaseStatusIsDone reports whether the named phase's Status line already
// indicates completion. Used by the reconciler to avoid clobbering an existing
// done line. Returns false when the phase or its Status line is absent.
func PhaseStatusIsDone(markdown, phase string) bool {
	lines := strings.Split(markdown, "\n")
	start := -1
	for i, ln := range lines {
		if m := phaseHeading.FindStringSubmatch(ln); m != nil && m[1] == phase {
			start = i
			break
		}
	}
	if start < 0 {
		return false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if phaseHeading.MatchString(lines[i]) {
			end = i
			break
		}
	}
	for i := start + 1; i < end; i++ {
		if m := statusLine.FindStringSubmatch(lines[i]); m != nil {
			value := lines[i][len(m[1]):] // text after the "Status:" label
			if notDoneMarker.MatchString(value) {
				return false
			}
			return strings.Contains(value, "✅") || doneMarker.MatchString(value)
		}
	}
	return false
}

// SetPhaseStatus rewrites the `**Status:**` line of the phase whose number is
// `phase` to `status`, returning the new markdown and whether anything changed.
// If the phase has no Status line, one is inserted as the first body line. If
// the phase heading is not found, the markdown is returned unchanged (false).
func SetPhaseStatus(markdown, phase, status string) (string, bool) {
	lines := strings.Split(markdown, "\n")
	start := -1
	for i, ln := range lines {
		if m := phaseHeading.FindStringSubmatch(ln); m != nil && m[1] == phase {
			start = i
			break
		}
	}
	if start < 0 {
		return markdown, false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if phaseHeading.MatchString(lines[i]) {
			end = i
			break
		}
	}
	for i := start + 1; i < end; i++ {
		if m := statusLine.FindStringSubmatch(lines[i]); m != nil {
			newLine := m[1] + status
			// Compare against the trailing-whitespace-trimmed stored line so a
			// stray trailing space doesn't count as a content change.
			if newLine == strings.TrimRight(lines[i], " \t") {
				return markdown, false // already the target value — no write
			}
			lines[i] = newLine
			return strings.Join(lines, "\n"), true
		}
	}
	insertAt := start + 1
	newLine := "**Status:** " + status
	out := append([]string{}, lines[:insertAt]...)
	if insertAt >= len(lines) || lines[insertAt] != "" {
		out = append(out, "") // ensure a blank separator after the heading
	}
	out = append(out, newLine)
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\n"), true
}
