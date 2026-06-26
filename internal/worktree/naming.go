package worktree

import (
	"strings"
	"unicode"
)

const (
	branchPrefix = "hive"
	maxSlugChars = 40
)

// BranchName returns the canonical hive branch name for a run.
// Format: hive/<runID>[/<slug>]
func BranchName(runID, taskTitleOrSlug string) string {
	slug := slugify(taskTitleOrSlug)
	if slug == "" {
		return branchPrefix + "/" + runID
	}
	return branchPrefix + "/" + runID + "/" + slug
}

// slugify converts arbitrary text into a git-branch-safe slug:
// lowercase ASCII letters/digits, hyphens for separators, no consecutive
// hyphens, no leading/trailing hyphens. Truncated to maxSlugChars without
// breaking a token in the middle (cuts at the last hyphen if one exists).
//
// Non-ASCII runes are dropped entirely (the rune contributes no character
// and no separator) so that downstream consumers only ever see git-safe
// ASCII slugs.
func slugify(s string) string {
	var b strings.Builder
	lastWasHyphen := true
	for _, r := range s {
		switch {
		case r > 127:
			// Skip non-ASCII entirely — no character, no separator.
			continue
		case unicode.IsUpper(r):
			b.WriteRune(unicode.ToLower(r))
			lastWasHyphen = false
		case unicode.IsLower(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastWasHyphen = false
		default:
			if !lastWasHyphen {
				b.WriteRune('-')
			}
			lastWasHyphen = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) <= maxSlugChars {
		return out
	}

	truncated := out[:maxSlugChars]
	if i := strings.LastIndex(truncated, "-"); i > 0 {
		truncated = truncated[:i]
	}
	return truncated
}
