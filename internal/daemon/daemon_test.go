package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/adapter/claudecode"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
	"github.com/rohilrs/Hive/pkg/rpc"
)

func buildFakeClaude(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fake-claude")
	cmd := exec.Command("go", "build", "-o", out, "../../scripts/fake-claude")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake-claude: %v\n%s", err, b)
	}
	return out
}

func buildHive(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "hive")
	cmd := exec.Command("go", "build", "-o", out, "../../cmd/hive")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build hive: %v\n%s", err, b)
	}
	return out
}

func setupFixtureRepo(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("bash", "../../testdata/setup.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("setup fixture: %v\n%s", err, out)
	}
	abs, _ := filepath.Abs("../../testdata/fixtures/repos/simple-go")
	return abs
}

func makeFakeRealHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	cdir := filepath.Join(home, ".claude")
	_ = os.MkdirAll(filepath.Join(cdir, "skills", "implement"), 0755)
	_ = os.MkdirAll(filepath.Join(cdir, "skills", "review-code"), 0755)
	_ = os.WriteFile(filepath.Join(cdir, "skills", "implement", "SKILL.md"), []byte("# implement"), 0644)
	_ = os.WriteFile(filepath.Join(cdir, "skills", "review-code", "SKILL.md"), []byte("# review"), 0644)
	_ = os.WriteFile(filepath.Join(cdir, ".credentials.json"), []byte("{}"), 0600)
	_ = os.WriteFile(filepath.Join(cdir, "settings.json"), []byte("{}"), 0644)
	return home
}

// TestDaemonEndToEndBuildPipeline is the integration smoke screen for the
// entire orchestrator: real claudecode.Adapter + fake-claude binary +
// seeded SQLite + RPC roundtrip. It drives a task from task.add through
// run.now and waits for the run to reach a terminal status.
//
// Why this test exists in package daemon (not _test): it pokes the
// unexported d.store directly to seed a Project row without having to add
// a project.create RPC just for tests.
//
// Composition root note: this _test.go file imports
// internal/adapter/claudecode — that's the only place outside cmd/hive
// where the daemon is paired with a concrete adapter. Production code in
// internal/daemon stays provider-agnostic.
func TestDaemonEndToEndBuildPipeline(t *testing.T) {
	fake := buildFakeClaude(t)
	hive := buildHive(t)
	fixture, _ := filepath.Abs("../../scripts/fake-claude/fixtures/approve_immediately.jsonl")
	repo := setupFixtureRepo(t)
	realHome := makeFakeRealHome(t)

	cfg := config.Default()
	sdk := anthropic.NewSDK(anthropic.SDKConfig{
		APIKey: "test",
		Model:  cfg.Models.Classifier,
	})
	adp := claudecode.New(claudecode.Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixture},
		HiveBinary: hive,
		RealHome:   realHome,
		Classifier: sdk,
	})

	hiveDir := t.TempDir()
	d, err := New(Config{HiveDir: hiveDir, Cfg: cfg, Adapter: adp})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	sockPath := filepath.Join(hiveDir, "daemon.sock")
	waitFor(t, func() bool { _, err := os.Stat(sockPath); return err == nil }, 3*time.Second)

	repoPath := repo
	if err := d.store.InsertProject(ctx, &store.Project{
		ID:       "p1",
		Slug:     "auth",
		Name:     "Auth",
		Status:   "active",
		RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}

	addResp := rpcCall(t, sockPath, rpc.MethodAddTask, map[string]any{
		"project_slug": "auth",
		"title":        "fix login bug",
		"body":         "session expires too soon",
		"priority":     "P1",
	})
	taskID, ok := addResp["task_id"].(string)
	if !ok || taskID == "" {
		t.Fatalf("task.add did not return task_id: %#v", addResp)
	}

	runResp := rpcCall(t, sockPath, rpc.MethodRunNow, map[string]any{"task_id": taskID})
	runID, ok := runResp["run_id"].(string)
	if !ok || runID == "" {
		t.Fatalf("run.now did not return run_id: %#v", runResp)
	}

	var finalRun *store.Run
	waitFor(t, func() bool {
		r, err := d.store.GetRun(ctx, runID)
		if err != nil {
			return false
		}
		finalRun = r
		return r.Status == "done" || r.Status == "needs_attention"
	}, 60*time.Second)

	// fake-claude's approve_immediately fixture emits a verdict tool call,
	// but it does NOT actually invoke `hive mcp-stage-server` (that requires
	// a real MCP client). So the path here is: implement stage runs, review
	// stage runs, no verdict received via UDS, classifier fallback called
	// (which fails because the fake Anthropic API rejects "test" key).
	// -> review stage's verdict becomes CHANGES_REQUESTED (fail-safe) for all
	//    iterations -> run lands in needs_attention.
	if finalRun == nil {
		t.Fatal("run never reached terminal state")
	}
	if finalRun.Status != "needs_attention" && finalRun.Status != "done" {
		t.Errorf("final status=%s (expected needs_attention or done)", finalRun.Status)
	}
}

