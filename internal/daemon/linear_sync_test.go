package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/sources"
	"github.com/rohilrs/Hive/internal/store"
)

func TestDeriveLinearState(t *testing.T) {
	cases := []struct {
		name   string
		status string
		gate   string
		want   string
	}{
		{"pending->todo", "pending", sequence.GateNone, "todo"},
		{"running->in_progress", "running", sequence.GateNone, "in_progress"},
		{"awaiting_merge->in_review", "running", sequence.GateAwaitingMerge, "in_review"},
		{"pr_open->in_review", "running", sequence.GatePROpen, "in_review"},
		{"satisfied->done", "running", sequence.GateSatisfied, "done"},
		{"needs_attention->blocked", "needs_attention", sequence.GateAwaitingMerge, "blocked"},
		{"abandoned->canceled", "abandoned", sequence.GateNone, "canceled"},
		{"done-no-pr->done", "done", sequence.GateNone, "done"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveLinearState(&store.Task{Status: c.status, GateState: c.gate})
			if got != c.want {
				t.Errorf("derive(status=%s gate=%s) = %q, want %q", c.status, c.gate, got, c.want)
			}
		})
	}
}

// fakeLinearWriter is a test double for linearIssueWriter. It records calls and
// can be configured to fail CreateIssue (to exercise best-effort fallback).
type fakeLinearWriter struct {
	createErr    error
	archiveErr   error
	contentErr   error
	createdFor   []string // titles passed to CreateIssue
	stateCalls   []string // "issueID:logical" per SetIssueState
	archivedFor  []string // issueIDs passed to ArchiveIssue
	contentCalls []string // "issueID:title" per UpdateIssueContent

	createMilestoneErr    error
	updateMilestoneErr    error
	archiveMilestoneErr   error
	setMilestoneErr       error
	docErr                error
	updateDocErr          error
	docCreateSeq          int      // monotonic, drives unique doc ids (survives createdDocs reset)
	createdDocs           []string // titles per CreateDocument
	updatedDocs           []string // docIDs per UpdateDocument
	milestonesCreated     []string // names per CreateProjectMilestone
	milestonesUpdated     []string // "msID:name" per UpdateProjectMilestone
	milestonesArchived    []string // msIDs per ArchiveProjectMilestone
	issueMilestoneLinks   []string // "issueID:msID" per SetIssueMilestone
	lastCreateMilestoneID string   // projectMilestoneID from most recent CreateIssue

	fetchIdentifier string   // returned by FetchIssueMeta
	fetchBranch     string   // returned by FetchIssueMeta
	fetchMetaErr    error    // FetchIssueMeta error
	fetchMetaFor    []string // issueIDs passed to FetchIssueMeta
}

func (f *fakeLinearWriter) FetchIssueMeta(_ context.Context, issueID string) (string, string, error) {
	f.fetchMetaFor = append(f.fetchMetaFor, issueID)
	if f.fetchMetaErr != nil {
		return "", "", f.fetchMetaErr
	}
	return f.fetchIdentifier, f.fetchBranch, nil
}

func (f *fakeLinearWriter) CreateIssue(_ context.Context, teamKey, projectID, title, body, projectMilestoneID string) (string, string, string, error) {
	if f.createErr != nil {
		return "", "", "", f.createErr
	}
	f.createdFor = append(f.createdFor, title)
	f.lastCreateMilestoneID = projectMilestoneID
	return "iss-" + title, "CONV-1", "https://linear/" + title, nil
}

func (f *fakeLinearWriter) CreateDocument(_ context.Context, projectID, title, content string) (string, error) {
	if f.docErr != nil {
		return "", f.docErr
	}
	f.createdDocs = append(f.createdDocs, title)
	f.docCreateSeq++
	return fmt.Sprintf("doc-%d", f.docCreateSeq), nil
}

func (f *fakeLinearWriter) UpdateDocument(_ context.Context, docID, title, content string) error {
	if f.updateDocErr != nil {
		return f.updateDocErr
	}
	f.updatedDocs = append(f.updatedDocs, docID)
	return nil
}

