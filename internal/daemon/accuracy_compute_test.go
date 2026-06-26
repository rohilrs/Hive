package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/store"
)

// initGitRepoWithFile creates a git repo at dir with an initial commit
// containing the given files (each with placeholder content). Returns dir.
func initGitRepoWithFile(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, dir, "git", "init", "-b", "main")
	mustRun(t, dir, "git", "config", "user.email", "test@example.com")
	mustRun(t, dir, "git", "config", "user.name", "Test")
	for _, f := range files {
		path := filepath.Join(dir, f)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte("// "+f+"\n"), 0o644)
	}
	mustRun(t, dir, "git", "add", ".")
	mustRun(t, dir, "git", "commit", "-m", "initial")
	return dir
}

// editFilesOnBranch creates a new branch, edits the given files (appends a
// line), commits.
func editFilesOnBranch(t *testing.T, dir, branch string, files ...string) {
	t.Helper()
	mustRun(t, dir, "git", "checkout", "-b", branch)
	for _, f := range files {
		path := filepath.Join(dir, f)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		// Append, so if file exists it grows; if not, it's created.
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fh.WriteString("// edited\n")
		_ = fh.Close()
	}
	mustRun(t, dir, "git", "add", ".")
	mustRun(t, dir, "git", "commit", "-m", "edit")
}

func mustRun(t *testing.T, dir, cmd string, args ...string) {
	t.Helper()
	c := exec.Command(cmd, args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", cmd, args, err, out)
	}
}

func TestTouchedFilesReturnsGitDiffNames(t *testing.T) {
	dir := initGitRepoWithFile(t, "a.go", "b.go", "c.go")
	editFilesOnBranch(t, dir, "feature", "a.go", "b.go")

	got, err := touchedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("touchedFiles: %v", err)
	}
	gotSet := map[string]bool{}
	for _, f := range got {
		gotSet[f] = true
	}
	if !gotSet["a.go"] || !gotSet["b.go"] || gotSet["c.go"] {
		t.Errorf("got %v, want exactly a.go + b.go", got)
	}
}

// TestTouchedFilesCapturesUncommittedEdits is the regression guard
// for the 2c.1 review bug: the original `main..HEAD` diff range
// returned empty when the worker edited files but didn't commit
// (which is the actual Hive build-pipeline behavior — pipelines
// don't commit per iteration). The current `git diff --name-only main`
// catches both committed AND uncommitted edits.
func TestTouchedFilesCapturesUncommittedEdits(t *testing.T) {
	dir := initGitRepoWithFile(t, "a.go", "b.go")
	// Branch + edit + DO NOT COMMIT (simulating worker mid-iteration).
	mustRun(t, dir, "git", "checkout", "-b", "feature")
	path := filepath.Join(dir, "a.go")
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = fh.WriteString("// uncommitted edit\n")
	_ = fh.Close()

	got, err := touchedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("touchedFiles: %v", err)
	}
	if len(got) != 1 || got[0] != "a.go" {
		t.Errorf("got %v, want [a.go] (uncommitted edit must be detected)", got)
	}
}

func TestComputeAndPersistAccuracySkipsNoPrediction(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "x", Status: "pending"})
	_ = d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "done"})

	// nil pred → skip with reason "no_prediction".
	computeAndPersistAccuracy(ctx, d.store, "r1", nil, "/nonexistent")

	got, err := d.store.GetPredictorAccuracy(ctx, "r1")
	if err != nil {
		t.Fatalf("GetPredictorAccuracy: %v", err)
	}
	if got.SkippedReason != "no_prediction" {
		t.Errorf("SkippedReason=%q want no_prediction", got.SkippedReason)
	}
	if got.Precision != nil || got.Recall != nil {
		t.Errorf("expected NULL precision/recall on skip")
	}
}

func TestComputeAndPersistAccuracySkipsNoPredictionsFiles(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "x", Status: "pending"})
	_ = d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "done"})

	pred := &predictor.Result{Files: []string{}}
	computeAndPersistAccuracy(ctx, d.store, "r1", pred, t.TempDir())

	got, _ := d.store.GetPredictorAccuracy(ctx, "r1")
	if got.SkippedReason != "no_predictions_files" {
		t.Errorf("SkippedReason=%q want no_predictions_files", got.SkippedReason)
	}
}

func TestComputeAndPersistAccuracyEndToEnd(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "x", Status: "pending"})
	_ = d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "done"})

	dir := initGitRepoWithFile(t, "a.go", "b.go", "c.go")
	editFilesOnBranch(t, dir, "feature", "a.go", "b.go")

	// Predicted: a.go, b.go, x.go (one miss). Touched (per git diff): a.go, b.go.
	// Precision = 2/3, Recall = 2/2 = 1.0.
	pred := &predictor.Result{Files: []string{"a.go", "b.go", "x.go"}}
	computeAndPersistAccuracy(ctx, d.store, "r1", pred, dir)

	got, err := d.store.GetPredictorAccuracy(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Precision == nil || *got.Precision < 0.66 || *got.Precision > 0.67 {
		t.Errorf("Precision=%v want ~0.667", got.Precision)
	}
	if got.Recall == nil || *got.Recall != 1.0 {
		t.Errorf("Recall=%v want 1.0", got.Recall)
	}
	if got.PredictedCount != 3 || got.TouchedCount != 2 || got.IntersectCount != 2 {
		t.Errorf("counts: %+v want 3/2/2", got)
	}
}

func TestComputeAndPersistAccuracySkipsNoEdits(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "x", Status: "pending"})
	_ = d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "done"})

	dir := initGitRepoWithFile(t, "a.go")
	// No branch / no edits → main..HEAD diff is empty.
	pred := &predictor.Result{Files: []string{"a.go"}}
	computeAndPersistAccuracy(ctx, d.store, "r1", pred, dir)

	got, _ := d.store.GetPredictorAccuracy(ctx, "r1")
	if got.SkippedReason != "no_edits" {
		t.Errorf("SkippedReason=%q want no_edits", got.SkippedReason)
	}
}

func TestComputeAndPersistAccuracySkipsNoWorktree(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "x", Status: "pending"})
	_ = d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "done"})

	pred := &predictor.Result{Files: []string{"a.go"}}
	computeAndPersistAccuracy(ctx, d.store, "r1", pred, "/path/that/does/not/exist")

	got, _ := d.store.GetPredictorAccuracy(ctx, "r1")
	if got.SkippedReason != "no_worktree" {
		t.Errorf("SkippedReason=%q want no_worktree", got.SkippedReason)
	}
}
