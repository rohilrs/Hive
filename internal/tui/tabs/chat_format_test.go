package tabs

import (
	"strings"
	"testing"
)

func TestFlattenTableExample1Projects(t *testing.T) {
	in := `You have 3 active projects:

| Slug | Name | ID | Repo |
|---|---|---|---|
| ` + "`hive`" + ` | Hive (smoke test) | ` + "`p-hive-smoke`" + ` | /mnt/e/Documents/Hive/hive |
| ` + "`smoke-3.7`" + ` | Smoke | ` + "`proj-1779613417850679036`" + ` | /tmp/smoke-repo |
| ` + "`test`" + ` | Test Project | ` + "`p1`" + ` | /mnt/e/Documents/Hive/hive |`

	// New shape: path joins the headline, no detail line when there are
	// no extra columns. The ID column is dropped (slug is the handle).
	want := `You have 3 active projects:

**Hive (smoke test)**  ·  hive  ·  /mnt/e/Documents/Hive/hive

**Smoke**  ·  smoke-3.7  ·  /tmp/smoke-repo

**Test Project**  ·  test  ·  /mnt/e/Documents/Hive/hive`

	got := flattenMarkdownTables(in)
	if got != want {
		t.Errorf("transform mismatch:\nGOT:\n%s\n\nWANT:\n%s", got, want)
	}
}

func TestFlattenTableExample2OneTask(t *testing.T) {
	in := `One task in **Hive (smoke test)**:

| ID | Title | Status | Priority | Pipeline |
|---|---|---|---|---|
| ` + "`task-1780030878884954185`" + ` | test smoke task | pending | P3 | build |`

	// New shape: single-line task — pipeline, priority, status (no ID, no detail).
	want := `One task in **Hive (smoke test)**:

**test smoke task**  ·  build  ·  P3  ·  pending`

	got := flattenMarkdownTables(in)
	if got != want {
		t.Errorf("transform mismatch:\nGOT:\n%s\n\nWANT:\n%s", got, want)
	}
}

func TestFlattenTableExample3OneTaskAcrossProjects(t *testing.T) {
	in := `One task across all projects:

| ID | Title | Status | Priority | Pipeline | Project |
|---|---|---|---|---|---|
| ` + "`task-1780030878884954185`" + ` | test smoke task | pending | P3 | build | ` + "`p-hive-smoke`" + ` |`

	// New shape: single-line task with project disambiguation suffix.
	want := `One task across all projects:

**test smoke task**  ·  build  ·  P3  ·  pending  (in p-hive-smoke)`

	got := flattenMarkdownTables(in)
	if got != want {
		t.Errorf("transform mismatch:\nGOT:\n%s\n\nWANT:\n%s", got, want)
	}
}

func TestFlattenProjectTableElidesLongPath(t *testing.T) {
	// Path is >32 chars and not under $HOME — should elide to first/…/lastTwo.
	in := `| Slug | Name | Repo |
|---|---|---|
| ` + "`x`" + ` | Long Path Project | /very/long/many/segment/path/foo/bar |`

	want := `**Long Path Project**  ·  x  ·  /very/…/foo/bar`

	got := flattenMarkdownTables(in)
	if got != want {
		t.Errorf("transform mismatch:\nGOT:\n%s\n\nWANT:\n%s", got, want)
	}
}

func TestFlattenProjectTableWithTaskCountsDetail(t *testing.T) {
	// A non-id, non-path extra column should land on the detail line.
	in := `| Slug | Name | Repo | task_counts |
|---|---|---|---|
| ` + "`hive`" + ` | Hive | /tmp/hive | 1 pending  ·  40 done |`

	want := `**Hive**  ·  hive  ·  /tmp/hive
  1 pending  ·  40 done`

	got := flattenMarkdownTables(in)
	if got != want {
		t.Errorf("transform mismatch:\nGOT:\n%s\n\nWANT:\n%s", got, want)
	}
}

func TestElideIDLongTaskID(t *testing.T) {
	got := elideID("task-1780030878884954185")
	want := "task-1780…4185"
	if got != want {
		t.Errorf("elideID: got %q want %q", got, want)
	}
}

