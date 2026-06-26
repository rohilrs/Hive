package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

func applyParams(slug, phase string, subs []decompose.ProposedSubtask) json.RawMessage {
	p, _ := json.Marshal(RoadmapDecomposeApplyParams{
		ProjectSlug: slug, Phase: phase, PhaseTitle: "P", RoadmapPath: "/r.md",
		SpecPath: "spec.md", Subtasks: subs,
	})
	return p
}

func TestRoadmapApply_NewTaskInserts(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")

	_, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "new work", Body: "b", Priority: "P1", Pipeline: "build"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	tasks, _ := d.store.ListPendingTasksByProject(context.Background(), proj.ID)
	if len(tasks) != 1 || tasks[0].Title != "new work" {
		t.Fatalf("want 1 new task, got %v", tasks)
	}
	if ph, _ := tasks[0].Metadata["roadmap_phase"].(string); ph != "1" {
		t.Errorf("phase metadata not stamped: %v", tasks[0].Metadata)
	}
	if len(fw.createdFor) != 1 {
		t.Errorf("new task should mirror to Linear; createdFor=%v", fw.createdFor)
	}
}

func TestRoadmapApply_HiveMergeRewritesAndPushes(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t-old", ProjectID: proj.ID, Source: "linear", SourceID: "iss-9",
		Title: "old title", Body: "old", Status: "pending", Pipeline: "build", Priority: "P1",
	})

	_, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "merged title", Body: "merged", Priority: "P1", Pipeline: "build", MergeFrom: "hive:t-old"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	got, _ := d.store.GetTask(context.Background(), "t-old")
	if got.Title != "merged title" {
		t.Errorf("hive merge should rewrite title; got %q", got.Title)
	}
	if ph, _ := got.Metadata["roadmap_phase"].(string); ph != "1" {
		t.Errorf("phase not stamped on merged task: %v", got.Metadata)
	}
	if len(fw.contentCalls) != 1 || fw.contentCalls[0] != "iss-9:merged title" {
		t.Errorf("merged content should push to Linear; contentCalls=%v", fw.contentCalls)
	}
	tasks, _ := d.store.ListPendingTasksByProject(context.Background(), proj.ID)
	if len(tasks) != 1 {
		t.Errorf("hive merge must not create a duplicate; got %d tasks", len(tasks))
	}
}

// TestRoadmapApply_HiveMergeBackfillsLinearBranchName: a Linear-sourced task
// reaching the merge path WITHOUT branch_name must get Linear's canonical branch
// backfilled — otherwise its worktree falls back to hive/run-<id>/<title>
// instead of rohilrshah/hba-NN (the naming drift seen on the Phase-4 tasks).
func TestRoadmapApply_HiveMergeBackfillsLinearBranchName(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{fetchIdentifier: "HBA-9", fetchBranch: "rohilrshah/hba-9-old-title"}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t-old", ProjectID: proj.ID, Source: "linear", SourceID: "iss-9",
		Title: "old title", Body: "old", Status: "pending", Pipeline: "build", Priority: "P1",
		// no branch_name in metadata — must be backfilled from Linear on merge.
	})

	_, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "merged", Body: "m", Priority: "P1", Pipeline: "build", MergeFrom: "hive:t-old"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	got, _ := d.store.GetTask(context.Background(), "t-old")
	if bn, _ := got.Metadata["branch_name"].(string); bn != "rohilrshah/hba-9-old-title" {
		t.Errorf("branch_name not backfilled on merge; got %q, metadata=%v", bn, got.Metadata)
	}
	if len(fw.fetchMetaFor) != 1 || fw.fetchMetaFor[0] != "iss-9" {
		t.Errorf("expected one FetchIssueMeta(iss-9); got %v", fw.fetchMetaFor)
	}
}

func TestRoadmapApply_LinearMergePullsAndPushes(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")

	_, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "pulled+merged", Body: "m", Priority: "P1", Pipeline: "build", MergeFrom: "linear:iss-77"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	tasks, _ := d.store.ListTasksBySource(context.Background(), proj.ID, "linear")
	var found *store.Task
	for i := range tasks {
		if tasks[i].SourceID == "iss-77" {
			found = &tasks[i]
		}
	}
	if found == nil {
		t.Fatal("linear merge should create a task with source_id=iss-77 (the pull)")
	}
	if found.Title != "pulled+merged" {
		t.Errorf("pulled task title=%q want merged", found.Title)
	}
	if len(fw.contentCalls) != 1 || fw.contentCalls[0] != "iss-77:pulled+merged" {
		t.Errorf("pulled+merged content should push to Linear; contentCalls=%v", fw.contentCalls)
	}
	if len(fw.createdFor) != 0 {
		t.Errorf("pull must not CreateIssue; createdFor=%v", fw.createdFor)
	}
}

