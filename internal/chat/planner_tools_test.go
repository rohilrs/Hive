package chat

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// initChatGitRepo creates a temp git repo on branch "main" with one commit so
// subsequent commits have a parent. Returns the repo path.
func initChatGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-m", "init")
	return dir
}

// gitLogOneline returns `git log -1 --oneline` for the repo.
func gitLogOneline(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "log", "-1", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestPlannerSaveRoadmapCommitsOnFeatureBranch verifies that with a non-empty
// featureBranch, SaveRoadmap commits the doc on the current branch, and with an
// empty featureBranch it writes the file but makes no commit.
func TestPlannerSaveRoadmapCommitsOnFeatureBranch(t *testing.T) {
	// With a feature branch: the doc is written AND committed.
	repo := initChatGitRepo(t)
	p := NewPlannerWriteTools(repo, "spec/x")
	in, _ := json.Marshal(map[string]string{
		"project_slug": "my-app",
		"content":      "# my-app roadmap\n\n## Phase 1\n",
	})
	res := p.SaveRoadmap(context.Background(), in)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(repo, "docs", "superpowers", "roadmaps", "my-app.md")); err != nil {
		t.Fatalf("roadmap file should exist: %v", err)
	}
	if log := gitLogOneline(t, repo); !strings.Contains(log, "plan: roadmap") {
		t.Errorf("HEAD commit should be the doc commit; got %q", log)
	}

	// Without a feature branch: file is written, but NO commit (HEAD unchanged).
	repo2 := initChatGitRepo(t)
	before := gitLogOneline(t, repo2)
	p2 := NewPlannerWriteTools(repo2, "")
	res2 := p2.SaveRoadmap(context.Background(), in)
	if res2.IsError {
		t.Fatalf("unexpected error: %s", res2.Content)
	}
	if _, err := os.Stat(filepath.Join(repo2, "docs", "superpowers", "roadmaps", "my-app.md")); err != nil {
		t.Fatalf("roadmap file should exist even without feature branch: %v", err)
	}
	if after := gitLogOneline(t, repo2); after != before {
		t.Errorf("empty feature branch must not commit; HEAD changed %q -> %q", before, after)
	}
}

func TestListSpecsReturnsRecentMarkdownAndSkipsNonMarkdown(t *testing.T) {
	dir := t.TempDir()
	specsDir := filepath.Join(dir, "docs", "superpowers", "specs")
	if err := os.MkdirAll(specsDir, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"2026-05-30-phase-7-doctor-design.md": "# Doctor\n\nDoctor spec.",
		"2026-05-28-my-app-feature-design.md": "# My-app feature\n\nFeature spec.",
		"2026-05-25-older-design.md":          "# Older\n\nOlder spec.",
		"ignored.txt":                         "not a spec",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(specsDir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	h := NewPlannerReadTools(dir).ListSpecs
	in, _ := json.Marshal(map[string]string{"project_slug": "my-app"})
	res := h(context.Background(), in)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "my-app-feature-design") {
		t.Fatalf("expected my-app spec in result; got: %s", res.Content)
	}
	if strings.Contains(res.Content, "ignored.txt") {
		t.Fatal("non-.md file should not be listed")
	}
	// Date-desc ordering: 2026-05-30 must appear before 2026-05-28 in the JSON output.
	doctorPath := `"path":"docs/superpowers/specs/2026-05-30-phase-7-doctor-design.md"`
	myAppPath := `"path":"docs/superpowers/specs/2026-05-28-my-app-feature-design.md"`
	doctorIdx := strings.Index(res.Content, doctorPath)
	myAppIdx := strings.Index(res.Content, myAppPath)
	if doctorIdx < 0 || myAppIdx < 0 {
		t.Fatalf("expected both spec paths in result; doctorIdx=%d myAppIdx=%d content=%s", doctorIdx, myAppIdx, res.Content)
	}
	if doctorIdx >= myAppIdx {
		t.Fatalf("expected 2026-05-30 spec to appear before 2026-05-28 (date desc); doctorIdx=%d myAppIdx=%d", doctorIdx, myAppIdx)
	}
}

func TestListSpecsHandlesMissingDir(t *testing.T) {
	dir := t.TempDir()
	h := NewPlannerReadTools(dir).ListSpecs
	in, _ := json.Marshal(map[string]string{"project_slug": "anything"})
	res := h(context.Background(), in)
	if res.IsError {
		t.Fatalf("missing dir should not error; got: %s", res.Content)
	}
	if !strings.Contains(res.Content, `"specs":[]`) {
		t.Fatalf("expected empty specs array; got: %s", res.Content)
	}
}

func TestReadDocReadsFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "docs", "x.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	h := NewPlannerReadTools(dir).ReadDoc
	in, _ := json.Marshal(map[string]string{"path": "docs/x.md"})
	res := h(context.Background(), in)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "hello") {
		t.Fatalf("expected file content in result; got: %s", res.Content)
	}
}

func TestReadDocRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	h := NewPlannerReadTools(dir).ReadDoc
	in, _ := json.Marshal(map[string]string{"path": "../escape.md"})
	res := h(context.Background(), in)
	if !res.IsError {
		t.Fatalf("expected error on path traversal; got: %s", res.Content)
	}
}

// readDocResult mirrors the JSON shape ReadDoc returns.
type readDocResult struct {
	Content    string `json:"content"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"next_offset"`
	TotalBytes int    `json:"total_bytes"`
	Truncated  bool   `json:"truncated"`
}

func callReadDoc(t *testing.T, dir, path string, offset int) readDocResult {
	t.Helper()
	h := NewPlannerReadTools(dir).ReadDoc
	in, _ := json.Marshal(map[string]any{"path": path, "offset": offset})
	res := h(context.Background(), in)
	if res.IsError {
		t.Fatalf("ReadDoc(%s, off=%d) error: %s", path, offset, res.Content)
	}
	var out readDocResult
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, res.Content)
	}
	return out
}

func TestReadDocFirstChunkTruncatesLargeFile(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", 100*1024) // 100 KB
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(big), 0644); err != nil {
		t.Fatal(err)
	}
	r := callReadDoc(t, dir, "big.md", 0)
	if !r.Truncated {
		t.Error("100KB doc should be truncated on the first chunk")
	}
	// Chunk must stay at/under the display-safe cap so claude-code shows it
	// inline instead of persisting it to a file (the dogfood failure).
	if len(r.Content) > readDocCapBytes {
		t.Errorf("first chunk %d bytes exceeds display-safe cap %d", len(r.Content), readDocCapBytes)
	}
	if r.TotalBytes != 100*1024 {
		t.Errorf("total_bytes=%d want %d", r.TotalBytes, 100*1024)
	}
	if r.NextOffset != len(r.Content) {
		t.Errorf("next_offset=%d should equal first-chunk length %d", r.NextOffset, len(r.Content))
	}
}

// TestReadDocLengthParam: a positive length bounds the window; a length larger
// than the cap is clamped to the cap (never persisted).
func TestReadDocLengthParam(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "d.md"), []byte(strings.Repeat("z", 50*1024)), 0644); err != nil {
		t.Fatal(err)
	}
	h := NewPlannerReadTools(dir).ReadDoc
	// explicit small window
	in, _ := json.Marshal(map[string]any{"path": "d.md", "offset": 0, "length": 500})
	var small readDocResult
	_ = json.Unmarshal([]byte(h(context.Background(), in).Content), &small)
	if len(small.Content) != 500 {
		t.Errorf("length=500 should return 500 bytes; got %d", len(small.Content))
	}
	// length above the cap is clamped to the cap
	inBig, _ := json.Marshal(map[string]any{"path": "d.md", "offset": 0, "length": 999999})
	var big readDocResult
	_ = json.Unmarshal([]byte(h(context.Background(), inBig).Content), &big)
	if len(big.Content) > readDocCapBytes {
		t.Errorf("length above cap must clamp to %d; got %d", readDocCapBytes, len(big.Content))
	}
}

