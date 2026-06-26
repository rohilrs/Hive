package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/adapter/claudecode"
	"github.com/rohilrs/Hive/internal/verdict"
)

type FileRefInput struct {
	Path      string `json:"path"`
	Line      int    `json:"line,omitempty"`
	Comment   string `json:"comment"`
	Reasoning string `json:"reasoning,omitempty"`
}

type VerdictInput struct {
	Verdict    string         `json:"verdict"`
	Confidence int            `json:"confidence"`
	FileRefs   []FileRefInput `json:"file_refs,omitempty"`
	Summary    string         `json:"summary,omitempty"`
}

type DocSubmitInput struct {
	Summary        string   `json:"summary"`
	FilesChanged   []string `json:"files_changed,omitempty"`
	ChangelogEntry string   `json:"changelog_entry,omitempty"`
}

func runMCPStageServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp-stage-server", flag.ContinueOnError)
	var notifySock, stageName, runID, toolName, permLog, daemonSock, docTool, chatMode string
	var oneshotToolName, oneshotToolDescription, oneshotSchemaFile, oneshotCaptureFile string
	var permissionOnly, chatTools, oneshot bool
	var approvalTimeout int
	fs.StringVar(&notifySock, "notify-sock", "", "UDS path the daemon is listening on (required)")
	fs.StringVar(&stageName, "stage", "", "stage name (required)")
	fs.StringVar(&runID, "run-id", "", "run id (required)")
	fs.StringVar(&toolName, "tool", "hive_submit_review_verdict", "verdict tool name")
	fs.BoolVar(&permissionOnly, "permission-only", false, "serve only hive_permission_check (Phase 4.5)")
	fs.StringVar(&permLog, "perm-log", "", "file to append permission-tool inputs to (Phase 4.5 diagnostic)")
	fs.StringVar(&daemonSock, "daemon-sock", "", "daemon RPC socket for approval.evaluate (Phase 4.5)")
	fs.IntVar(&approvalTimeout, "approval-timeout", 300, "seconds to wait for the daemon decision (Phase 4.6 ask mode)")
	fs.StringVar(&docTool, "doc-tool", "", "serve hive_submit_documentation forwarding to --daemon-sock (Phase 4.4)")
	fs.BoolVar(&chatTools, "chat-tools", false, "serve the chat read-tools, forwarding to --daemon-sock chat.tool (Phase 6.1 CC chat provider)")
	fs.StringVar(&chatMode, "mode", "", "chat-tools mode: \"\"/\"chat\" (default chat tools) or \"plan\" (planner tools); only honored with --chat-tools (Phase 8.A T6b)")
	fs.BoolVar(&oneshot, "oneshot", false, "serve a single configurable tool; capture args to --capture-args-file and exit (Phase 8.B T2)")
	fs.StringVar(&oneshotToolName, "tool-name", "", "advertised tool name in --oneshot mode")
	fs.StringVar(&oneshotToolDescription, "tool-description", "", "advertised tool description in --oneshot mode")
	fs.StringVar(&oneshotSchemaFile, "tool-input-schema-file", "", "path to JSON file containing the tool's input schema (--oneshot)")
	fs.StringVar(&oneshotCaptureFile, "capture-args-file", "", "path to write the captured tool args to (--oneshot)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Mode mutual-exclusivity: pick exactly one server mode. Default is
	// the verdict server (notify-sock/stage/run-id required, below).
	modes := 0
	if permissionOnly {
		modes++
	}
	if docTool != "" {
		modes++
	}
	if chatTools {
		modes++
	}
	if oneshot {
		modes++
	}
	if modes > 1 {
		return fmt.Errorf("--permission-only / --doc-tool / --chat-tools / --oneshot are mutually exclusive")
	}
	if permissionOnly {
		// Phase 4.5: serve the permission-prompt-tool, forwarding each
		// decision to the daemon's approval engine. notify-sock not used.
		return runPermissionServer(ctx, stageName, runID, permLog, daemonSock, approvalTimeout)
	}
	if docTool != "" {
		// Phase 4.4: serve the non-blocking documentation tool, forwarding
		// the structured summary to the daemon. notify-sock not used.
		return runDocServer(ctx, stageName, runID, docTool, daemonSock)
	}
	if chatTools {
		// Phase 6.1: serve the chat read-tools for the CC chat provider,
		// forwarding each call to the daemon's chat.tool RPC. Not tied to a
		// stage/run — notify-sock/stage/run-id not used.
		return runChatToolsServer(ctx, daemonSock, chatMode)
	}
	if oneshot {
		// Phase 8.B T2: serve one configurable tool, capture args to a
		// file, exit after one call. Used by claudecli.OneshotToolRunner
		// to do a single tool_use turn on the CC subscription.
		return runOneshotToolServer(ctx, oneshotToolName, oneshotToolDescription, oneshotSchemaFile, oneshotCaptureFile)
	}
	if notifySock == "" || stageName == "" || runID == "" {
		return fmt.Errorf("--notify-sock, --stage, --run-id are required")
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name: "hive-stage-mcp", Version: "0.1.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: toolName,
		Description: "Submit the verdict for this stage. You MUST call this tool to complete the stage. " +
			"When verdict=CHANGES_REQUESTED, file_refs is REQUIRED and must enumerate every change you want " +
			"made, each anchored to a path (and line when relevant) with a comment stating WHAT to change. " +
			"Include reasoning so the implementer understands WHY.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"verdict":    map[string]any{"type": "string", "enum": []string{"APPROVE", "CHANGES_REQUESTED"}},
				"confidence": map[string]any{"type": "integer", "minimum": 0, "maximum": 100},
				"file_refs": map[string]any{
					"type":        "array",
					"description": "Required when verdict=CHANGES_REQUESTED. Each entry anchors a change request.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":      map[string]any{"type": "string"},
							"line":      map[string]any{"type": "integer", "minimum": 0},
							"comment":   map[string]any{"type": "string"},
							"reasoning": map[string]any{"type": "string"},
						},
						"required": []string{"path", "comment"},
					},
				},
				"summary": map[string]any{
					"type":        "string",
					"description": "One-paragraph overall finding / cross-cutting verdict, in addition to per-file file_refs. Use this for holistic observations (e.g. an architectural concern) that are not anchored to a single file.",
				},
			},
			"required": []string{"verdict", "confidence"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, input VerdictInput) (*mcp.CallToolResult, struct{}, error) {
		refs := make([]verdict.FileRef, len(input.FileRefs))
		for i, r := range input.FileRefs {
			refs[i] = verdict.FileRef{Path: r.Path, Line: r.Line, Comment: r.Comment, Reasoning: r.Reasoning}
		}
		ack, err := verdict.Forward(notifySock, verdict.Frame{
			RunID: runID, Stage: stageName,
			Verdict: input.Verdict, Confidence: input.Confidence, FileRefs: refs,
			Summary: input.Summary,
		})
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "verdict forward failed: " + err.Error()}},
				IsError: true,
			}, struct{}{}, nil
		}
		if ack == nil || !ack.OK {
			msg := "daemon rejected verdict"
			if ack != nil && ack.Error != "" {
				msg = ack.Error
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: msg}},
				IsError: true,
			}, struct{}{}, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "verdict recorded"}},
		}, struct{}{}, nil
	})

	return server.Run(ctx, &mcp.StdioTransport{})
}