// best-effort: a Linear push failure must NOT fail the Hive-side merge.
func TestRoadmapApply_HiveMergeSucceedsWhenLinearPushFails(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{contentErr: errors.New("linear 500")}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t-old", ProjectID: proj.ID, Source: "linear", SourceID: "iss-9",
		Title: "old", Body: "old", Status: "pending", Pipeline: "build", Priority: "P1",
	})
	out, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "merged", Body: "m", Priority: "P1", Pipeline: "build", MergeFrom: "hive:t-old"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var res RoadmapDecomposeApplyResult
	_ = json.Unmarshal(out, &res)
	if res.Merged != 1 {
		t.Errorf("merge must complete despite Linear push failure; Merged=%d errors=%v", res.Merged, res.Errors)
	}
	got, _ := d.store.GetTask(context.Background(), "t-old")
	if got.Title != "merged" {
		t.Errorf("title should be rewritten even when Linear push fails; got %q", got.Title)
	}
}

// linear:<uuid> whose issue is already a Hive task must reroute to a hive-merge
// (rewrite the existing task), NOT create a duplicate.
func TestRoadmapApply_AlreadyPulledReroutesToMerge(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t-mirror", ProjectID: proj.ID, Source: "linear", SourceID: "iss-55",
		Title: "old", Body: "old", Status: "pending", Pipeline: "build", Priority: "P1",
	})
	_, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "merged", Body: "m", Priority: "P1", Pipeline: "build", MergeFrom: "linear:iss-55"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	tasks, _ := d.store.ListPendingTasksByProject(context.Background(), proj.ID)
	if len(tasks) != 1 {
		t.Errorf("already-pulled reroute must not create a duplicate; got %d tasks", len(tasks))
	}
	got, _ := d.store.GetTask(context.Background(), "t-mirror")
	if got.Title != "merged" {
		t.Errorf("existing mirrored task should be rewritten; got %q", got.Title)
	}
}

// malformed merge_from records an error, creates nothing.
func TestRoadmapApply_BadMergeFromRecordsError(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	d.SetLinearWriter(&fakeLinearWriter{})
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	out, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "x", Body: "b", Priority: "P1", Pipeline: "build", MergeFrom: "bogus:xyz"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var res RoadmapDecomposeApplyResult
	_ = json.Unmarshal(out, &res)
	if len(res.Errors) == 0 {
		t.Error("bad merge_from should record an error")
	}
	tasks, _ := d.store.ListPendingTasksByProject(context.Background(), proj.ID)
	if len(tasks) != 0 {
		t.Errorf("bad merge_from must not create a task; got %d", len(tasks))
	}
}

// applyLinearPull must link the pulled issue to its phase's milestone when the
// roadmap is mirrored. Best-effort: the link is attempted after insert+content
// push; a failure logs but never fails the apply.
func TestRoadmapApply_LinearPullLinksMilestone(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	// seed the mirror map so phase 2a has a milestone id (persisted on the project)
	if err := d.saveMirrorState(context.Background(), proj, mirrorState{
		DocumentID: "doc-1", Milestones: map[string]string{"2a": "ms-2a"},
	}); err != nil {
		t.Fatal(err)
	}

	_, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "2a",
		[]decompose.ProposedSubtask{{Title: "t", Body: "b", Priority: "P1", Pipeline: "build", MergeFrom: "linear:iss-88"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	found := false
	for _, l := range fw.issueMilestoneLinks {
		if l == "iss-88:ms-2a" {
			found = true
		}
	}
	if !found {
		t.Errorf("pulled issue should link to phase 2a milestone; got %v", fw.issueMilestoneLinks)
	}
}

// applyHiveMerge must publish EventTaskUpdated so a live TUI sees the rewritten
// title without a full re-fetch.
func TestRoadmapApply_HiveMergePublishesUpdate(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	d.SetLinearWriter(&fakeLinearWriter{})
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t-old", ProjectID: proj.ID, Source: "inbox",
		Title: "old", Body: "old", Status: "pending", Pipeline: "build", Priority: "P1",
	})

	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	_, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "merged", Body: "m", Priority: "P1", Pipeline: "build", MergeFrom: "hive:t-old"}}))
	if rerr != nil {
		t.Fatal(rerr)
	}

	// Drain the bus until we see EventTaskUpdated for "t-old" with title "merged".
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != rpc.EventTaskUpdated {
				continue
			}
			gotID, _ := ev.Data["task_id"].(string)
			gotTitle, _ := ev.Data["title"].(string)
			if gotID == "t-old" && gotTitle == "merged" {
				return // test passes
			}
		case <-deadline:
			t.Fatal("no EventTaskUpdated for task_id=t-old with title=merged published within 2s")
		}
	}
}

