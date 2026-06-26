package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestHandleRunResumeMissingRunIDReturnsInvalidParams(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	_, rpcErr := srv.handleRunResume(context.Background(), json.RawMessage(`{}`))
	if rpcErr == nil {
		t.Fatal("expected ErrInvalidParams for missing run_id")
	}
	if rpcErr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want %d", rpcErr.Code, rpc.ErrInvalidParams)
	}
}

func TestHandleRunResumeUnknownRunReturnsErrInternal(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	_, rpcErr := srv.handleRunResume(context.Background(), json.RawMessage(`{"run_id":"ghost"}`))
	if rpcErr == nil {
		t.Fatal("expected error for unknown run")
	}
	if rpcErr.Code != rpc.ErrInternal {
		t.Errorf("code=%d, want %d (ErrInternal); message=%q", rpcErr.Code, rpc.ErrInternal, rpcErr.Message)
	}
	if !strings.Contains(rpcErr.Message, "run not found") {
		t.Errorf("message=%q, want 'run not found' substring", rpcErr.Message)
	}
}

// healthSnapshot drives handleHealth and unmarshals the result. Shared
// helper for the focused TestDaemonHealthRPC* tests below.
func healthSnapshot(t *testing.T, d *Daemon) HealthSnapshot {
	t.Helper()
	s := NewRPCServer(d)
	raw, rpcErr := s.handleHealth(context.Background())
	if rpcErr != nil {
		t.Fatalf("handleHealth: rpc err %v", rpcErr)
	}
	var snap HealthSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return snap
}

// TestDaemonHealthRPCSnapshotShape verifies handleHealth populates the
// non-negative-counter / schema-version fields. Sign/range only — see
// the focused tests below for LastTickUnix and MCP defaults.
func TestDaemonHealthRPCSnapshotShape(t *testing.T) {
	d := newTestDaemon(t)
	snap := healthSnapshot(t, d)

	if snap.SchemaVersionDB != store.MaxSchemaVersion {
		t.Errorf("SchemaVersionDB=%d, want MaxSchemaVersion=%d", snap.SchemaVersionDB, store.MaxSchemaVersion)
	}
	if snap.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds=%d; expected >= 0", snap.UptimeSeconds)
	}
	if snap.ActiveRuns < 0 {
		t.Errorf("ActiveRuns=%d; expected >= 0", snap.ActiveRuns)
	}
	if snap.PendingApprovals < 0 {
		t.Errorf("PendingApprovals=%d; expected >= 0", snap.PendingApprovals)
	}
}

// TestDaemonHealthRPCLastTickAtZeroBeforeTick locks in the documented
// contract: a freshly-constructed daemon (no scheduler.tick() ever called)
// reports LastTickUnix == 0. Catches a regression where handleHealth
// stops reading scheduler.LastTickAt() or always stamps a non-zero value.
func TestDaemonHealthRPCLastTickAtZeroBeforeTick(t *testing.T) {
	d := newTestDaemon(t)
	snap := healthSnapshot(t, d)

	if snap.LastTickUnix != 0 {
		t.Errorf("LastTickUnix=%d before any tick; want 0", snap.LastTickUnix)
	}
}

// TestDaemonHealthRPCLastTickAtAfterTick verifies LastTickUnix mirrors
// scheduler.LastTickAt().Unix() once a tick has run. Catches a regression
// where handleHealth's struct literal forgets to wire LastTickUnix at all
// (the field would stay zero even after a tick).
func TestDaemonHealthRPCLastTickAtAfterTick(t *testing.T) {
	d := newTestDaemon(t)
	d.scheduler.tick(context.Background())

	snap := healthSnapshot(t, d)

	if snap.LastTickUnix <= 0 {
		t.Fatalf("LastTickUnix=%d after tick; want > 0", snap.LastTickUnix)
	}
	if got, want := snap.LastTickUnix, d.scheduler.LastTickAt().Unix(); got != want {
		t.Errorf("LastTickUnix=%d, want scheduler.LastTickAt().Unix()=%d", got, want)
	}
}