// runPermissionServer serves the Phase 4.5 spike permission-prompt-tool.
// It ALWAYS allows, and logs the exact input Claude Code passes to
// stderr (captured by the adapter) so we can learn the undocumented
// --permission-prompt-tool contract empirically. The full engine
// (rules + daemon bridge + fail-closed deny) replaces this once the
// mechanism is validated.
func runPermissionServer(ctx context.Context, stageName, runID, permLog, daemonSock string, approvalTimeout int) error {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "hive-perm-mcp", Version: "0.1.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "hive_permission_check",
		Description: "Authorize a tool call. Returns an allow/deny decision for the proposed tool use.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, struct{}, error) {
		// Confirmed contract (Phase 4.5 spike): Claude Code passes
		// {"tool_name": "...", "input": {<tool args>}, "tool_use_id": "..."}.
		toolName, _ := input["tool_name"].(string)
		inner, _ := input["input"].(map[string]any)

		// Diagnostic log (kept from the spike): the MCP server's stderr is
		// owned by claude, so append to a file Hive can read.
		if permLog != "" {
			raw, _ := json.Marshal(input)
			if f, ferr := os.OpenFile(permLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); ferr == nil {
				_, _ = fmt.Fprintf(f, "run=%s stage=%s input=%s\n", runID, stageName, string(raw))
				_ = f.Close()
			}
		}

		// Fail-closed default: if we can't reach the daemon engine, deny.
		behavior := "deny"
		message := "approval daemon unreachable (fail-closed)"
		if daemonSock != "" {
			if decision, reason, ok := forwardApproval(daemonSock, runID, stageName, toolName, inner, approvalTimeout); ok {
				if decision == "approve" {
					behavior = "allow"
				} else {
					behavior = "deny"
					message = reason
				}
			}
		}

		var payload map[string]any
		if behavior == "allow" {
			payload = map[string]any{"behavior": "allow", "updatedInput": inner}
		} else {
			payload = map[string]any{"behavior": "deny", "message": message}
		}
		out, _ := json.Marshal(payload)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
		}, struct{}{}, nil
	})

	return server.Run(ctx, &mcp.StdioTransport{})
}

