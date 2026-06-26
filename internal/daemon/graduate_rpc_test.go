package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestHandleProjectGraduateRequiresSlug(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	_, rpcErr := srv.handleProjectGraduate(context.Background(), json.RawMessage(`{}`))
	if rpcErr == nil || rpcErr.Code != rpc.ErrInvalidParams {
		t.Fatalf("want ErrInvalidParams, got %v", rpcErr)
	}
}

func TestHandleProjectGraduateUnknownProject(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	_, rpcErr := srv.handleProjectGraduate(context.Background(), json.RawMessage(`{"project_slug":"ghost"}`))
	if rpcErr == nil || rpcErr.Code != rpc.ErrProjectNotFound {
		t.Fatalf("want ErrProjectNotFound, got %v", rpcErr)
	}
}

func TestHandleProjectGraduateStatus(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	// absent → exists:false
	raw, rpcErr := srv.handleProjectGraduateStatus(context.Background(), json.RawMessage(`{"project_slug":"none"}`))
	if rpcErr != nil {
		t.Fatalf("unexpected rpc err: %v", rpcErr)
	}
	var resp map[string]any
	json.Unmarshal(raw, &resp)
	if resp["exists"] != false {
		t.Errorf("absent project should be exists:false; got %v", resp)
	}
	// present → exists:true
	d.persistGraduateResult(graduate.GraduateResult{Slug: "p", Mode: "dry-run", Outcome: "blocked"})
	raw, rpcErr = srv.handleProjectGraduateStatus(context.Background(), json.RawMessage(`{"project_slug":"p"}`))
	if rpcErr != nil {
		t.Fatalf("unexpected rpc err: %v", rpcErr)
	}
	json.Unmarshal(raw, &resp)
	if resp["exists"] != true {
		t.Errorf("present project should be exists:true; got %v", resp)
	}
}

// TestHandleProjectGraduateInFlightReturnsInProgress pins the per-project
// concurrency guard: when a graduation is already in flight for a project (its
// guard held), a second project.graduate must reject with ErrInvalidParams and an
// "already in progress" message and must NOT spawn another run (the guard stays
// held — the RPC never owned it). Mirrors the merge-queue guard interlock test.
func TestHandleProjectGraduateInFlightReturnsInProgress(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)
	srv := NewRPCServer(d)

	slug := "grad"
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: slug, Slug: slug, Name: "Grad", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	// A configured runner so the call passes the runner-nil check and reaches the
	// guard. A canned clean verdict keeps it harmless if it ever did spawn.
	d.SetGraduateRunner(stubGraduateRunner{verdict: graduate.GraduationVerdict{Status: "COMPLETE"}})

	// Simulate a graduation already in flight for this project.
	if !d.graduateInFlight.tryAcquire(slug) {
		t.Fatal("guard should be free at setup")
	}

	params, _ := json.Marshal(GraduateParams{ProjectSlug: slug})
	_, rpcErr := srv.handleProjectGraduate(ctx, params)
	if rpcErr == nil {
		t.Fatal("expected an in-progress error while the guard is held")
	}
	if rpcErr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want %d; message=%q", rpcErr.Code, rpc.ErrInvalidParams, rpcErr.Message)
	}
	if !strings.Contains(rpcErr.Message, "already in progress") {
		t.Errorf("message=%q, want it to mention already in progress", rpcErr.Message)
	}
	// The RPC must NOT have released the guard it never owned, and must not have
	// spawned (which would release on completion): the guard is still held.
	if d.graduateInFlight.tryAcquire(slug) {
		t.Error("guard must still be held by the in-flight graduation (RPC must not touch it)")
	}
}