// rpcCall opens a one-shot Unix-socket connection, writes one
// newline-terminated JSON request, reads one newline-terminated response,
// fails the test on transport or RPC error, and returns the decoded
// Result as a map. Result is *json.RawMessage on the wire per Response[T],
// but for the two methods this test uses (task.add, run.now) the result
// payload is a flat map.
func rpcCall(t *testing.T, sockPath, method string, params map[string]any) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, _ := json.Marshal(map[string]any{
		"id":     "req-1",
		"method": method,
		"params": params,
	})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write rpc request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64*1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read rpc response: %v", err)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("unmarshal rpc response: %v: %s", err, string(buf[:n]))
	}
	if resp.Error != nil {
		t.Fatalf("rpc %s: %s", method, resp.Error.Message)
	}
	return resp.Result
}

// waitFor polls cond every 50ms until it returns true or timeout elapses.
// Fails the test on timeout. Used instead of fixed sleeps so subprocess
// boot + scheduler-tick latencies don't make this test flaky.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// noopAdapter is a minimal adapter.Adapter for daemon-lifecycle tests
// that don't exercise RunStage.
type noopAdapter struct{}

func (noopAdapter) Name() string { return "noop" }
func (noopAdapter) Close() error { return nil }
func (noopAdapter) ClassifyVerdict(_ context.Context, _ string) (*adapter.Verdict, error) {
	return &adapter.Verdict{Kind: adapter.VerdictChangesRequested}, nil
}
func (noopAdapter) RunStage(_ context.Context, _ adapter.StageRequest) (*adapter.StageOutput, error) {
	return &adapter.StageOutput{}, nil
}

// fileRefsMissingAdapter is a fake adapter that succeeds on implement and
// returns verdict.ErrFileRefsMissing on review, exercising the daemon's
// REVIEW_FEEDBACK_MISSING status mapping in executePipeline.
type fileRefsMissingAdapter struct{}

func (fileRefsMissingAdapter) Name() string { return "file-refs-missing" }
func (fileRefsMissingAdapter) Close() error { return nil }
func (fileRefsMissingAdapter) ClassifyVerdict(_ context.Context, _ string) (*adapter.Verdict, error) {
	return &adapter.Verdict{Kind: adapter.VerdictChangesRequested}, nil
}
func (fileRefsMissingAdapter) RunStage(_ context.Context, req adapter.StageRequest) (*adapter.StageOutput, error) {
	if req.StageName == "review" {
		return nil, verdict.ErrFileRefsMissing
	}
	return &adapter.StageOutput{}, nil
}

