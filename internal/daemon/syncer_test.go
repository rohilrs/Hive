package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/sources"
	"github.com/rohilrs/Hive/internal/store"
)

type fakeSource struct {
	name  string
	items []sources.SourceItem
	err   error
}

func (f *fakeSource) Name() string { return f.name }
func (f *fakeSource) Fetch(_ context.Context, _ string, _ json.RawMessage) ([]sources.SourceItem, error) {
	return f.items, f.err
}

// newSyncTestDaemon builds a daemon with an in-memory store + bus, mirroring
// the inline construction other daemon tests use (no shared newTestDaemon).
func newSyncTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	hiveDir := t.TempDir()
	cfg, err := config.Load(config.LoadOptions{ConfigDir: hiveDir, SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.TUI.HeartbeatSeconds = 0
	d, err := New(Config{HiveDir: hiveDir, Cfg: cfg, Adapter: minimalAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSyncInsertsAndIsolatesErrors(t *testing.T) {
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "A", Slug: "a", Name: "A", Status: "active",
		Sources: map[string]any{"good": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, &store.Project{
		ID: "B", Slug: "b", Name: "B", Status: "active",
		Sources: map[string]any{"bad": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}

	d.RegisterSource(&fakeSource{name: "good", items: []sources.SourceItem{
		{SourceID: "1", Title: "first", Body: "body", State: "open"},
	}})
	d.RegisterSource(&fakeSource{name: "bad", err: errors.New("boom")})

	rep := d.Sync(ctx, "")

	// project A got a pending task inserted for src "1".
	tasks, err := s.ListTasksBySource(ctx, "A", "good")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("project A tasks = %d, want 1", len(tasks))
	}
	if tasks[0].SourceID != "1" || tasks[0].Status != "pending" {
		t.Errorf("task = {src=%q status=%q}, want {src=1 status=pending}", tasks[0].SourceID, tasks[0].Status)
	}
	if got := rep.PerSource["good"]; got == nil || got.Inserted != 1 {
		t.Errorf("good result = %+v, want Inserted=1", got)
	}
	// bad source error recorded + isolated (did not abort good).
	if got := rep.PerSource["bad"]; got == nil || got.Error == "" {
		t.Errorf("bad result = %+v, want non-empty Error", got)
	}

	// Sync recorded per-source state: the "good" source has a non-zero
	// last-sync timestamp and its counts mirror the report.
	status := d.SourceStatus()
	if e, ok := status["good"]; !ok || e.LastSyncUnix == 0 {
		t.Errorf("SourceStatus[good] = %+v, want non-zero LastSyncUnix", e)
	} else if e.Inserted != 1 {
		t.Errorf("SourceStatus[good].Inserted = %d, want 1", e.Inserted)
	}
}

func TestDueSources(t *testing.T) {
	now := time.Unix(1000, 0)
	intervals := map[string]time.Duration{"inbox": time.Minute, "github": 30 * time.Minute, "off": 0}
	last := map[string]time.Time{"inbox": now.Add(-2 * time.Minute), "github": now.Add(-5 * time.Minute)}
	due := dueSources(now, last, intervals)
	has := func(n string) bool {
		for _, d := range due {
			if d == n {
				return true
			}
		}
		return false
	}
	if !has("inbox") {
		t.Error("inbox overdue -> due")
	}
	if has("github") {
		t.Error("github not yet due")
	}
	if has("off") {
		t.Error("interval 0 -> never due")
	}
}

func TestDueSourcesNeverSyncedIsDue(t *testing.T) {
	due := dueSources(time.Unix(1000, 0), map[string]time.Time{}, map[string]time.Duration{"inbox": time.Minute})
	if len(due) != 1 || due[0] != "inbox" {
		t.Fatalf("never-synced should be due, got %v", due)
	}
}

func TestSyncClosesAbsentThenUntouchedAfterStart(t *testing.T) {
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "A", Slug: "a", Name: "A", Status: "active",
		Sources: map[string]any{"good": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{name: "good", items: []sources.SourceItem{
		{SourceID: "1", Title: "first", Body: "body", State: "open"},
	}}
	d.RegisterSource(src)

	// First sync inserts the pending task.
	rep := d.Sync(ctx, "")
	if rep.PerSource["good"].Inserted != 1 {
		t.Fatalf("first sync Inserted = %d, want 1", rep.PerSource["good"].Inserted)
	}

	// Second sync: source returns no items -> pending task becomes source_closed.
	src.items = nil
	rep = d.Sync(ctx, "")
	if rep.PerSource["good"].Closed != 1 {
		t.Fatalf("second sync Closed = %d, want 1", rep.PerSource["good"].Closed)
	}
	tasks, err := s.ListTasksBySource(ctx, "A", "good")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != "source_closed" {
		t.Fatalf("task status = %q, want source_closed", tasks[0].Status)
	}
}

func TestSyncRunningTaskUntouchedWhenAbsent(t *testing.T) {
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "A", Slug: "a", Name: "A", Status: "active",
		Sources: map[string]any{"good": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}

	// Seed a task already in "running" state with a source_id absent upstream.
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-running", ProjectID: "A", Source: "good", SourceID: "99",
		Title: "in flight", Body: "x", Status: "running", Pipeline: "build",
	}); err != nil {
		t.Fatal(err)
	}

	// Source returns no items -> running task must NOT be closed.
	d.RegisterSource(&fakeSource{name: "good", items: nil})
	rep := d.Sync(ctx, "")
	if rep.PerSource["good"] != nil && rep.PerSource["good"].Closed != 0 {
		t.Errorf("Closed = %d, want 0 (running task untouched)", rep.PerSource["good"].Closed)
	}
	got, err := s.GetTask(ctx, "task-running")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Errorf("running task status = %q, want running", got.Status)
	}
}

func TestMarkSyncedAdvancesPollClock(t *testing.T) {
	// A registered-but-unbound source must record a sync ATTEMPT so the poll
	// loop doesn't re-fire it every tick.
	d := newSyncTestDaemon(t)
	d.markSynced("github")
	st := d.SourceStatus()
	if st["github"].LastSyncUnix == 0 {
		t.Fatal("markSynced should stamp last-sync for an attempted (unbound) source")
	}
	// And it should no longer be "due" within its interval.
	ls := map[string]time.Time{"github": time.Unix(st["github"].LastSyncUnix, 0)}
	if due := dueSources(time.Unix(st["github"].LastSyncUnix, 0).Add(time.Second), ls, map[string]time.Duration{"github": time.Hour}); len(due) != 0 {
		t.Errorf("just-synced source should not be due, got %v", due)
	}
}

func TestSyncerLinearDedupSkipsLinkedGitHubTwin(t *testing.T) {
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	// Project bound to BOTH github (repo rohilrs/Hive) and linear.
	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{
			"github": map[string]any{"repo": "rohilrs/Hive"},
			"linear": map[string]any{"teams": []any{"HBA"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Pre-seed a github task already imported for issue #42 — the gh source
	// writes a BARE number into source_id.
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-gh-42", ProjectID: "P", Source: "github", SourceID: "42",
		Title: "from gh", Body: "x", Status: "pending", Pipeline: "build",
	}); err != nil {
		t.Fatal(err)
	}

	// Linear source returns one issue whose LinkedGitHub points at the SAME
	// repo + the same issue number.
	d.RegisterSource(&fakeSource{name: "linear", items: []sources.SourceItem{
		{
			SourceID: "lin-HBA-42", Title: "Linear twin", Body: "y", State: "open",
			LinkedGitHub: &sources.LinkedGitHubRef{
				Owner: "rohilrs", Repo: "Hive", IssueNum: 42,
				URL: "https://github.com/rohilrs/Hive/issues/42",
			},
		},
	}})

	rep := d.Sync(ctx, "linear")

	if got := rep.PerSource["linear"]; got == nil || got.Inserted != 0 {
		t.Fatalf("linear Inserted = %+v, want 0 (dedup hit)", got)
	}
	// Only the original gh task should exist; no Linear shadow.
	ghTasks, _ := s.ListTasksBySource(ctx, "P", "github")
	linTasks, _ := s.ListTasksBySource(ctx, "P", "linear")
	if len(ghTasks) != 1 {
		t.Errorf("github tasks = %d, want 1", len(ghTasks))
	}
	if len(linTasks) != 0 {
		t.Errorf("linear tasks = %d, want 0 (deduped)", len(linTasks))
	}
}

func TestSyncerLinearDedupKeepsDifferentRepo(t *testing.T) {
	// The gh source writes a bare issue number as source_id (e.g. "42").
	// That number is only globally unique within a single repo, so a Linear
	// issue linking to issue #42 in a DIFFERENT repo than the project's gh
	// binding must NOT be deduped — it's different work.
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{
			"github": map[string]any{"repo": "rohilrs/Hive"},
			"linear": map[string]any{"teams": []any{"HBA"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-gh-42", ProjectID: "P", Source: "github", SourceID: "42",
		Title: "from gh (Hive#42)", Body: "x", Status: "pending", Pipeline: "build",
	}); err != nil {
		t.Fatal(err)
	}

	d.RegisterSource(&fakeSource{name: "linear", items: []sources.SourceItem{
		{
			SourceID: "lin-OTHER-42", Title: "Linear in other repo", Body: "z", State: "open",
			LinkedGitHub: &sources.LinkedGitHubRef{
				Owner: "otheruser", Repo: "otherrepo", IssueNum: 42,
				URL: "https://github.com/otheruser/otherrepo/issues/42",
			},
		},
	}})

	rep := d.Sync(ctx, "linear")

	if got := rep.PerSource["linear"]; got == nil || got.Inserted != 1 {
		t.Fatalf("linear Inserted = %+v, want 1 (different repo, no dedup)", got)
	}
}

func TestSyncerLinearIngestsItemWithoutLinkedGitHub(t *testing.T) {
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{
			"github": map[string]any{"repo": "rohilrs/Hive"},
			"linear": map[string]any{"teams": []any{"HBA"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Pre-existing gh task — but the Linear item has no LinkedGitHub so it
	// can't possibly be a dup of any gh task.
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-gh-1", ProjectID: "P", Source: "github", SourceID: "1",
		Title: "irrelevant gh", Status: "pending", Pipeline: "build",
	}); err != nil {
		t.Fatal(err)
	}
	d.RegisterSource(&fakeSource{name: "linear", items: []sources.SourceItem{
		{SourceID: "lin-x", Title: "linear only", Body: "b", State: "open"},
	}})

	rep := d.Sync(ctx, "linear")
	if got := rep.PerSource["linear"]; got == nil || got.Inserted != 1 {
		t.Fatalf("linear Inserted = %+v, want 1", got)
	}
}

func TestSyncerInsertPersistsMetadata(t *testing.T) {
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{"linear": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}
	d.RegisterSource(&fakeSource{name: "linear", items: []sources.SourceItem{
		{
			SourceID: "lin-42", Title: "with metadata", Body: "b", State: "open",
			Metadata: map[string]string{
				"branch_name": "rohil/HBA-42-add-login",
				"external_id": "HBA-42",
			},
		},
	}})

	rep := d.Sync(ctx, "linear")
	if rep.PerSource["linear"].Inserted != 1 {
		t.Fatalf("Inserted = %d, want 1", rep.PerSource["linear"].Inserted)
	}
	tasks, err := s.ListTasksBySource(ctx, "P", "linear")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	got := tasks[0].Metadata
	if got["branch_name"] != "rohil/HBA-42-add-login" {
		t.Errorf("branch_name = %v, want rohil/HBA-42-add-login", got["branch_name"])
	}
	if got["external_id"] != "HBA-42" {
		t.Errorf("external_id = %v, want HBA-42", got["external_id"])
	}
}

func TestSyncerUpdateMergesMetadata(t *testing.T) {
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{"linear": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}

	// Seed an existing pending task with initial metadata.
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-lin", ProjectID: "P", Source: "linear", SourceID: "lin-42",
		Title: "old title", Body: "old body", Status: "pending", Pipeline: "build",
		Metadata: map[string]any{
			"external_id": "HBA-42",
			"branch_name": "old",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Source returns an updated payload (title/body changed -> OpUpdate)
	// with a Metadata map that ONLY mentions branch_name + a brand-new key.
	// The existing external_id key must be preserved, branch_name must be
	// overwritten, and linked_github_url must be added.
	d.RegisterSource(&fakeSource{name: "linear", items: []sources.SourceItem{
		{
			SourceID: "lin-42", Title: "new title", Body: "new body", State: "open",
			Metadata: map[string]string{
				"branch_name":       "new",
				"linked_github_url": "https://github.com/rohilrs/Hive/issues/42",
			},
		},
	}})

	rep := d.Sync(ctx, "linear")
	if rep.PerSource["linear"].Updated != 1 {
		t.Fatalf("Updated = %d, want 1", rep.PerSource["linear"].Updated)
	}

	got, err := s.GetTask(ctx, "task-lin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["external_id"] != "HBA-42" {
		t.Errorf("external_id = %v, want preserved HBA-42", got.Metadata["external_id"])
	}
	if got.Metadata["branch_name"] != "new" {
		t.Errorf("branch_name = %v, want new", got.Metadata["branch_name"])
	}
	if got.Metadata["linked_github_url"] != "https://github.com/rohilrs/Hive/issues/42" {
		t.Errorf("linked_github_url = %v, want added", got.Metadata["linked_github_url"])
	}
}

func TestProjectGitHubRepoExtractsBindingShape(t *testing.T) {
	// Mirrors what scanProject produces: sources is a map[string]any whose
	// "github" value is itself a map[string]any with "repo".
	cases := []struct {
		name string
		p    *store.Project
		want string
	}{
		{"happy path", &store.Project{Sources: map[string]any{"github": map[string]any{"repo": "rohilrs/Hive"}}}, "rohilrs/Hive"},
		{"no sources", &store.Project{Sources: nil}, ""},
		{"no github binding", &store.Project{Sources: map[string]any{"linear": map[string]any{}}}, ""},
		{"github binding is not an object", &store.Project{Sources: map[string]any{"github": "rohilrs/Hive"}}, ""},
		{"repo missing", &store.Project{Sources: map[string]any{"github": map[string]any{"labels": []any{"x"}}}}, ""},
		{"repo not a string", &store.Project{Sources: map[string]any{"github": map[string]any{"repo": 42}}}, ""},
		{"nil project", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectGitHubRepo(tc.p); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- Reverse-direction dedup: gh sync skips items already covered by Linear ---
//
// These mirror the three "Linear dedup skips/keeps" tests above but in the
// opposite direction. Together they make cross-source dedup symmetric: order
// of sync no longer matters.

func TestSyncerGitHubDedupSkipsLinkedTwin(t *testing.T) {
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	// Project bound to BOTH github (repo rohilrs/Hive) and linear.
	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{
			"github": map[string]any{"repo": "rohilrs/Hive"},
			"linear": map[string]any{"teams": []any{"HBA"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Pre-seed a Linear task that already references gh issue #42 via
	// metadata.linked_github_url. gh sync should skip an incoming #42.
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-lin-HBA-42", ProjectID: "P", Source: "linear", SourceID: "lin-HBA-42",
		Title: "from Linear", Body: "x", Status: "pending", Pipeline: "build",
		Metadata: map[string]any{
			"linked_github_url": "https://github.com/rohilrs/Hive/issues/42",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// gh source returns two issues: #42 (a Linear twin) and #99 (new).
	d.RegisterSource(&fakeSource{name: "github", items: []sources.SourceItem{
		{SourceID: "42", Title: "gh twin", Body: "y", State: "open"},
		{SourceID: "99", Title: "gh fresh", Body: "z", State: "open"},
	}})

	rep := d.Sync(ctx, "github")

	if got := rep.PerSource["github"]; got == nil || got.Inserted != 1 {
		t.Fatalf("github Inserted = %+v, want 1 (twin deduped, fresh inserted)", got)
	}
	ghTasks, _ := s.ListTasksBySource(ctx, "P", "github")
	if len(ghTasks) != 1 || ghTasks[0].SourceID != "99" {
		t.Errorf("github tasks = %+v, want 1 task with source_id=99", ghTasks)
	}
}

func TestSyncerGitHubDedupKeepsDifferentRepo(t *testing.T) {
	// Linear task references a github URL pointing at a DIFFERENT repo than
	// the project's gh binding. A gh sync that returns issue #42 in the bound
	// repo is NOT a dup — different repo = different work item.
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{
			"github": map[string]any{"repo": "rohilrs/Hive"},
			"linear": map[string]any{"teams": []any{"HBA"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-lin-OTHER", ProjectID: "P", Source: "linear", SourceID: "lin-OTHER-42",
		Title: "from Linear (other repo)", Body: "x", Status: "pending", Pipeline: "build",
		Metadata: map[string]any{
			"linked_github_url": "https://github.com/otheruser/otherrepo/issues/42",
		},
	}); err != nil {
		t.Fatal(err)
	}
	d.RegisterSource(&fakeSource{name: "github", items: []sources.SourceItem{
		{SourceID: "42", Title: "gh issue 42 in bound repo", Body: "y", State: "open"},
	}})

	rep := d.Sync(ctx, "github")
	if got := rep.PerSource["github"]; got == nil || got.Inserted != 1 {
		t.Fatalf("github Inserted = %+v, want 1 (different repo — not a dup)", got)
	}
}

func TestSyncerGitHubDedupKeepsItemWhenLinearMetadataAbsent(t *testing.T) {
	// Linear task has no metadata at all — gh sync proceeds normally.
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{
			"github": map[string]any{"repo": "rohilrs/Hive"},
			"linear": map[string]any{"teams": []any{"HBA"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Insert sets Metadata to {} (InsertTask defaults nil → empty map), so
	// linked_github_url lookup misses and the gh item passes through.
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-lin-noMeta", ProjectID: "P", Source: "linear", SourceID: "lin-x",
		Title: "linear, no metadata", Status: "pending", Pipeline: "build",
	}); err != nil {
		t.Fatal(err)
	}
	d.RegisterSource(&fakeSource{name: "github", items: []sources.SourceItem{
		{SourceID: "42", Title: "fresh gh", Body: "y", State: "open"},
	}})

	rep := d.Sync(ctx, "github")
	if got := rep.PerSource["github"]; got == nil || got.Inserted != 1 {
		t.Fatalf("github Inserted = %+v, want 1 (no Linear ref to dedup against)", got)
	}
}

func TestSyncerGitHubDedupKeepsItemWhenLinearTaskHasNoLinkedURL(t *testing.T) {
	// Linear task has metadata but no linked_github_url key. gh sync should
	// still ingest its items — the dedup lookup misses and they pass through.
	ctx := context.Background()
	d := newSyncTestDaemon(t)
	s := d.Store()

	if err := s.InsertProject(ctx, &store.Project{
		ID: "P", Slug: "p", Name: "P", Status: "active",
		Sources: map[string]any{
			"github": map[string]any{"repo": "rohilrs/Hive"},
			"linear": map[string]any{"teams": []any{"HBA"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &store.Task{
		ID: "task-lin-otherMeta", ProjectID: "P", Source: "linear", SourceID: "lin-y",
		Title: "linear, other metadata", Status: "pending", Pipeline: "build",
		Metadata: map[string]any{
			"branch_name": "rohil/HBA-9-something",
			"identifier":  "HBA-9",
		},
	}); err != nil {
		t.Fatal(err)
	}
	d.RegisterSource(&fakeSource{name: "github", items: []sources.SourceItem{
		{SourceID: "42", Title: "fresh gh", Body: "y", State: "open"},
	}})

	rep := d.Sync(ctx, "github")
	if got := rep.PerSource["github"]; got == nil || got.Inserted != 1 {
		t.Fatalf("github Inserted = %+v, want 1 (no linked_github_url to dedup against)", got)
	}
}
