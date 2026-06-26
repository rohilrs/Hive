package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/sources"
	"github.com/rohilrs/Hive/internal/store"
)

// fakeSource is already defined in syncer_test.go (same package).

func TestGatherExistingWork_CombinesAndDedups(t *testing.T) {
	d := newTestDaemon(t)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")

	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t1", ProjectID: proj.ID, Source: "linear", SourceID: "iss-pulled",
		Title: "Pulled task", Status: "pending", Pipeline: "build", Priority: "P1",
	})
	d.RegisterSource(&fakeSource{name: "linear", items: []sources.SourceItem{
		{SourceID: "iss-pulled", Title: "Pulled task", State: "open"},
		{SourceID: "iss-new", Title: "Un-pulled issue", State: "open", Metadata: map[string]string{"external_id": "HBA-99"}},
		{SourceID: "iss-closed", Title: "Closed", State: "closed"},
	}})

	items, err := d.gatherExistingWork(context.Background(), proj, "")
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]bool{}
	for _, it := range items {
		refs[it.Ref] = true
	}
	if !refs["hive:t1"] {
		t.Error("missing hive:t1")
	}
	if !refs["linear:iss-new"] {
		t.Error("missing un-pulled linear:iss-new")
	}
	if refs["linear:iss-pulled"] {
		t.Error("iss-pulled is already a hive task; must not be double-listed")
	}
	if refs["linear:iss-closed"] {
		t.Error("closed linear issue must be excluded")
	}
	for _, it := range items {
		if it.Ref == "linear:iss-new" && it.ExternalID != "HBA-99" {
			t.Errorf("ExternalID not propagated: got %q", it.ExternalID)
		}
	}
}

func TestGatherExistingWork_ExcludesOtherPhase(t *testing.T) {
	d := newTestDaemon(t)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t-p1", ProjectID: proj.ID, Source: "inbox", Title: "phase1", Status: "pending",
		Pipeline: "build", Priority: "P1", Metadata: map[string]any{"roadmap_phase": "1"},
	})
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t-p2", ProjectID: proj.ID, Source: "inbox", Title: "phase2", Status: "pending",
		Pipeline: "build", Priority: "P1", Metadata: map[string]any{"roadmap_phase": "2"},
	})
	d.RegisterSource(&fakeSource{name: "linear", items: nil})

	items, _ := d.gatherExistingWork(context.Background(), proj, "1")
	var foundP1 bool
	for _, it := range items {
		if it.TaskID == "t-p2" {
			t.Error("phase-2 task must be excluded when targetPhase=1")
		}
		if it.TaskID == "t-p1" {
			foundP1 = true
		}
	}
	if !foundP1 {
		t.Error("phase-1 task must be included when targetPhase=1")
	}
}

func TestGatherExistingWork_LinearFailSoft(t *testing.T) {
	d := newTestDaemon(t)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t1", ProjectID: proj.ID, Source: "inbox", Title: "local", Status: "pending",
		Pipeline: "build", Priority: "P1",
	})
	d.RegisterSource(&fakeSource{name: "linear", err: context.DeadlineExceeded})

	items, err := d.gatherExistingWork(context.Background(), proj, "")
	if err != nil {
		t.Fatalf("Linear failure must be soft, got err: %v", err)
	}
	if len(items) != 1 || items[0].Ref != "hive:t1" {
		t.Fatalf("want just hive:t1 on Linear failure, got %v", items)
	}
}

func TestGatherExistingWork_Cap(t *testing.T) {
	d := newTestDaemon(t)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	ctx := context.Background()

	// Insert 61 pending tasks — one more than the cap.
	for i := 0; i < 61; i++ {
		id := fmt.Sprintf("t-cap-%d", i)
		_ = d.store.InsertTask(ctx, &store.Task{
			ID: id, ProjectID: proj.ID, Source: "inbox",
			Title: "cap task", Status: "pending", Pipeline: "build", Priority: "P1",
		})
	}
	d.RegisterSource(&fakeSource{name: "linear"}) // no items

	items, err := d.gatherExistingWork(ctx, proj, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 60 {
		t.Errorf("expected 60 items (cap), got %d", len(items))
	}
}

func TestFormatExistingWorkBlock(t *testing.T) {
	out := formatExistingWorkBlock([]ExistingItem{
		{Ref: "hive:t1", ExternalID: "HBA-1", Title: "Do thing", Body: "details"},
		{Ref: "linear:u2", ExternalID: "HBA-2", Title: "Other", Body: ""},
	})
	if !strings.Contains(out, "[hive:t1]") || !strings.Contains(out, "(HBA-1)") || !strings.Contains(out, "Do thing") {
		t.Errorf("block missing item fields:\n%s", out)
	}
}
