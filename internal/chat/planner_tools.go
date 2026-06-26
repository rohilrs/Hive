package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// readDocCapBytes bounds ONE read_doc response. It is deliberately well under
// claude-code's MCP inline-display limit (~25KB): a larger result gets persisted
// to a file and the model only sees a ~2KB preview, which silently breaks reading.
// 16KB displays inline reliably; larger docs are paged via offset (next_offset
// until truncated=false). A `length` arg can request an even smaller window, but
// never larger than this cap.
const readDocCapBytes = 16 * 1024

// firstLineTitle scans this many lines into a spec file looking for the leading `# heading`.
const titleScanLines = 30

// PlannerReadTools bundles the read-only planner tools. cwd is the
// project's repo_path — all scanned/read paths are relative to it.
type PlannerReadTools struct {
	cwd string
}

// NewPlannerReadTools constructs the bundle.
func NewPlannerReadTools(cwd string) *PlannerReadTools {
	return &PlannerReadTools{cwd: cwd}
}

// ListSpecs scans <cwd>/docs/superpowers/specs/*.md and returns up to
// 20 of the most recent (by YYYY-MM-DD filename prefix), with title
// pulled from each file's first markdown heading. project_slug is
// currently advisory — the model passes it for future slug-boost
// ranking but V1 returns all recent specs so the planner can reason
// across the repo's design history. Missing dir → empty list, not error.
func (p *PlannerReadTools) ListSpecs(ctx context.Context, input json.RawMessage) ToolResult {
	var args struct {
		ProjectSlug string `json:"project_slug"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return jsonErr(err)
	}
	specsDir := filepath.Join(p.cwd, "docs", "superpowers", "specs")
	entries, err := os.ReadDir(specsDir)
	if err != nil {
		// Missing dir is normal for a new project — return empty list, not error.
		if os.IsNotExist(err) {
			return ToolResult{Content: `{"specs":[]}`}
		}
		return jsonErr(err)
	}
	type specRef struct {
		Path  string `json:"path"`
		Title string `json:"title,omitempty"`
		Date  string `json:"date,omitempty"`
	}
	var matches []specRef
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		// Date prefix YYYY-MM-DD (10 chars) if present.
		date := ""
		if len(name) >= 10 && name[4] == '-' && name[7] == '-' {
			date = name[:10]
		}
		relPath := filepath.Join("docs", "superpowers", "specs", name)
		matches = append(matches, specRef{
			Path:  relPath,
			Date:  date,
			Title: firstLineTitle(filepath.Join(specsDir, name)),
		})
	}
	// Sort by date desc (most recent first); take top 20.
	sort.Slice(matches, func(i, j int) bool { return matches[i].Date > matches[j].Date })
	if len(matches) > 20 {
		matches = matches[:20]
	}
	out := struct {
		Specs []specRef `json:"specs"`
	}{Specs: matches}
	b, _ := json.Marshal(out)
	return ToolResult{Content: string(b)}
}

// firstLineTitle returns the first markdown heading text from the file,
// or "" on error / no heading. Best-effort — used only for display.
func firstLineTitle(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.SplitN(string(b), "\n", titleScanLines) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

// coerceInt normalizes a read_doc numeric arg (offset/length) that may arrive as
// a JSON number (float64 after a generic unmarshal), a numeric JSON string,
// json.Number, or nil/absent (→ 0). Returns an error only for a non-numeric
// string or an unexpected type.
func coerceInt(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return int(x), nil
	case json.Number:
		n, err := x.Int64()
		return int(n), err
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, nil
		}
		return strconv.Atoi(s)
	default:
		return 0, fmt.Errorf("offset has unexpected type %T", v)
	}
}

// ReadDoc reads a markdown file relative to cwd. Path traversal (..)
// is rejected. Content is paginated in readDocCapBytes chunks via offset.
func (p *PlannerReadTools) ReadDoc(ctx context.Context, input json.RawMessage) ToolResult {
	var args struct {
		Path string `json:"path"`
		// Offset/Length are `any` so we accept them as a JSON number OR a JSON
		// string. The chat-tools MCP server advertises a generic schema (no
		// per-param types), so the model sometimes serializes these integers as
		// strings — a strict `int` field would reject them with a type error and
		// break pagination. coerceInt normalizes both forms.
		Offset any `json:"offset"`
		Length any `json:"length"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return jsonErr(err)
	}
	off, oerr := coerceInt(args.Offset)
	if oerr != nil {
		return ToolResult{Content: `{"error":"offset must be an integer (number or numeric string)"}`, IsError: true}
	}
	length, lerr := coerceInt(args.Length)
	if lerr != nil {
		return ToolResult{Content: `{"error":"length must be an integer (number or numeric string)"}`, IsError: true}
	}
	clean := filepath.Clean(args.Path)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return ToolResult{Content: `{"error":"path must be relative and within cwd"}`, IsError: true}
	}
	full := filepath.Join(p.cwd, clean)
	b, err := os.ReadFile(full)
	if err != nil {
		return jsonErr(err)
	}
	total := len(b)
	if off < 0 {
		off = 0
	}
	if off > total {
		off = total
	}
	// Per-call window: default the full cap; a positive length can request a
	// smaller window, but never larger than the cap (so the result always
	// displays inline).
	chunk := readDocCapBytes
	if length > 0 && length < chunk {
		chunk = length
	}
	end := off + chunk
	if end >= total {
		end = total
	} else {
		// Snap the chunk end back to a UTF-8 rune boundary so a multibyte char
		// isn't split across chunks (which would corrupt the content and
		// misalign the next read). b[end] is safe to index here (end < total).
		for end > off && !utf8.RuneStart(b[end]) {
			end--
		}
	}
	out := struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		Offset     int    `json:"offset"`
		NextOffset int    `json:"next_offset"`
		TotalBytes int    `json:"total_bytes"`
		Truncated  bool   `json:"truncated"` // true => more remains; call again with offset=next_offset
	}{Path: clean, Content: string(b[off:end]), Offset: off, NextOffset: end, TotalBytes: total, Truncated: end < total}
	log.Printf("planner read_doc: %s bytes %d-%d/%d truncated=%v", clean, off, end, total, end < total)
	j, _ := json.Marshal(out)
	return ToolResult{Content: string(j)}
}

