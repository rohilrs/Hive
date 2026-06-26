package tabs

import (
	"os"
	"regexp"
	"strings"
)

// elideID shortens a long Hive typed identifier like "proj-1779613417850679036"
// or "task-1780030878884954185" to a recognizable short form. Pattern: keep
// the prefix (everything up to and including the dash), then 4 chars,
// ellipsis, last 4 chars. Short IDs (<=16 chars) pass through unchanged.
// Strings that don't look like Hive typed IDs (no known prefix) pass through
// unchanged regardless of length — this avoids mangling paths or other strings.
//
// Examples:
//
//	"task-1780030878884954185" -> "task-1780…4185"
//	"proj-1779613417850679036" -> "proj-1779…9036"
//	"hive", "p1", "p-hive-smoke"  -> unchanged (<=16 chars)
//	"/mnt/e/Documents/Hive/hive"  -> unchanged (not a typed ID)
func elideID(id string) string {
	if len(id) <= 16 {
		return id
	}
	// Only elide strings that start with a known Hive ID prefix.
	// This prevents mangling filesystem paths or other long non-ID strings.
	if !looksLikeTypedID(id) {
		return id
	}
	dashIdx := strings.Index(id, "-")
	if dashIdx < 0 {
		return id
	}
	// prefix includes the dash: "task-", "proj-", "run-", "p-"
	prefix := id[:dashIdx+1]
	suffix := id[dashIdx+1:]
	sr := []rune(suffix)
	if len(sr) <= 8 {
		// Numeric part is already short — return as-is.
		return id
	}
	return prefix + string(sr[:4]) + "…" + string(sr[len(sr)-4:])
}

// elidePath shortens a long filesystem path. Substitutes $HOME with ~
// when applicable; if still >32 chars, shows first segment + /…/ + last 2
// segments. "/mnt/e/Documents/Hive/hive" (26 chars) stays under 32 so
// it passes through unchanged.
//
// Examples:
//
//	"/mnt/e/Documents/Hive/hive"         -> "/mnt/e/Documents/Hive/hive"   (26 chars, unchanged)
//	"/very/long/many/segment/path/foo/bar" -> "/very/…/foo/bar"
func elidePath(p string) string {
	home := os.Getenv("HOME")
	if home != "" && strings.HasPrefix(p, home) {
		p = "~" + p[len(home):]
	}
	if len(p) <= 32 {
		return p
	}
	// Split on "/" and reconstruct first-segment/…/last-two-segments.
	// For an absolute path the first element after split is "".
	parts := strings.Split(p, "/")
	if len(parts) < 4 {
		return p
	}
	// parts[0] is "" for absolute paths; parts[1] is the first real segment.
	first := "/" + parts[1]
	last2 := strings.Join(parts[len(parts)-2:], "/")
	return first + "/…/" + last2
}

