package claudecode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	fakeClaudeOnce sync.Once
	fakeClaudePath string
	fakeClaudeErr  error
)

// buildFakeClaude builds the fake-claude binary once per test process
// and returns the cached path. Subsequent callers within the same
// `go test ./...` run skip the rebuild — important for performance,
// since this package has ~7 callers across test files.
//
// We don't use t.Cleanup to delete the binary: the OS reclaims
// os.TempDir between runs, and tying cleanup to any single test's
// lifetime would defeat the sharing.
func buildFakeClaude(t *testing.T) string {
	t.Helper()
	fakeClaudeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fake-claude-*")
		if err != nil {
			fakeClaudeErr = err
			return
		}
		out := filepath.Join(dir, "fake-claude")
		cmd := exec.Command("go", "build", "-o", out, "../../../scripts/fake-claude")
		if b, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeErr = fmt.Errorf("build fake-claude: %w\n%s", err, b)
			return
		}
		fakeClaudePath = out
	})
	if fakeClaudeErr != nil {
		t.Fatal(fakeClaudeErr)
	}
	return fakeClaudePath
}

func TestSubprocessParsesApprovedFixture(t *testing.T) {
	fake := buildFakeClaude(t)
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/approve_immediately.jsonl")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := NewSubprocess(SubprocessConfig{
		Binary: fake,
		Args:   []string{"-fixture", fixture, "-p", "ignored prompt"},
	})
	result, err := cmd.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var sawApprove bool
	for _, ev := range result.Events {
		if ev.Type == EventToolUse && ev.ToolName == "hive_submit_review_verdict" {
			sawApprove = true
		}
	}
	if !sawApprove {
		t.Error("did not see verdict tool call in events")
	}
}

func TestSubprocessOnEventInvokedPerEvent(t *testing.T) {
	fake := buildFakeClaude(t)
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/approve_immediately.jsonl")

	var (
		mu     sync.Mutex
		seen   []EventType
		stamps []time.Time
	)
	sub := NewSubprocess(SubprocessConfig{
		Binary: fake,
		Args:   []string{"-fixture", fixture, "-p", "ignored"},
		OnEvent: func(ev Event, when time.Time) {
			mu.Lock()
			seen = append(seen, ev.Type)
			stamps = append(stamps, when)
			mu.Unlock()
		},
	})
	if _, err := sub.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("OnEvent never invoked")
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i].Before(stamps[i-1]) {
			t.Errorf("OnEvent timestamps not monotonic at i=%d", i)
		}
	}
}

