package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/chat"
)

// warmupChatMCP pre-spawns the chat-tools mcp-stage-server, runs a quick
// initialize+notifications/initialized+tools/list handshake, and exits.
// Purpose: warm the OS page cache for the hive binary so claude's
// per-turn copy starts in <100ms rather than 300-500ms, narrowing the
// startup race where the model issues a tool_use before MCP registration
// completes. We do NOT depend on this server's tools being visible to
// claude — claude spawns its own copy from --mcp-config — only on it
// being a cheap warmup. Returns the first error encountered; callers can
// ignore it (the real per-turn run reports proper errors).
//
// hiveBinary may be empty in tests with a stub claude binary; in that
// case we skip the warmup. claudeBinary is unused — the warmup invokes
// the hive binary directly with the mcp-stage-server subcommand.
//
// mcpMode mirrors the real per-turn subprocess: "" or "chat" for the
// default tools, "plan" for the planner tools. The warmup doesn't need
// the tools list to be useful, but matching mode keeps the cache warm
// for the exact tools/list response shape the real subprocess returns.
func warmupChatMCP(ctx context.Context, claudeBinary, hiveBinary, daemonSock, mcpMode string) error {
	_ = claudeBinary // accepted for symmetry; warmup uses the hive binary directly
	if hiveBinary == "" || daemonSock == "" {
		return nil
	}
	args := []string{"mcp-stage-server", "--chat-tools", "--daemon-sock", daemonSock}
	if mcpMode != "" {
		args = append(args, "--mode", mcpMode)
	}
	cmd := exec.CommandContext(ctx, hiveBinary, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Send the minimal MCP handshake: initialize, initialized, tools/list.
	// We don't care about the actual tools — the goal is to make the daemon
	// answer chat.tool-related queries once so its handlers and routes are
	// resident, and to ensure the binary is in page cache.
	_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"hive-chat-warmup","version":"1"}}}` + "\n"))
	_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"))
	// Read two response lines (init + tools/list).
	buf := make([]byte, 64*1024)
	_, _ = stdout.Read(buf)
	_, _ = stdout.Read(buf)
	_ = stdin.Close()
	_ = cmd.Wait()
	return nil
}

// buildChatEnv is like buildWorkerEnv but explicitly STRIPS
// ANTHROPIC_API_KEY so claude -p falls back to the user's Claude Code
// subscription. The whole point of the "claude-code" chat provider is to
// avoid API billing; if ANTHROPIC_API_KEY is set in the daemon's env
// claude would otherwise prefer it (apiKeySource=ANTHROPIC_API_KEY) and
// each chat turn would bill ~$0.05-0.20 against the API account.
func buildChatEnv(stageHome string) []string {
	full := buildWorkerEnv(stageHome)
	out := full[:0]
	for _, kv := range full {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ChatAgent runs the Hive chat agent on a Claude subscription via `claude -p`
// (no API key). It exposes the chat read tools through a per-call hive_chat
// MCP server (forwarding to the daemon's chat.tool RPC) and maps the
// stream-json events to chat.Frames. Satisfies internal/chat.Agent.
//
// Each Send spins up its own HOME-redirect scope + mcp.json under scratchRoot
// (the same machinery RunStage uses) so concurrent turns / sessions don't
// share state. --resume continuity is threaded through the daemon's store via
// the ChatSessionStore seam: the claude session id reported on the system/init
// event is recorded after each turn and replayed as --resume on the next.
//
// Phase 8.A T6b: ChatAgent supports two modes:
//   - mode="" (or "chat"): the default Q&A chat session — advertises
//     ChatToolNames via the hive_chat MCP server, uses the regular chat
//     system prompt.
//   - mode="plan": planner-kind sessions — advertises PlannerToolNames
//     instead, uses the planner system prompt. The mode is threaded to
//     the chat-tools MCP server subprocess via --mode plan so its
//     tools/list response matches.
type ChatAgent struct {
	cfg          Config // reuse adapter Config: Binary, ExtraArgs, HiveBinary, DaemonSocket, RealHome, Model
	systemPrompt string
	scratchRoot  string           // where per-session StageDir + mcp.json live (e.g. <hiveDir>/chat)
	sessions     ChatSessionStore // --resume continuity

	// toolNames is the list of tool basenames (no mcp__hive_chat__ prefix)
	// the agent advertises via --allowedTools. Defaults to ChatToolNames.
	toolNames []string

	// mcpMode is "" (chat) or "plan". When non-empty, the chat-tools
	// MCP subprocess is spawned with --mode <mcpMode> so its tools/list
	// matches the agent's advertised toolNames.
	mcpMode string

	scopeMu    sync.Mutex            // protects scopeCache
	scopeCache map[string]*ScopeInfo // stable scratch dir path → materialized ScopeInfo
}

// ChatSessionStore is the minimal store seam for --resume continuity. The
// daemon wires *store.Store (which has Get/SetChatProviderSession).
type ChatSessionStore interface {
	GetChatProviderSession(ctx context.Context, sessionID string) (string, error)
	SetChatProviderSession(ctx context.Context, sessionID, providerSessionID string) error
}

// ChatAgent satisfies the chat.Agent interface.
var _ chat.Agent = (*ChatAgent)(nil)

// ReapOrphans walks <scratchRoot>/* and removes any directory whose
// name (the sessionID) is not present in knownSessions. Returns the list
// of removed sessionIDs for logging.
//
// Called once at daemon startup. Covers orphan dirs left behind by
// pre-EvictSession sessions, daemon crashes, or external rm of a
// chat_sessions row. A new session created after reap is safe because
// the materialize step recreates the dir on first use.
//
// Errors walking the dir are returned without partial cleanup — better
// to skip the reap than to half-delete and confuse downstream code.
// Errors removing individual dirs are tracked but don't abort the loop.
func (a *ChatAgent) ReapOrphans(_ context.Context, knownSessions map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(a.scratchRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing to reap; scratchRoot is created on first turn
		}
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if knownSessions[name] {
			continue
		}
		path := filepath.Join(a.scratchRoot, name)
		// Best-effort: log via the returned slice; a stuck-busy dir
		// (e.g. permissions, open fd from a parallel process) is not
		// fatal. Next startup will retry.
		if err := os.RemoveAll(path); err == nil {
			removed = append(removed, name)
			a.scopeMu.Lock()
			delete(a.scopeCache, path)
			a.scopeMu.Unlock()
		}
	}
	return removed, nil
}

// EvictSession drops the per-session scope-cache entry AND reaps the
// on-disk scratch dir under <scratchRoot>/<sessionID>/. Called by the
// daemon's chat.delete handler so a deleted session leaves no residue.
//
// Safe to call for an unknown sessionID (it's a no-op + a best-effort
// disk reap that silently swallows "no such file" — RemoveAll already
// returns nil for absent paths). Safe to call multiple times.
//
// The startup reaper (daemon-side) walks <scratchRoot>/* and removes
// dirs whose sessionID is no longer in the store. This method handles
// the explicit-delete path; the reaper handles orphan dirs from
// crashes or pre-EvictSession sessions.
func (a *ChatAgent) EvictSession(sessionID string) {
	if sessionID == "" {
		return
	}
	scratch := filepath.Join(a.scratchRoot, sessionID)
	a.scopeMu.Lock()
	delete(a.scopeCache, scratch)
	a.scopeMu.Unlock()
	_ = os.RemoveAll(scratch)
}

// NewChatAgent constructs the provider for the default chat mode. systemPrompt
// is appended via --append-system-prompt on every turn; scratchRoot is the
// parent dir for per-turn scopes; sessions may be nil (then --resume continuity
// is disabled). The agent advertises ChatToolNames via the hive_chat MCP server.
func NewChatAgent(cfg Config, systemPrompt, scratchRoot string, sessions ChatSessionStore) *ChatAgent {
	return &ChatAgent{
		cfg:          cfg,
		systemPrompt: systemPrompt,
		scratchRoot:  scratchRoot,
		sessions:     sessions,
		toolNames:    ChatToolNames,
		mcpMode:      "", // default mode — the MCP subprocess gets no --mode flag
		scopeCache:   make(map[string]*ScopeInfo),
	}
}

// NewPlannerChatAgent constructs a planner-mode CC chat agent (Phase 8.A T6b).
// It mirrors NewChatAgent but advertises PlannerToolNames via the hive_chat
// MCP server (the subprocess is spawned with --mode plan) and uses the
// planner system prompt the caller provides. scratchRoot should typically be
// a planner-specific dir (e.g. <hiveDir>/plan) so planner sessions and regular
// chat sessions don't share --resume state.
func NewPlannerChatAgent(cfg Config, systemPrompt, scratchRoot string, sessions ChatSessionStore) *ChatAgent {
	return &ChatAgent{
		cfg:          cfg,
		systemPrompt: systemPrompt,
		scratchRoot:  scratchRoot,
		sessions:     sessions,
		toolNames:    PlannerToolNames,
		mcpMode:      "plan",
		scopeCache:   make(map[string]*ScopeInfo),
	}
}

// SystemPromptForTest exposes the configured system prompt for tests that
// need to verify per-session prompt routing (Phase 8.A T6b). Not for
// production use.
func (a *ChatAgent) SystemPromptForTest() string { return a.systemPrompt }

// AllowedToolsForTest exposes the configured tool basenames the agent
// advertises via --allowedTools. Phase 8.A T6b. Not for production use.
func (a *ChatAgent) AllowedToolsForTest() []string { return a.toolNames }

// MCPModeForTest exposes the configured chat-tools MCP server mode
// (empty for chat, "plan" for planner). Phase 8.A T6b. Not for production use.
func (a *ChatAgent) MCPModeForTest() string { return a.mcpMode }

// Send runs ONE user message through `claude -p`, streaming the model's text +
// tool results as chat.Frames. On return the accumulated assistant text is
// appended to conv.Messages so the daemon's persistence layer sees a complete
// turn (the SDK agent appends out.Assistant; here we synthesize the equivalent
// from the streamed text). The claude session id is recorded for next-turn
// --resume.
// ChatToolNames are the read-only and mutating chat tools the daemon advertises
// via the hive_chat MCP server. Chat constrains Claude to exactly these (prefixed
// mcp__hive_chat__) so it can't shell out. This is the single source of truth:
// cmd/hive's chat-tools MCP server (cmd_mcp.go) consumes it directly, so both
// the --allowedTools list and the MCP tools/list are always in sync.
var ChatToolNames = []string{
	// Read tools.
	"hive_list_tasks", "hive_get_task", "hive_status", "hive_cost_summary",
	"hive_active_workers", "hive_get_run", "hive_search", "hive_list_projects",
	// Mutating tools (6.1b-i + 6.1b-ii + 7).
	"hive_add_task", "hive_run_now", "hive_abandon",
	"hive_edit_task", "hive_approve", "hive_deny",
	"hive_decompose",
	// Stubs (6.1b-ii; non-Mutating).
	"hive_resume", "hive_predict",
}

// PlannerToolNames is the tool palette for planner-kind chat sessions
// (Phase 8.A T6b). Doc/spec tools (list specs / read docs / save roadmap /
// save spec) + codebase-grounding read tools (search code / query capsule).
// Both the CC chat agent's --allowedTools flag AND the chat-tools MCP server's
// tools/list (when invoked with --mode plan) consume this list, so they stay in
// sync automatically. MUST mirror chat.NewPlannerRegistry's tools — enforced by
// TestPlannerToolNamesMatchRegistry.
var PlannerToolNames = []string{
	"hive_list_specs", "hive_read_doc",
	"hive_save_roadmap", "hive_save_spec",
	"hive_search_code", "hive_query_capsule",
}

// disallowedChatBuiltins are Claude Code built-ins to ban during chat. The
// list is deliberately narrow:
//   - Task* (TaskList/TaskCreate/TaskGet/TaskUpdate/TaskOutput/TaskStop),
//     TodoWrite, Task — shadow our mcp__hive_chat__hive_list_tasks etc. by
//     name (model picks TaskList over the longer mcp__ name unbidden)
//
// Only list names Claude Code still knows: a deny rule for a removed tool
// (e.g. the old "TaskSearch") prints `… matches no known tool …` to stderr
// every turn, which is harmless but masks the real failure reason in surfaced
// errors. Keep this list pruned to live tool names.
//   - ToolSearch — model uses it to "search" for tools instead of calling
//     ours directly, then errors out
//   - Bash, Read, Write, Edit, Glob, Grep, NotebookEdit, BashOutput,
//     KillShell — file/shell access, has no place in a read-only Q&A; would
//     otherwise let the model shell out (saw "hive: command not found" in
//     early smokes where it tried `bash -c hive task list`)
//   - WebFetch, WebSearch — outbound network, irrelevant
//
// Banning more (Skill / AskUserQuestion / EnterPlanMode / etc.) was tested
// and made the model output tool-call JSON as text instead of issuing real
// tool_use blocks — leave those alone.
var disallowedChatBuiltins = []string{
	"TaskList", "TaskCreate", "TaskGet", "TaskOutput",
	"TaskStop", "TaskUpdate", "ToolSearch", "TodoWrite", "Task",
	"Bash", "Read", "Write", "Edit", "Glob", "Grep", "NotebookEdit",
	"BashOutput", "KillShell", "WebFetch", "WebSearch",
}

func (a *ChatAgent) Send(ctx context.Context, conv *chat.Conversation, userMsg string, emit func(chat.Frame)) error {
	// 1. Stable per-session scratch dir + HOME-redirect scope + chat-tools mcp.json.
	// Using a fixed path (<scratchRoot>/<sessionID>) instead of a per-turn
	// timestamp suffix means claude's --resume can find its previous session state
	// in <scratch>/home/.claude/projects/<cwd-hash>/<session-id>.jsonl across turns.
	// Turn 1 stores its session under this dir; turn 2 passes --resume and finds it
	// in the same dir. Without this fix, each turn got a fresh scratch dir with an
	// empty .claude/projects, causing "no conversation found with session ID" on turn 2+.
	scratch := filepath.Join(a.scratchRoot, conv.SessionID)
	scope, err := a.getOrMaterializeScope(scratch)
	if err != nil {
		emit(chat.Frame{Kind: "error", Text: err.Error()})
		return fmt.Errorf("chat scope: %w", err)
	}
	mcpReq := MCPConfigRequest{
		DestDir:       scratch,
		HiveBinary:    a.cfg.HiveBinary,
		ChatTools:     true,
		ChatToolsMode: a.mcpMode, // "" or "plan" — see PlannerToolNames docstring
		DaemonSocket:  a.cfg.DaemonSocket,
	}
	if a.cfg.UseHTTPChat {
		base, err := os.ReadFile(a.cfg.MCPURLPath)
		if err != nil {
			emit(chat.Frame{Kind: "error", Text: err.Error()})
			return fmt.Errorf("read mcp.url: %w (is the daemon running?)", err)
		}
		mcpReq.UseHTTPFor = map[string]bool{"chat": true}
		mcpReq.MCPBaseURL = strings.TrimSpace(string(base))
		mcpReq.ChatSessionID = conv.SessionID
	}
	mcpPath, err := WriteMCPConfig(mcpReq)
	if err != nil {
		emit(chat.Frame{Kind: "error", Text: err.Error()})
		return fmt.Errorf("chat mcp config: %w", err)
	}

	// 1b. Warm the chat-tools MCP path: pre-spawn the same server and run a
	// quick init+tools/list+exit handshake. This warms the OS file cache for
	// the hive binary and confirms the daemon socket is reachable before
	// claude spawns its own copy. The model can still race the actual
	// per-turn registration (claude spawns a separate copy via --mcp-config),
	// but a hot binary + warm pages cut MCP startup from ~300-500ms to
	// <100ms — short enough that the model's thinking-tokens window covers
	// it. Failures are non-fatal (the real run will report them clearly).
	warmCtx, warmCancel := context.WithTimeout(ctx, 3*time.Second)
	_ = warmupChatMCP(warmCtx, a.cfg.Binary, a.cfg.HiveBinary, a.cfg.DaemonSocket, a.mcpMode)
	warmCancel()

	// 2. Resolve --resume from the prior claude session for this chat session.
	var prevSession string
	if conv.SessionID != "" && a.sessions != nil {
		prevSession, _ = a.sessions.GetChatProviderSession(ctx, conv.SessionID)
	}

	// 3. Build args, mirroring RunStage's assembly. ExtraArgs first so the
	// fake-claude -fixture flag (tests) lands ahead of the real flag set.
	args := append([]string{}, a.cfg.ExtraArgs...)
	args = append(args, "--output-format", "stream-json", "--verbose")
	if a.systemPrompt != "" {
		args = append(args, "--append-system-prompt", a.systemPrompt)
	}
	if a.cfg.Model != "" {
		args = append(args, "--model", a.cfg.Model)
	}
	if mcpPath != "" {
		// --strict-mcp-config: use ONLY the hive_chat server from mcpPath and
		// ignore every other MCP source. Without it, claude (running under the
		// subscription HOME) also spins up the operator's account-level
		// claude.ai connectors (Google Drive/Gmail/Calendar) and plugin servers
		// (github, playwright) on every turn — all irrelevant to a read-only
		// Q&A / planner session, and Google Drive in particular fails its auth
		// handshake every time, polluting startup with connection errors and
		// latency. Strict mode keeps the session hermetic to our tools.
		args = append(args, "--mcp-config", mcpPath, "--strict-mcp-config")
	}
	// Chat is read-only Q&A: constrain Claude to ONLY the hive_chat MCP tools
	// (no Bash/Read/etc.) so it answers via the daemon's data rather than
	// shelling out / acting like a coding agent in whatever cwd it's in.
	// --allowedTools allows exactly these without a permission prompt, so we
	// do NOT pass --dangerously-skip-permissions (which would re-enable Bash).
	//
	// Chat is read-only Q&A — constrain claude to ONLY the hive_chat MCP
	// tools. --allowedTools auto-approves our MCP tools (no permission
	// prompt); --disallowedTools bans the Claude Code built-ins whose
	// names would otherwise be picked by the model over the longer
	// mcp__hive_chat__* ones (e.g. TaskList → "list pending tasks"), and
	// the file/shell tools (Bash, Read, etc.) which have no business in a
	// read-only Q&A session.
	allowed := make([]string, 0, len(a.toolNames))
	for _, t := range a.toolNames {
		allowed = append(allowed, "mcp__hive_chat__"+t)
	}
	args = append(args, "--allowedTools", strings.Join(allowed, " "))
	args = append(args, "--disallowedTools", strings.Join(disallowedChatBuiltins, " "))
	if prevSession != "" {
		args = append(args, "--resume", prevSession)
	}
	args = append(args, "-p", userMsg)

	// 4. Run the subprocess, mapping events -> frames as they arrive.
	var (
		textBuf       strings.Builder // accumulated assistant text for the synthetic conv entry
		claudeSession string          // session_id from system/init (for --resume next turn)
		turnModel     = a.cfg.Model   // overridden by any model the events report
	)
	// Track tool_use_id → tool_name across this turn. tool_result blocks carry
	// only the id; we look up the name from the matching tool_use emitted
	// earlier in the stream by the assistant.
	toolNameByID := map[string]string{}
	onEvent := func(ev Event, _ time.Time) {
		switch ev.Type {
		case EventSystem:
			if ev.SessionID != "" {
				claudeSession = ev.SessionID
			}
			if ev.Model != "" {
				turnModel = ev.Model
			}
		case EventText:
			text := ev.Delta
			if text == "" {
				text = ev.Text
			}
			if text != "" {
				textBuf.WriteString(text)
				emit(chat.Frame{Kind: "text", Text: text})
			}
		case EventToolResult:
			tool := ev.ToolName
			if tool == "" {
				tool = toolNameByID[ev.ToolUseID]
			}
			if tool == "" {
				tool = ev.ToolUseID
			}
			emit(chat.Frame{Kind: "tool_result", Tool: tool, Result: ev.Output})
		case EventResult:
			if ev.Model != "" {
				turnModel = ev.Model
			}
		}
		// Real claude nests assistant text + tool results inside the assistant/
		// user message.content[] blocks (the EventText/EventToolResult cases
		// above only fire for the legacy top-level fake-claude fixture shape).
		// These two paths are mutually exclusive in practice — real claude is
		// nested-only, the fake fixture is top-level-only — so the same turn's
		// text is never emitted twice.
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "text":
				if block.Text != "" {
					textBuf.WriteString(block.Text)
					emit(chat.Frame{Kind: "text", Text: block.Text})
				}
			case "tool_use":
				// Capture id→name so tool_result blocks emitted later in the
				// same stream (which carry only the id) can resolve the name.
				if block.ID != "" && block.Name != "" {
					toolNameByID[block.ID] = block.Name
				}
			case "tool_result":
				name := toolNameByID[block.ToolUseID]
				if name == "" {
					name = block.ToolUseID // defensive: fall back to id when name unknown
				}
				emit(chat.Frame{Kind: "tool_result", Tool: name, Result: stringifyContent(block.Content)})
			}
		}
	}

	envSlice := buildChatEnv(scope.StageHome)
	// Thread the chat session id so the per-turn mcp-stage-server can stamp
	// every chat.tool RPC with it; the daemon needs it to route tool_proposed
	// frames back to the right chat.send stream.
	if conv.SessionID != "" {
		envSlice = append(envSlice, "HIVE_CHAT_SESSION_ID="+conv.SessionID)
	}
	sub := NewSubprocess(SubprocessConfig{
		Binary: a.cfg.Binary,
		Args:   args,
		Env:    envSlice,
		// Run in the neutral scratch dir, not the daemon's cwd (the repo) —
		// otherwise Claude picks up the repo's CLAUDE.md and behaves like a
		// coding agent instead of answering via the chat tools.
		Cwd:     scratch,
		OnEvent: onEvent,
	})
	_, runErr := sub.Run(ctx)
	if runErr != nil {
		emit(chat.Frame{Kind: "error", Text: runErr.Error()})
		return runErr
	}

	// 5. Persist the claude session for next-turn --resume; append the
	// synthetic assistant message; emit turn_done.
	if claudeSession != "" && a.sessions != nil && conv.SessionID != "" {
		if err := a.sessions.SetChatProviderSession(ctx, conv.SessionID, claudeSession); err != nil {
			// Non-fatal: losing the resume handle just means the next turn
			// starts fresh. The turn itself succeeded.
			emit(chat.Frame{Kind: "error", Text: fmt.Sprintf("warn: persist resume session: %v", err)})
		}
	}
	if assistantText := textBuf.String(); assistantText != "" {
		conv.Messages = append(conv.Messages, anth.NewAssistantMessage(anth.NewTextBlock(assistantText)))
	}
	// Subscription cost is not surfaced by `claude -p` (it's flat-rate), so
	// CostUSD stays 0 for the CC provider — conv.CostUSD is left untouched.
	emit(chat.Frame{Kind: "turn_done", Model: turnModel, CostUSD: 0})
	return nil
}

// getOrMaterializeScope returns a cached ScopeInfo for stageDir, materializing
// a fresh one (and caching it) on the first call. Caching avoids redundant
// RemoveAll+symlink I/O on subsequent turns of the same chat session, and
// ensures a single canonical scope object per session for pointer-identity tests.
// MaterializeScope is not called again after the first turn because the scratch
// dir (and its .claude/projects/... session state) must remain stable so that
// claude's --resume can find the prior turn's conversation on disk.
func (a *ChatAgent) getOrMaterializeScope(stageDir string) (*ScopeInfo, error) {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	if existing, ok := a.scopeCache[stageDir]; ok {
		return existing, nil
	}
	scope, err := MaterializeScope(ScopeRequest{StageDir: stageDir, RealHome: a.cfg.RealHome})
	if err != nil {
		return nil, err
	}
	a.scopeCache[stageDir] = scope
	return scope, nil
}

// stringifyContent renders a nested tool_result block's "content" payload to a
// plain string. Real claude emits it either as a JSON string ("output text") or
// as an array of content blocks ([{"type":"text","text":"..."}]); both collapse
// to the concatenated text. Anything else falls back to the raw JSON bytes.
func stringifyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return string(raw)
}