func TestElideIDLongProjID(t *testing.T) {
	got := elideID("proj-1779613417850679036")
	want := "proj-1779…9036"
	if got != want {
		t.Errorf("elideID: got %q want %q", got, want)
	}
}

func TestElideIDShortIDPassthrough(t *testing.T) {
	for _, in := range []string{"hive", "p1", "p-hive-smoke", "task-1"} {
		if got := elideID(in); got != in {
			t.Errorf("elideID(%q) = %q, want passthrough", in, got)
		}
	}
}

func TestElidePathPassthroughUnderThreshold(t *testing.T) {
	// 26 chars, under 32 → unchanged
	in := "/mnt/e/Documents/Hive/hive"
	if got := elidePath(in); got != in {
		t.Errorf("elidePath: got %q want %q", got, in)
	}
}

func TestStripHandleBackticks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "slug handle",
			in:   "**Hive**  ·  `hive`",
			want: "**Hive**  ·  hive",
		},
		{
			name: "task id handle",
			in:   "**test smoke task**  ·  `task-1780…4185`",
			want: "**test smoke task**  ·  task-1780…4185",
		},
		{
			name: "tilde path",
			in:   "  5 pending  ·  `~/Hive/hive`",
			want: "  5 pending  ·  ~/Hive/hive",
		},
		{
			name: "leaves real code span with space alone",
			in:   "Run `go test ./...` to verify",
			want: "Run `go test ./...` to verify",
		},
		{
			name: "leaves real code span with parens alone",
			in:   "Call `fmt.Sprintf(\"%d\", n)` to format",
			want: "Call `fmt.Sprintf(\"%d\", n)` to format",
		},
		{
			name: "mixed: strips slug but leaves code span",
			in:   "Slug `hive` — run `go build` next",
			want: "Slug hive — run `go build` next",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripHandleBackticks(tc.in)
			if got != tc.want {
				t.Errorf("stripHandleBackticks:\nGOT:  %q\nWANT: %q", got, tc.want)
			}
		})
	}
}

func TestElidePathLongShowsFirstAndLastSegments(t *testing.T) {
	// >32 chars
	in := "/very/long/many/segment/path/foo/bar"
	got := elidePath(in)
	// Expect first segment + "/…/" + last two: "/very/…/foo/bar"
	want := "/very/…/foo/bar"
	if got != want {
		t.Errorf("elidePath: got %q want %q", got, want)
	}
}

func TestParseStackedBlockHeadlineOnly(t *testing.T) {
	item, n, ok := parseStackedBlock([]string{"**Name**  ·  slug  ·  ~/path"}, 0)
	if !ok {
		t.Fatal("not parsed")
	}
	if item.Name != "Name" {
		t.Errorf("Name=%q", item.Name)
	}
	if len(item.Tail) != 2 || item.Tail[0] != "slug" || item.Tail[1] != "~/path" {
		t.Errorf("Tail=%v", item.Tail)
	}
	if item.Detail != "" {
		t.Errorf("Detail should be empty, got %q", item.Detail)
	}
	if n != 1 {
		t.Errorf("consumed=%d, want 1", n)
	}
}

func TestParseStackedBlockHeadlinePlusDetail(t *testing.T) {
	lines := []string{
		"**Hive (smoke test)**  ·  hive  ·  ~/Hive/hive",
		"  1 pending  ·  40 done",
		"",
		"**Next**",
	}
	item, n, ok := parseStackedBlock(lines, 0)
	if !ok {
		t.Fatal("not parsed")
	}
	if item.Name != "Hive (smoke test)" {
		t.Errorf("Name=%q", item.Name)
	}
	if item.Detail != "1 pending  ·  40 done" {
		t.Errorf("Detail=%q", item.Detail)
	}
	if n != 2 {
		t.Errorf("consumed=%d, want 2", n)
	}
}