// TestDaemonHealthRPCMCPDefaultsWhenListenerNotBound verifies the
// negative case: when the MCP HTTP listener was never bound (the test
// daemon is constructed via New(), not Start()), MCPHTTPAddr is empty
// and MCPHTTPListenerOK is false. The positive case (non-empty Addr
// after Start()) requires the full boot path and is covered indirectly
// by TestDaemonStartedAtSetAtBoot — handleHealth just reads s.d.MCPHTTPAddr().
func TestDaemonHealthRPCMCPDefaultsWhenListenerNotBound(t *testing.T) {
	d := newTestDaemon(t)
	snap := healthSnapshot(t, d)

	if snap.MCPHTTPAddr != "" {
		t.Errorf("MCPHTTPAddr=%q on unbound listener; want \"\"", snap.MCPHTTPAddr)
	}
	if snap.MCPHTTPListenerOK {
		t.Errorf("MCPHTTPListenerOK=true on unbound listener; want false")
	}
}

// stubDecomposeRunner is the local stub for decompose.Runner used in
// handler tests. Tests construct it with a fixed TurnOutput.
type stubDecomposeRunner struct {
	out *anthropic.TurnOutput
	err error
}

func (s *stubDecomposeRunner) RunTurn(ctx context.Context, in anthropic.TurnInput) (*anthropic.TurnOutput, error) {
	return s.out, s.err
}

func TestHandleDecomposeHappyPath(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "task-1", ProjectID: "p1", Source: "inbox", Title: "ship", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	// Inject a stub runner that returns a 2-subtask tool_use.
	d.decomposeRunner = &stubDecomposeRunner{out: &anthropic.TurnOutput{
		StopReason: "tool_use",
		TokensIn:   1000,
		TokensOut:  500,
		ToolCalls: []anthropic.ToolCall{
			{ID: "tu1", Name: "submit_subtasks", Input: json.RawMessage(`{"subtasks":[{"title":"a","body":"ba","priority":"P0","pipeline":"build"},{"title":"b","body":"bb","priority":"P1"}]}`)},
		},
	}}

	s := NewRPCServer(d)
	raw, rpcErr := s.handleDecompose(ctx, json.RawMessage(`{"task_id":"task-1"}`))
	if rpcErr != nil {
		t.Fatalf("handleDecompose: %v", rpcErr)
	}
	var got DecomposeResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Subtasks) != 2 {
		t.Errorf("got %d subtasks, want 2", len(got.Subtasks))
	}
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("Model=%q, want claude-sonnet-4-6", got.Model)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 500 {
		t.Errorf("tokens in/out=%d/%d, want 1000/500", got.InputTokens, got.OutputTokens)
	}
}

func TestHandleDecomposeTaskNotFound(t *testing.T) {
	d := newTestDaemon(t)
	d.decomposeRunner = &stubDecomposeRunner{} // shouldn't be called
	s := NewRPCServer(d)
	_, rpcErr := s.handleDecompose(context.Background(), json.RawMessage(`{"task_id":"missing"}`))
	if rpcErr == nil {
		t.Fatal("expected error for missing task; got nil")
	}
}

func TestHandleDecomposeApplyHappyPath(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "task-1", ProjectID: "p1", Source: "inbox", Title: "ship", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	s := NewRPCServer(d)
	body := `{"parent_task_id":"task-1","subtasks":[
		{"title":"a","body":"ba","priority":"P0","pipeline":"build"},
		{"title":"b","body":"bb","priority":"P1","pipeline":"debug"}
	]}`
	raw, rpcErr := s.handleDecomposeApply(ctx, json.RawMessage(body))
	if rpcErr != nil {
		t.Fatalf("handleDecomposeApply: %v", rpcErr)
	}
	var got DecomposeApplyResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.InsertedTaskIDs) != 2 {
		t.Errorf("got %d ids, want 2", len(got.InsertedTaskIDs))
	}
	children, err := d.store.ListChildTasks(ctx, "task-1")
	if err != nil {
		t.Fatalf("ListChildTasks: %v", err)
	}
	if len(children) != 2 {
		t.Errorf("got %d children, want 2", len(children))
	}
}

func TestHandleDecomposeApplyParentNotFound(t *testing.T) {
	d := newTestDaemon(t)
	s := NewRPCServer(d)
	body := `{"parent_task_id":"missing","subtasks":[{"title":"a","body":"b","priority":"P0","pipeline":"build"}]}`
	_, rpcErr := s.handleDecomposeApply(context.Background(), json.RawMessage(body))
	if rpcErr == nil {
		t.Fatal("expected error for missing parent; got nil")
	}
	if rpcErr.Code != rpc.ErrTaskNotFound {
		t.Errorf("code=%d, want ErrTaskNotFound", rpcErr.Code)
	}
}