func (f *fakeLinearWriter) CreateProjectMilestone(_ context.Context, projectID, name, description string, sortOrder float64) (string, error) {
	if f.createMilestoneErr != nil {
		return "", f.createMilestoneErr
	}
	f.milestonesCreated = append(f.milestonesCreated, name)
	return "ms-" + name, nil
}

func (f *fakeLinearWriter) UpdateProjectMilestone(_ context.Context, msID, name, description string, sortOrder float64) error {
	if f.updateMilestoneErr != nil {
		return f.updateMilestoneErr
	}
	f.milestonesUpdated = append(f.milestonesUpdated, msID+":"+name)
	return nil
}

func (f *fakeLinearWriter) ArchiveProjectMilestone(_ context.Context, msID string) error {
	if f.archiveMilestoneErr != nil {
		return f.archiveMilestoneErr
	}
	f.milestonesArchived = append(f.milestonesArchived, msID)
	return nil
}

func (f *fakeLinearWriter) SetIssueMilestone(_ context.Context, issueID, milestoneID string) error {
	if f.setMilestoneErr != nil {
		return f.setMilestoneErr
	}
	f.issueMilestoneLinks = append(f.issueMilestoneLinks, issueID+":"+milestoneID)
	return nil
}

func (f *fakeLinearWriter) SetIssueState(_ context.Context, teamKey, issueID, logical string) error {
	f.stateCalls = append(f.stateCalls, issueID+":"+logical)
	return nil
}

func (f *fakeLinearWriter) ArchiveIssue(_ context.Context, issueID string) error {
	if f.archiveErr != nil {
		return f.archiveErr
	}
	f.archivedFor = append(f.archivedFor, issueID)
	return nil
}

func (f *fakeLinearWriter) UpdateIssueContent(_ context.Context, teamKey, issueID, title, body string) error {
	if f.contentErr != nil {
		return f.contentErr
	}
	f.contentCalls = append(f.contentCalls, issueID+":"+title)
	return nil
}

// insertWriteBackProject creates a project with a Linear binding that has
// write_back enabled, via the real store path (InsertProject +
// UpdateProjectSources), and returns the re-fetched project.
func insertWriteBackProject(t *testing.T, d *Daemon, slug, teamKey, projectID string) *store.Project {
	t.Helper()
	ctx := context.Background()
	proj := &store.Project{
		ID:     newID("proj"),
		Slug:   slug,
		Name:   slug,
		Status: "active",
	}
	if err := d.store.InsertProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	binding := map[string]any{
		"linear": map[string]any{
			"teams":      []any{teamKey},
			"projects":   []any{projectID},
			"write_back": true,
		},
	}
	if err := d.store.UpdateProjectSources(ctx, proj.ID, binding); err != nil {
		t.Fatal(err)
	}
	got, err := d.store.GetProjectBySlug(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// addTaskViaRPC drives handleAddTask end-to-end so the mirror branch runs.
func addTaskViaRPC(t *testing.T, srv *RPCServer, slug, title string) {
	t.Helper()
	params, _ := json.Marshal(AddTaskParams{ProjectSlug: slug, Title: title})
	if _, rerr := srv.handleAddTask(context.Background(), json.RawMessage(params)); rerr != nil {
		t.Fatalf("handleAddTask: %v", rerr)
	}
}

// getOnlyTask returns the single task in a project, failing if there is not
// exactly one.
func getOnlyTask(t *testing.T, d *Daemon, projectID string) *store.Task {
	t.Helper()
	tasks, err := d.store.ListTasksByProject(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected exactly 1 task, got %d", len(tasks))
	}
	return tasks[0]
}

func TestMirrorOnCreate(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "conv", "CONV", "57925d22")
	addTaskViaRPC(t, srv, "conv", "Do the thing")
	got := getOnlyTask(t, d, proj.ID)
	if got.Source != "linear" || got.SourceID == "" {
		t.Errorf("task not mirrored: source=%q source_id=%q", got.Source, got.SourceID)
	}
	if got.LinearSyncedState != "todo" {
		t.Errorf("synced_state=%q want todo", got.LinearSyncedState)
	}
	if got.Metadata == nil || got.Metadata["external_id"] != "CONV-1" {
		t.Errorf("external_id metadata=%v want CONV-1", got.Metadata)
	}
	if len(fw.createdFor) != 1 {
		t.Errorf("CreateIssue calls = %d, want 1", len(fw.createdFor))
	}
}

func TestMirrorOnCreate_FailureInsertsLocal(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	d.SetLinearWriter(&fakeLinearWriter{createErr: errors.New("linear down")})
	proj := insertWriteBackProject(t, d, "conv", "CONV", "57925d22")
	addTaskViaRPC(t, srv, "conv", "Do the thing")
	got := getOnlyTask(t, d, proj.ID)
	if got.SourceID != "" || got.Source == "linear" {
		t.Errorf("failed mirror should stay Hive-local: source=%q source_id=%q", got.Source, got.SourceID)
	}
}

func TestOutboxPushesDiff(t *testing.T) {
	d := newTestDaemon(t)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "conv", "CONV", "57925d22")
	// a mirrored task that has been dispatched (running) but synced_state still "todo"
	task := &store.Task{
		ID: "t1", ProjectID: proj.ID, Source: "linear", SourceID: "iss-1",
		Title: "x", Status: "running", Pipeline: "build", GateState: sequence.GateNone,
		LinearSyncedState: "todo",
	}
	if err := d.store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	d.reconcileOnce(context.Background())
	if len(fw.stateCalls) != 1 || fw.stateCalls[0] != "iss-1:in_progress" {
		t.Errorf("stateCalls=%v want [iss-1:in_progress]", fw.stateCalls)
	}
	got, _ := d.store.GetTask(context.Background(), "t1")
	if got.LinearSyncedState != "in_progress" {
		t.Errorf("synced_state=%q want in_progress", got.LinearSyncedState)
	}
	// second pass: no diff -> no extra push
	fw.stateCalls = nil
	d.reconcileOnce(context.Background())
	if len(fw.stateCalls) != 0 {
		t.Errorf("second pass pushed again: %v", fw.stateCalls)
	}
}