// stripCellBackticks removes surrounding backtick spans from a cell value,
// e.g. "`hive`" → "hive". Single backtick-quoted values only; code fences
// inside cells are not a realistic concern.
func stripCellBackticks(s string) string {
	if strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`") && len(s) >= 2 {
		return s[1 : len(s)-1]
	}
	return s
}

// parseTableRow splits a GFM pipe-delimited row into trimmed cell strings.
// Leading/trailing pipes are removed; each cell has its surrounding
// whitespace stripped and backtick-spans unwrapped.
func parseTableRow(line string) []string {
	t := strings.Trim(strings.TrimSpace(line), "|")
	parts := strings.Split(t, "|")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = stripCellBackticks(strings.TrimSpace(p))
	}
	return out
}

// flattenTableBlock converts a GFM table block (header + separator + data
// rows) into stacked markdown blocks per the chat output rendering spec.
// Three shapes are supported based on the header columns:
//
//   - Task table (header has Status + Priority + Pipeline): single-line items
//     "**<title>**  ·  <pipeline>  ·  <priority>  ·  <status>" with an
//     optional " (in <slug>)" suffix when a Project column is present. No
//     detail line. No task ID.
//   - Project table (header has Slug + Repo or Slug + Path): the repo path
//     joins the headline — "**<name>**  ·  <slug>  ·  ~/path". The detail
//     line (if any non-ID, non-path column has a value) joins task counts.
//   - Generic table: the legacy name+handle+detail shape.
func flattenTableBlock(block []string) []string {
	if len(block) < 3 {
		// Need at minimum header + separator + 1 data row.
		return block
	}

	headers := parseTableRow(block[0])
	nCols := len(headers)

	// Parse data rows (skip index 1 = separator row).
	var dataRows [][]string
	for _, line := range block[2:] {
		row := parseTableRow(line)
		// Pad short rows to nCols.
		for len(row) < nCols {
			row = append(row, "")
		}
		dataRows = append(dataRows, row)
	}
	if len(dataRows) == 0 {
		return nil
	}

	// Header-shape classification. Look up known column indices once.
	colByName := map[string]int{}
	for i, h := range headers {
		colByName[strings.ToLower(h)] = i
	}
	hasCol := func(names ...string) bool {
		for _, n := range names {
			if _, ok := colByName[strings.ToLower(n)]; !ok {
				return false
			}
		}
		return true
	}
	anyCol := func(names ...string) bool {
		for _, n := range names {
			if _, ok := colByName[strings.ToLower(n)]; ok {
				return true
			}
		}
		return false
	}

	switch {
	case hasCol("status", "priority", "pipeline"):
		return flattenTaskTable(headers, colByName, dataRows)
	case hasCol("slug") && anyCol("repo", "path"):
		return flattenProjectTable(headers, colByName, dataRows)
	default:
		return flattenGenericTable(headers, dataRows)
	}
}

// flattenTaskTable emits single-line task items, no detail line, no ID.
// Format: "**<title>**  ·  <pipeline>  ·  <priority>  ·  <status>" with
// " (in <slug>)" appended when a Project column is present.
func flattenTaskTable(headers []string, colByName map[string]int, dataRows [][]string) []string {
	titleIdx, ok := colByName["title"]
	if !ok {
		// Fall back to "name" or column 0 if no Title — keeps misformed tables visible.
		if i, hit := colByName["name"]; hit {
			titleIdx = i
		} else {
			titleIdx = 0
		}
	}
	pipelineIdx := colByName["pipeline"]
	priorityIdx := colByName["priority"]
	statusIdx := colByName["status"]
	projectIdx, hasProject := colByName["project"]

	cell := func(row []string, i int) string {
		if i >= 0 && i < len(row) {
			return row[i]
		}
		return ""
	}

	var out []string
	for ri, row := range dataRows {
		if ri > 0 {
			out = append(out, "")
		}
		title := cell(row, titleIdx)
		pipeline := cell(row, pipelineIdx)
		priority := cell(row, priorityIdx)
		status := cell(row, statusIdx)

		headline := "**" + title + "**"
		// Build the tail: pipeline · priority · status (only non-empty).
		var tail []string
		if pipeline != "" {
			tail = append(tail, pipeline)
		}
		if priority != "" {
			tail = append(tail, priority)
		}
		if status != "" {
			tail = append(tail, status)
		}
		if len(tail) > 0 {
			headline += "  ·  " + strings.Join(tail, "  ·  ")
		}
		if hasProject {
			if proj := cell(row, projectIdx); proj != "" {
				headline += "  (in " + proj + ")"
			}
		}
		out = append(out, headline)
	}
	return out
}

// flattenProjectTable emits "**<name>**  ·  <slug>  ·  ~/path" headlines
// with the path elided. Any remaining non-id, non-path columns (e.g. a
// task_counts column produced by the tool) become the detail line.
func flattenProjectTable(headers []string, colByName map[string]int, dataRows [][]string) []string {
	nameIdx, ok := colByName["name"]
	if !ok {
		if i, hit := colByName["title"]; hit {
			nameIdx = i
		} else {
			nameIdx = 0
		}
	}
	slugIdx := colByName["slug"]
	pathIdx, hasPath := colByName["path"]
	if !hasPath {
		pathIdx, hasPath = colByName["repo"]
	}

	cell := func(row []string, i int) string {
		if i >= 0 && i < len(row) {
			return row[i]
		}
		return ""
	}

	// Detail columns: everything we didn't use AND isn't an ID-looking column.
	used := map[int]bool{nameIdx: true, slugIdx: true}
	if hasPath {
		used[pathIdx] = true
	}
	var detailIdxs []int
	for i, h := range headers {
		if used[i] {
			continue
		}
		if strings.EqualFold(h, "id") || looksLikeIDHeader(h) {
			// IDs are noise — slug is already the handle.
			continue
		}
		detailIdxs = append(detailIdxs, i)
	}

	var out []string
	for ri, row := range dataRows {
		if ri > 0 {
			out = append(out, "")
		}
		name := cell(row, nameIdx)
		slug := cell(row, slugIdx)
		path := ""
		if hasPath {
			path = elidePath(cell(row, pathIdx))
		}

		headline := "**" + name + "**"
		if slug != "" {
			headline += "  ·  " + slug
		}
		if path != "" {
			headline += "  ·  " + path
		}
		out = append(out, headline)

		var details []string
		for _, i := range detailIdxs {
			v := cell(row, i)
			if v == "" {
				continue
			}
			details = append(details, elidePath(elideID(v)))
		}
		if len(details) > 0 {
			out = append(out, "  "+strings.Join(details, "  ·  "))
		}
	}
	return out
}

// flattenGenericTable is the legacy name+handle+detail shape, kept for
// tables that aren't recognizably tasks or projects (e.g. ad-hoc tool
// output, run lists with arbitrary columns).
func flattenGenericTable(headers []string, dataRows [][]string) []string {
	// nameCol: priority Name > Title > first non-ID col > col 0
	nameColIdx := -1
	for i, h := range headers {
		if strings.EqualFold(h, "name") || strings.EqualFold(h, "title") {
			nameColIdx = i
			break
		}
	}
	if nameColIdx < 0 {
		for i := range headers {
			isID := false
			for _, row := range dataRows {
				if i < len(row) && looksLikeTypedID(row[i]) {
					isID = true
					break
				}
			}
			if !isID {
				nameColIdx = i
				break
			}
		}
	}
	if nameColIdx < 0 {
		nameColIdx = 0
	}

	// handleCol: Slug > ID > any typed-ID-shaped column (pick shortest values).
	handleColIdx := -1
	for i, h := range headers {
		if strings.EqualFold(h, "slug") {
			handleColIdx = i
			break
		}
	}
	if handleColIdx < 0 {
		for i, h := range headers {
			if i == nameColIdx {
				continue
			}
			if strings.EqualFold(h, "id") {
				handleColIdx = i
				break
			}
		}
	}
	if handleColIdx < 0 {
		bestLen := -1
		for i := range headers {
			if i == nameColIdx {
				continue
			}
			allMatch := true
			maxLen := 0
			for _, row := range dataRows {
				v := ""
				if i < len(row) {
					v = row[i]
				}
				if !looksLikeTypedID(v) {
					allMatch = false
					break
				}
				if len(v) > maxLen {
					maxLen = len(v)
				}
			}
			if allMatch && (bestLen < 0 || maxLen < bestLen) {
				bestLen = maxLen
				handleColIdx = i
			}
		}
	}

	// Drop long ID columns (they're noise — handle covers it).
	dropCols := make(map[int]bool)
	for i, h := range headers {
		if i == nameColIdx || i == handleColIdx {
			continue
		}
		if strings.EqualFold(h, "id") || looksLikeIDHeader(h) {
			for _, row := range dataRows {
				v := ""
				if i < len(row) {
					v = row[i]
				}
				if len(v) > 16 {
					dropCols[i] = true
					break
				}
			}
		}
	}

	var out []string
	for ri, row := range dataRows {
		if ri > 0 {
			out = append(out, "")
		}
		name := ""
		if nameColIdx < len(row) {
			name = row[nameColIdx]
		}
		handle := ""
		if handleColIdx >= 0 && handleColIdx < len(row) {
			handle = elideID(row[handleColIdx])
		}
		headline := "**" + name + "**"
		if handle != "" {
			headline += "  ·  " + handle
		}
		out = append(out, headline)

		var details []string
		for i, h := range headers {
			if i == nameColIdx || i == handleColIdx || dropCols[i] {
				continue
			}
			v := ""
			if i < len(row) {
				v = row[i]
			}
			if v == "" {
				continue
			}
			v = elideID(v)
			v = elidePath(v)
			if strings.EqualFold(h, "project") {
				v = "in " + v
			}
			details = append(details, v)
		}
		if len(details) > 0 {
			out = append(out, "  "+strings.Join(details, "  ·  "))
		}
	}
	return out
}

// looksLikeTypedID returns true if v looks like a Hive typed ID:
// starts with one of the known prefixes followed by a dash.
func looksLikeTypedID(v string) bool {
	if v == "" {
		return false
	}
	for _, pfx := range []string{"proj-", "task-", "run-", "p-"} {
		if strings.HasPrefix(v, pfx) {
			return true
		}
	}
	return false
}

// handleBackTickRE matches backtick-wrapped identifier-like spans:
// slugs (letters/digits/_-./), typed IDs (e.g. task-1780…4185 — the
// horizontal-ellipsis U+2026 from elideID is included explicitly), or
// tilde-prefixed paths. The character class is deliberately narrow so
// it never strips backticks around real code (anything with spaces,
// parens, semicolons, quotes, etc.).
var handleBackTickRE = regexp.MustCompile("`([\\p{L}\\p{N}_./~\\x{2026}-]+)`")

// stripHandleBackticks removes backtick wrappers around identifiers
// (slugs, task-/run-/proj-/p- IDs, and tilde-prefixed paths). Leaves
// genuine code spans alone — anything containing whitespace, parens,
// or other non-identifier punctuation is not matched.
//
// Why: the model occasionally still emits `slug` despite the prompt
// asking it not to. Glamour renders backticks as inline code with a
// background tint that visually bleeds past the content in the narrow
// chat panel, so we strip them as a render-time fallback.
func stripHandleBackticks(s string) string {
	return handleBackTickRE.ReplaceAllString(s, "$1")
}

// looksLikeIDHeader returns true when a column header is an ID-like name.
func looksLikeIDHeader(h string) bool {
	lower := strings.ToLower(h)
	return lower == "id" || strings.HasSuffix(lower, "_id") || strings.HasSuffix(lower, " id")
}

// canonicalizeStackedBlock reshapes a parsed stacked-block item to match
// the chat output spec when the model emits one of two legacy shapes:
//
//  1. Project with the repo path on the detail line. The model emits:
//
//     **Name**  ·  slug
//     /mnt/e/Documents/Hive/hive
//
//     We promote the path onto the headline and clear the detail line:
//
//     **Name**  ·  slug  ·  ~/Hive/hive
//
//  2. Task with the task ID on the headline and status/priority/pipeline
//     on the detail line. The model emits:
//
//     **title**  ·  task-1780…4185
//     pending  ·  P3  ·  build
//
//     Per spec, tasks render as ONE LINE with NO task ID. We drop the
//     typed ID from the tail, merge detail tokens into the tail, sort by
//     spec order (pipeline · priority · status), and clear the detail.
//
// Conservative detection: only reshape when the shape is unambiguous.
// Prose-bold lines (e.g. **Note:** explanation) are left alone because
// they wouldn't pass looksLikeStackedHeadline in the first place.
func canonicalizeStackedBlock(item stackedBlockItem) stackedBlockItem {
	// Task detection: a typed-ID in the tail OR a detail line composed
	// of task-shaped tokens (pipeline / priority / status). The model
	// occasionally emits either combination.
	detailTokens := splitTailPieces(item.Detail)
	isTaskByID := tailHasTypedID(item.Tail)
	isTaskByDetail := item.Detail != "" && allTaskTokens(detailTokens)
	if isTaskByID || isTaskByDetail {
		// Drop typed IDs from the tail; merge detail tokens in.
		merged := dropTypedIDs(item.Tail)
		if isTaskByDetail {
			merged = append(merged, detailTokens...)
		}
		item.Tail = sortTaskTokens(merged)
		item.Detail = ""
		return item
	}

	// Project detection: detail looks like a single path AND the tail
	// doesn't already contain one. Move the path to the tail with
	// elision applied.
	if item.Detail != "" && looksLikePath(item.Detail) && !tailHasPath(item.Tail) {
		item.Tail = append(item.Tail, elidePath(item.Detail))
		item.Detail = ""
		return item
	}

	return item
}

// splitTailPieces splits a "  ·  "-joined detail line into trimmed pieces.
// Returns nil for an empty string so callers can use len() == 0 checks.
func splitTailPieces(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "  ·  ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// tailHasTypedID is true if any piece in the tail is a typed Hive ID.
// task-/run-/proj-/p- prefixes, with or without elision (U+2026).
func tailHasTypedID(tail []string) bool {
	for _, p := range tail {
		if looksLikeTypedID(p) {
			return true
		}
	}
	return false
}

// dropTypedIDs returns a new slice with typed-ID entries removed.
// Used to strip task-/run- IDs from the tail per spec (tasks render
// with no ID; the title is the recognition handle).
func dropTypedIDs(tail []string) []string {
	out := make([]string, 0, len(tail))
	for _, p := range tail {
		if !looksLikeTypedID(p) {
			out = append(out, p)
		}
	}
	return out
}

// allTaskTokens is true if every piece in the slice is a recognizable
// task token (pipeline name, priority Px, or status keyword). Empty
// input returns false.
func allTaskTokens(pieces []string) bool {
	if len(pieces) == 0 {
		return false
	}
	for _, p := range pieces {
		if !isTaskToken(p) {
			return false
		}
	}
	return true
}

// isTaskToken classifies a piece as a pipeline/priority/status keyword.
// Used by canonicalizeStackedBlock to detect "this detail line is
// actually a task tail in disguise".
func isTaskToken(s string) bool {
	return taskTokenClass(s) != taskTokenOther
}

const (
	taskTokenPipeline = 1
	taskTokenPriority = 2
	taskTokenStatus   = 3
	taskTokenOther    = 4
)

// taskTokenClass returns the canonical sort class for a task tail piece.
// Sort order: pipeline · priority · status · other.
func taskTokenClass(s string) int {
	switch strings.ToLower(s) {
	case "build", "plan", "debug", "finish-branch", "finishbranch":
		return taskTokenPipeline
	case "pending", "running", "done", "needs_attention", "error", "abandoned", "source_closed":
		return taskTokenStatus
	}
	if len(s) == 2 && (s[0] == 'P' || s[0] == 'p') {
		switch s[1] {
		case '0', '1', '2', '3', '4':
			return taskTokenPriority
		}
	}
	return taskTokenOther
}

// sortTaskTokens orders the tail by spec class (pipeline first, then
// priority, then status). Stable within a class so the model's original
// order is preserved if it emitted multiples of one class.
func sortTaskTokens(pieces []string) []string {
	type indexed struct {
		piece string
		class int
		idx   int
	}
	tagged := make([]indexed, len(pieces))
	for i, p := range pieces {
		tagged[i] = indexed{piece: p, class: taskTokenClass(p), idx: i}
	}
	// Insertion sort keeps things stable + tiny; avoids importing sort
	// for a slice that's always ≤4 elements in practice.
	for i := 1; i < len(tagged); i++ {
		for j := i; j > 0; j-- {
			if tagged[j-1].class > tagged[j].class ||
				(tagged[j-1].class == tagged[j].class && tagged[j-1].idx > tagged[j].idx) {
				tagged[j-1], tagged[j] = tagged[j], tagged[j-1]
				continue
			}
			break
		}
	}
	out := make([]string, len(tagged))
	for i, t := range tagged {
		out[i] = t.piece
	}
	return out
}

// looksLikePath is true if the string starts with '/' or '~' — both
// absolute paths and $HOME-substituted paths. The elided form
// /first/…/lastTwo/segments also passes since it starts with '/'.
func looksLikePath(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	return t[0] == '/' || t[0] == '~'
}

// tailHasPath is true if any tail piece looks like a path. Used to
// avoid double-appending when the model already put the path on the
// headline (so we don't move detail-path to tail unnecessarily).
func tailHasPath(tail []string) bool {
	for _, p := range tail {
		if looksLikePath(p) {
			return true
		}
	}
	return false
}

// stackedBlockItem represents one parsed stacked-block item:
// a bold "name", optional "tail" pieces after the name on the headline
// (joined by "  ·  "), and an optional detail line (indented).
//
// We use this to bypass glamour entirely for stacked-block runs — glamour
// collapses single \n between non-blank lines via soft-break, so getting
// the headline directly above the detail line required either hard-paragraph
// breaks (blank line) or rendering ourselves. We chose the latter so the
// rendered output is one bold line + one indented dim line, no padding.
type stackedBlockItem struct {
	Name   string   // the **bold** content (no asterisks)
	Tail   []string // pieces after "  ·  " on the headline (slug, path, etc.)
	Detail string   // the indented detail line (no leading whitespace), or ""
}

// looksLikeStackedHeadline reports whether a line opens a stacked block —
// it starts with **bold** at column 0 and contains the "  ·  " separator OR
// is a bare **bold** line that may stand alone as an item with no tail.
//
// We accept both shapes so the chunker treats consecutive bare-bold lines
// (e.g. one-task-no-tail edge case) as stacked-block content rather than
// kicking them through glamour with the wrong padding.
func looksLikeStackedHeadline(line string) bool {
	if !strings.HasPrefix(line, "**") {
		return false
	}
	// Must close the bold span on the same line.
	if strings.Index(line[2:], "**") < 0 {
		return false
	}
	// Must be a stacked-block style line: either the separator is present
	// OR the line is just a **bold** opener (with possibly trailing punct
	// from elision). We tighten on separator present to avoid grabbing
	// arbitrary prose-bold like "**Important:** see docs".
	return strings.Contains(line, "**  ·  ") || strings.HasSuffix(strings.TrimSpace(line), "**")
}

// parseStackedBlock parses one item starting at the given line index.
// Returns the parsed item, the number of source lines consumed, and ok=false
// if the lines don't match the stacked-block pattern.
//
// Pattern accepted:
//   - Line 1: "**Name**" optionally followed by "  ·  piece  ·  piece…"
//   - Line 2 (optional): a line starting with whitespace = the detail line
func parseStackedBlock(lines []string, i int) (item stackedBlockItem, consumed int, ok bool) {
	if i >= len(lines) {
		return
	}
	headline := lines[i]
	if !strings.HasPrefix(headline, "**") {
		return
	}
	endBold := strings.Index(headline[2:], "**")
	if endBold < 0 {
		return
	}
	item.Name = headline[2 : 2+endBold]
	rest := headline[2+endBold+2:]
	// Optional "  ·  tail · piece · piece" after the name.
	rest = strings.TrimSpace(rest)
	rest = strings.TrimPrefix(rest, "·")
	rest = strings.TrimSpace(rest)
	if rest != "" {
		for _, p := range strings.Split(rest, "  ·  ") {
			p = strings.TrimSpace(p)
			if p != "" {
				item.Tail = append(item.Tail, p)
			}
		}
	}
	consumed = 1
	// Optional detail line: the very next line, indented.
	if i+1 < len(lines) {
		next := lines[i+1]
		if next != "" && (next[0] == ' ' || next[0] == '\t') {
			item.Detail = strings.TrimSpace(next)
			consumed = 2
		}
	}
	ok = true
	return
}