func TestHandleDecomposeApplyInvalidSubtask(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "task-1", ProjectID: "p1", Source: "inbox", Title: "ship", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	s := NewRPCServer(d)
	body := `{"parent_task_id":"task-1","subtasks":[{"title":"","body":"b","priority":"P0","pipeline":"build"}]}`
	_, rpcErr := s.handleDecomposeApply(ctx, json.RawMessage(body))
	if rpcErr == nil {
		t.Fatal("expected error for empty title; got nil")
	}
}

// TestHandleDecomposeApplyRejectsOversizeTitle pins the anti-tamper
// gap fix: a 250-char title (>200 cap) must be rejected by the daemon
// re-validation even though the propose-side cap was bypassed.
func TestHandleDecomposeApplyRejectsOversizeTitle(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "task-1", ProjectID: "p1", Source: "inbox", Title: "ship", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	s := NewRPCServer(d)
	longTitle := strings.Repeat("a", 250) // 250 chars > 200 limit
	body := fmt.Sprintf(`{"parent_task_id":"task-1","subtasks":[{"title":%q,"body":"b","priority":"P0","pipeline":"build"}]}`, longTitle)
	_, rpcErr := s.handleDecomposeApply(ctx, json.RawMessage(body))
	if rpcErr == nil {
		t.Fatal("expected error for oversize title; got nil")
	}
	if rpcErr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want ErrInvalidParams", rpcErr.Code)
	}
}

// TestHandleDecomposeApplyRejectsOversizeCount pins the anti-tamper
// gap fix: 21 subtasks (>HardMaxSubtasks=20) must be rejected even
// when the propose-side cap was bypassed.
func TestHandleDecomposeApplyRejectsOversizeCount(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "task-1", ProjectID: "p1", Source: "inbox", Title: "ship", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	s := NewRPCServer(d)
	var sb strings.Builder
	sb.WriteString(`{"parent_task_id":"task-1","subtasks":[`)
	for i := 0; i < 21; i++ { // 21 > HardMaxSubtasks=20
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"title":"t%d","body":"b","priority":"P0","pipeline":"build"}`, i)
	}
	sb.WriteString(`]}`)
	_, rpcErr := s.handleDecomposeApply(ctx, json.RawMessage(sb.String()))
	if rpcErr == nil {
		t.Fatal("expected error for oversize count; got nil")
	}
	if rpcErr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want ErrInvalidParams", rpcErr.Code)
	}
}

// waitForEventType drains the bus channel until an event of the given
// type arrives (or the timeout fires). Other event types are silently
// skipped so unrelated daemon-emitted events (e.g. resync on subscribe)
// don't fail the assertion.
func waitForEventType(t *testing.T, ch <-chan rpc.EventMessage, want rpc.EventType, timeout time.Duration) rpc.EventMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event published within %s", want, timeout)
		}
	}
}

func TestHandleAddProjectPublishesEvent(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	params := json.RawMessage(`{"slug":"acme","name":"Acme","repo_path":"/tmp/acme"}`)
	out, rpcErr := srv.handleAddProject(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("handleAddProject: %+v", rpcErr)
	}
	var res struct {
		ProjectID string `json:"project_id"`
		Slug      string `json:"slug"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.ProjectID == "" || res.Slug != "acme" {
		t.Errorf("result project_id=%q slug=%q", res.ProjectID, res.Slug)
	}

	ev := waitForEventType(t, ch, rpc.EventProjectCreated, 2*time.Second)
	if got, _ := ev.Data["project_id"].(string); got != res.ProjectID {
		t.Errorf("event project_id=%q want %q", got, res.ProjectID)
	}
	if got, _ := ev.Data["slug"].(string); got != "acme" {
		t.Errorf("event slug=%q want acme", got)
	}
	if got, _ := ev.Data["name"].(string); got != "Acme" {
		t.Errorf("event name=%q want Acme", got)
	}
	if got, _ := ev.Data["repo_path"].(string); got != "/tmp/acme" {
		t.Errorf("event repo_path=%q want /tmp/acme", got)
	}
}