// TestRoadmapApply_LinearPullStampsMeta covers the metadata-enrichment fix: a
// freshly-pulled Linear issue gets external_id + branch_name stamped (from a
// FetchIssueMeta re-fetch), not just roadmap metadata.
func TestRoadmapApply_LinearPullStampsMeta(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{fetchIdentifier: "HBA-99", fetchBranch: "rohil/hba-99-thing"}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")

	if _, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "pulled", Body: "b", Priority: "P1", Pipeline: "build", MergeFrom: "linear:iss-88"}})); rerr != nil {
		t.Fatal(rerr)
	}
	tasks, _ := d.store.ListTasksBySource(context.Background(), proj.ID, "linear")
	var found *store.Task
	for i := range tasks {
		if tasks[i].SourceID == "iss-88" {
			found = &tasks[i]
		}
	}
	if found == nil {
		t.Fatal("expected a pulled task with source_id=iss-88")
	}
	if found.Metadata["external_id"] != "HBA-99" {
		t.Errorf("external_id=%v, want HBA-99 (the Linear identifier)", found.Metadata["external_id"])
	}
	if found.Metadata["branch_name"] != "rohil/hba-99-thing" {
		t.Errorf("branch_name=%v, want the canonical Linear branch", found.Metadata["branch_name"])
	}
	if len(fw.fetchMetaFor) != 1 || fw.fetchMetaFor[0] != "iss-88" {
		t.Errorf("FetchIssueMeta called for %v, want [iss-88]", fw.fetchMetaFor)
	}
}

func TestRoadmapApplyPersistsRelevantFiles(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo-rf", "HBA", "proj-rf-uuid")

	params := applyParams("demo-rf", "1", []decompose.ProposedSubtask{
		{Title: "t1", Body: "b1", Priority: "P1", Pipeline: "build", RelevantFiles: []string{"src/a.ts", "src/b.ts"}},
	})
	_, rerr := srv.handleRoadmapDecomposeApply(context.Background(), params)
	if rerr != nil {
		t.Fatalf("apply: %v", rerr)
	}

	tasks, err := d.store.ListPendingTasksByProject(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("ListPendingTasksByProject: %v", err)
	}
	var found *store.Task
	for _, task := range tasks {
		if task.Title == "t1" {
			found = task
			break
		}
	}
	if found == nil {
		t.Fatal("inserted task t1 not found")
	}
	if got, _ := found.Metadata["relevant_files"].(string); got != "src/a.ts,src/b.ts" {
		t.Errorf("relevant_files metadata = %q, want \"src/a.ts,src/b.ts\"", got)
	}
}

// TestRoadmapApply_LinearPullMetaFetchFailsSoft: a FetchIssueMeta error must not
// abort the pull — the task is still created (without the enriched metadata).
func TestRoadmapApply_LinearPullMetaFetchFailsSoft(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{fetchMetaErr: errors.New("linear unreachable")}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")

	if _, rerr := srv.handleRoadmapDecomposeApply(context.Background(), applyParams("demo", "1",
		[]decompose.ProposedSubtask{{Title: "pulled", Body: "b", Priority: "P1", Pipeline: "build", MergeFrom: "linear:iss-77"}})); rerr != nil {
		t.Fatal(rerr)
	}
	tasks, _ := d.store.ListTasksBySource(context.Background(), proj.ID, "linear")
	if len(tasks) == 0 {
		t.Fatal("pull must still create the task even when meta fetch fails")
	}
}
