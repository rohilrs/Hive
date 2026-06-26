package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/store"
)

func TestMirrorStateRoundTripPreservesBinding(t *testing.T) {
	d := newTestDaemon(t)
	// project with an existing linear binding (teams/projects/write_back)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	// fresh project → empty mirror
	if got := loadMirrorState(proj); got.DocumentID != "" || len(got.Milestones) != 0 {
		t.Fatalf("fresh project should have empty mirror, got %+v", got)
	}
	// save a mirror state
	want := mirrorState{DocumentID: "doc-1", Milestones: map[string]string{"1": "ms-1", "2a": "ms-2a"}}
	if err := d.saveMirrorState(context.Background(), proj, want); err != nil {
		t.Fatal(err)
	}
	// re-fetch from store → mirror persisted AND the original binding preserved
	reloaded, err := d.store.GetProjectBySlug(context.Background(), proj.Slug)
	if err != nil {
		t.Fatal(err)
	}
	got := loadMirrorState(reloaded)
	if got.DocumentID != "doc-1" || got.Milestones["1"] != "ms-1" || got.Milestones["2a"] != "ms-2a" {
		t.Errorf("mirror not round-tripped: %+v", got)
	}
	// the write_back binding must survive (saveMirrorState must not clobber it)
	if _, _, ok := linearWriteTarget(reloaded); !ok {
		t.Error("saveMirrorState clobbered the linear write-back binding")
	}
}

// writeRoadmapFile writes a roadmap markdown into a temp repo and returns the repo root.
func writeRoadmapFile(t *testing.T, slug, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "superpowers", "roadmaps")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

const sampleRoadmap = `# demo roadmap

## Phase 1 — Foundation

First phase summary line.

Spec: [docs/superpowers/specs/p1.md](docs/superpowers/specs/p1.md)

## Phase 2a — Capture

Second phase summary.
`