// forwardApproval sends one approval.evaluate RPC to the daemon socket
// and returns (decision, reason, ok). ok=false on any transport/RPC
// error so the caller fails closed (deny).
func forwardApproval(sockPath, runID, stage, toolName string, input map[string]any, approvalTimeout int) (string, string, bool) {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return "", "", false
	}
	defer conn.Close()
	reqBody, _ := json.Marshal(map[string]any{
		"id":     fmt.Sprintf("perm-%d", time.Now().UnixNano()),
		"method": "approval.evaluate",
		"params": map[string]any{"run_id": runID, "stage": stage, "tool_name": toolName, "input": input},
	})
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		return "", "", false
	}
	// Ask mode: the daemon may block until an operator responds (up to its
	// HookTimeoutSeconds). Wait that long + a buffer so we don't deny early.
	readTimeout := time.Duration(approvalTimeout+30) * time.Second
	if approvalTimeout <= 0 {
		readTimeout = 630 * time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	buf := make([]byte, 64*1024)
	n, err := conn.Read(buf)
	if err != nil {
		return "", "", false
	}
	var resp struct {
		Result struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf[:n], &resp); err != nil || resp.Error != nil {
		return "", "", false
	}
	return resp.Result.Decision, resp.Result.Reason, true
}

// runDocServer serves the Phase 4.4 hive_submit_documentation tool. The
// documenter is non-blocking, so this just forwards the structured summary
// to the daemon (best-effort) and returns success to the worker regardless.
func runDocServer(ctx context.Context, stageName, runID, toolName, daemonSock string) error {
	if toolName == "" {
		toolName = "hive_submit_documentation"
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "hive-docs-mcp", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: toolName,
		Description: "Report what you documented for this change. Call this AFTER writing the " +
			"CHANGELOG/doc files: pass a one-paragraph summary, the list of files you changed, " +
			"and the CHANGELOG entry text. Optional — it records structured output for the run.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary":         map[string]any{"type": "string"},
				"files_changed":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"changelog_entry": map[string]any{"type": "string"},
			},
			"required": []string{"summary"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, input DocSubmitInput) (*mcp.CallToolResult, struct{}, error) {
		ok := forwardDocumentation(daemonSock, runID, stageName, input)
		msg := "documentation recorded"
		if !ok {
			msg = "documentation could not be forwarded to the daemon (non-fatal)"
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, struct{}{}, nil
	})
	return server.Run(ctx, &mcp.StdioTransport{})
}

