// Package roadmap parses planner-written roadmap markdown into structured
// phases. The format is documented in PlannerSystemPrompt and produced by
// hive_save_roadmap. Tolerant — operator-written edits should still parse.
package roadmap

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Phase is one numbered section of a roadmap. Number is the raw identifier
// after "Phase " ("1", "1a", "2"); Title is everything after the colon up
// to end of heading; Body is the full section text (excluding the heading
// line itself); SpecPaths are markdown link hrefs that look like paths.
type Phase struct {
	Number    string
	Title     string
	Body      string
	SpecPaths []string
}

// Roadmap is the parsed document.
type Roadmap struct {
	Phases []Phase
}

// FindPhase returns the phase whose Number matches (case-sensitive).
func (r *Roadmap) FindPhase(number string) (Phase, bool) {
	for _, p := range r.Phases {
		if p.Number == number {
			return p, true
		}
	}
	return Phase{}, false
}

// phaseHeading matches an H2 phase heading. Both planner-written conventions
// are accepted:
//   - "## Phase <number><sep> <title>"   (e.g. "## Phase 1 — Foundation")
//   - "## <number><sep> <title>"          (e.g. "## 1. Foundation", "## 2a. Capture")
//
// The literal word "Phase" is optional. <number> is digits + optional lowercase
// suffix (1, 1a, 12b). <sep> is a period, colon, em-dash, en-dash, or hyphen.
// Requiring a leading number (after the optional "Phase ") means non-phase H2s
// like "## Progress snapshot" are correctly ignored.
var phaseHeading = regexp.MustCompile(`^##\s+(?:Phase\s+)?(\d+[a-z]?)\s*[.:—–-]\s*(.+?)\s*$`)

// mdLink matches inline markdown links [text](href). We extract href.
var mdLink = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

// Parse the roadmap markdown. Returns an error if no phase headings are
// found — that is, the body has no `## Phase N: ...` lines.
func Parse(b []byte) (*Roadmap, error) {
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)

	var phases []Phase
	var current *Phase
	var bodyBuf bytes.Buffer

	flush := func() {
		if current == nil {
			return
		}
		current.Body = strings.TrimSpace(bodyBuf.String())
		current.SpecPaths = extractSpecPaths(current.Body)
		phases = append(phases, *current)
		current = nil
		bodyBuf.Reset()
	}

	for scanner.Scan() {
		line := scanner.Text()
		if m := phaseHeading.FindStringSubmatch(line); m != nil {
			flush()
			current = &Phase{Number: m[1], Title: m[2]}
			continue
		}
		if current != nil {
			bodyBuf.WriteString(line)
			bodyBuf.WriteByte('\n')
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("roadmap.Parse: scan: %w", err)
	}
	if len(phases) == 0 {
		return nil, fmt.Errorf("roadmap.Parse: no phase headings found (expected H2 like `## Phase 1 — Title`, `## 1. Title`, or `## 2a: Title`)")
	}
	return &Roadmap{Phases: phases}, nil
}

// extractSpecPaths pulls href values out of markdown links whose href
// looks like a file path (ends in .md, doesn't start with http). Used
// to find the "Spec: [...](...)" lines without parsing them strictly —
// any .md link in the phase body counts.
func extractSpecPaths(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mdLink.FindAllStringSubmatch(body, -1) {
		href := m[2]
		if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
			continue
		}
		if !strings.HasSuffix(href, ".md") {
			continue
		}
		if seen[href] {
			continue
		}
		seen[href] = true
		out = append(out, href)
	}
	return out
}
