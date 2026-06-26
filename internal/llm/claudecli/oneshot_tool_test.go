package claudecli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
)

// buildOneshotFakeClaude writes a small shell-script wrapper that
// simulates a single `claude -p` tool-use turn for OneshotToolRunner
// tests. It bypasses the actual MCP roundtrip: instead of running the
// stdio MCP server and routing a tools/call, the wrapper just reads the
// MCP config the runner wrote, finds the `--capture-args-file` path,
// and writes a sentinel subtasks blob to it directly. The runner's
// production plumbing (tempdir/MCP-config/exec/read-capture) is what's
// under test here; the real MCP roundtrip uses the same mcp-go server
// pattern as the chat-tools and stage servers (covered by their own
// tests) and is exercised end-to-end in T6 live smoke against real
// claude.
//
// Behavior controlled by env:
//   - HIVE_FAKE_ONESHOT_MODE=write   → parse mcp config, write capture, exit 0
//   - HIVE_FAKE_ONESHOT_MODE=skip    → exit 0 without writing capture
//   - HIVE_FAKE_ONESHOT_ENV_DUMP=path → before anything, dump full env to <path>
//
// We do NOT exec fake-claude — there's no fixture for the runner to
// consume (it reads its result from the capture file, not stdout).
func buildOneshotFakeClaude(t *testing.T) string {
	t.Helper()
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "fake-claude-oneshot.sh")
	body := `#!/bin/sh
set -e

if [ -n "$HIVE_FAKE_ONESHOT_ENV_DUMP" ]; then
  env > "$HIVE_FAKE_ONESHOT_ENV_DUMP"
fi

if [ -n "$HIVE_FAKE_ONESHOT_ARGS_DUMP" ]; then
  printf '%s\n' "$@" > "$HIVE_FAKE_ONESHOT_ARGS_DUMP"
fi

if [ -n "$HIVE_FAKE_ONESHOT_CWD_DUMP" ]; then
  pwd > "$HIVE_FAKE_ONESHOT_CWD_DUMP"
fi

# Find the --mcp-config arg value (used to find the capture path).
mcp_config=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--mcp-config" ]; then
    mcp_config="$arg"
    break
  fi
  prev="$arg"
done

if [ "$HIVE_FAKE_ONESHOT_MODE" = "write" ] && [ -n "$mcp_config" ]; then
  cap=$(python3 -c "import json,sys; d=json.load(open('$mcp_config')); s=d['mcpServers']['hive_oneshot']; a=s['args']; print(a[a.index('--capture-args-file')+1])")
  if [ -n "$cap" ]; then
    printf '%s' '{"subtasks":[{"title":"t1","body":"b1","priority":"P1","pipeline":"build"}]}' > "$cap.tmp"
    mv "$cap.tmp" "$cap"
  fi
fi

exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	return script
}

func TestOneshotRunnerHappyPath(t *testing.T) {
	t.Setenv("HIVE_FAKE_ONESHOT_MODE", "write")
	bin := buildOneshotFakeClaude(t)
	r := NewOneshotToolRunner(Config{Binary: bin})
	out, err := r.RunTurn(context.Background(), anthropic.TurnInput{
		Model:  "claude-sonnet-4-6",
		System: "You decompose tasks.",
		Messages: []anth.MessageParam{
			anth.NewUserMessage(anth.NewTextBlock("decompose this")),
		},
		Tools: []anthropic.ToolDef{{
			Name:        "submit_subtasks",
			Description: "Submit the breakdown",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"subtasks": map[string]any{"type": "array"},
				},
			},
		}},
		MaxTokens: 4096,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", out.StopReason)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected 1 ToolCall, got %d", len(out.ToolCalls))
	}
	tc := out.ToolCalls[0]
	if tc.Name != "submit_subtasks" {
		t.Errorf("ToolCall name = %q, want submit_subtasks", tc.Name)
	}
	var parsed struct {
		Subtasks []struct {
			Title    string `json:"title"`
			Priority string `json:"priority"`
		} `json:"subtasks"`
	}
	if err := json.Unmarshal(tc.Input, &parsed); err != nil {
		t.Fatalf("unmarshal captured input: %v\nraw=%s", err, string(tc.Input))
	}
	if len(parsed.Subtasks) != 1 || parsed.Subtasks[0].Title != "t1" || parsed.Subtasks[0].Priority != "P1" {
		t.Errorf("unexpected captured payload: %s", string(tc.Input))
	}
}

func TestOneshotRunnerNoToolCall(t *testing.T) {
	t.Setenv("HIVE_FAKE_ONESHOT_MODE", "skip")
	bin := buildOneshotFakeClaude(t)
	r := NewOneshotToolRunner(Config{Binary: bin})
	_, err := r.RunTurn(context.Background(), anthropic.TurnInput{
		Model:  "x",
		System: "sys",
		Messages: []anth.MessageParam{
			anth.NewUserMessage(anth.NewTextBlock("decompose")),
		},
		Tools: []anthropic.ToolDef{{Name: "submit_subtasks", InputSchema: map[string]any{"type": "object"}}},
	})
	if err == nil {
		t.Fatal("expected error when claude exits without writing capture file")
	}
	if !strings.Contains(err.Error(), "did not call the tool") && !strings.Contains(err.Error(), "no capture") {
		t.Errorf("error should mention missing capture; got %v", err)
	}
}

func TestOneshotRunnerRequiresExactlyOneTool(t *testing.T) {
	r := NewOneshotToolRunner(Config{Binary: "/nonexistent-claude-binary"})

	// Zero tools.
	_, err := r.RunTurn(context.Background(), anthropic.TurnInput{
		Messages: []anth.MessageParam{anth.NewUserMessage(anth.NewTextBlock("x"))},
		Tools:    nil,
	})
	if err == nil || !strings.Contains(err.Error(), "exactly 1 tool") {
		t.Errorf("expected exactly-1-tool error; got %v", err)
	}

	// Two tools.
	_, err = r.RunTurn(context.Background(), anthropic.TurnInput{
		Messages: []anth.MessageParam{anth.NewUserMessage(anth.NewTextBlock("x"))},
		Tools:    []anthropic.ToolDef{{Name: "a"}, {Name: "b"}},
	})
	if err == nil || !strings.Contains(err.Error(), "exactly 1 tool") {
		t.Errorf("expected exactly-1-tool error; got %v", err)
	}
}

func TestOneshotRunnerStripsAPIKey(t *testing.T) {
	// Set a sentinel ANTHROPIC_API_KEY in the test process env and assert
	// the env passed to the claude subprocess does NOT contain it. The
	// shell wrapper dumps env to a file for inspection.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-sentinel-DO-NOT-USE")
	t.Setenv("HIVE_FAKE_ONESHOT_MODE", "write")
	envDump := filepath.Join(t.TempDir(), "env.dump")
	t.Setenv("HIVE_FAKE_ONESHOT_ENV_DUMP", envDump)

	bin := buildOneshotFakeClaude(t)
	r := NewOneshotToolRunner(Config{Binary: bin})
	_, err := r.RunTurn(context.Background(), anthropic.TurnInput{
		System:   "sys",
		Messages: []anth.MessageParam{anth.NewUserMessage(anth.NewTextBlock("decompose this"))},
		Tools:    []anthropic.ToolDef{{Name: "submit_subtasks", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	b, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("read env dump: %v (the wrapper did not run?)", err)
	}
	if strings.Contains(string(b), "ANTHROPIC_API_KEY=sk-test-sentinel-DO-NOT-USE") {
		t.Errorf("ANTHROPIC_API_KEY leaked into claude env (must be stripped):\n%s", string(b))
	}
}

// runOneshotCapturingArgs runs the fake-claude runner with the given Config and
// TurnInput.Model, returning the argv claude was invoked with.
func runOneshotCapturingArgs(t *testing.T, cfg Config, turnModel string) string {
	t.Helper()
	t.Setenv("HIVE_FAKE_ONESHOT_MODE", "write")
	argsDump := filepath.Join(t.TempDir(), "args.dump")
	t.Setenv("HIVE_FAKE_ONESHOT_ARGS_DUMP", argsDump)
	cfg.Binary = buildOneshotFakeClaude(t)
	r := NewOneshotToolRunner(cfg)
	if _, err := r.RunTurn(context.Background(), anthropic.TurnInput{
		Model:    turnModel,
		System:   "sys",
		Messages: []anth.MessageParam{anth.NewUserMessage(anth.NewTextBlock("go"))},
		Tools:    []anthropic.ToolDef{{Name: "submit_subtasks", InputSchema: map[string]any{"type": "object"}}},
	}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	b, err := os.ReadFile(argsDump)
	if err != nil {
		t.Fatalf("read args dump: %v", err)
	}
	return string(b)
}

// TestOneshotRunnerPassesModel: cfg.Model is passed as --model and WINS over the
// per-turn TurnInput.Model (so the composition root can pin the decompose model);
// when cfg.Model is empty, TurnInput.Model is used; when both empty, no --model.
func TestOneshotRunnerPassesModel(t *testing.T) {
	// cfg.Model set → it is passed, overriding the turn model.
	args := runOneshotCapturingArgs(t, Config{Model: "claude-opus-4-8"}, "claude-sonnet-4-6")
	if !strings.Contains(args, "--model\nclaude-opus-4-8\n") {
		t.Errorf("cfg.Model should be passed as --model and win; got:\n%s", args)
	}
	if strings.Contains(args, "claude-sonnet-4-6") {
		t.Errorf("turn model must not appear when cfg.Model is set; got:\n%s", args)
	}

	// cfg.Model empty → fall back to the turn model.
	args = runOneshotCapturingArgs(t, Config{}, "claude-sonnet-4-6")
	if !strings.Contains(args, "--model\nclaude-sonnet-4-6\n") {
		t.Errorf("turn model should be passed when cfg.Model empty; got:\n%s", args)
	}

	// both empty → no --model (defer to CLI default).
	args = runOneshotCapturingArgs(t, Config{}, "")
	if strings.Contains(args, "--model") {
		t.Errorf("no --model expected when both empty; got:\n%s", args)
	}
}

// TestRunCapturedToolRoamingArgs asserts the roaming entry point passes the
// worktree cwd (process cwd + --add-dir), the extra read-only built-in tools
// (Read/Grep/Glob/Bash) alongside the MCP capture tool in --allowedTools, and a
// custom --max-turns into the spawned claude.
func TestRunCapturedToolRoamingArgs(t *testing.T) {
	t.Setenv("HIVE_FAKE_ONESHOT_MODE", "write")
	argsDump := filepath.Join(t.TempDir(), "args.dump")
	cwdDump := filepath.Join(t.TempDir(), "cwd.dump")
	t.Setenv("HIVE_FAKE_ONESHOT_ARGS_DUMP", argsDump)
	t.Setenv("HIVE_FAKE_ONESHOT_CWD_DUMP", cwdDump)

	// A real worktree the runner can chdir into.
	wt := t.TempDir()

	r := NewOneshotToolRunner(Config{Binary: buildOneshotFakeClaude(t)})
	out, err := r.runCapturedTool(context.Background(), capturedToolParams{
		Cwd:          wt,
		System:       "sys",
		UserPrompt:   "audit it",
		Tool:         anthropic.ToolDef{Name: "submit_verdict", InputSchema: map[string]any{"type": "object"}},
		ExtraAllowed: []string{"Read", "Grep", "Glob", "Bash"},
		MaxTurns:     30,
	})
	if err != nil {
		t.Fatalf("runCapturedTool: %v", err)
	}
	if out == nil || len(out.ToolCalls) != 1 {
		t.Fatalf("expected a captured tool call; got %+v", out)
	}

	argsB, err := os.ReadFile(argsDump)
	if err != nil {
		t.Fatalf("read args dump: %v", err)
	}
	args := string(argsB)

	// --allowedTools is a single space-separated flag value: the MCP tool plus
	// each extra built-in tool. Assert the value line contains all of them.
	wantAllowed := "mcp__hive_oneshot__submit_verdict Read Grep Glob Bash"
	if !strings.Contains(args, "--allowedTools\n"+wantAllowed+"\n") {
		t.Errorf("--allowedTools should carry MCP tool + extras as one space-joined value %q; got:\n%s", wantAllowed, args)
	}
	if !strings.Contains(args, "--max-turns\n30\n") {
		t.Errorf("expected --max-turns 30; got:\n%s", args)
	}
	if !strings.Contains(args, "--add-dir\n"+wt+"\n") {
		t.Errorf("expected --add-dir %s; got:\n%s", wt, args)
	}

	cwdB, err := os.ReadFile(cwdDump)
	if err != nil {
		t.Fatalf("read cwd dump: %v", err)
	}
	gotCwd := strings.TrimSpace(string(cwdB))
	// macOS/tmp symlink-resolves; compare via EvalSymlinks to be safe.
	wantCwd, _ := filepath.EvalSymlinks(wt)
	resolvedGot, _ := filepath.EvalSymlinks(gotCwd)
	if resolvedGot != wantCwd && gotCwd != wt {
		t.Errorf("claude cwd = %q, want %q (worktree)", gotCwd, wt)
	}
}
