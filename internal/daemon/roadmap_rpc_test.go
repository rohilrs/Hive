package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// writeRoadmap sets up a project repo on disk with a roadmap file at
// docs/superpowers/roadmaps/<slug>.md and optionally a sibling spec.
// Returns the absolute repo path so the test can wire it as the
// project's RepoPath.
func writeRoadmap(t *testing.T, slug, roadmapBody string, specs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	roadmapDir := filepath.Join(root, "docs", "superpowers", "roadmaps")
	if err := os.MkdirAll(roadmapDir, 0o755); err != nil {
		t.Fatalf("mkdir roadmaps: %v", err)
	}
	if err := os.WriteFile(filepath.Join(roadmapDir, slug+".md"), []byte(roadmapBody), 0o644); err != nil {
		t.Fatalf("write roadmap: %v", err)
	}
	// Write each spec at its repo-relative path; create parent dirs.
	for rel, body := range specs {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir spec parent %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write spec %s: %v", full, err)
		}
	}
	return root
}

// setupDecomposeTest builds a daemon with a project "demo" whose roadmap has a
// phase "1", and a stub runner that returns 2 subtasks. Mirrors the inline
// setup the pre-existing decompose handler test used.
func setupDecomposeTest(t *testing.T) (*Daemon, *RPCServer) {
	t.Helper()
	d := newTestDaemon(t)
	ctx := context.Background()
	roadmapBody := "# Roadmap\n\n## Phase 1: First\n\nPhase one body.\n"
	repoRoot := writeRoadmap(t, "demo", roadmapBody, map[string]string{})
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "demo", Name: "Proj", RepoPath: &repoRoot, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	d.decomposeRunner = &stubDecomposeRunner{out: &anthropic.TurnOutput{
		StopReason: "tool_use", TokensIn: 10, TokensOut: 5,
		ToolCalls: []anthropic.ToolCall{{ID: "tu1", Name: "submit_subtasks",
			Input: json.RawMessage(`{"subtasks":[
				{"title":"first","body":"b1","priority":"P1","pipeline":"build"},
				{"title":"second","body":"b2","priority":"P2","pipeline":"build"}
			]}`)}},
	}}
	return d, NewRPCServer(d)
}

