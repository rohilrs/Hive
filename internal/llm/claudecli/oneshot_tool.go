package claudecli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
)

// Compile-time check that OneshotToolRunner satisfies the narrow runner
// shape decompose.Runner expects. We don't import internal/decompose to
// avoid a cycle (decompose has no business depending on a specific
// runner implementation), so this is a structural shape check.
var _ interface {
	RunTurn(ctx context.Context, in anthropic.TurnInput) (*anthropic.TurnOutput, error)
} = (*OneshotToolRunner)(nil)

// OneshotToolRunner implements decompose.Runner by spawning `claude -p`
// with a stdio MCP server (this same hive binary in
// `mcp-stage-server --oneshot` mode) that exposes ONE tool. The runner
// reads the captured tool-call args from a temp file after claude exits
// and returns them as a synthetic *anthropic.TurnOutput shaped like a
// tool_use turn.
//
// This is the CC-subscription alternative to *anthropic.SDK for the
// decompose path: no per-turn API billing (claude -p uses the user's
// Claude subscription so long as ANTHROPIC_API_KEY is NOT exported into
// its env — see filterEnv below; that's the 6.1cc bug-#2 lesson).
//
// Spawn cost ~3s for real claude. Tests bypass the MCP roundtrip by
// having a wrapper script write the capture file directly.
type OneshotToolRunner struct {
	cfg Config
}

// NewOneshotToolRunner constructs a runner. cfg.Binary defaults to
// "claude" (matching NewClient).
func NewOneshotToolRunner(cfg Config) *OneshotToolRunner {
	if cfg.Binary == "" {
		cfg.Binary = "claude"
	}
	return &OneshotToolRunner{cfg: cfg}
}

// capturedToolParams parameterizes the shared spawn+capture core
// (runCapturedTool). The decompose path (RunTurn) and the graduate
// completion audit (RunRoamingTool) both build one of these.
type capturedToolParams struct {
	Cwd          string            // worktree to run claude in; "" = inherit
	System       string            // --append-system-prompt
	UserPrompt   string            // the prompt argv
	Tool         anthropic.ToolDef // the single capture tool
	ExtraAllowed []string          // extra built-in tools to allow (e.g. Read/Grep/Glob)
	MaxTurns     int               // claude --max-turns; <=0 defaults to 5
	Model        string            // per-call model override (cfg.Model still wins if set)
}

// RunTurn satisfies decompose.Runner. Requires exactly one tool in
// in.Tools. Returns a *anthropic.TurnOutput whose ToolCalls slice has
// one entry: {ID: synthetic, Name: tool.Name, Input: <captured JSON>}.
// TokensIn/Out stay zero (subscription, no billing data).
func (r *OneshotToolRunner) RunTurn(ctx context.Context, in anthropic.TurnInput) (*anthropic.TurnOutput, error) {
	if len(in.Tools) != 1 {
		return nil, fmt.Errorf("oneshot: requires exactly 1 tool, got %d", len(in.Tools))
	}
	return r.runCapturedTool(ctx, capturedToolParams{
		System:     in.System,
		UserPrompt: concatUserText(in.Messages),
		Tool:       in.Tools[0],
		MaxTurns:   5,
		Model:      in.Model,
	})
}

// RunRoamingTool spawns claude in cwd with the built-in tools in allowExtra plus
// the single capture tool, capturing the tool call. Used by the graduate
// completion audit, which must inspect a real worktree (it passes the read-only
// Read/Grep/Glob set).
func (r *OneshotToolRunner) RunRoamingTool(ctx context.Context, cwd, system, userPrompt string, tool anthropic.ToolDef, allowExtra []string, maxTurns int) (*anthropic.TurnOutput, error) {
	return r.runCapturedTool(ctx, capturedToolParams{
		Cwd: cwd, System: system, UserPrompt: userPrompt, Tool: tool, ExtraAllowed: allowExtra, MaxTurns: maxTurns,
	})
}