func TestSubprocessSignalDeliversToProcess(t *testing.T) {
	sub := NewSubprocess(SubprocessConfig{
		Binary: "/bin/sleep",
		Args:   []string{"30"},
	})
	doneCh := make(chan error, 1)
	go func() {
		_, err := sub.Run(context.Background())
		doneCh <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := sub.Signal("SIGTERM"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case <-doneCh:
		// good — process exited from signal
	case <-time.After(3 * time.Second):
		t.Fatal("subprocess did not exit after SIGTERM")
	}
}

func TestSubprocessRespectsCancellation(t *testing.T) {
	fake := buildFakeClaude(t)
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/tool_call_stall.jsonl")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := NewSubprocess(SubprocessConfig{Binary: fake, Args: []string{"-fixture", fixture}})
	_, err := cmd.Run(ctx)
	if err == nil {
		t.Error("expected timeout error")
	}
}

// TestSubprocessStderrInError verifies that when the subprocess exits non-zero
// the returned error message includes the captured stderr text, not just
// "exit status N". This is critical for the chat agent: when claude -p fails
// (e.g. invalid --resume session, auth error, MCP load failure) the error
// frame written to chat_messages shows the actual cause rather than an opaque
// exit code.
func TestSubprocessStderrInError(t *testing.T) {
	// Use /bin/sh to emit a known stderr message and exit non-zero.
	// This avoids any dependency on fake-claude's fixture format.
	sub := NewSubprocess(SubprocessConfig{
		Binary: "/bin/sh",
		Args:   []string{"-c", `echo "session not found: bad-id" >&2; exit 1`},
	})
	_, err := sub.Run(context.Background())
	if err == nil {
		t.Fatal("expected non-nil error from non-zero exit")
	}
	if !strings.Contains(err.Error(), "session not found: bad-id") {
		t.Errorf("error does not contain stderr text; got: %s", err.Error())
	}
}

// On a failed turn claude prints the real reason (auth 401, overload, etc.) on
// STDOUT as a result/assistant frame while STDERR holds only a benign warning
// (a stale deny rule). The surfaced error must LEAD with the stdout reason so
// it isn't a red herring — this reproduces the planner "exit status 1 (stderr:
// Permission deny rule TaskSearch ...)" misdiagnosis.
func TestSubprocessSurfacesStdoutReasonOverStderrWarning(t *testing.T) {
	const realReason = "Failed to authenticate. API Error: 401 Invalid authentication credentials"
	const stderrWarn = `Permission deny rule "TaskSearch" matches no known tool — check for typos.`
	script := fmt.Sprintf(`
echo %q >&2
printf '%%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}'
printf '%%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true}'
exit 1`, stderrWarn, realReason)
	sub := NewSubprocess(SubprocessConfig{Binary: "/bin/sh", Args: []string{"-c", script}})
	_, err := sub.Run(context.Background())
	if err == nil {
		t.Fatal("expected non-nil error from non-zero exit")
	}
	msg := err.Error()
	if !strings.Contains(msg, realReason) {
		t.Errorf("surfaced error omits the real stdout reason; got: %s", msg)
	}
	// The real reason must appear before the stderr warning (lead, not buried).
	if i, j := strings.Index(msg, realReason), strings.Index(msg, "TaskSearch"); j != -1 && i > j {
		t.Errorf("stdout reason should lead the stderr warning; got: %s", msg)
	}
}

func TestFailureReasonPrefersResultTextThenAssistantThenSubtype(t *testing.T) {
	assistant := Event{Type: "assistant", Message: Message{Content: []ContentBlock{{Type: "text", Text: "partial answer"}}}}
	resultText := Event{Type: EventResult, Subtype: "error_during_execution", IsError: true, Result: "API Error: 529 overloaded"}
	resultBare := Event{Type: EventResult, Subtype: "error_max_turns", IsError: true}

	if got := failureReason([]Event{assistant, resultText}); got != "API Error: 529 overloaded" {
		t.Errorf("result text should win; got %q", got)
	}
	if got := failureReason([]Event{assistant, resultBare}); got != "partial answer" {
		t.Errorf("assistant text should win when result has no text; got %q", got)
	}
	if got := failureReason([]Event{resultBare}); got != "error_max_turns" {
		t.Errorf("subtype should be the last resort; got %q", got)
	}
	if got := failureReason(nil); got != "" {
		t.Errorf("no events should yield empty reason; got %q", got)
	}
}

func TestOnStartedFiresWithSubprocessPID(t *testing.T) {
	var capturedPID int
	cfg := SubprocessConfig{
		Binary: "true", // exits immediately, exit 0
		OnStarted: func(pid int) error {
			capturedPID = pid
			return nil
		},
	}
	sub := NewSubprocess(cfg)
	res, err := sub.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if capturedPID <= 0 {
		t.Errorf("capturedPID=%d, want > 0", capturedPID)
	}
	_ = res
}

func TestOnExitedFiresOnGracefulExit(t *testing.T) {
	var startedPID, exitedPID int
	cfg := SubprocessConfig{
		Binary:    "true",
		OnStarted: func(pid int) error { startedPID = pid; return nil },
		OnExited:  func(pid int) { exitedPID = pid },
	}
	sub := NewSubprocess(cfg)
	if _, err := sub.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitedPID == 0 {
		t.Errorf("OnExited never fired")
	}
	if exitedPID != startedPID {
		t.Errorf("exitedPID=%d != startedPID=%d", exitedPID, startedPID)
	}
}

func TestOnStartedErrAbortsRun(t *testing.T) {
	cfg := SubprocessConfig{
		Binary: "sleep",
		Args:   []string{"30"}, // long-running so kill is observable
		OnStarted: func(pid int) error {
			return fmt.Errorf("stamp failed")
		},
	}
	sub := NewSubprocess(cfg)
	start := time.Now()
	_, err := sub.Run(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from OnStarted to propagate")
	}
	if !strings.Contains(err.Error(), "stamp failed") {
		t.Errorf("error doesn't reference OnStarted: %v", err)
	}
	// Must NOT have waited for the 30s sleep — the subprocess was killed.
	if elapsed > 5*time.Second {
		t.Errorf("elapsed=%v; subprocess wasn't killed promptly", elapsed)
	}
}

func TestNilCallbacksTolerated(t *testing.T) {
	cfg := SubprocessConfig{
		Binary: "true",
		// OnStarted + OnExited deliberately nil.
	}
	sub := NewSubprocess(cfg)
	if _, err := sub.Run(context.Background()); err != nil {
		t.Errorf("Run with nil callbacks: %v", err)
	}
}