// TestReadDocPaginatesWholeFile is the core fix: paging through with
// offset=next_offset reconstructs the FULL doc, and truncated flips to false on
// the final chunk.
func TestReadDocPaginatesWholeFile(t *testing.T) {
	dir := t.TempDir()
	full := strings.Repeat("x", 85*1024) + "END" // ~85KB, > one chunk
	if err := os.WriteFile(filepath.Join(dir, "spec.md"), []byte(full), 0644); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	off, guard := 0, 0
	for {
		r := callReadDoc(t, dir, "spec.md", off)
		got.WriteString(r.Content)
		if r.TotalBytes != len(full) {
			t.Fatalf("total_bytes=%d want %d", r.TotalBytes, len(full))
		}
		if !r.Truncated {
			break
		}
		off = r.NextOffset
		if guard++; guard > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if got.String() != full {
		t.Errorf("reassembled doc (%d bytes) != original (%d bytes)", got.Len(), len(full))
	}
}

// TestReadDocChunkBoundaryRuneSafe: a multibyte rune straddling the 64KB boundary
// must not be split — each chunk is valid UTF-8 and the join is exact.
func TestReadDocChunkBoundaryRuneSafe(t *testing.T) {
	dir := t.TempDir()
	// Place a 3-byte rune (天) so it straddles byte 65536.
	body := strings.Repeat("a", readDocCapBytes-1) + "天" + strings.Repeat("b", 100)
	if err := os.WriteFile(filepath.Join(dir, "u.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	first := callReadDoc(t, dir, "u.md", 0)
	if !utf8.ValidString(first.Content) {
		t.Error("first chunk must be valid UTF-8 (rune not split at the boundary)")
	}
	second := callReadDoc(t, dir, "u.md", first.NextOffset)
	if !utf8.ValidString(second.Content) {
		t.Error("second chunk must be valid UTF-8")
	}
	if first.Content+second.Content != body {
		t.Error("rune-safe chunks must rejoin to the exact original")
	}
}

// TestReadDocOffsetAcceptsString is the fix for the dogfood bug: the MCP layer
// advertises a generic schema, so the planner serialized the offset as a STRING.
// A strict int field rejected it ("type mismatch") and broke pagination.
func TestReadDocOffsetAcceptsString(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("a", 100) + "TAIL"
	if err := os.WriteFile(filepath.Join(dir, "d.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	h := NewPlannerReadTools(dir).ReadDoc
	// offset as a STRING, the way the planner actually sent it.
	in, _ := json.Marshal(map[string]any{"path": "d.md", "offset": "100"})
	res := h(context.Background(), in)
	if res.IsError {
		t.Fatalf("string offset must be accepted; got error: %s", res.Content)
	}
	var out readDocResult
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "TAIL" {
		t.Errorf("string offset=100 should read from byte 100; got %q", out.Content)
	}
	if out.Offset != 100 {
		t.Errorf("echoed offset=%d want 100", out.Offset)
	}
	// numeric form must still work identically.
	inNum, _ := json.Marshal(map[string]any{"path": "d.md", "offset": 100})
	if h(context.Background(), inNum).IsError {
		t.Error("numeric offset must also be accepted")
	}
}

func TestReadDocOffsetPastEOF(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.md"), []byte("short"), 0644); err != nil {
		t.Fatal(err)
	}
	r := callReadDoc(t, dir, "s.md", 9999)
	if r.Content != "" || r.Truncated {
		t.Errorf("offset past EOF should return empty, not-truncated; got content=%q truncated=%v", r.Content, r.Truncated)
	}
}

func TestSaveRoadmapWritesFile(t *testing.T) {
	dir := t.TempDir()
	p := NewPlannerWriteTools(dir, "")
	in, _ := json.Marshal(map[string]string{
		"project_slug": "my-app",
		"content":      "# my-app roadmap\n\n## Phase 1\n",
	})
	res := p.SaveRoadmap(context.Background(), in)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	body, err := os.ReadFile(filepath.Join(dir, "docs", "superpowers", "roadmaps", "my-app.md"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(body), "# my-app roadmap") {
		t.Fatalf("content not written; got: %s", body)
	}
	if !strings.Contains(res.Content, `"path":"docs/superpowers/roadmaps/my-app.md"`) {
		t.Fatalf("expected relative path in result; got: %s", res.Content)
	}
}

func TestSaveRoadmapOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	p := NewPlannerWriteTools(dir, "")
	in1, _ := json.Marshal(map[string]string{"project_slug": "x", "content": "v1"})
	_ = p.SaveRoadmap(context.Background(), in1)
	in2, _ := json.Marshal(map[string]string{"project_slug": "x", "content": "v2"})
	res := p.SaveRoadmap(context.Background(), in2)
	if res.IsError {
		t.Fatalf("overwrite should succeed; got: %s", res.Content)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "docs", "superpowers", "roadmaps", "x.md"))
	if string(body) != "v2" {
		t.Fatalf("expected v2, got %s", body)
	}
}

func TestSaveSpecWritesFile(t *testing.T) {
	dir := t.TempDir()
	p := NewPlannerWriteTools(dir, "")
	in, _ := json.Marshal(map[string]string{
		"slug":    "auth",
		"date":    "2026-06-02",
		"content": "# Auth spec\n",
	})
	res := p.SaveSpec(context.Background(), in)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	expected := filepath.Join(dir, "docs", "superpowers", "specs", "2026-06-02-auth.md")
	body, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(body), "# Auth spec") {
		t.Fatalf("content not written; got: %s", body)
	}
	if !strings.Contains(res.Content, `"path":"docs/superpowers/specs/2026-06-02-auth.md"`) {
		t.Fatalf("expected relative path in result; got: %s", res.Content)
	}
}

func TestSaveSpecRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := NewPlannerWriteTools(dir, "")
	in, _ := json.Marshal(map[string]string{"slug": "auth", "date": "2026-06-02", "content": "v1"})
	_ = p.SaveSpec(context.Background(), in)
	res := p.SaveSpec(context.Background(), in)
	if !res.IsError {
		t.Fatalf("second save should refuse overwrite; got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "already exists") {
		t.Fatalf("expected 'already exists' in error; got: %s", res.Content)
	}
}

func TestSaveSpecRejectsBadDate(t *testing.T) {
	dir := t.TempDir()
	p := NewPlannerWriteTools(dir, "")
	in, _ := json.Marshal(map[string]string{"slug": "auth", "date": "not-a-date", "content": "v1"})
	res := p.SaveSpec(context.Background(), in)
	if !res.IsError {
		t.Fatal("expected error on bad date")
	}
}