func TestHandleEditProjectPublishesEvent(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	// Seed a project to edit.
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "acme", Name: "Acme", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}

	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	params := json.RawMessage(`{"slug":"acme","name":"Acme Renamed","repo_path":"/tmp/acme2"}`)
	_, rpcErr := srv.handleEditProject(ctx, params)
	if rpcErr != nil {
		t.Fatalf("handleEditProject: %+v", rpcErr)
	}

	ev := waitForEventType(t, ch, rpc.EventProjectUpdated, 2*time.Second)
	if got, _ := ev.Data["project_id"].(string); got != "p1" {
		t.Errorf("event project_id=%q want p1", got)
	}
	if got, _ := ev.Data["name"].(string); got != "Acme Renamed" {
		t.Errorf("event name=%q want 'Acme Renamed'", got)
	}
	if got, _ := ev.Data["repo_path"].(string); got != "/tmp/acme2" {
		t.Errorf("event repo_path=%q want /tmp/acme2", got)
	}
	if del, _ := ev.Data["deleted"].(bool); del {
		t.Error("event deleted=true on edit; want absent/false")
	}
}

// TestHandleEditProject_WritesIntegration verifies project.edit persists the
// [integration] settings to the per-project config overlay so the effective
// config (and effectiveFeatureBranchForProject) reflect them.
func TestHandleEditProject_WritesIntegration(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "acme", Name: "Acme", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}

	params := json.RawMessage(`{"slug":"acme","feature_branch":"spec/x","task_auto_integrate":true,"merge_method":"squash","auto_fix_ci":true}`)
	if _, rpcErr := srv.handleEditProject(ctx, params); rpcErr != nil {
		t.Fatalf("handleEditProject: %+v", rpcErr)
	}

	if got := d.scheduler.effectiveFeatureBranchForProject("acme"); got != "spec/x" {
		t.Errorf("effectiveFeatureBranchForProject = %q, want spec/x", got)
	}
	cfg := d.effectiveConfigForProject("acme")
	if cfg.Integration.MergeMethod != "squash" {
		t.Errorf("MergeMethod = %q, want squash", cfg.Integration.MergeMethod)
	}
	if !cfg.Integration.TaskAutoIntegrate {
		t.Errorf("TaskAutoIntegrate = false, want true")
	}
	if !cfg.Integration.AutoFixCI {
		t.Errorf("AutoFixCI = false, want true")
	}
}

func TestAddProjectPersistsDispatchAndIntegration(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	rp := initGitRepo(t)
	body, _ := json.Marshal(map[string]any{
		"slug": "np", "name": "NP", "repo_path": rp,
		"dispatch_mode":  "auto_all",
		"target_branch":  "develop", // [scheduler] base, persisted independent of mode
		"feature_branch": "spec/x", "merge_method": "squash",
		"task_auto_integrate": true, "auto_fix_ci": true,
	})
	if _, rerr := srv.handleAddProject(ctx, body); rerr != nil {
		t.Fatalf("handleAddProject: %s", rerr.Message)
	}
	if got := d.scheduler.effectiveDispatchModeForProject("np"); got != "auto_all" {
		t.Errorf("dispatch_mode = %q, want auto_all", got)
	}
	if got := d.scheduler.effectiveFeatureBranchForProject("np"); got != "spec/x" {
		t.Errorf("feature_branch = %q, want spec/x", got)
	}
	// target_branch persists in a non-sequenced (auto_all) project.
	if got := d.scheduler.effectiveTargetBranchForProject("np"); got != "develop" {
		t.Errorf("target_branch = %q, want develop", got)
	}
}

func TestAddProjectRejectsSequenced(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	body, _ := json.Marshal(map[string]any{"slug": "sq", "name": "S", "dispatch_mode": "sequenced"})
	if _, rerr := srv.handleAddProject(context.Background(), body); rerr == nil {
		t.Error("create with sequenced must be rejected (no roadmap at create)")
	}
}

func TestEditProjectAppliesDispatchModeAndListsCanSequence(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	rp := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "pe", Slug: "pe", Name: "PE", Status: "active", RepoPath: &rp})

	// Edit to auto_all.
	body, _ := json.Marshal(map[string]any{"slug": "pe", "dispatch_mode": "auto_all"})
	if _, rerr := srv.handleEditProject(ctx, body); rerr != nil {
		t.Fatalf("handleEditProject: %s", rerr.Message)
	}
	if got := d.scheduler.effectiveDispatchModeForProject("pe"); got != "auto_all" {
		t.Errorf("dispatch_mode = %q, want auto_all", got)
	}

	// list_projects reports CanSequence=false (no roadmap on this repo).
	raw, rerr := srv.handleListProjects(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var views []rpc.ProjectView
	_ = json.Unmarshal(raw, &views)
	for _, v := range views {
		if v.Slug == "pe" && v.CanSequence {
			t.Error("CanSequence should be false without a roadmap")
		}
	}
}