func TestCanonicalizeStackedBlockProjectPathOnDetailMovesToHeadline(t *testing.T) {
	// The legacy / model-default shape: project name + slug on headline,
	// path on the indented detail line. canonicalize should move the
	// path up so the headline carries name · slug · path.
	in := stackedBlockItem{
		Name:   "Hive (smoke test)",
		Tail:   []string{"hive"},
		Detail: "/mnt/e/Documents/Hive/hive",
	}
	got := canonicalizeStackedBlock(in)
	if got.Detail != "" {
		t.Errorf("Detail should be cleared, got %q", got.Detail)
	}
	if len(got.Tail) != 2 || got.Tail[0] != "hive" || got.Tail[1] != "/mnt/e/Documents/Hive/hive" {
		t.Errorf("Tail=%v, want [hive /mnt/e/Documents/Hive/hive]", got.Tail)
	}
}

func TestCanonicalizeStackedBlockProjectPathAlreadyOnHeadlineNoOp(t *testing.T) {
	// The canonical shape — already correct, nothing to do.
	in := stackedBlockItem{
		Name:   "Hive",
		Tail:   []string{"hive", "/mnt/e/Documents/Hive/hive"},
		Detail: "1 pending  ·  40 done",
	}
	got := canonicalizeStackedBlock(in)
	if got.Detail != "1 pending  ·  40 done" {
		t.Errorf("Detail should be preserved, got %q", got.Detail)
	}
	if len(got.Tail) != 2 || got.Tail[1] != "/mnt/e/Documents/Hive/hive" {
		t.Errorf("Tail mutated unexpectedly: %v", got.Tail)
	}
}

func TestCanonicalizeStackedBlockTaskWithIDAndStatusDetailCollapses(t *testing.T) {
	// Model emits task ID on tail + status/priority/pipeline on detail.
	// canonicalize should drop the ID, merge detail into tail, sort.
	in := stackedBlockItem{
		Name:   "test smoke task",
		Tail:   []string{"task-1780…4185"},
		Detail: "pending  ·  P3  ·  build",
	}
	got := canonicalizeStackedBlock(in)
	if got.Detail != "" {
		t.Errorf("Detail should be cleared, got %q", got.Detail)
	}
	// Spec order: pipeline (build) · priority (P3) · status (pending).
	want := []string{"build", "P3", "pending"}
	if len(got.Tail) != len(want) {
		t.Fatalf("Tail length=%d want %d, Tail=%v", len(got.Tail), len(want), got.Tail)
	}
	for i, w := range want {
		if got.Tail[i] != w {
			t.Errorf("Tail[%d]=%q want %q", i, got.Tail[i], w)
		}
	}
}

func TestCanonicalizeStackedBlockTaskAlreadyCanonicalNoOp(t *testing.T) {
	// Already one-line, no ID — nothing to do.
	in := stackedBlockItem{
		Name:   "test smoke task",
		Tail:   []string{"build", "P3", "pending"},
		Detail: "",
	}
	got := canonicalizeStackedBlock(in)
	if got.Detail != "" {
		t.Errorf("Detail should stay empty, got %q", got.Detail)
	}
	if len(got.Tail) != 3 || got.Tail[0] != "build" {
		t.Errorf("Tail mutated: %v", got.Tail)
	}
}

func TestCanonicalizeStackedBlockTaskUnsortedDetailReordered(t *testing.T) {
	// Model emits detail tokens out of spec order. canonicalize should
	// sort to pipeline · priority · status.
	in := stackedBlockItem{
		Name:   "foo",
		Tail:   []string{"task-1779…0000"},
		Detail: "P2  ·  pending  ·  plan",
	}
	got := canonicalizeStackedBlock(in)
	want := []string{"plan", "P2", "pending"}
	if len(got.Tail) != len(want) {
		t.Fatalf("Tail=%v want %v", got.Tail, want)
	}
	for i, w := range want {
		if got.Tail[i] != w {
			t.Errorf("Tail[%d]=%q want %q", i, got.Tail[i], w)
		}
	}
}

func TestCanonicalizeStackedBlockProseLeftAlone(t *testing.T) {
	// A bare bold name with a non-path, non-task-token detail should be
	// untouched (e.g. a model-emitted callout). Conservative detection.
	in := stackedBlockItem{
		Name:   "Note",
		Tail:   nil,
		Detail: "This is some unrelated prose detail.",
	}
	got := canonicalizeStackedBlock(in)
	if got.Detail != "This is some unrelated prose detail." {
		t.Errorf("Detail should be preserved, got %q", got.Detail)
	}
	if len(got.Tail) != 0 {
		t.Errorf("Tail should stay empty, got %v", got.Tail)
	}
}