func TestHandleRoadmapDecomposeReturnsProposals(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()

	const slug = "proj"
	const phaseNum = "8b"
	roadmapBody := `# Roadmap

## Phase 8a: Planner

Planner phase body.

## Phase 8b: Decomposer

Decomposer phase body.

Spec: [Phase 8 design](docs/superpowers/specs/phase-8.md)

## Phase 9: Future

Not this one.
`
	specs := map[string]string{
		"docs/superpowers/specs/phase-8.md": "# Phase 8 spec body\n",
	}
	repoRoot := writeRoadmap(t, slug, roadmapBody, specs)

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: slug, Name: "Proj", RepoPath: &repoRoot, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	// Stub runner returns 3 subtasks via the synthetic tool_use turn.
	d.decomposeRunner = &stubDecomposeRunner{out: &anthropic.TurnOutput{
		StopReason: "tool_use",
		TokensIn:   1234,
		TokensOut:  567,
		ToolCalls: []anthropic.ToolCall{
			{ID: "tu1", Name: "submit_subtasks", Input: json.RawMessage(`{"subtasks":[
				{"title":"first","body":"b1","priority":"P0","pipeline":"build"},
				{"title":"second","body":"b2","priority":"P1","pipeline":"build"},
				{"title":"third","body":"b3","priority":"P2","pipeline":"debug"}
			]}`)},
		},
	}}

	srv := NewRPCServer(d)

	ch, unsub := d.bus.Subscribe()
	defer unsub()

	body, _ := json.Marshal(RoadmapDecomposeParams{ProjectSlug: slug, Phase: phaseNum})
	raw, rpcErr := srv.handleRoadmapDecompose(ctx, body)
	if rpcErr != nil {
		t.Fatalf("handleRoadmapDecompose: code=%d msg=%s", rpcErr.Code, rpcErr.Message)
	}

	// Handler now returns a decompose_id immediately.
	var ack struct {
		DecomposeID string `json:"decompose_id"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil || ack.DecomposeID == "" {
		t.Fatalf("start must return a decompose_id; got %s err=%v", raw, err)
	}

	// Wait for the decompose.proposed event and verify result fields.
	// The in-memory bus carries the *RoadmapDecomposeResult directly (no JSON
	// round-trip), so assert against the typed value.
	ev := waitForEventType(t, ch, rpc.EventDecomposeProposed, 5*time.Second)
	if ev.Data["decompose_id"] != ack.DecomposeID {
		t.Errorf("event decompose_id=%q, want %q", ev.Data["decompose_id"], ack.DecomposeID)
	}
	got, ok := ev.Data["result"].(*RoadmapDecomposeResult)
	if !ok || got == nil {
		t.Fatalf("proposed event result is not *RoadmapDecomposeResult; data=%v", ev.Data)
	}
	if len(got.Subtasks) != 3 {
		t.Errorf("got %d subtasks, want 3", len(got.Subtasks))
	}
	if got.PhaseNumber != phaseNum {
		t.Errorf("PhaseNumber=%q, want %q", got.PhaseNumber, phaseNum)
	}
	if got.PhaseTitle != "Decomposer" {
		t.Errorf("PhaseTitle=%q, want %q", got.PhaseTitle, "Decomposer")
	}
	wantRoadmap := filepath.Join(repoRoot, "docs", "superpowers", "roadmaps", slug+".md")
	if got.RoadmapPath != wantRoadmap {
		t.Errorf("RoadmapPath=%q, want %q", got.RoadmapPath, wantRoadmap)
	}
	if len(got.SpecPaths) != 1 || got.SpecPaths[0] != "docs/superpowers/specs/phase-8.md" {
		t.Errorf("SpecPaths=%v, want [docs/superpowers/specs/phase-8.md]", got.SpecPaths)
	}
	if got.TokensIn != 1234 || got.TokensOut != 567 {
		t.Errorf("tokens in/out=%d/%d, want 1234/567", got.TokensIn, got.TokensOut)
	}
}

func TestHandleRoadmapDecomposeMissingRunner(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()

	const slug = "proj"
	repoRoot := writeRoadmap(t, slug, "## Phase 1: X\n\nBody.\n", nil)
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: slug, Name: "Proj", RepoPath: &repoRoot, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	// Explicit nil (newTestDaemon doesn't wire decomposeRunner, but make
	// the intent loud so a future helper change doesn't silently break
	// this test).
	d.decomposeRunner = nil

	srv := NewRPCServer(d)
	body, _ := json.Marshal(RoadmapDecomposeParams{ProjectSlug: slug, Phase: "1"})
	_, rpcErr := srv.handleRoadmapDecompose(ctx, body)
	if rpcErr == nil {
		t.Fatal("expected error for missing decomposeRunner; got nil")
	}
	if rpcErr.Code != rpc.ErrInternal {
		t.Errorf("code=%d, want %d (ErrInternal)", rpcErr.Code, rpc.ErrInternal)
	}
	if !strings.Contains(rpcErr.Message, "composition root") {
		t.Errorf("message=%q, want substring 'composition root'", rpcErr.Message)
	}
}

func TestHandleRoadmapDecomposeMissingRoadmap(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()

	// Repo exists but no docs/superpowers/roadmaps/<slug>.md.
	repoRoot := t.TempDir()
	const slug = "proj"
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: slug, Name: "Proj", RepoPath: &repoRoot, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	// Wire the runner so we get past the runner-nil guard and hit the
	// roadmap-missing path (inside the goroutine via runDecomposeWork).
	d.decomposeRunner = &stubDecomposeRunner{}

	srv := NewRPCServer(d)

	ch, unsub := d.bus.Subscribe()
	defer unsub()

	body, _ := json.Marshal(RoadmapDecomposeParams{ProjectSlug: slug, Phase: "1"})
	raw, rpcErr := srv.handleRoadmapDecompose(ctx, body)
	if rpcErr != nil {
		t.Fatalf("handler returned synchronous error: %v", rpcErr)
	}
	var ack struct {
		DecomposeID string `json:"decompose_id"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil || ack.DecomposeID == "" {
		t.Fatalf("start must return a decompose_id; got %s err=%v", raw, err)
	}

	// The goroutine will fail to read the roadmap and publish decompose.failed.
	ev := waitForEventType(t, ch, rpc.EventDecomposeFailed, 5*time.Second)
	msg, _ := ev.Data["error"].(string)
	// The roadmap is absent from both the working tree and any branch (the
	// fixture isn't a git repo), so decompose fails with the not-found error
	// naming the project + the `hive plan` remedy.
	if !strings.Contains(msg, slug) || !strings.Contains(msg, "not found") {
		t.Errorf("failed event error=%q, want it to mention %q and 'not found'", msg, slug)
	}
}

func TestHandleRoadmapDecomposeMissingPhase(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()

	const slug = "proj"
	roadmapBody := `## Phase 1: First

Body.

## Phase 2: Second

Body.
`
	repoRoot := writeRoadmap(t, slug, roadmapBody, nil)
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: slug, Name: "Proj", RepoPath: &repoRoot, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	d.decomposeRunner = &stubDecomposeRunner{}

	srv := NewRPCServer(d)

	ch, unsub := d.bus.Subscribe()
	defer unsub()

	body, _ := json.Marshal(RoadmapDecomposeParams{ProjectSlug: slug, Phase: "99"})
	raw, rpcErr := srv.handleRoadmapDecompose(ctx, body)
	if rpcErr != nil {
		t.Fatalf("handler returned synchronous error: %v", rpcErr)
	}
	var ack struct {
		DecomposeID string `json:"decompose_id"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil || ack.DecomposeID == "" {
		t.Fatalf("start must return a decompose_id; got %s err=%v", raw, err)
	}

	// The goroutine will fail to find phase "99" and publish decompose.failed.
	ev := waitForEventType(t, ch, rpc.EventDecomposeFailed, 5*time.Second)
	msg, _ := ev.Data["error"].(string)
	if !strings.Contains(msg, `"99"`) {
		t.Errorf("failed event error=%q, want substring '\"99\"'", msg)
	}
}

func TestRoadmapDecomposeStartReturnsIDAndPublishesProposed(t *testing.T) {
	d, srv := setupDecomposeTest(t)

	ch, unsub := d.bus.Subscribe()
	defer unsub()

	body, _ := json.Marshal(RoadmapDecomposeParams{ProjectSlug: "demo", Phase: "1"})
	raw, rerr := srv.handleRoadmapDecompose(context.Background(), body)
	if rerr != nil {
		t.Fatalf("start returned RPC error: %v", rerr)
	}
	var ack struct {
		DecomposeID string `json:"decompose_id"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil || ack.DecomposeID == "" {
		t.Fatalf("start must return a decompose_id; got %s err=%v", raw, err)
	}

	// The goroutine publishes a proposed event carrying the id + result.
	for i := 0; i < 40; i++ {
		select {
		case ev := <-ch:
			if ev.Type == rpc.EventDecomposeProposed && ev.Data["decompose_id"] == ack.DecomposeID {
				if ev.Data["result"] == nil {
					t.Fatal("proposed event missing result payload")
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for decompose.proposed")
		}
	}
	t.Fatal("never received decompose.proposed for the started id")
}

func TestRoadmapDecomposePublishesFailedOnError(t *testing.T) {
	d, srv := setupDecomposeTest(t)
	d.decomposeRunner = &stubDecomposeRunner{err: errors.New("model boom")} // override: runner errors

	ch, unsub := d.bus.Subscribe()
	defer unsub()

	body, _ := json.Marshal(RoadmapDecomposeParams{ProjectSlug: "demo", Phase: "1"})
	if _, rerr := srv.handleRoadmapDecompose(context.Background(), body); rerr != nil {
		t.Fatalf("start returned RPC error: %v", rerr)
	}
	for i := 0; i < 20; i++ {
		select {
		case ev := <-ch:
			if ev.Type == rpc.EventDecomposeFailed {
				if msg, _ := ev.Data["error"].(string); msg == "" {
					t.Fatal("failed event missing error detail")
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for decompose.failed")
		}
	}
	t.Fatal("never received decompose.failed")
}

// panicRunner is a decompose.Runner whose RunTurn always panics with "boom".
// It is used to verify that runDecomposeAsync recovers from panics and
// publishes a decompose.failed event rather than crashing the daemon.
type panicRunner struct{}

func (panicRunner) RunTurn(_ context.Context, _ anthropic.TurnInput) (*anthropic.TurnOutput, error) {
	panic("boom")
}

func TestRoadmapDecomposePublishesFailedOnPanic(t *testing.T) {
	d, srv := setupDecomposeTest(t)
	d.decomposeRunner = panicRunner{}

	ch, unsub := d.bus.Subscribe()
	defer unsub()

	body, _ := json.Marshal(RoadmapDecomposeParams{ProjectSlug: "demo", Phase: "1"})
	if _, rerr := srv.handleRoadmapDecompose(context.Background(), body); rerr != nil {
		t.Fatalf("start returned RPC error: %v", rerr)
	}

	ev := waitForEventType(t, ch, rpc.EventDecomposeFailed, 5*time.Second)
	msg, _ := ev.Data["error"].(string)
	if !strings.Contains(msg, "panicked") {
		t.Errorf("failed event error=%q, want substring 'panicked'", msg)
	}
}

func TestHandleRoadmapSyncLinear(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	fw := &fakeLinearWriter{}
	d.SetLinearWriter(fw)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	root := writeRoadmapFile(t, "demo", sampleRoadmap)
	// persist repo_path so the handler's GetProjectBySlug re-fetch sees it
	rp := root
	if err := d.store.UpdateProject(context.Background(), proj.ID, nil, &rp, nil); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(map[string]any{"project_slug": "demo"})
	out, rerr := srv.handleRoadmapSyncLinear(context.Background(), params)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var res RoadmapSyncLinearResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Document != 1 {
		t.Errorf("Document=%d want 1", res.Document)
	}
	if res.Milestones != 2 {
		t.Errorf("Milestones=%d want 2 (sampleRoadmap has phases 1 + 2a)", res.Milestones)
	}
}

func TestHandleRoadmapSyncLinear_MissingProject(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	params, _ := json.Marshal(map[string]any{"project_slug": "nope"})
	_, rerr := srv.handleRoadmapSyncLinear(context.Background(), params)
	if rerr == nil {
		t.Fatal("expected error for missing project")
	}
	if rerr.Code != rpc.ErrProjectNotFound {
		t.Errorf("code=%v want ErrProjectNotFound", rerr.Code)
	}
}

func TestRoadmapDecomposeResultCarriesMergeFrom(t *testing.T) {
	in := RoadmapDecomposeResult{Subtasks: []decompose.ProposedSubtask{{Title: "t", Body: "b", Priority: "P1", MergeFrom: "linear:u1"}}}
	raw, _ := json.Marshal(in)
	var out RoadmapDecomposeResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Subtasks[0].MergeFrom != "linear:u1" {
		t.Errorf("merge_from lost in result round-trip: %q", out.Subtasks[0].MergeFrom)
	}
}

// TestHandleRoadmapPlanSetup_CreatesAndChecksOut verifies the happy path:
// the handler creates the feature branch off main, checks it out in the repo,
// reports created=true, and persists feature_branch to the per-project config.
func TestHandleRoadmapPlanSetup_CreatesAndChecksOut(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	repo := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "slug", Name: "Demo", RepoPath: &repo, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	params, _ := json.Marshal(map[string]any{"project_slug": "slug", "feature_branch": "spec/feat"})
	out, rerr := srv.handleRoadmapPlanSetup(ctx, params)
	if rerr != nil {
		t.Fatalf("handleRoadmapPlanSetup: %v", rerr)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res["feature_branch"] != "spec/feat" {
		t.Errorf("feature_branch = %v, want spec/feat", res["feature_branch"])
	}
	if res["created"] != true {
		t.Errorf("created = %v, want true", res["created"])
	}

	head, err := gitC(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if head != "spec/feat" {
		t.Errorf("HEAD = %q, want spec/feat", head)
	}

	if got := d.scheduler.effectiveFeatureBranchForProject("slug"); got != "spec/feat" {
		t.Errorf("persisted feature branch = %q, want spec/feat", got)
	}
}

// TestHandleRoadmapPlanSetup_RefusesDirtyTree verifies the handler refuses to
// switch branches when the working tree has uncommitted changes, and does not
// switch off the original branch.
func TestHandleRoadmapPlanSetup_RefusesDirtyTree(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	repo := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "slug", Name: "Demo", RepoPath: &repo, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	params, _ := json.Marshal(map[string]any{"project_slug": "slug", "feature_branch": "spec/feat"})
	_, rerr := srv.handleRoadmapPlanSetup(ctx, params)
	if rerr == nil {
		t.Fatal("expected error for dirty working tree")
	}
	if !strings.Contains(rerr.Message, "uncommitted changes") {
		t.Errorf("error = %q, want mention of uncommitted changes", rerr.Message)
	}

	head, err := gitC(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if head != "main" {
		t.Errorf("HEAD = %q, want main (must not switch branches)", head)
	}
}

// TestHandleRoadmapPlanSetup_InertWhenNoFeatureBranch verifies that when neither
// the param nor the per-project config supplies a feature branch, the handler is
// inert: it returns {"active": false}, does no branch work, and leaves the repo
// on its original branch (no checkout). Backward-compat: plan sessions on
// projects that never opted into the integration loop must not get a branch.
func TestHandleRoadmapPlanSetup_InertWhenNoFeatureBranch(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	repo := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "slug", Name: "Demo", RepoPath: &repo, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	params, _ := json.Marshal(map[string]any{"project_slug": "slug"})
	out, rerr := srv.handleRoadmapPlanSetup(ctx, params)
	if rerr != nil {
		t.Fatalf("handleRoadmapPlanSetup: %v", rerr)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res["active"] != false {
		t.Errorf("active = %v, want false", res["active"])
	}

	head, err := gitC(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if head != "main" {
		t.Errorf("HEAD = %q, want main (inert setup must not check out a branch)", head)
	}
}