func TestHandleDeleteProjectPublishesEvent(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "acme", Name: "Acme", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}

	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	params := json.RawMessage(`{"slug":"acme"}`)
	_, rpcErr := srv.handleDeleteProject(ctx, params)
	if rpcErr != nil {
		t.Fatalf("handleDeleteProject: %+v", rpcErr)
	}

	ev := waitForEventType(t, ch, rpc.EventProjectUpdated, 2*time.Second)
	if got, _ := ev.Data["project_id"].(string); got != "p1" {
		t.Errorf("event project_id=%q want p1", got)
	}
	if del, _ := ev.Data["deleted"].(bool); !del {
		t.Error("event deleted=false on delete; want true")
	}
}

// TestSourcesBindLinearWriteBackAmbiguousTeams verifies that binding a linear
// source with write_back:true and two teams (no --wb-team) is rejected with
// ErrInvalidParams. This exercises the bind-time validation added in Task 3.
func TestSourcesBindLinearWriteBackAmbiguousTeams(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p-wb", Slug: "conv", Name: "Conv", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewRPCServer(d)
	params, _ := json.Marshal(SourcesBindParams{
		Slug:   "conv",
		Source: "linear",
		Binding: map[string]any{
			"teams":      []string{"CONV", "OPS"}, // ambiguous: two teams, no wb_team
			"projects":   []string{"proj-uuid"},
			"write_back": true,
		},
	})
	_, rpcErr := srv.handleSourcesBind(ctx, params)
	if rpcErr == nil {
		t.Fatal("expected ErrInvalidParams for ambiguous teams; got nil")
	}
	if rpcErr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want ErrInvalidParams (%d)", rpcErr.Code, rpc.ErrInvalidParams)
	}
}

// TestSourcesBindLinearWriteBackSingleTeamProject verifies that a binding with
// write_back:true and exactly one team + one project is accepted without error.
func TestSourcesBindLinearWriteBackSingleTeamProject(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p-wb2", Slug: "conv2", Name: "Conv2", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewRPCServer(d)
	params, _ := json.Marshal(SourcesBindParams{
		Slug:   "conv2",
		Source: "linear",
		Binding: map[string]any{
			"teams":      []string{"CONV"},
			"projects":   []string{"57925d22"},
			"write_back": true,
		},
	})
	_, rpcErr := srv.handleSourcesBind(ctx, params)
	if rpcErr != nil {
		t.Fatalf("expected success for single team+project with write_back; got %+v", rpcErr)
	}
}

// TestSourcesBindLinearWriteBackTargetOutOfWindow verifies that an explicit
// wb_project not present in the bound projects (read filter) is rejected — a
// mirror target outside the ingest window would be auto-closed on the next poll.
func TestSourcesBindLinearWriteBackTargetOutOfWindow(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p-wb3", Slug: "conv3", Name: "Conv3", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewRPCServer(d)
	params, _ := json.Marshal(SourcesBindParams{
		Slug:   "conv3",
		Source: "linear",
		Binding: map[string]any{
			"teams":      []string{"CONV", "OPS"},
			"projects":   []string{"proj-a", "proj-b"},
			"write_back": true,
			"wb_team":    "CONV",
			"wb_project": "proj-c", // not in projects -> out of read window
		},
	})
	_, rpcErr := srv.handleSourcesBind(ctx, params)
	if rpcErr == nil || rpcErr.Code != rpc.ErrInvalidParams {
		t.Fatalf("expected ErrInvalidParams for out-of-window wb_project; got %+v", rpcErr)
	}
}

func TestHandleTaskFinish_RequiresCompletedBuildRun(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	proj := &store.Project{ID: "p1", Slug: "demo", Name: "Demo", Status: "active"}
	if err := d.store.InsertProject(ctx, proj); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "t1", ProjectID: "p1", Source: "inbox", Title: "x", Status: "pending",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"task_id": "t1"})
	_, rerr := srv.handleTaskFinish(ctx, params)
	if rerr == nil {
		t.Fatal("expected error: no completed build run to finish")
	}
	if rerr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want ErrInvalidParams (%d)", rerr.Code, rpc.ErrInvalidParams)
	}
}

func TestHandleTaskFinish_MissingTaskID(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	params, _ := json.Marshal(map[string]any{})
	_, rerr := srv.handleTaskFinish(ctx, params)
	if rerr == nil {
		t.Fatal("expected error for missing task_id")
	}
	if rerr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want ErrInvalidParams (%d)", rerr.Code, rpc.ErrInvalidParams)
	}
}