// forwardDocumentation sends one documentation.submit to the daemon socket.
// Best-effort (fire-and-forget with a short read for the ack); returns
// false on any transport error — the documenter stage does not fail on it.
func forwardDocumentation(sockPath, runID, stage string, in DocSubmitInput) bool {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	reqBody, _ := json.Marshal(map[string]any{
		"id":     fmt.Sprintf("doc-%d", time.Now().UnixNano()),
		"method": "documentation.submit",
		"params": map[string]any{
			"run_id":          runID,
			"stage":           stage,
			"summary":         in.Summary,
			"files_changed":   in.FilesChanged,
			"changelog_entry": in.ChangelogEntry,
		},
	})
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return false
	}
	var resp struct {
		Result struct {
			OK bool `json:"ok"`
		} `json:"result"`
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(buf[:n], &resp); err != nil || resp.Error != nil {
		return false
	}
	return resp.Result.OK
}

// chatReadTools is an alias for claudecode.ChatToolNames — the single source of
// truth for the chat tool name list. The MCP stage server (here) and the CC
// chat agent (internal/adapter/claudecode/chat_agent.go) both consume it so the
// MCP tools/list and the --allowedTools flag stay in sync automatically.
var chatReadTools = claudecode.ChatToolNames

// chatToolsForMode picks the tool basename list to advertise based on the
// chat-tools server mode. Phase 8.A T6b:
//   - "" or "chat" → claudecode.ChatToolNames (the regular chat palette;
//     existing CC chat-provider behavior, unchanged)
//   - "plan" → claudecode.PlannerToolNames (the 4 planner tools used by
//     CC planner sessions)
//
// Unknown values fall through to ChatToolNames so a typo in the spawn
// argv degrades to "normal chat" rather than zero tools.
func chatToolsForMode(mode string) []string {
	if mode == "plan" {
		return claudecode.PlannerToolNames
	}
	return claudecode.ChatToolNames
}

// runChatToolsServer serves the Phase 6.1 chat read-tools for the CC chat
// provider. Each tool forwards its input to the daemon's chat.tool RPC, which
// runs the SAME chat.Registry handler the SDK agent uses. The input schema is
// permissive ({"type":"object","additionalProperties":true}) — the daemon
// owns validation.
//
// mode selects the tool palette: "" or "chat" for the regular chat tools,
// "plan" for the planner tools (Phase 8.A T6b). When mode="plan", the daemon
// side's handleChatTool looks up the per-session planner registry so the
// planner tool names dispatch to the right handlers.
//
// chatToolOut wraps a chat read-tool's forwarded result so the mcp-go SDK
// emits a valid object-typed StructuredContent that Claude reads. The
// Result MUST be a concrete-typed field: `any` (or `json.RawMessage`)
// produces an empty inferred output schema that Claude's MCP client
// rejects with `outputSchema.properties.result: Invalid input`, which
// crashes tools/list and leaves the tool unavailable. Passing the raw
// JSON as a string keeps the schema valid; the model reads the same
// JSON from TextContent in the result.
type chatToolOut struct {
	Result string `json:"result"`
}

func runChatToolsServer(ctx context.Context, daemonSock, mode string) error {
	chatSessionID := os.Getenv("HIVE_CHAT_SESSION_ID")
	server := mcp.NewServer(&mcp.Implementation{Name: "hive-chat-mcp", Version: "0.1.0"}, nil)
	for _, name := range chatToolsForMode(mode) {
		tool := name // capture
		mcp.AddTool(server, &mcp.Tool{
			Name:        tool,
			Description: "Hive chat tool " + tool + ". Forwards to the Hive daemon.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
		}, func(ctx context.Context, req *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, chatToolOut, error) {
			content, isErr := forwardChatTool(daemonSock, tool, chatSessionID, input)
			// See chatToolOut docstring for why Result is a string.
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: content}},
				IsError: isErr,
			}, chatToolOut{Result: content}, nil
		})
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

