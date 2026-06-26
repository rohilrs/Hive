package claudecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/scavenger"
	"github.com/rohilrs/Hive/internal/verdict"
)

func buildHiveForAdapter(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "hive")
	cmd := exec.Command("go", "build", "-o", out, "../../../cmd/hive")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build hive: %v\n%s", err, b)
	}
	return out
}

func makeFakeRealHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	cdir := filepath.Join(home, ".claude")
	_ = os.MkdirAll(filepath.Join(cdir, "skills", "review-code"), 0755)
	_ = os.WriteFile(filepath.Join(cdir, "skills", "review-code", "SKILL.md"), []byte("# review"), 0644)
	_ = os.WriteFile(filepath.Join(cdir, ".credentials.json"), []byte("{}"), 0600)
	_ = os.WriteFile(filepath.Join(cdir, "settings.json"), []byte("{}"), 0644)
	return home
}

func TestAdapterFallsBackToClassifier(t *testing.T) {
	fake := buildFakeClaude(t)
	_ = buildHiveForAdapter(t)
	realHome := makeFakeRealHome(t)
	stageDir := t.TempDir()

	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/no_verdict_tool.jsonl")

	a := New(Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixture},
		HiveBinary: "/unused/hive",
		RealHome:   realHome,
		Classifier: &fakeSDK{response: &anthropic.VerdictResult{Verdict: "APPROVE", Confidence: 70}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := a.RunStage(ctx, adapter.StageRequest{
		RunID: "r", StageName: "review",
		StageDir: stageDir, Cwd: stageDir,
		// VerdictToolName left empty -> no listener bound, classifier path
		Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict == nil {
		t.Fatal("nil verdict")
	}
	if out.Verdict.FromTool {
		t.Error("expected classifier fallback")
	}
}

func TestAdapterPassesPluginDirWhenScavengerInitialized(t *testing.T) {
	fake := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)
	stageDir := t.TempDir()

	// Worktree (Hive's per-run dir). The per-run InstallPlugin has already
	// created the worktree-local plugin tree before the adapter runs.
	worktree := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	pluginRoot := filepath.Join(worktree, ".scavenger", "claude-plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".claude-plugin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"), []byte(`{"name":"scavenger"}`), 0644); err != nil {
		t.Fatal(err)
	}

	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/no_verdict_tool.jsonl")
	scavClient := scavenger.NewClient(scavenger.Config{Binary: "scavenger"})

	a := New(Config{
		Binary:           fake,
		ExtraArgs:        []string{"-fixture", fixture},
		HiveBinary:       "/unused/hive",
		RealHome:         realHome,
		Classifier:       &fakeSDK{response: &anthropic.VerdictResult{Verdict: "APPROVE", Confidence: 70}},
		Scavenger:        scavClient,
		ScavengerEnabled: true,
		ScavengerBinary:  "scavenger",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.RunStage(ctx, adapter.StageRequest{
		RunID:     "r",
		StageName: "review",
		StageDir:  stageDir,
		Cwd:       worktree,
		Timeout:   3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// --plugin-dir points at the worktree-local plugin (no RunDir -> source).
	argv := readFakeClaudeArgv(t, worktree)
	if !argvHasFlagWithValue(argv, "--plugin-dir", pluginRoot) {
		t.Errorf("--plugin-dir not pointing at worktree plugin\nargv=%v\nwant=%s", argv, pluginRoot)
	}
	// No symlink should be created at the worktree's .scavenger — it is the
	// real per-run plugin dir, not a link into the canonical repo.
	info, err := os.Lstat(filepath.Join(worktree, ".scavenger"))
	if err != nil {
		t.Fatalf("worktree .scavenger should exist: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("worktree .scavenger is a symlink; should be a real dir")
	}
}

func TestAdapterSkipsPluginDirWhenScavengerNotInitialized(t *testing.T) {
	fake := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)
	stageDir := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "wt") // no .scavenger plugin here
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/no_verdict_tool.jsonl")

	a := New(Config{
		Binary:           fake,
		ExtraArgs:        []string{"-fixture", fixture},
		HiveBinary:       "/unused/hive",
		RealHome:         realHome,
		Classifier:       &fakeSDK{response: &anthropic.VerdictResult{Verdict: "APPROVE", Confidence: 70}},
		Scavenger:        scavenger.NewClient(scavenger.Config{Binary: "scavenger"}),
		ScavengerEnabled: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.RunStage(ctx, adapter.StageRequest{
		RunID:     "r",
		StageName: "review",
		StageDir:  stageDir,
		Cwd:       worktree,
		Timeout:   3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// No worktree-local plugin -> no --plugin-dir flag.
	argv := readFakeClaudeArgv(t, worktree)
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--plugin-dir" {
			t.Errorf("unexpected --plugin-dir flag with no worktree plugin: argv=%v", argv)
		}
	}
}

func TestPluginSourcedFromWorktreeNotSymlinked(t *testing.T) {
	wt := t.TempDir()
	// Simulate the per-run InstallPlugin output in the worktree.
	pluginSrc := filepath.Join(wt, ".scavenger", "claude-plugin", ".claude-plugin")
	if err := os.MkdirAll(pluginSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginSrc, "plugin.json"),
		[]byte(`{"name":"scavenger"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := scavengerPluginSource(wt)
	want := filepath.Join(wt, ".scavenger", "claude-plugin")
	if dir != want {
		t.Errorf("plugin source = %q, want %q", dir, want)
	}
}

// TestDrainListenerReturnsErrFileRefsMissing exercises the adapter's
// drainListener helper for the rejection → ErrFileRefsMissing path.
//
// fake-claude can't make real MCP tool calls, so we exercise the select
// logic directly: spin up a real verdict.Listener, forward a
// CHANGES_REQUESTED frame with no file_refs (which the listener rejects
// and pushes to its buffered rejections channel), then call drainListener
// and verify it surfaces verdict.ErrFileRefsMissing.
func TestDrainListenerReturnsErrFileRefsMissing(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "verdict.sock")
	l, err := verdict.Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	// Forward a CHANGES_REQUESTED frame with no file_refs. The listener
	// goroutine rejects it synchronously and pushes the ack to the buffered
	// rejections channel before Forward returns.
	if _, err := verdict.Forward(sockPath, verdict.Frame{
		RunID: "r-test", Stage: "review",
		Verdict: "CHANGES_REQUESTED", Confidence: 70,
		// FileRefs intentionally omitted
	}); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	v, drainErr := drainListener(l)
	if v != nil {
		t.Errorf("expected nil verdict, got %+v", v)
	}
	if !errors.Is(drainErr, verdict.ErrFileRefsMissing) {
		t.Errorf("expected ErrFileRefsMissing, got %v", drainErr)
	}
}

func TestRunStagePopulatesTokens(t *testing.T) {
	// fake-claude with the approve_immediately fixture has a result
	// event carrying usage. After RunStage returns, StageOutput.Tokens
	// must reflect those values.
	bin := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/approve_immediately.jsonl")
	a := New(Config{
		Binary:     bin,
		ExtraArgs:  []string{"-fixture", fixture},
		RealHome:   realHome,
		HiveBinary: "/unused/hive",
	})

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := a.RunStage(ctx, adapter.StageRequest{
		RunID: "r1", StageName: "review", Iter: 0, Model: "claude-haiku-4-5",
		Cwd: dir, StageDir: filepath.Join(dir, "stage"),
		VerdictToolName: "hive_submit_review_verdict",
	})
	if err != nil {
		t.Fatalf("RunStage: %v", err)
	}
	if out.Tokens.Input == 0 || out.Tokens.Output == 0 {
		t.Errorf("expected non-zero tokens, got %+v", out.Tokens)
	}
}

func TestRunStagePopulatesToolCalls(t *testing.T) {
	bin := buildFakeClaude(t)
	fixture, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "fake-claude", "fixtures", "approve_immediately.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{Binary: bin, ExtraArgs: []string{"-fixture", fixture}, RealHome: t.TempDir(), HiveBinary: "hive"})

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := a.RunStage(ctx, adapter.StageRequest{
		RunID: "r1", StageName: "review", Iter: 0, Model: "claude-haiku-4-5",
		Cwd: dir, StageDir: filepath.Join(dir, "stage"),
		VerdictToolName: "hive_submit_review_verdict",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) == 0 {
		t.Fatalf("expected at least one tool call extracted, got 0")
	}
	// First tool call should be the verdict tool that produced the APPROVE.
	found := false
	for _, tc := range out.ToolCalls {
		if tc.Name == "hive_submit_review_verdict" {
			found = true
			if !tc.Success {
				t.Errorf("verdict tool call should be success: %+v", tc)
			}
			if tc.EndedAt.IsZero() {
				t.Errorf("verdict tool call should have EndedAt: %+v", tc)
			}
		}
	}
	if !found {
		t.Errorf("hive_submit_review_verdict not found in tool calls: %+v", out.ToolCalls)
	}
}

// TestRunStagePopulatesToolCallsFromNestedFormat verifies the adapter
// extracts tool calls from real-claude's NESTED stream-json shape
// (tool_use inside assistant.message.content[], tool_result inside
// user.message.content[]). The original TestRunStagePopulatesToolCalls
// exercises the top-level shape used by older fixtures; this test
// is the regression guard for the 3.1 smoke bug where real claude
// emitted 17 tool_use blocks in the implement stage and the adapter
// saw 0.
func TestRunStagePopulatesToolCallsFromNestedFormat(t *testing.T) {
	bin := buildFakeClaude(t)
	fixture, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "fake-claude", "fixtures", "real_claude_nested.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{Binary: bin, ExtraArgs: []string{"-fixture", fixture}, RealHome: t.TempDir(), HiveBinary: "hive"})

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := a.RunStage(ctx, adapter.StageRequest{
		RunID: "r1", StageName: "review", Iter: 0, Model: "claude-sonnet-4-6",
		Cwd: dir, StageDir: filepath.Join(dir, "stage"),
		VerdictToolName: "hive_submit_review_verdict",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls from nested format, got %d: %+v", len(out.ToolCalls), out.ToolCalls)
	}
	names := map[string]bool{}
	for _, tc := range out.ToolCalls {
		names[tc.Name] = true
	}
	if !names["Read"] || !names["hive_submit_review_verdict"] {
		t.Errorf("expected Read + hive_submit_review_verdict tool calls; got %v", names)
	}
}

// TestAdapterStallMonitorKillsOnL2 wires the full adapter+monitor path
// against the tool_call_stall fixture: fake-claude emits a tool_use for
// `Bash sleep 9999` and then sleeps 600s. With a short tool-call
// timeout, the monitor should SIGTERM the fake-claude subprocess
// within ~poll-interval after the threshold, and RunStage should
// return ErrToolCallStall with the tool name in the wrapped error.
func TestAdapterStallMonitorKillsOnL2(t *testing.T) {
	bin := buildFakeClaude(t)
	fixture, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "fake-claude", "fixtures", "tool_call_stall.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	store := &fakeStallStore{}
	cfg := Config{
		Binary:               bin,
		ExtraArgs:            []string{"-fixture", fixture},
		HiveBinary:           "/bin/true",
		RealHome:             t.TempDir(),
		StallStore:           store,
		StallToolCallTimeout: 100 * time.Millisecond,
	}
	a := New(cfg)
	stageDir := filepath.Join(t.TempDir(), "stage")

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = a.RunStage(ctx, adapter.StageRequest{
		RunID:      "run-stall",
		StageName:  "implement",
		StageDir:   stageDir,
		StageID:    42,
		UserPrompt: "do a thing",
	})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrToolCallStall) {
		t.Fatalf("got err=%v want ErrToolCallStall", err)
	}
	// Monitor poll interval defaults to 5s when not overridden; allow
	// a generous upper bound but ensure the kill is far short of the
	// 10-minute fixture sleep.
	if elapsed > 10*time.Second {
		t.Errorf("RunStage took %s; SIGTERM didn't fire promptly", elapsed)
	}
	tool, ok := StallToolFromError(err)
	if !ok || tool != "Bash" {
		t.Errorf("StallToolFromError = %q,%v want Bash,true", tool, ok)
	}
	ins := store.snapshotInserts()
	if len(ins) != 1 || ins[0].Layer != 2 {
		t.Errorf("inserts=%+v want 1 L2 row", ins)
	}
	if ins[0].StageID != 42 {
		t.Errorf("StageID=%d want 42", ins[0].StageID)
	}
}

// TestSetWorkerCallbacksForwardedToSubprocess verifies that callbacks
// installed via Adapter.SetWorkerCallbacks are curried with the
// per-stage RunID and fired by the underlying Subprocess on start +
// exit. This is the wiring seam the daemon uses to stamp/clear
// runs.worker_pid for restart-recovery.
func TestSetWorkerCallbacksForwardedToSubprocess(t *testing.T) {
	fake := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)
	stageDir := t.TempDir()
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/no_verdict_tool.jsonl")

	a := New(Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixture},
		HiveBinary: "/unused/hive",
		RealHome:   realHome,
	})

	var (
		startedRunID string
		startedPID   int
		exitedRunID  string
		exitedPID    int
	)
	a.SetWorkerCallbacks(
		func(runID string, pid int) error {
			startedRunID = runID
			startedPID = pid
			return nil
		},
		func(runID string, pid int) {
			exitedRunID = runID
			exitedPID = pid
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.RunStage(ctx, adapter.StageRequest{
		RunID: "test-run-123", StageName: "review",
		StageDir: stageDir, Cwd: stageDir,
		Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunStage: %v", err)
	}

	if startedRunID != "test-run-123" {
		t.Errorf("OnStarted runID=%q, want test-run-123", startedRunID)
	}
	if startedPID <= 0 {
		t.Errorf("OnStarted PID=%d, want > 0", startedPID)
	}
	if exitedRunID != "test-run-123" {
		t.Errorf("OnExited runID=%q, want test-run-123", exitedRunID)
	}
	if exitedPID != startedPID {
		t.Errorf("OnExited PID=%d != OnStarted PID=%d", exitedPID, startedPID)
	}
}

// TestDrainListenerSummaryPropagates verifies that a Frame carrying a non-empty
// Summary produces an adapter.Verdict with the same Summary, and that a Frame
// with no Summary leaves Verdict.Summary empty (zero value).
func TestDrainListenerSummaryPropagates(t *testing.T) {
	t.Run("summary_copied", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "verdict.sock")
		l, err := verdict.Listen(sockPath)
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		defer l.Close()

		want := "Cross-cutting error handling is absent; callers never check returned errors."
		if _, err := verdict.Forward(sockPath, verdict.Frame{
			RunID: "r-s", Stage: "review",
			Verdict: "APPROVE", Confidence: 90,
			Summary: want,
		}); err != nil {
			t.Fatalf("Forward: %v", err)
		}

		v, drainErr := drainListener(l)
		if drainErr != nil {
			t.Fatalf("unexpected error: %v", drainErr)
		}
		if v == nil {
			t.Fatal("expected non-nil Verdict")
		}
		if v.Summary != want {
			t.Errorf("Verdict.Summary=%q want %q", v.Summary, want)
		}
	})

	t.Run("absent_summary_is_empty", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "verdict.sock")
		l, err := verdict.Listen(sockPath)
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		defer l.Close()

		if _, err := verdict.Forward(sockPath, verdict.Frame{
			RunID: "r-ns", Stage: "review",
			Verdict: "APPROVE", Confidence: 95,
			// Summary intentionally omitted
		}); err != nil {
			t.Fatalf("Forward: %v", err)
		}

		v, drainErr := drainListener(l)
		if drainErr != nil {
			t.Fatalf("unexpected error: %v", drainErr)
		}
		if v == nil {
			t.Fatal("expected non-nil Verdict")
		}
		if v.Summary != "" {
			t.Errorf("expected empty Summary, got %q", v.Summary)
		}
	})
}

// TestDrainListenerReturnsNilNilWhenNoEvent verifies that drainListener
// returns (nil, nil) — the "no tool call" signal — when neither channel
// has an event within the timeout window.
func TestDrainListenerReturnsNilNilWhenNoEvent(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "verdict.sock")
	l, err := verdict.Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	// Nothing forwarded — both channels are empty.
	v, drainErr := drainListener(l)
	if v != nil || drainErr != nil {
		t.Errorf("expected (nil, nil), got (%+v, %v)", v, drainErr)
	}
}

// TestAdapterNeverPassesEmptyPromptArg reproduces the claude v2.1.161 breakage:
// `claude --print` (-p) rejects an empty positional prompt
// ("Input must be provided ... when using --print"). The adapter must never
// emit `-p ""`, even when the caller passes an empty UserPrompt.
func TestAdapterNeverPassesEmptyPromptArg(t *testing.T) {
	fake := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)
	worktree := t.TempDir()
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/no_verdict_tool.jsonl")

	a := New(Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixture},
		HiveBinary: "/unused/hive",
		RealHome:   realHome,
		Classifier: &fakeSDK{response: &anthropic.VerdictResult{Verdict: "APPROVE", Confidence: 70}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.RunStage(ctx, adapter.StageRequest{
		RunID:      "r",
		StageName:  "implement",
		StageDir:   t.TempDir(),
		Cwd:        worktree,
		Timeout:    3 * time.Second,
		UserPrompt: "", // the trigger
	})
	if err != nil {
		t.Fatal(err)
	}

	argv := readFakeClaudeArgv(t, worktree)
	if argvHasFlagWithValue(argv, "-p", "") {
		t.Errorf("adapter emitted `-p \"\"` (claude --print rejects an empty prompt)\nargv=%v", argv)
	}
}

// TestAdapterPassesStrictMCPConfig verifies that whenever the stage worker is
// given an --mcp-config, it is also locked down with --strict-mcp-config — so
// the autonomous worker only sees Hive's stage servers (hive-stage, scavenger)
// and NOT the user's global MCP servers (Gmail/Calendar/Drive etc.).
func TestAdapterPassesStrictMCPConfig(t *testing.T) {
	fake := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)
	worktree := t.TempDir()
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/no_verdict_tool.jsonl")

	a := New(Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixture},
		HiveBinary: "/unused/hive",
		RealHome:   realHome,
		Classifier: &fakeSDK{response: &anthropic.VerdictResult{Verdict: "APPROVE", Confidence: 70}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.RunStage(ctx, adapter.StageRequest{
		RunID:           "r",
		StageName:       "review",
		StageDir:        t.TempDir(),
		Cwd:             worktree,
		Timeout:         3 * time.Second,
		UserPrompt:      "review it",
		VerdictToolName: "hive_submit_review_verdict", // forces an --mcp-config
	})
	if err != nil {
		t.Fatal(err)
	}

	argv := readFakeClaudeArgv(t, worktree)
	hasMCP, hasStrict := false, false
	for _, a := range argv {
		if a == "--mcp-config" {
			hasMCP = true
		}
		if a == "--strict-mcp-config" {
			hasStrict = true
		}
	}
	if !hasMCP {
		t.Fatalf("test precondition: expected --mcp-config in argv\nargv=%v", argv)
	}
	if !hasStrict {
		t.Errorf("--mcp-config present but --strict-mcp-config missing (global MCP servers leak in)\nargv=%v", argv)
	}
}

// TestAdapterStageTimeoutSurfacesTypedError is the regression for the cryptic
// "signal: killed": when the stage subprocess outlives StageRequest.Timeout the
// adapter must return a typed ErrStageTimeout (not the raw SIGKILL), so the
// daemon reports "implement timed out after Xs" instead of "signal: killed".
func TestAdapterStageTimeoutSurfacesTypedError(t *testing.T) {
	fake := buildFakeClaude(t)
	_ = buildHiveForAdapter(t)
	realHome := makeFakeRealHome(t)
	stageDir := t.TempDir()

	// slow_hang.jsonl delays 5s before its next line; a 300ms stage timeout kills
	// the subprocess mid-sleep.
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/slow_hang.jsonl")

	a := New(Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixture},
		HiveBinary: "/unused/hive",
		RealHome:   realHome,
	})

	ctx := context.Background()
	_, err := a.RunStage(ctx, adapter.StageRequest{
		RunID: "r", StageName: "implement",
		StageDir: stageDir, Cwd: stageDir,
		Timeout: 300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a stage-timeout error, got nil")
	}
	if !errors.Is(err, ErrStageTimeout) {
		t.Fatalf("err is not ErrStageTimeout: %v", err)
	}
	if !strings.Contains(err.Error(), "implement timed out") {
		t.Errorf("error message not descriptive: %q", err.Error())
	}
}