func TestHandleTaskFinish_UnknownTask(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	params, _ := json.Marshal(map[string]any{"task_id": "ghost"})
	_, rerr := srv.handleTaskFinish(ctx, params)
	if rerr == nil {
		t.Fatal("expected error for unknown task")
	}
	if rerr.Code != rpc.ErrTaskNotFound {
		t.Errorf("code=%d, want ErrTaskNotFound (%d)", rerr.Code, rpc.ErrTaskNotFound)
	}
}

func TestDecomposeApplyPersistsDepsAndRelevantFiles(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "demo", Name: "Demo", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "parent1", ProjectID: "p1", Source: "inbox", Title: "parent", Body: "x", Priority: "P1", Status: "pending", Pipeline: "build"}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	// Use raw JSON so we don't need to import the decompose package.
	params := json.RawMessage(`{
		"parent_task_id": "parent1",
		"subtasks": [
			{"title": "s0", "body": "b", "priority": "P1", "pipeline": "build", "relevant_files": ["a.go"]},
			{"title": "s1", "body": "b", "priority": "P1", "pipeline": "build", "depends_on": [0], "relevant_files": ["b.go"]}
		]
	}`)
	out, rerr := srv.handleDecomposeApply(ctx, params)
	if rerr != nil {
		t.Fatalf("apply: %v", rerr)
	}
	var res DecomposeApplyResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(res.InsertedTaskIDs) != 2 {
		t.Fatalf("inserted %d, want 2", len(res.InsertedTaskIDs))
	}

	t0, err := d.store.GetTask(ctx, res.InsertedTaskIDs[0])
	if err != nil {
		t.Fatalf("GetTask s0: %v", err)
	}
	t1, err := d.store.GetTask(ctx, res.InsertedTaskIDs[1])
	if err != nil {
		t.Fatalf("GetTask s1: %v", err)
	}

	if got, _ := t0.Metadata["relevant_files"].(string); got != "a.go" {
		t.Errorf("s0 relevant_files = %q, want \"a.go\"", got)
	}
	if got, _ := t1.Metadata["depends_on"].(string); got != res.InsertedTaskIDs[0] {
		t.Errorf("s1 depends_on = %q, want %q", got, res.InsertedTaskIDs[0])
	}
	if got, _ := t1.Metadata["relevant_files"].(string); got != "b.go" {
		t.Errorf("s1 relevant_files = %q, want \"b.go\"", got)
	}
}

// TestAbandonSupersededRunKeepsDone verifies that abandoning an old superseded
// run of an already-done task does NOT regress the task back to needs_attention.
// The gate is satisfied, so refreshTaskStatus should keep the task at "done"
// regardless of which run is abandoned.
func TestAbandonSupersededRunKeepsDone(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	// Insert supporting project.
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "p1", Name: "P", Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	// Seed task: status "done", gate satisfied (sequence merged).
	if err := d.store.InsertTask(ctx, &store.Task{
		ID:        "task-1",
		ProjectID: "p1",
		Title:     "test task",
		Pipeline:  "build",
		Status:    "done",
		GateState: sequence.GateSatisfied,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	now := time.Now()
	older := now.Add(-10 * time.Minute)

	// rNew: the winning done run (newer).
	if err := d.store.InsertRun(ctx, &store.Run{
		ID:        "run-new",
		TaskID:    "task-1",
		ProjectID: "p1",
		Pipeline:  "build",
		Status:    "done",
		EndedAt:   &now,
	}); err != nil {
		t.Fatalf("InsertRun new: %v", err)
	}

	// rOld: the superseded done run (older) — this is the one we will abandon.
	if err := d.store.InsertRun(ctx, &store.Run{
		ID:        "run-old",
		TaskID:    "task-1",
		ProjectID: "p1",
		Pipeline:  "build",
		Status:    "done",
		EndedAt:   &older,
	}); err != nil {
		t.Fatalf("InsertRun old: %v", err)
	}

	// Abandon the OLD (superseded) run via the RPC handler.
	params, _ := json.Marshal(map[string]any{"run_id": "run-old"})
	_, rpcErr := srv.handleRunAbandon(ctx, json.RawMessage(params))
	if rpcErr != nil {
		t.Fatalf("handleRunAbandon: %+v", rpcErr)
	}

	// Task must still be "done" — abandoning a superseded run must not regress it.
	got, err := d.store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("task status = %q, want %q (abandon of superseded run must not regress done task)", got.Status, "done")
	}
}