func TestBackfillMirrorsPendingUnmirrored(t *testing.T) {
	d := newTestDaemon(t)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "conv", "CONV", "57925d22")
	task := &store.Task{
		ID: "t2", ProjectID: proj.ID, Source: "inbox", SourceID: "",
		Title: "backfill me", Status: "pending", Pipeline: "build",
	}
	if err := d.store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	d.reconcileOnce(context.Background())
	got, _ := d.store.GetTask(context.Background(), "t2")
	if got.Source != "linear" || got.SourceID == "" {
		t.Errorf("backfill did not mirror: source=%q source_id=%q", got.Source, got.SourceID)
	}
	if len(fw.createdFor) != 1 {
		t.Errorf("CreateIssue calls=%d want 1", len(fw.createdFor))
	}
	// backfilled task is pending -> derived state "todo" == synced "todo": no push.
	if len(fw.stateCalls) != 0 {
		t.Errorf("pending backfill should not push state, got %v", fw.stateCalls)
	}
	if got.LinearSyncedState != "todo" {
		t.Errorf("synced_state=%q want todo", got.LinearSyncedState)
	}
}

// TestLoopSafety_NoReingestOrStatusFlip asserts the ingest reconciler leaves a
// dispatched (non-pending) mirrored task alone: the open Linear item matches by
// source_id so it is not re-inserted, and the running task is non-pending so it
// is neither updated nor closed. The result is zero ops.
func TestLoopSafety_NoReingestOrStatusFlip(t *testing.T) {
	task := store.Task{ID: "t1", Source: "linear", SourceID: "iss-1", Title: "x", Status: "running"}
	item := sources.SourceItem{SourceID: "iss-1", Title: "x", State: "open"}
	ops := sources.Reconcile([]store.Task{task}, []sources.SourceItem{item})
	if len(ops) != 0 {
		t.Errorf("running mirrored task should be untouched by reconcile; got ops=%+v", ops)
	}
}

func TestDeriveLinearState_NonSeqPRLifecycle(t *testing.T) {
	// A non-sequenced finish-branch task is status=done once the PR opens; the
	// gate distinguishes In Review (open) from Done (merged).
	inReview := deriveLinearState(&store.Task{Status: "done", GateState: sequence.GateAwaitingMerge})
	if inReview != "in_review" {
		t.Errorf("done+awaiting_merge = %q, want in_review", inReview)
	}
	done := deriveLinearState(&store.Task{Status: "done", GateState: sequence.GateSatisfied})
	if done != "done" {
		t.Errorf("done+satisfied = %q, want done", done)
	}
}

