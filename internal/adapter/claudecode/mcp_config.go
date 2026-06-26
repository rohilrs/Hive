package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// MCPConfigRequest describes the per-stage .mcp.json file written into
// the stage scratch directory and pointed at via claude -p's
// --mcp-config flag. Each stage spawns its own stdio MCP server
// (`hive mcp-stage-server`) that forwards verdict tool calls to the UDS
// listener bound by the adapter (spec §5.6).
type MCPConfigRequest struct {
	DestDir    string
	HiveBinary string
	NotifySock string
	RunID      string
	StageName  string
	ToolName   string
	Server     string

	// ScavengerEnabled adds a "scavenger" entry to mcpServers so the
	// worker can call scavenger's MCP tools (get_capsule, etc.). The
	// command is whatever ScavengerBinary points at (defaults to
	// "scavenger" if empty); the arg is "mcp-bridge" (scavenger's
	// stdio MCP subcommand).
	ScavengerEnabled bool
	ScavengerBinary  string

	// ApprovalsEnabled (Phase 4.5) adds a "hive_perm" server exposing the
	// hive_permission_check tool, referenced by claude's
	// --permission-prompt-tool. Separate server (underscore key) so the
	// flag reference resolves unambiguously and the verdict "hive-stage"
	// server is untouched.
	ApprovalsEnabled bool

	// DaemonSocket is forwarded to the permission tool so it can reach the
	// daemon's approval engine (approval.evaluate). Empty = the tool runs
	// fail-closed (can't reach the daemon -> deny).
	DaemonSocket string

	// ApprovalTimeoutSeconds bounds the permission tool's wait for the
	// daemon decision (ask mode may block until an operator responds).
	ApprovalTimeoutSeconds int

	// DocToolName (Phase 4.4 follow-up) adds a "hive_docs" server exposing
	// hive_submit_documentation, forwarding to DaemonSocket. Independent of
	// the verdict/permission servers.
	DocToolName string

	// ChatTools (Phase 6.1) adds a "hive_chat" server exposing the chat
	// read-tools, forwarding each call to DaemonSocket's chat.tool RPC. Used
	// by the CC chat provider's `claude -p` subprocess.
	ChatTools bool

	// ChatToolsMode (Phase 8.A T6b) selects which tool set the chat-tools
	// MCP subprocess advertises. "" (the default) means the regular chat
	// tools (ChatToolNames). "plan" means the planner tools
	// (PlannerToolNames). Ignored unless ChatTools is true. Threaded into
	// the subprocess argv as --mode <ChatToolsMode>; the cmd_mcp.go side
	// switches the registered tool list accordingly.
	ChatToolsMode string

	// MCPBaseURL is the daemon's HTTP MCP base URL (e.g.
	// "http://127.0.0.1:54321"). Read from ~/.hive/mcp.url by the
	// caller. Required when any of UseHTTPFor[*] is true.
	MCPBaseURL string

	// UseHTTPFor toggles HTTP transport per server-kind. Keys: "chat",
	// "stage", "perm". When true, the corresponding server entry uses
	// {"type":"http","url":...} instead of stdio. False or missing ->
	// existing stdio block. Set per-context during smoke validation;
	// will become unconditionally true at cutover (Phase 6.3C).
	UseHTTPFor map[string]bool

	// ChatSessionID is required when UseHTTPFor["chat"] is true so the
	// emitted URL embeds the session.
	ChatSessionID string
}

