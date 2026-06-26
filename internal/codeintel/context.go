package codeintel

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Pre-retrieval context bounds — keep the injected block small so it doesn't
// crowd out the spec in the decompose prompt or blow token budgets.
const (
	contextMaxTerms    = 12
	contextHitsPerTerm = 4
	contextMaxHits     = 30
	contextMaxCapsules = 4
	contextMaxBytes    = 8 * 1024
	contextCapsuleLine = 200
	contextSnippetMax  = 300 // max chars of a single search-hit snippet
)

var (
	ctxBacktickRE = regexp.MustCompile("`([^`\n]{2,80})`")
	ctxCamelRE    = regexp.MustCompile(`\b[A-Za-z][a-z0-9]+[A-Z][A-Za-z0-9]+\b`)
	ctxSafeRE     = regexp.MustCompile(`^[\w/.\-]+$`)
)

// extractTerms pulls candidate code identifiers from sourceText: backtick-quoted
// spans and CamelCase tokens, filtered to a git-grep-safe charset, deduped, capped.
// snake_case and ALLCAPS identifiers are only captured when backtick-quoted (the
// CamelCase regex needs a lower→upper transition) — an acceptable v1 gap since
// specs reliably backtick the identifiers that matter.
func extractTerms(sourceText string, max int) []string {
	set := map[string]bool{}
	var terms []string
	add := func(t string) {
		t = strings.TrimSpace(t)
		if len(t) < 3 || len(t) > 80 || set[t] || !ctxSafeRE.MatchString(t) {
			return
		}
		set[t] = true
		terms = append(terms, t)
	}
	for _, m := range ctxBacktickRE.FindAllStringSubmatch(sourceText, -1) {
		add(m[1])
	}
	for _, m := range ctxCamelRE.FindAllString(sourceText, -1) {
		add(m)
	}
	if len(terms) > max {
		terms = terms[:max]
	}
	return terms
}

// BuildContext greps the grounding ref for code identifiers mentioned in
// sourceText (a spec / phase body) and returns a CODEBASE CONTEXT block for the
// decompose prompt: search hits + a few capsules for blast radius. Returns "" if
// g is nil, nothing matches, or grounding fails (degrade-safe).
func BuildContext(ctx context.Context, g *Grounder, sourceText string) string {
	if g == nil {
		return ""
	}
	terms := extractTerms(sourceText, contextMaxTerms)
	if len(terms) == 0 {
		return ""
	}

	type capTarget struct{ file, sym string }
	var hits []Hit
	seen := map[string]bool{}
	capSeen := map[string]bool{}
	var capTargets []capTarget
	for _, term := range terms {
		found, err := g.Search(ctx, term, contextHitsPerTerm)
		if err != nil {
			continue
		}
		for _, h := range found {
			key := h.File + ":" + strconv.Itoa(h.Line)
			if seen[key] {
				continue
			}
			seen[key] = true
			hits = append(hits, h)
			capKey := h.File + "\x00" + term
			if len(capTargets) < contextMaxCapsules && !capSeen[capKey] {
				capSeen[capKey] = true
				capTargets = append(capTargets, capTarget{h.File, term})
			}
			if len(hits) >= contextMaxHits {
				break
			}
		}
		if len(hits) >= contextMaxHits {
			break
		}
	}
	if len(hits) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CODEBASE CONTEXT — what already exists on the target branch (%s). Ground your sub-tasks in this: do NOT propose work that's already implemented, set relevant_files to REAL paths shown here, and account for blast radius.\n\nSearch hits:\n", g.Ref())
	for _, h := range hits {
		if b.Len() > contextMaxBytes {
			break
		}
		// Cap each snippet so one minified/generated line can't blow the block.
		snippet := h.Snippet
		if len(snippet) > contextSnippetMax {
			snippet = snippet[:contextSnippetMax] + "…"
		}
		fmt.Fprintf(&b, "- %s:%d: %s\n", h.File, h.Line, snippet)
	}

	var caps strings.Builder
	for _, t := range capTargets {
		if b.Len()+caps.Len() > contextMaxBytes {
			break
		}
		c, err := g.Capsule(ctx, t.file, t.sym)
		if err != nil || c == nil {
			continue
		}
		callers := truncateLine(c.Callers, contextCapsuleLine)
		callees := truncateLine(c.Callees, contextCapsuleLine)
		if callers == "" && callees == "" {
			continue
		}
		fmt.Fprintf(&caps, "- %s (%s) — callers: %s; callees: %s\n", t.sym, t.file, callers, callees)
	}
	if caps.Len() > 0 {
		b.WriteString("\nSymbol blast radius:\n")
		b.WriteString(caps.String())
	}
	return b.String()
}

// truncateLine flattens s to one line and caps it at max runes.
func truncateLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