// TestDaemonReviewFeedbackMissingStatus verifies that when the adapter's
// review stage returns verdict.ErrFileRefsMissing, executePipeline detects
// it via errors.Is and marks the run with status="error" and a summary that
// names REVIEW_FEEDBACK_MISSING. This exercises the chain:
//
//	adapter.RunStage → pipeline.Run → executePipeline → store.MarkRunEnded
func TestDaemonReviewFeedbackMissingStatus(t *testing.T) {
	repo := setupFixtureRepo(t)

	hiveDir := t.TempDir()
	cfg := config.Default()
	d, err := New(Config{
		HiveDir: hiveDir,
		Cfg:     cfg,
		Adapter: noopAdapter{}, // replaced below via SetAdapter
	})
	if err != nil {
		t.Fatal(err)
	}
	d.SetAdapter(fileRefsMissingAdapter{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	sockPath := filepath.Join(hiveDir, "daemon.sock")
	waitFor(t, func() bool { _, err := os.Stat(sockPath); return err == nil }, 3*time.Second)

	repoPath := repo
	if err := d.store.InsertProject(ctx, &store.Project{
		ID:       "p1",
		Slug:     "auth",
		Name:     "Auth",
		Status:   "active",
		RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}

	addResp := rpcCall(t, sockPath, rpc.MethodAddTask, map[string]any{
		"project_slug": "auth",
		"title":        "fix login bug",
		"body":         "session expires too soon",
		"priority":     "P1",
	})
	taskID, ok := addResp["task_id"].(string)
	if !ok || taskID == "" {
		t.Fatalf("task.add did not return task_id: %#v", addResp)
	}

	runResp := rpcCall(t, sockPath, rpc.MethodRunNow, map[string]any{"task_id": taskID})
	runID, ok := runResp["run_id"].(string)
	if !ok || runID == "" {
		t.Fatalf("run.now did not return run_id: %#v", runResp)
	}

	var finalRun *store.Run
	waitFor(t, func() bool {
		r, err := d.store.GetRun(ctx, runID)
		if err != nil {
			return false
		}
		finalRun = r
		return r.Status == "done" || r.Status == "needs_attention" || r.Status == "error"
	}, 30*time.Second)

	if finalRun == nil {
		t.Fatal("run never reached terminal state")
	}
	if finalRun.Status != "error" {
		t.Errorf("status=%q want %q", finalRun.Status, "error")
	}
	if !strings.Contains(finalRun.Summary, "REVIEW_FEEDBACK_MISSING") {
		t.Errorf("summary=%q want it to contain %q", finalRun.Summary, "REVIEW_FEEDBACK_MISSING")
	}
}

// TestResolvePipelineRegistered verifies that New registers a "resolve" entry
// in the pipeline map so the scheduler can dispatch resolve runs.
func TestResolvePipelineRegistered(t *testing.T) {
	hiveDir := t.TempDir()
	cfg := config.Default()
	d, err := New(Config{
		HiveDir: hiveDir,
		Cfg:     cfg,
		Adapter: noopAdapter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.pipelines["resolve"]; !ok {
		t.Fatal("resolve pipeline not registered")
	}
}

// TestReapStaleChatSessionsAtStartup verifies that reapStaleChatSessions
// closes stale-open chat sessions and leaves fresh or already-ended ones
// untouched. The test drives the method directly (package-level access) via
// a daemon constructed with an in-memory store, without starting the full
// Start() loop.
func TestReapStaleChatSessionsAtStartup(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Chat.OpenSessionStaleHours = 1

	d := &Daemon{
		store: st,
		cfg:   Config{Cfg: cfg},
	}
	d.ctx = ctx

	now := time.Now().Unix()

	// stale-open: started 2 hours ago → should be reaped
	if err := st.InsertChatSession(ctx, &store.ChatSession{
		ID: "stale", Surface: "cli", StartedAt: now - 7200,
	}); err != nil {
		t.Fatal(err)
	}

	// fresh-open: started 30 minutes ago → must not be reaped
	if err := st.InsertChatSession(ctx, &store.ChatSession{
		ID: "fresh", Surface: "cli", StartedAt: now - 1800,
	}); err != nil {
		t.Fatal(err)
	}

	d.reapStaleChatSessions()

	stale, err := st.GetChatSession(ctx, "stale")
	if err != nil {
		t.Fatalf("GetChatSession stale: %v", err)
	}
	if stale.EndedAt == 0 {
		t.Errorf("stale session: EndedAt still 0 after reap")
	}

	fresh, err := st.GetChatSession(ctx, "fresh")
	if err != nil {
		t.Fatalf("GetChatSession fresh: %v", err)
	}
	if fresh.EndedAt != 0 {
		t.Errorf("fresh session: EndedAt=%d, want 0 (should not be reaped)", fresh.EndedAt)
	}
}

// TestReapStaleChatSessionsDisabledWhenZero verifies that setting
// OpenSessionStaleHours = 0 disables the reaper (no sessions are touched).
func TestReapStaleChatSessionsDisabledWhenZero(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Chat.OpenSessionStaleHours = 0 // disabled

	d := &Daemon{
		store: st,
		cfg:   Config{Cfg: cfg},
	}
	d.ctx = ctx

	// Insert a session that would otherwise be reaped (started 24 hours ago).
	if err := st.InsertChatSession(ctx, &store.ChatSession{
		ID: "old", Surface: "cli", StartedAt: time.Now().Unix() - 86400,
	}); err != nil {
		t.Fatal(err)
	}

	d.reapStaleChatSessions() // must be a no-op

	got, err := st.GetChatSession(ctx, "old")
	if err != nil {
		t.Fatalf("GetChatSession: %v", err)
	}
	if got.EndedAt != 0 {
		t.Errorf("reaper ran despite OpenSessionStaleHours=0: EndedAt=%d", got.EndedAt)
	}
}

// TestDaemonStartedAtSetAtBoot verifies New stamps startedAt so doctor.health
// can compute uptime from it. We don't need to Start the daemon — startedAt is
// set in New (not Start) intentionally so it survives Start failures.
func TestDaemonStartedAtSetAtBoot(t *testing.T) {
	hiveDir := t.TempDir()
	cfg := config.Default()
	d, err := New(Config{
		HiveDir: hiveDir,
		Cfg:     cfg,
		Adapter: noopAdapter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.StartedAt().IsZero() {
		t.Errorf("Daemon.StartedAt zero after construction; expected set")
	}
	if time.Since(d.StartedAt()) > time.Second {
		t.Errorf("Daemon.StartedAt %v stale (>1s old); expected recent", d.StartedAt())
	}
}

func TestDaemonWithNilScavengerNoOp(t *testing.T) {
	hiveDir := t.TempDir()
	cfg := config.Default()

	d, err := New(Config{
		HiveDir:   hiveDir,
		Cfg:       cfg,
		Adapter:   noopAdapter{},
		Scavenger: nil, // explicit
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()
	// Wait for Start to finish wiring before calling Stop, per the Start
	// contract (Stop reads d.mcpServer/d.cancel/d.listener that Start writes).
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	d.Stop()
	// No panic, daemon comes up and down cleanly without a scavenger.
}