func TestSyncRoadmap_CreatesDocAndMilestones(t *testing.T) {
	d := newTestDaemon(t)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	root := writeRoadmapFile(t, "demo", sampleRoadmap)
	proj.RepoPath = &root

	if err := d.syncRoadmapToLinear(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	if len(fw.createdDocs) != 1 {
		t.Errorf("want 1 doc created, got %v", fw.createdDocs)
	}
	if len(fw.milestonesCreated) != 2 {
		t.Errorf("want 2 milestones created, got %v", fw.milestonesCreated)
	}
	reloaded, _ := d.store.GetProject(context.Background(), proj.ID)
	m := loadMirrorState(reloaded)
	if m.DocumentID == "" || m.Milestones["1"] == "" || m.Milestones["2a"] == "" {
		t.Errorf("mirror map not persisted: %+v", m)
	}
}

func TestSyncRoadmap_Idempotent(t *testing.T) {
	d := newTestDaemon(t)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	root := writeRoadmapFile(t, "demo", sampleRoadmap)
	proj.RepoPath = &root

	if err := d.syncRoadmapToLinear(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	// second run on the now-populated project: all updates, zero creates.
	reloaded, _ := d.store.GetProject(context.Background(), proj.ID)
	reloaded.RepoPath = &root
	fw.milestonesCreated = nil
	fw.createdDocs = nil
	if err := d.syncRoadmapToLinear(context.Background(), reloaded); err != nil {
		t.Fatal(err)
	}
	if len(fw.createdDocs) != 0 || len(fw.milestonesCreated) != 0 {
		t.Errorf("second sync should create nothing; docs=%v ms=%v", fw.createdDocs, fw.milestonesCreated)
	}
	if len(fw.updatedDocs) == 0 || len(fw.milestonesUpdated) != 2 {
		t.Errorf("second sync should update; docs=%v ms=%v", fw.updatedDocs, fw.milestonesUpdated)
	}
}

func TestSyncRoadmap_ArchivesRemovedPhase(t *testing.T) {
	d := newTestDaemon(t)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	root := writeRoadmapFile(t, "demo", sampleRoadmap)
	proj.RepoPath = &root
	_ = d.syncRoadmapToLinear(context.Background(), proj)

	// Re-save the roadmap WITHOUT phase 2a.
	_ = os.WriteFile(filepath.Join(root, "docs/superpowers/roadmaps/demo.md"),
		[]byte("# demo roadmap\n\n## Phase 1 — Foundation\n\nonly phase 1 now.\n"), 0644)
	reloaded, _ := d.store.GetProject(context.Background(), proj.ID)
	reloaded.RepoPath = &root
	if err := d.syncRoadmapToLinear(context.Background(), reloaded); err != nil {
		t.Fatal(err)
	}
	if len(fw.milestonesArchived) != 1 {
		t.Errorf("removed phase 2a should archive its milestone; got %v", fw.milestonesArchived)
	}
	final, _ := d.store.GetProject(context.Background(), proj.ID)
	if _, ok := loadMirrorState(final).Milestones["2a"]; ok {
		t.Error("archived phase 2a should be dropped from the mirror map")
	}
}

func TestSyncRoadmap_SelfHealsDeletedDocument(t *testing.T) {
	d := newTestDaemon(t)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	root := writeRoadmapFile(t, "demo", sampleRoadmap)
	proj.RepoPath = &root
	// first sync creates the doc
	if err := d.syncRoadmapToLinear(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := d.store.GetProject(context.Background(), proj.ID)
	reloaded.RepoPath = &root
	oldDocID := loadMirrorState(reloaded).DocumentID
	if oldDocID == "" {
		t.Fatal("first sync should have stored a document id")
	}
	// simulate the doc being hand-deleted in Linear: UpdateDocument now fails,
	// CreateDocument succeeds → self-heal should recreate + store a NEW id.
	fw.updateDocErr = errors.New("document not found")
	fw.createdDocs = nil
	if err := d.syncRoadmapToLinear(context.Background(), reloaded); err != nil {
		// best-effort: a returned error is acceptable here since UpdateDocument failed,
		// but the recreate must still have happened. Do NOT t.Fatal on err.
		_ = err
	}
	if len(fw.createdDocs) != 1 {
		t.Errorf("self-heal should recreate the document; createdDocs=%v", fw.createdDocs)
	}
	final, _ := d.store.GetProject(context.Background(), proj.ID)
	newID := loadMirrorState(final).DocumentID
	if newID == "" || newID == oldDocID {
		t.Errorf("self-heal should store a NEW document id; old=%q new=%q", oldDocID, newID)
	}
}

func TestSyncRoadmap_BackfillsIssueLinks(t *testing.T) {
	d := newTestDaemon(t)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	root := writeRoadmapFile(t, "demo", sampleRoadmap)
	proj.RepoPath = &root
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t1", ProjectID: proj.ID, Source: "linear", SourceID: "iss-77",
		Title: "x", Status: "pending", Pipeline: "build", Priority: "P1",
		Metadata: map[string]any{"roadmap_phase": "2a"},
	})
	if err := d.syncRoadmapToLinear(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range fw.issueMilestoneLinks {
		if l == "iss-77:ms-Phase 2a — Capture" {
			found = true
		}
	}
	if !found {
		t.Errorf("issue iss-77 should link to phase 2a milestone; got %v", fw.issueMilestoneLinks)
	}
}

func TestSyncAfterSave_SkipsNonWriteBack(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	// project with NO linear binding → not write-back; helper must no-op.
	proj := &store.Project{ID: newID("proj"), Slug: "plain", Name: "Plain", Status: "active"}
	if err := d.store.InsertProject(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"project_slug": "plain"})
	srv.syncRoadmapMirrorAfterSave(in)
	time.Sleep(80 * time.Millisecond) // let the goroutine run; it should do nothing
	if len(fw.createdDocs) != 0 || len(fw.milestonesCreated) != 0 {
		t.Errorf("non-write-back project must not mirror; docs=%v ms=%v", fw.createdDocs, fw.milestonesCreated)
	}
}

func TestSyncAfterSave_NoOpOnBadInput(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	srv.syncRoadmapMirrorAfterSave(json.RawMessage(`{}`)) // no project_slug
	srv.syncRoadmapMirrorAfterSave(json.RawMessage(`not json`))
	time.Sleep(50 * time.Millisecond)
	if len(fw.createdDocs) != 0 {
		t.Errorf("bad input must no-op; docs=%v", fw.createdDocs)
	}
}