// WriteMCPConfig writes mcp.json into DestDir and returns its absolute
// path. The hive-stage entry is included only when NotifySock is set
// (i.e. the stage has a verdict tool); the scavenger entry is included
// when ScavengerEnabled is true. If neither is set, returns ("", nil) —
// the caller should skip --mcp-config in that case. Defaults:
// Server="hive-stage", ToolName="hive_submit_review_verdict",
// ScavengerBinary="scavenger".
func WriteMCPConfig(req MCPConfigRequest) (string, error) {
	if req.DestDir == "" {
		return "", fmt.Errorf("dest_dir required")
	}
	if !req.ScavengerEnabled && req.NotifySock == "" && !req.ApprovalsEnabled && req.DocToolName == "" && !req.ChatTools {
		return "", nil // no servers requested for this stage
	}
	servers := map[string]any{}

	if req.ApprovalsEnabled {
		if req.UseHTTPFor["perm"] {
			if req.MCPBaseURL == "" || req.RunID == "" || req.StageName == "" {
				return "", fmt.Errorf("mcp http perm requires MCPBaseURL + RunID + StageName")
			}
			servers["hive_perm"] = map[string]any{
				"type": "http",
				"url":  req.MCPBaseURL + "/mcp/perm/" + req.RunID + "/" + req.StageName,
			}
		} else {
			if req.HiveBinary == "" {
				return "", fmt.Errorf("hive_binary required when approvals enabled")
			}
			servers["hive_perm"] = map[string]any{
				"command": req.HiveBinary,
				"args": []string{
					"mcp-stage-server",
					"--permission-only",
					"--stage", req.StageName,
					"--run-id", req.RunID,
					"--perm-log", filepath.Join(req.DestDir, "permission_check.jsonl"),
					"--daemon-sock", req.DaemonSocket,
					"--approval-timeout", strconv.Itoa(req.ApprovalTimeoutSeconds),
				},
			}
		}
	}

	if req.NotifySock != "" {
		if req.Server == "" {
			req.Server = "hive-stage"
		}
		if req.ToolName == "" {
			req.ToolName = "hive_submit_review_verdict"
		}
		if req.UseHTTPFor["stage"] {
			if req.MCPBaseURL == "" || req.RunID == "" || req.StageName == "" {
				return "", fmt.Errorf("mcp http stage requires MCPBaseURL + RunID + StageName")
			}
			servers[req.Server] = map[string]any{
				"type": "http",
				"url":  req.MCPBaseURL + "/mcp/stage/" + req.RunID + "/" + req.StageName,
			}
		} else {
			if req.HiveBinary == "" {
				return "", fmt.Errorf("hive_binary required when notify_sock is set")
			}
			servers[req.Server] = map[string]any{
				"command": req.HiveBinary,
				"args": []string{
					"mcp-stage-server",
					"--notify-sock", req.NotifySock,
					"--stage", req.StageName,
					"--run-id", req.RunID,
					"--tool", req.ToolName,
				},
			}
		}
	}
	if req.DocToolName != "" {
		if req.HiveBinary == "" {
			return "", fmt.Errorf("hive_binary required when doc tool is set")
		}
		servers["hive_docs"] = map[string]any{
			"command": req.HiveBinary,
			"args": []string{
				"mcp-stage-server",
				"--doc-tool", req.DocToolName,
				"--stage", req.StageName,
				"--run-id", req.RunID,
				"--daemon-sock", req.DaemonSocket,
			},
		}
	}
	if req.ChatTools {
		if req.UseHTTPFor["chat"] {
			if req.MCPBaseURL == "" || req.ChatSessionID == "" {
				return "", fmt.Errorf("mcp http chat requires MCPBaseURL + ChatSessionID")
			}
			servers["hive_chat"] = map[string]any{
				"type": "http",
				"url":  req.MCPBaseURL + "/mcp/chat/" + req.ChatSessionID,
			}
		} else {
			if req.HiveBinary == "" {
				return "", fmt.Errorf("hive_binary required when chat tools enabled")
			}
			chatArgs := []string{
				"mcp-stage-server",
				"--chat-tools",
				"--daemon-sock", req.DaemonSocket,
			}
			if req.ChatToolsMode != "" {
				chatArgs = append(chatArgs, "--mode", req.ChatToolsMode)
			}
			servers["hive_chat"] = map[string]any{
				"type":    "stdio",
				"command": req.HiveBinary,
				"args":    chatArgs,
			}
		}
	}
	if req.ScavengerEnabled {
		bin := req.ScavengerBinary
		if bin == "" {
			bin = "scavenger"
		}
		servers["scavenger"] = map[string]any{
			"command": bin,
			"args":    []string{"mcp-bridge"},
		}
	}

	cfg := map[string]any{"mcpServers": servers}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(req.DestDir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(req.DestDir, "mcp.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return "", err
	}
	return path, nil
}