// forwardChatTool sends one chat.tool RPC to the daemon socket and returns
// the (content, is_error) the registry handler produced. On any transport or
// RPC error it returns ("error: <reason>", true) so the model sees a failure
// rather than a silent empty result.
func forwardChatTool(sockPath, tool, sessionID string, input map[string]any) (string, bool) {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return "error: " + err.Error(), true
	}
	defer conn.Close()
	reqBody, _ := json.Marshal(map[string]any{
		"id":     fmt.Sprintf("chat-tool-%d", time.Now().UnixNano()),
		"method": "chat.tool",
		"params": map[string]any{
			"tool":       tool,
			"input":      input,
			"session_id": sessionID,
		},
	})
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		return "error: " + err.Error(), true
	}
	// Mutating tools block the daemon's RPC handler on a user confirm gate
	// (chat.confirm round-trip via the chat.send stream). The user has up to
	// [chat] confirm_timeout_seconds (default 300) to answer y/n. The deadline
	// here must comfortably cover that, plus the underlying tool work. Use
	// HIVE_CHAT_TOOL_READ_TIMEOUT_SECONDS (set by the daemon's CC chat agent
	// when spawning mcp-stage-server, see deferred.md) if present; otherwise
	// 600s — generous for confirm + slow tools, still bounded.
	readTimeoutSec := 600
	if v := os.Getenv("HIVE_CHAT_TOOL_READ_TIMEOUT_SECONDS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			readTimeoutSec = n
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Duration(readTimeoutSec) * time.Second))
	buf := make([]byte, 256*1024)
	n, err := conn.Read(buf)
	if err != nil {
		return "error: " + err.Error(), true
	}
	var resp struct {
		Result struct {
			Content string `json:"content"`
			IsError bool   `json:"is_error"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return "error: " + err.Error(), true
	}
	if resp.Error != nil {
		return "error: " + resp.Error.Message, true
	}
	return resp.Result.Content, resp.Result.IsError
}

// --- cobra wrapper (added in Phase 1c Task 7) ---

var (
	mcpNotifySock, mcpStageName, mcpRunID, mcpToolName, mcpPermLog, mcpDaemonSock string
	mcpDocTool                                                                    string
	mcpChatMode                                                                   string
	mcpPermissionOnly                                                             bool
	mcpChatTools                                                                  bool
	mcpApprovalTimeout                                                            int
	mcpOneshot                                                                    bool
	mcpOneshotToolName, mcpOneshotToolDescription                                 string
	mcpOneshotSchemaFile, mcpOneshotCaptureFile                                   string
)

var mcpStageServerCmd = &cobra.Command{
	Use:   "mcp-stage-server",
	Short: "Per-stage MCP server (spawned by Claude Code)",
	Long: `Per-stage MCP server invoked by Claude Code via --mcp-config.
Advertises verdict tools to the worker and forwards calls to the daemon
over the per-stage Unix domain socket (--notify-sock).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cliArgs := []string{
			"--notify-sock", mcpNotifySock,
			"--stage", mcpStageName,
			"--run-id", mcpRunID,
			"--tool", mcpToolName,
		}
		if mcpPermissionOnly {
			cliArgs = append(cliArgs, "--permission-only")
		}
		if mcpPermLog != "" {
			cliArgs = append(cliArgs, "--perm-log", mcpPermLog)
		}
		if mcpDaemonSock != "" {
			cliArgs = append(cliArgs, "--daemon-sock", mcpDaemonSock)
		}
		if mcpDocTool != "" {
			cliArgs = append(cliArgs, "--doc-tool", mcpDocTool)
		}
		if mcpChatTools {
			cliArgs = append(cliArgs, "--chat-tools")
		}
		if mcpChatMode != "" {
			cliArgs = append(cliArgs, "--mode", mcpChatMode)
		}
		if mcpApprovalTimeout > 0 {
			cliArgs = append(cliArgs, "--approval-timeout", fmt.Sprintf("%d", mcpApprovalTimeout))
		}
		if mcpOneshot {
			cliArgs = append(cliArgs, "--oneshot")
		}
		if mcpOneshotToolName != "" {
			cliArgs = append(cliArgs, "--tool-name", mcpOneshotToolName)
		}
		if mcpOneshotToolDescription != "" {
			cliArgs = append(cliArgs, "--tool-description", mcpOneshotToolDescription)
		}
		if mcpOneshotSchemaFile != "" {
			cliArgs = append(cliArgs, "--tool-input-schema-file", mcpOneshotSchemaFile)
		}
		if mcpOneshotCaptureFile != "" {
			cliArgs = append(cliArgs, "--capture-args-file", mcpOneshotCaptureFile)
		}
		return runMCPStageServer(cmd.Context(), cliArgs)
	},
}