// jsonErr returns a ToolResult carrying a JSON-encoded error payload.
func jsonErr(err error) ToolResult {
	return ToolResult{Content: fmt.Sprintf(`{"error":%q}`, err.Error()), IsError: true}
}

// PlannerWriteTools bundles the mutating planner tools. Register each in
// the chat.Registry with Mutating=true so the confirm gate surfaces them.
type PlannerWriteTools struct {
	cwd string
	// featureBranch is the integration-loop feature branch the planner session
	// runs on (plan_setup has already checked it out). When non-empty, each
	// saved doc is committed to it; empty means no commit (legacy / inert).
	featureBranch string
}

// NewPlannerWriteTools constructs the bundle. featureBranch is the integration
// feature branch (already checked out by plan_setup); pass "" to disable
// per-save doc commits.
func NewPlannerWriteTools(cwd, featureBranch string) *PlannerWriteTools {
	return &PlannerWriteTools{cwd: cwd, featureBranch: featureBranch}
}

// commitDoc stages + commits a single doc on the repo's CURRENT branch
// (plan_setup has checked out the feature branch). Best-effort: logs on
// error, never fails the tool. A no-change re-save is a no-op.
func commitDoc(cwd, relPath, message string) {
	add := exec.Command("git", "-C", cwd, "add", relPath)
	if out, err := add.CombinedOutput(); err != nil {
		log.Printf("planner doc-commit: git add %s: %v (%s)", relPath, err, strings.TrimSpace(string(out)))
		return
	}
	staged, _ := exec.Command("git", "-C", cwd, "diff", "--cached", "--name-only").Output()
	if strings.TrimSpace(string(staged)) == "" {
		return // nothing changed; not an error
	}
	if out, err := exec.Command("git", "-C", cwd, "commit", "-m", message).CombinedOutput(); err != nil {
		log.Printf("planner doc-commit: git commit: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

// SaveRoadmap writes docs/superpowers/roadmaps/<project_slug>.md.
// Overwrites if file exists — the planner owns the roadmap as a living
// doc updated turn-by-turn. The confirm gate is the operator's safety.
func (p *PlannerWriteTools) SaveRoadmap(ctx context.Context, input json.RawMessage) ToolResult {
	var args struct {
		ProjectSlug string `json:"project_slug"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return jsonErr(err)
	}
	if args.ProjectSlug == "" || args.Content == "" {
		return ToolResult{Content: `{"error":"project_slug and content are required"}`, IsError: true}
	}
	if strings.ContainsAny(args.ProjectSlug, `/\`) {
		return ToolResult{Content: `{"error":"project_slug must not contain path separators"}`, IsError: true}
	}
	dir := filepath.Join(p.cwd, "docs", "superpowers", "roadmaps")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return jsonErr(err)
	}
	target := filepath.Join(dir, args.ProjectSlug+".md")
	if err := os.WriteFile(target, []byte(args.Content), 0644); err != nil {
		return jsonErr(err)
	}
	rel := filepath.ToSlash(filepath.Join("docs", "superpowers", "roadmaps", args.ProjectSlug+".md"))
	if p.featureBranch != "" {
		commitDoc(p.cwd, rel, "plan: roadmap "+args.ProjectSlug)
	}
	out := struct {
		Path string `json:"path"`
	}{Path: rel}
	j, _ := json.Marshal(out)
	return ToolResult{Content: string(j)}
}

// SaveSpec writes docs/superpowers/specs/<date>-<slug>.md. Refuses to
// overwrite — the model must pick a fresh slug or date if the file
// exists. Protects in-flight specs from accidental clobber across
// resumed sessions.
func (p *PlannerWriteTools) SaveSpec(ctx context.Context, input json.RawMessage) ToolResult {
	var args struct {
		Slug    string `json:"slug"`
		Date    string `json:"date"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return jsonErr(err)
	}
	if args.Slug == "" || args.Date == "" || args.Content == "" {
		return ToolResult{Content: `{"error":"slug, date, content are required"}`, IsError: true}
	}
	if !dateRE.MatchString(args.Date) {
		return ToolResult{Content: `{"error":"date must be YYYY-MM-DD"}`, IsError: true}
	}
	if strings.ContainsAny(args.Slug, `/\`) {
		return ToolResult{Content: `{"error":"slug must not contain path separators"}`, IsError: true}
	}
	dir := filepath.Join(p.cwd, "docs", "superpowers", "specs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return jsonErr(err)
	}
	name := args.Date + "-" + args.Slug + ".md"
	target := filepath.Join(dir, name)
	if _, err := os.Stat(target); err == nil {
		return ToolResult{Content: fmt.Sprintf(`{"error":"spec already exists at %s; pick a fresh slug or date"}`, target), IsError: true}
	}
	if err := os.WriteFile(target, []byte(args.Content), 0644); err != nil {
		return jsonErr(err)
	}
	rel := filepath.ToSlash(filepath.Join("docs", "superpowers", "specs", name))
	if p.featureBranch != "" {
		commitDoc(p.cwd, rel, "plan: spec "+name)
	}
	out := struct {
		Path string `json:"path"`
	}{Path: rel}
	j, _ := json.Marshal(out)
	return ToolResult{Content: string(j)}
}