// runCapturedTool is the shared spawn+capture core: it writes the tool schema
// and an --oneshot MCP config to a tempdir, spawns claude with the single
// capture tool (plus any p.ExtraAllowed built-ins), reads the capture file
// (the source of truth, exit code is advisory), and returns a synthetic
// tool_use *anthropic.TurnOutput.
func (r *OneshotToolRunner) runCapturedTool(ctx context.Context, p capturedToolParams) (*anthropic.TurnOutput, error) {
	tool := p.Tool

	// Marshal the input schema so the --oneshot server can re-emit it on
	// tools/list verbatim (must round-trip through JSON to handle the
	// []any vs []string quirks in 'required' that anthropic.SDK also
	// guards against).
	schemaJSON, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("oneshot: marshal tool schema: %w", err)
	}

	tmp, err := os.MkdirTemp("", "hive-oneshot-")
	if err != nil {
		return nil, fmt.Errorf("oneshot: tempdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	schemaPath := filepath.Join(tmp, "schema.json")
	if err := os.WriteFile(schemaPath, schemaJSON, 0o644); err != nil {
		return nil, fmt.Errorf("oneshot: write schema: %w", err)
	}
	capturePath := filepath.Join(tmp, "captured.json")
	mcpConfigPath := filepath.Join(tmp, "mcp.json")

	// The MCP config tells claude to spawn THIS binary in --oneshot mode.
	// os.Executable() points at the running hive binary so claude can
	// re-exec it without needing PATH lookup.
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("oneshot: find self: %w", err)
	}
	mcpCfg := map[string]any{
		"mcpServers": map[string]any{
			"hive_oneshot": map[string]any{
				"type":    "stdio",
				"command": self,
				"args": []string{
					"mcp-stage-server",
					"--oneshot",
					"--tool-name", tool.Name,
					"--tool-description", tool.Description,
					"--tool-input-schema-file", schemaPath,
					"--capture-args-file", capturePath,
				},
			},
		},
	}
	cfgBytes, err := json.Marshal(mcpCfg)
	if err != nil {
		return nil, fmt.Errorf("oneshot: marshal mcp config: %w", err)
	}
	if err := os.WriteFile(mcpConfigPath, cfgBytes, 0o644); err != nil {
		return nil, fmt.Errorf("oneshot: write mcp config: %w", err)
	}

	userPrompt := p.UserPrompt
	if userPrompt == "" {
		return nil, fmt.Errorf("oneshot: empty user message")
	}

	// --allowedTools is a single space-separated flag value (matching the CC
	// chat agent): the MCP capture tool plus each requested read-only built-in.
	// Skip empty extra entries so a stray "" can't produce a double space /
	// malformed flag value in the join below.
	allowed := []string{"mcp__hive_oneshot__" + tool.Name}
	for _, t := range p.ExtraAllowed {
		if t != "" {
			allowed = append(allowed, t)
		}
	}

	maxTurns := p.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 5 // tool_use turn cycle can run 3+ in practice
	}

	// Spawn claude. ExtraArgs first so test wrappers (-fixture for
	// fake-claude) land ahead of the real flag set, matching the
	// claudecli Client and the CC chat agent.
	args := append([]string{}, r.cfg.ExtraArgs...)
	args = append(args,
		"--print",
		"--output-format", "json",
		"--mcp-config", mcpConfigPath,
		"--allowedTools", strings.Join(allowed, " "),
	)
	// Let claude read the worktree when roaming. Both --add-dir and cmd.Dir
	// (set below) point at p.Cwd on purpose: claude's filesystem sandbox roots
	// on the dirs passed via --add-dir, not merely on the process pwd, so
	// omitting --add-dir would leave Read/Grep/Glob/Bash unable to reach the
	// worktree even though we cd'd into it. Don't "simplify" by dropping one.
	if p.Cwd != "" {
		args = append(args, "--add-dir", p.Cwd)
	}
	// Pin the model: cfg.Model (composition-root config) wins over the per-call
	// model, both over the claude CLI default. Without this the subscription
	// decompose path ignored the requested model entirely.
	if model := r.cfg.Model; model != "" {
		args = append(args, "--model", model)
	} else if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	if p.System != "" {
		args = append(args, "--append-system-prompt", p.System)
	}
	args = append(args,
		"--max-turns", strconv.Itoa(maxTurns),
		userPrompt,
	)
	cmd := exec.CommandContext(ctx, r.cfg.Binary, args...)
	if p.Cwd != "" {
		// Paired with --add-dir above: cmd.Dir sets the process pwd, --add-dir
		// grants the sandbox read access. Both are required (see comment above).
		cmd.Dir = p.Cwd
	}
	// Strip ANTHROPIC_API_KEY so claude uses subscription auth (6.1cc
	// bug #2: with the key set, claude prefers apiKeySource=ANTHROPIC_API_KEY
	// and bills the API at ~$0.05-0.20/turn).
	cmd.Env = filterEnv(os.Environ(), "ANTHROPIC_API_KEY")
	cmd.Stderr = os.Stderr // surface claude warnings to the operator
	// claude may exit non-zero (e.g. max_turns reached AFTER a successful
	// tool call), but if the capture file was written the tool fired and
	// the runner should succeed. Treat exit code as advisory; the capture
	// file is the source of truth.
	runErr := cmd.Run()

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		// No capture + claude error → claude bailed before calling the tool.
		if runErr != nil {
			return nil, fmt.Errorf("oneshot: claude exited (%w) without calling the tool", runErr)
		}
		return nil, fmt.Errorf("oneshot: claude did not call the tool (no capture file at %s): %w", capturePath, err)
	}
	if len(captured) == 0 {
		return nil, fmt.Errorf("oneshot: claude did not call the tool (empty capture file)")
	}

	// Build a synthetic TurnOutput shaped like the SDK's tool_use turn.
	// The synthetic ID is content-addressed enough to be unique within
	// one process (test doesn't care; real callers don't depend on it).
	return &anthropic.TurnOutput{
		StopReason: "tool_use",
		ToolCalls: []anthropic.ToolCall{{
			ID:    "oneshot-" + strconv.FormatInt(int64(len(captured)), 10),
			Name:  tool.Name,
			Input: json.RawMessage(captured),
		}},
	}, nil
}

// concatUserText extracts the text from the FIRST user message in the
// running conversation. The decompose call sends exactly one user
// message; we just need its prompt body as a string for `claude -p`.
//
// MessageParam.Content is []ContentBlockParamUnion; text blocks live on
// OfText.Text. Non-text blocks (tool_use, tool_result, etc.) shouldn't
// appear in a fresh decompose call but we skip them defensively.
func concatUserText(messages []anth.MessageParam) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Role != anth.MessageParamRoleUser {
			continue
		}
		for _, block := range m.Content {
			if block.OfText != nil {
				b.WriteString(block.OfText.Text)
			}
		}
		break // first user message only
	}
	return b.String()
}

// filterEnv returns a copy of env with all "<key>=..." entries removed.
// Match is whole-key — "ANTHROPIC_API_KEY" does NOT match
// "ANTHROPIC_API_KEY_FALLBACK".
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}