func init() {
	mcpStageServerCmd.Flags().StringVar(&mcpNotifySock, "notify-sock", "", "")
	mcpStageServerCmd.Flags().StringVar(&mcpStageName, "stage", "", "")
	mcpStageServerCmd.Flags().StringVar(&mcpRunID, "run-id", "", "")
	mcpStageServerCmd.Flags().StringVar(&mcpToolName, "tool", "hive_submit_review_verdict", "")
	mcpStageServerCmd.Flags().BoolVar(&mcpPermissionOnly, "permission-only", false, "serve only hive_permission_check (Phase 4.5 spike)")
	mcpStageServerCmd.Flags().StringVar(&mcpPermLog, "perm-log", "", "file to append permission-tool inputs to (Phase 4.5 spike)")
	mcpStageServerCmd.Flags().StringVar(&mcpDaemonSock, "daemon-sock", "", "daemon RPC socket for approval.evaluate (Phase 4.5)")
	mcpStageServerCmd.Flags().StringVar(&mcpDocTool, "doc-tool", "", "serve hive_submit_documentation (Phase 4.4)")
	mcpStageServerCmd.Flags().BoolVar(&mcpChatTools, "chat-tools", false, "serve the chat read-tools, forwarding to --daemon-sock chat.tool (Phase 6.1)")
	mcpStageServerCmd.Flags().StringVar(&mcpChatMode, "mode", "", "chat-tools mode: \"\"/\"chat\" or \"plan\"; only honored with --chat-tools (Phase 8.A T6b)")
	mcpStageServerCmd.Flags().IntVar(&mcpApprovalTimeout, "approval-timeout", 300, "seconds to wait for the daemon decision (Phase 4.6)")
	mcpStageServerCmd.Flags().BoolVar(&mcpOneshot, "oneshot", false, "serve a single configurable tool; capture args + exit (Phase 8.B T2)")
	mcpStageServerCmd.Flags().StringVar(&mcpOneshotToolName, "tool-name", "", "advertised tool name (--oneshot)")
	mcpStageServerCmd.Flags().StringVar(&mcpOneshotToolDescription, "tool-description", "", "advertised tool description (--oneshot)")
	mcpStageServerCmd.Flags().StringVar(&mcpOneshotSchemaFile, "tool-input-schema-file", "", "path to JSON file containing the tool input schema (--oneshot)")
	mcpStageServerCmd.Flags().StringVar(&mcpOneshotCaptureFile, "capture-args-file", "", "path to write captured tool args to (--oneshot)")
	// notify-sock/stage/run-id required only in verdict mode; the
	// special-purpose modes (permission-only, doc-tool, chat-tools,
	// oneshot) each have their own required-arg checks.
	mcpStageServerCmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		// Mutual exclusivity of modes.
		modes := 0
		if mcpPermissionOnly {
			modes++
		}
		if mcpDocTool != "" {
			modes++
		}
		if mcpChatTools {
			modes++
		}
		if mcpOneshot {
			modes++
		}
		if modes > 1 {
			return fmt.Errorf("--permission-only / --doc-tool / --chat-tools / --oneshot are mutually exclusive")
		}
		if mcpPermissionOnly || mcpDocTool != "" || mcpChatTools || mcpOneshot {
			return nil
		}
		if mcpNotifySock == "" || mcpStageName == "" || mcpRunID == "" {
			return fmt.Errorf("--notify-sock, --stage, --run-id are required")
		}
		return nil
	}
}