func TestRenderMarkdownLegacyProjectShapePromotesPathToHeadline(t *testing.T) {
	// End-to-end: feed the bad shape through renderMarkdown and confirm
	// the path lands on the headline (one bold line with name + slug +
	// path, no indented detail).
	tab := NewChat()
	tab.width, tab.height = 80, 20
	in := "**Hive (smoke test)**  ·  hive\n  /mnt/e/Documents/Hive/hive"
	out := tab.renderMarkdown(in, 76)
	plain := stripANSI(out)
	lines := strings.Split(plain, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (canonicalized), got %d:\n%q", len(lines), plain)
	}
	if !strings.Contains(lines[0], "Hive (smoke test)") {
		t.Errorf("missing name: %q", lines[0])
	}
	if !strings.Contains(lines[0], "hive") {
		t.Errorf("missing slug: %q", lines[0])
	}
	if !strings.Contains(lines[0], "/mnt/e/Documents/Hive/hive") {
		t.Errorf("missing path: %q", lines[0])
	}
}

func TestRenderMarkdownLegacyTaskShapeCollapsesToOneLine(t *testing.T) {
	// End-to-end: bad task shape (ID + 2-line) should render as one line
	// with no ID, in spec order pipeline · priority · status.
	tab := NewChat()
	tab.width, tab.height = 80, 20
	in := "**test smoke task**  ·  task-1780…4185\n  pending  ·  P3  ·  build"
	out := tab.renderMarkdown(in, 76)
	plain := stripANSI(out)
	lines := strings.Split(plain, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d:\n%q", len(lines), plain)
	}
	if strings.Contains(lines[0], "task-1780") {
		t.Errorf("task ID should be stripped: %q", lines[0])
	}
	if !strings.Contains(lines[0], "build") || !strings.Contains(lines[0], "P3") || !strings.Contains(lines[0], "pending") {
		t.Errorf("missing canonical tokens: %q", lines[0])
	}
	// Spec order: pipeline before priority before status.
	bi := strings.Index(lines[0], "build")
	pi := strings.Index(lines[0], "P3")
	si := strings.Index(lines[0], "pending")
	if !(bi < pi && pi < si) {
		t.Errorf("tokens out of spec order (want build < P3 < pending): %q", lines[0])
	}
}

func TestParseStackedBlockTaskFormatNoDetail(t *testing.T) {
	// Single-line task format: title · type · priority · status
	item, n, ok := parseStackedBlock([]string{"**test smoke task**  ·  build  ·  P3  ·  pending"}, 0)
	if !ok {
		t.Fatal("not parsed")
	}
	if item.Name != "test smoke task" {
		t.Errorf("Name=%q", item.Name)
	}
	if len(item.Tail) != 3 || item.Tail[0] != "build" || item.Tail[1] != "P3" || item.Tail[2] != "pending" {
		t.Errorf("Tail=%v", item.Tail)
	}
	if n != 1 {
		t.Errorf("consumed=%d, want 1", n)
	}
}

func TestRenderMarkdownStackedBlockNoBlankPad(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	// One headline + detail pair — should render as exactly 2 visible lines
	// with no blank line between (the whole point of the chunked renderer).
	in := "**Hive (smoke test)**  ·  hive\n  1 pending  ·  ~/Hive/hive"
	out := tab.renderMarkdown(in, 76)
	plain := stripANSI(out)
	lines := strings.Split(plain, "\n")
	// Strip trailing blanks for stable counting.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 lines (no blank padding), got %d:\n%q", len(lines), plain)
	}
	if !strings.Contains(lines[0], "Hive (smoke test)") {
		t.Errorf("line 1 missing name: %q", lines[0])
	}
	if !strings.Contains(lines[1], "1 pending") {
		t.Errorf("line 2 missing detail: %q", lines[1])
	}
}