// deleteViaRPC drives handleDeleteTask end-to-end so the archive-mirror branch
// runs. Fails the test on RPC error.
func deleteViaRPC(t *testing.T, srv *RPCServer, taskID string) {
	t.Helper()
	params, _ := json.Marshal(GetTaskParams{TaskID: taskID})
	if _, rerr := srv.handleDeleteTask(context.Background(), json.RawMessage(params)); rerr != nil {
		t.Fatalf("handleDeleteTask: %v", rerr)
	}
}

// TestDeleteArchivesLinearMirror: deleting a mirrored task on a write-back
// project archives its Linear issue (so the syncer can't re-import it) AND
// removes the local row.
func TestDeleteArchivesLinearMirror(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "conv", "CONV", "57925d22")
	addTaskViaRPC(t, srv, "conv", "Do the thing")
	task := getOnlyTask(t, d, proj.ID)

	deleteViaRPC(t, srv, task.ID)

	if len(fw.archivedFor) != 1 || fw.archivedFor[0] != task.SourceID {
		t.Errorf("archivedFor=%v, want [%s]", fw.archivedFor, task.SourceID)
	}
	if _, err := d.store.GetTask(context.Background(), task.ID); err == nil {
		t.Error("task row should be deleted")
	}
}

// TestDeleteNonMirroredTaskSkipsArchive: a Hive-local task (mirror failed →
// stays source=inbox) must NOT trigger an archive call on delete.
func TestDeleteNonMirroredTaskSkipsArchive(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{createErr: errors.New("linear down")}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "conv", "CONV", "57925d22")
	addTaskViaRPC(t, srv, "conv", "local only") // mirror fails → source=inbox
	task := getOnlyTask(t, d, proj.ID)

	deleteViaRPC(t, srv, task.ID)

	if len(fw.archivedFor) != 0 {
		t.Errorf("non-mirrored task should not archive; got %v", fw.archivedFor)
	}
}

// TestDeleteArchiveFailureStillDeletes: archiving is best-effort — a Linear
// failure must not block local deletion.
func TestDeleteArchiveFailureStillDeletes(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{archiveErr: errors.New("linear 500")}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "conv", "CONV", "57925d22")
	addTaskViaRPC(t, srv, "conv", "Do the thing")
	task := getOnlyTask(t, d, proj.ID)

	deleteViaRPC(t, srv, task.ID)

	if _, err := d.store.GetTask(context.Background(), task.ID); err == nil {
		t.Error("task must be deleted even when Linear archive fails")
	}
}

func TestReconcileOnce_MergeToDoneSameTick(t *testing.T) {
	d := newTestDaemon(t)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	d.prGateway = &stubGateway{merged: true, baseRef: "main"}
	proj := insertWriteBackProject(t, d, "conv", "CONV", "57925d22")
	// A mirrored task with an open PR (gate=awaiting_merge), synced as in_review.
	task := &store.Task{
		ID: "m1", ProjectID: proj.ID, Source: "linear", SourceID: "iss-1",
		Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge,
		LinearSyncedState: "in_review",
	}
	if err := d.store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	_ = d.store.InsertRun(context.Background(), &store.Run{ID: "mr", TaskID: "m1", ProjectID: proj.ID, Pipeline: "finish-branch", Status: "done"})
	_ = d.store.SetRunPR(context.Background(), "mr", "https://github.com/o/r/pull/1", 1)

	d.reconcileOnce(context.Background())

	got, _ := d.store.GetTask(context.Background(), "m1")
	if got.GateState != sequence.GateSatisfied {
		t.Errorf("inbox: gate=%q, want satisfied", got.GateState)
	}
	if len(fw.stateCalls) != 1 || fw.stateCalls[0] != "iss-1:done" {
		t.Errorf("outbox same tick: stateCalls=%v, want [iss-1:done]", fw.stateCalls)
	}
}
