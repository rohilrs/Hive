package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/approval"
	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/store/pricing"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// chatSystemPrefix is the short system prompt for the chat agent. The tool
// definitions themselves are advertised separately by the registry.
const chatSystemPrefix = `You are Hive's assistant. Use the tools to answer questions about tasks, runs, costs, and daemon status. Be concise.

# Output format for the Hive TUI

Your responses render inside a narrow terminal panel (usually 60–80 cols
wide). Follow these formatting rules strictly:

1. NEVER use markdown tables. The terminal renderer cannot fit them.
   No exceptions, even for "structured" data.

2. For lists of items (even single-item lists), use the stacked block format:

       **<Name>**  ·  <slug-or-short-id>
         <one short detail line>

   Separate items with a single blank line. Name is bold. The
   handle (slug for projects, short ID for tasks) is plain text — do
   NOT wrap it in backticks. The detail line is a ` + "`·`" + `-joined summary
   of the most useful facts.

3. Open list responses with a one-sentence count ("You have 3 active
   projects:" / "Two pending tasks:"). Use stacked blocks even when
   there is only one result — do not collapse to prose. Consistency
   matters more than compactness.

4. Elide long identifiers:
   - Task / Run IDs (task-… / run-…): first 4 chars after the prefix +
     ellipsis + last 4, e.g. task-1780…4185. Do NOT wrap in backticks.
   - Paths: substitute $HOME with ~. If still long, show
     /firstSegment/…/lastTwo/segments.
   - Project IDs (proj-…): never show; the slug is the handle.

5. Detail lines per item type:
   - Project: headline is **Name**  ·  slug  ·  ~/path (path elided with
     ~ for $HOME and /first/…/lastTwo/segments for paths over 32 chars).
     Detail line shows non-zero task counts only:
       1 pending  ·  28 needs_attention  ·  40 done
     Omit a status bucket if its count is 0. Omit the detail line
     entirely if there are no tasks. The path goes on the HEADLINE,
     NEVER on a second indented line.

     BAD (path on detail line):
       **Hive (smoke test)**  ·  hive
         /mnt/e/Documents/Hive/hive

     GOOD (path on headline, optional counts on detail):
       **Hive (smoke test)**  ·  hive  ·  ~/Documents/Hive/hive
         1 pending  ·  40 done

   - Task: ONE LINE only, no detail line. Format:
       **<title>**  ·  <pipeline>  ·  <priority>  ·  <status>
     Do NOT include the task ID anywhere — the title is enough for the
     user to recognize. If two tasks share the same title in the same
     response, append " (in <project-slug>)" to disambiguate.

     BAD (task ID on tail + status on second line):
       **test smoke task**  ·  task-1780…4185
         pending  ·  P3  ·  build

     GOOD (single line, no ID):
       **test smoke task**  ·  build  ·  P3  ·  pending

   - Run: ONE LINE only. Format:
       **<title-or-pipeline>**  ·  <status>  ·  <pipeline>  ·  <age>

6. Use bold only for item names (the recognition target). Do not bold
   statuses, counts, or headers.

7. Do not wrap output in code fences unless it is genuinely code the
   user should copy.

8. If the user explicitly asks for "full IDs", "raw data", or "all
   columns", show full values — but still use stacked blocks, never
   tables.

9. Tool result conventions:
   - When a tool_result content is exactly {"error":"user cancelled, do not retry"}: stop trying that tool, briefly acknowledge, and wait for the next user message. Do not look for workarounds.
   - When a tool_result content begins with [user edited args before running] : the user adjusted your proposed args before approval; the rest of the content is the actual tool result from the edited call.`

// chatCostFn maps (model, tokensIn, tokensOut) to USD using the shared
// pricing table. Unknown models cost 0 (Lookup ok=false), matching the
// pipeline's "record NULL/zero rather than guess" convention.
func chatCostFn(model string, tokensIn, tokensOut int64) float64 {
	m, ok := pricing.Lookup(model)
	if !ok {
		return 0
	}
	return pricing.Cost(int(tokensIn), int(tokensOut), 0, m)
}

// buildSDKChatAgent builds the read-tool registry, router, and SDK-backed
// agent from the daemon's config + store. It reads the API key from the
// configured env var; an empty key is a hard error (no offline fallback).
// The composition root calls this for the "api" provider and injects the
// result via SetChatAgent.
func (d *Daemon) buildSDKChatAgent() (*chat.SDKAgent, error) {
	keyEnv := d.cfg.Cfg.Chat.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "ANTHROPIC_API_KEY"
	}
	key := os.Getenv(keyEnv)
	if key == "" {
		return nil, fmt.Errorf("chat: %s not set", keyEnv)
	}

	defaultModel := d.cfg.Cfg.Chat.DefaultModel
	reasoningModel := d.cfg.Cfg.Chat.ReasoningModel

	sdk := anthropic.NewSDK(anthropic.SDKConfig{APIKey: key, Model: defaultModel})
	registry := d.chatRegistry()
	router := chat.NewRouter(sdk, defaultModel, reasoningModel)

	agent := chat.NewSDKAgent(sdk, registry, router, chat.Config{
		DefaultModel:   defaultModel,
		ReasoningModel: reasoningModel,
		MaxIters:       d.cfg.Cfg.Chat.MaxIters,
		SystemPrefix:   chatSystemPrefix,
	}, chatCostFn)
	agent.SetConfirmGate(newDaemonConfirmGate(d, d.cfg.Cfg.Chat.ConfirmTimeoutSeconds))
	agent.SetAutoConfirm(d.cfg.Cfg.Chat.AutoConfirm)
	return agent, nil
}

// BuildSDKChatAgent is the exported wrapper the composition root
// (cmd/hive/cmd_daemon) calls to build the SDK-backed chat agent for the "api"
// provider. It returns chat.Agent (not the concrete type) so the caller can
// inject it via SetChatAgent without importing the SDK type. An error means no
// API key — the caller logs it and leaves the agent nil (chat disabled).
func (d *Daemon) BuildSDKChatAgent() (chat.Agent, error) {
	a, err := d.buildSDKChatAgent()
	if err != nil {
		// Return an untyped-nil chat.Agent on error so the caller's `agent ==
		// nil` check works (returning a typed-nil *SDKAgent would be non-nil).
		return nil, err
	}
	return a, nil
}

// BuildPlannerSDKAgent builds an SDK-backed chat agent configured for
// planner mode (Phase 8.A T6): planner registry composed over the daemon's
// chat registry (so the planner inherits read tools for situational
// awareness), planner system prompt scoped to the project, and a router
// with ForceSonnet=true (every Q&A turn goes to Sonnet — the classify-driven
// economy is the wrong default for design work).
//
// The composition root wires this via SetPlannerAgentFor so streamChat can
// build a fresh per-session agent each time a planner-kind session runs.
// An error means no API key — handled the same way as BuildSDKChatAgent.
func (d *Daemon) BuildPlannerSDKAgent(slug, cwd string) (chat.Agent, error) {
	keyEnv := d.cfg.Cfg.Chat.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "ANTHROPIC_API_KEY"
	}
	key := os.Getenv(keyEnv)
	if key == "" {
		return nil, fmt.Errorf("chat: %s not set", keyEnv)
	}

	defaultModel := d.cfg.Cfg.Chat.DefaultModel
	reasoningModel := d.cfg.Cfg.Chat.ReasoningModel

	sdk := anthropic.NewSDK(anthropic.SDKConfig{APIKey: key, Model: reasoningModel})
	plannerRegistry := chat.NewPlannerRegistry(cwd, d.chatRegistry(), d.scheduler.effectiveFeatureBranchForProject(slug), d.plannerGrounderFor(slug, cwd))
	router := chat.NewRouter(sdk, defaultModel, reasoningModel)
	router.ForceSonnet = true

	agent := chat.NewSDKAgent(sdk, plannerRegistry, router, chat.Config{
		DefaultModel:   defaultModel,
		ReasoningModel: reasoningModel,
		MaxIters:       d.cfg.Cfg.Chat.MaxIters,
		SystemPrefix:   chat.PlannerSystemPrompt(slug, cwd),
	}, chatCostFn)
	agent.SetConfirmGate(newDaemonConfirmGate(d, d.cfg.Cfg.Chat.ConfirmTimeoutSeconds))
	agent.SetAutoConfirm(d.cfg.Cfg.Chat.AutoConfirm)
	return agent, nil
}

// plannerRegistryForSession resolves a planner session's bound project to a
// cwd and returns a planner-aware chat.Registry composed over the daemon's
// base chat registry. Called by handleChatTool when the CC chat-tools MCP
// server forwards a tool from a planner-kind session, so the planner write
// tools (hive_save_roadmap, hive_save_spec) and planner read tools
// (hive_list_specs, hive_read_doc) dispatch through the SAME chat.tool RPC
// the regular chat tools use.
func (d *Daemon) plannerRegistryForSession(ctx context.Context, sess *store.ChatSession) (*chat.Registry, error) {
	if sess == nil {
		return nil, fmt.Errorf("nil session")
	}
	if sess.ProjectSlug == "" {
		return nil, fmt.Errorf("planner session has no project_slug")
	}
	proj, err := d.store.GetProjectBySlug(ctx, sess.ProjectSlug)
	if err != nil {
		return nil, fmt.Errorf("project %q: %w", sess.ProjectSlug, err)
	}
	if proj == nil || proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, fmt.Errorf("project %q has no repo_path", sess.ProjectSlug)
	}
	return chat.NewPlannerRegistry(*proj.RepoPath, d.chatRegistry(), d.scheduler.effectiveFeatureBranchForProject(sess.ProjectSlug), d.plannerGrounderFor(sess.ProjectSlug, *proj.RepoPath)), nil
}

// chatProvider exposes the configured chat provider ("api" | "claude-code")
// so streamChat knows whether to rehydrate SDK history (api) or rely on the
// CC provider's --resume (claude-code). Empty means "api" by default.
func (d *Daemon) chatProvider() string { return d.cfg.Cfg.Chat.Provider }

// chatRegistry builds the read-only tool registry. Each handler
// unmarshals its input, calls an existing store/daemon method, and
// JSON-marshals the result into ToolResult.Content. Errors become an
// {"error":...} IsError result so the model can recover gracefully.
//
// The same registry backs both the SDK agent (buildSDKChatAgent) and the
// chat.tool RPC (handleChatTool), so the CC chat provider's MCP server runs
// the exact same tool handlers the API agent does.
func (d *Daemon) chatRegistry() *chat.Registry {
	r := chat.NewRegistry()

	objSchema := func(props map[string]any, required ...string) map[string]any {
		req := make([]any, 0, len(required))
		for _, k := range required {
			req = append(req, k)
		}
		return map[string]any{"type": "object", "properties": props, "required": req}
	}

	// hive_list_tasks — pending tasks (the broadest list method that does
	// not require a project arg). Only `limit` is honored today; project/
	// status filters aren't advertised so the model doesn't expect filtering
	// that isn't wired (a richer list method can add them later).
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_list_tasks",
			Description: "List pending (queued) tasks. Optional limit caps the count.",
			InputSchema: objSchema(map[string]any{
				"limit": map[string]any{"type": "integer"},
			}),
		},
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				Limit int `json:"limit"`
			}
			_ = json.Unmarshal(input, &in)
			tasks, err := d.store.ListPendingTasks(ctx)
			if err != nil {
				return chatErr(err)
			}
			type taskSummary struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Status   string `json:"status"`
				Priority string `json:"priority"`
				Pipeline string `json:"pipeline"`
				Project  string `json:"project_id"`
			}
			out := make([]taskSummary, 0, len(tasks))
			for _, t := range tasks {
				if in.Limit > 0 && len(out) >= in.Limit {
					break
				}
				out = append(out, taskSummary{
					ID: t.ID, Title: t.Title, Status: t.Status,
					Priority: t.Priority, Pipeline: t.Pipeline, Project: t.ProjectID,
				})
			}
			return chatJSON(out)
		},
	})

	// hive_get_task — full task by id.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_get_task",
			Description: "Fetch a single task by its id.",
			InputSchema: objSchema(map[string]any{
				"id": map[string]any{"type": "string"},
			}, "id"),
		},
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			task, err := d.store.GetTask(ctx, in.ID)
			if err != nil {
				return chatErr(err)
			}
			return chatJSON(task)
		},
	})

	// hive_status — pending count + running runs, compact.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_status",
			Description: "Daemon status: pending task count and currently running runs.",
			InputSchema: objSchema(map[string]any{}),
		},
		Handler: func(ctx context.Context, _ json.RawMessage) chat.ToolResult {
			pending, err := d.store.ListPendingTasks(ctx)
			if err != nil {
				return chatErr(err)
			}
			// Phase 4.3.1 #3: chat tools surface every running row to the
			// model including child fix runs — use the all-inclusive view.
			running, err := d.store.ListAllRunningRuns(ctx)
			if err != nil {
				return chatErr(err)
			}
			type runView struct {
				ID       string `json:"id"`
				TaskID   string `json:"task_id"`
				Pipeline string `json:"pipeline"`
				Status   string `json:"status"`
			}
			runs := make([]runView, 0, len(running))
			for _, rr := range running {
				runs = append(runs, runView{ID: rr.ID, TaskID: rr.TaskID, Pipeline: rr.Pipeline, Status: rr.Status})
			}
			return chatJSON(map[string]any{
				"pending_tasks": len(pending),
				"running":       runs,
			})
		},
	})

	// hive_cost_summary — pre-rolled cost rollups.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_cost_summary",
			Description: "Cost rollups by day, model, pipeline, and project.",
			InputSchema: objSchema(map[string]any{}),
		},
		Handler: func(ctx context.Context, _ json.RawMessage) chat.ToolResult {
			cs, err := d.store.CostSummary(ctx)
			if err != nil {
				return chatErr(err)
			}
			return chatJSON(cs)
		},
	})

	// hive_active_workers — currently running runs.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_active_workers",
			Description: "List currently running runs (active workers).",
			InputSchema: objSchema(map[string]any{}),
		},
		Handler: func(ctx context.Context, _ json.RawMessage) chat.ToolResult {
			// Phase 4.3.1 #3: chat tools surface every running row to the
			// model including child fix runs — use the all-inclusive view.
			running, err := d.store.ListAllRunningRuns(ctx)
			if err != nil {
				return chatErr(err)
			}
			return chatJSON(running)
		},
	})

	// hive_get_run — full run by id.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_get_run",
			Description: "Fetch a single run by its id.",
			InputSchema: objSchema(map[string]any{
				"id": map[string]any{"type": "string"},
			}, "id"),
		},
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			run, err := d.store.GetRun(ctx, in.ID)
			if err != nil {
				return chatErr(err)
			}
			return chatJSON(run)
		},
	})

	// hive_search — FTS over events (the only FTS-indexed corpus today).
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_search",
			Description: "Full-text search over event log messages.",
			InputSchema: objSchema(map[string]any{
				"query": map[string]any{"type": "string"},
			}, "query"),
		},
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			events, err := d.store.SearchEvents(ctx, in.Query, 25)
			if err != nil {
				return chatErr(err)
			}
			return chatJSON(events)
		},
	})

	// hive_add_task — create a task in a project's inbox. Mutating: requires
	// confirm unless listed in [chat] auto_confirm.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_add_task",
			Description: "Create a task in a project's inbox. Returns {task_id}.",
			InputSchema: objSchema(map[string]any{
				"project_slug": map[string]any{"type": "string"},
				"title":        map[string]any{"type": "string"},
				"body":         map[string]any{"type": "string"},
				"priority":     map[string]any{"type": "string", "enum": []any{"P0", "P1", "P2", "P3"}},
				"pipeline":     map[string]any{"type": "string"},
			}, "project_slug", "title"),
		},
		Mutating: true,
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				ProjectSlug string `json:"project_slug"`
				Title       string `json:"title"`
				Body        string `json:"body"`
				Priority    string `json:"priority"`
				Pipeline    string `json:"pipeline"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			project, err := d.store.GetProjectBySlug(ctx, in.ProjectSlug)
			if err != nil {
				return chatErr(err)
			}
			if in.Priority == "" {
				in.Priority = "P3"
			}
			if in.Pipeline == "" {
				in.Pipeline = "build"
			}
			task := &store.Task{
				ID:        newID("task"),
				ProjectID: project.ID,
				Source:    "chat",
				Title:     in.Title,
				Body:      in.Body,
				Priority:  in.Priority,
				Status:    "pending",
				Pipeline:  in.Pipeline,
			}
			if err := d.store.InsertTask(ctx, task); err != nil {
				return chatErr(err)
			}
			return chatJSON(map[string]string{"task_id": task.ID})
		},
	})

	// hive_run_now — dispatch a pending task immediately. Returns {run_id}.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_run_now",
			Description: "Dispatch a pending task to run now. Returns {run_id}.",
			InputSchema: objSchema(map[string]any{
				"task_id": map[string]any{"type": "string"},
			}, "task_id"),
		},
		Mutating: true,
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			runID, err := d.scheduler.RunNow(ctx, in.TaskID)
			if err != nil {
				return chatErr(err)
			}
			return chatJSON(map[string]string{"run_id": runID})
		},
	})

	// hive_abandon — cancel a running run (or mark a queued run abandoned).
	// Returns {cancelled, run_id}.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_abandon",
			Description: "Cancel a running run. Returns {cancelled, run_id}.",
			InputSchema: objSchema(map[string]any{
				"run_id": map[string]any{"type": "string"},
				"reason": map[string]any{"type": "string"},
			}, "run_id"),
		},
		Mutating: true,
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				RunID  string `json:"run_id"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			cancelled := d.cancelRun(in.RunID)
			reason := in.Reason
			if reason == "" {
				reason = "abandoned via chat"
			}
			if err := d.store.MarkRunEnded(ctx, in.RunID, "abandoned", reason); err != nil {
				return chatErr(err)
			}
			return chatJSON(map[string]any{"cancelled": cancelled, "run_id": in.RunID})
		},
	})

	// hive_edit_task — update an existing task's title and/or body. Mutating.
	// Only title/body are editable today (matches store.UpdateTaskContent);
	// priority/pipeline edits would require a new store method (deferred).
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_edit_task",
			Description: "Edit an existing task's title or body. Returns {task_id, updated}.",
			InputSchema: objSchema(map[string]any{
				"task_id": map[string]any{"type": "string"},
				"title":   map[string]any{"type": "string"},
				"body":    map[string]any{"type": "string"},
			}, "task_id", "title"),
		},
		Mutating: true,
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				TaskID string `json:"task_id"`
				Title  string `json:"title"`
				Body   string `json:"body"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			if err := d.store.UpdateTaskContent(ctx, in.TaskID, in.Title, in.Body); err != nil {
				return chatErr(err)
			}
			return chatJSON(map[string]any{"task_id": in.TaskID, "updated": true})
		},
	})

	// hive_approve — resolve a pending approval as approved. Mutating.
	// approval_id comes from the user's earlier interaction with
	// `hive approvals pending` or from the TUI.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_approve",
			Description: "Approve a pending approval by its approval_id. Returns {resolved}.",
			InputSchema: objSchema(map[string]any{
				"approval_id": map[string]any{"type": "string"},
			}, "approval_id"),
		},
		Mutating: true,
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				ApprovalID string `json:"approval_id"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			if in.ApprovalID == "" {
				return chatErr(fmt.Errorf("approval_id is required"))
			}
			found := d.resolvePending(in.ApprovalID, approval.Decision{
				Kind:   approval.DecisionApprove,
				Reason: "approved via chat",
				RuleID: "operator",
			})
			return chatJSON(map[string]any{"resolved": found})
		},
	})

	// hive_deny — resolve a pending approval as denied. Mutating. Optional
	// reason is included in the decision audit trail.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_deny",
			Description: "Deny a pending approval by its approval_id. Returns {resolved}.",
			InputSchema: objSchema(map[string]any{
				"approval_id": map[string]any{"type": "string"},
				"reason":      map[string]any{"type": "string"},
			}, "approval_id"),
		},
		Mutating: true,
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				ApprovalID string `json:"approval_id"`
				Reason     string `json:"reason"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			if in.ApprovalID == "" {
				return chatErr(fmt.Errorf("approval_id is required"))
			}
			reason := in.Reason
			if reason == "" {
				reason = "denied via chat"
			}
			found := d.resolvePending(in.ApprovalID, approval.Decision{
				Kind:   approval.DecisionDeny,
				Reason: reason,
				RuleID: "operator",
			})
			return chatJSON(map[string]any{"resolved": found})
		},
	})

	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_resume",
			Description: "Resume a run that's in needs_attention/error/abandoned by re-launching the pipeline against the existing worktree. Pipeline restarts from stage 0; prior cost + stalls + prediction are preserved. Use hive_run_now for a fresh restart (new run, new worktree) instead.",
			InputSchema: objSchema(map[string]any{
				"run_id": map[string]any{"type": "string"},
			}, "run_id"),
		},
		Mutating: true, // chat confirm gate fires before the daemon spawns a worker
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var p struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return toolErr("invalid params: " + err.Error())
			}
			return d.handleHiveResume(ctx, p.RunID)
		},
	})

	// hive_predict — fully implemented: decodes {task_id, refresh} and
	// delegates to handleHivePredict which runs the predictor on-demand.
	// Non-Mutating (read-like, no confirm gate required).
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_predict",
			Description: "Recompute the cost/files prediction for a task. Returns candidate files, capsule symbols, and per-call metrics. Pass refresh=true to also persist the new prediction to the task's latest non-terminal run (if any). Costs roughly $0.001-0.005 per call (one Haiku classify).",
			InputSchema: objSchema(map[string]any{
				"task_id": map[string]any{"type": "string"},
				"refresh": map[string]any{"type": "boolean"},
			}, "task_id"),
		},
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var p struct {
				TaskID  string `json:"task_id"`
				Refresh bool   `json:"refresh"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return toolErr("invalid params: " + err.Error())
			}
			return d.handleHivePredict(ctx, p.TaskID, p.Refresh)
		},
	})

	// hive_list_projects — return all projects, newest first. Useful for
	// the agent to discover what slug names exist before creating tasks.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_list_projects",
			Description: "List all projects (id, slug, name, repo_path, status). Read-only.",
			InputSchema: objSchema(map[string]any{
				"status": map[string]any{"type": "string", "description": "Optional status filter (e.g. 'active'); empty for all."},
			}),
		},
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(input, &in)
			projects, err := d.store.ListProjects(ctx, in.Status)
			if err != nil {
				return chatErr(err)
			}
			// Attach per-project task_counts so the agent can render
			// "5 pending · 1 running · ~/Hive/hive" without an N+1 followup.
			// counts is built from a single GROUP BY (cheap); a nil inner
			// map means "no tasks for that project" — we omit task_counts
			// in that case so the model doesn't render zero-buckets.
			counts, err := d.store.TaskCountsByProject(ctx)
			if err != nil {
				return chatErr(err)
			}
			out := make([]map[string]any, len(projects))
			for i, p := range projects {
				row := map[string]any{
					"id":        p.ID,
					"slug":      p.Slug,
					"name":      p.Name,
					"repo_path": p.RepoPath,
					"status":    p.Status,
				}
				if c, ok := counts[p.ID]; ok && len(c) > 0 {
					row["task_counts"] = c
				}
				out[i] = row
			}
			return chatJSON(out)
		},
	})

	// hive_decompose — break a task into a sequence of independently-shippable
	// sub-tasks (Sonnet) and insert them as children of the original task.
	// Mutating: the existing chat confirm gate fires BEFORE the Handler runs,
	// so the user's y/n on the tool_proposed frame approves the IDEA of
	// decomposing — not a specific breakdown. The Handler then runs BOTH
	// task.decompose (LLM call → proposal) AND task.decompose_apply (insert
	// children) atomically in one shot. This differs from the CLI's two-step
	// preview-then-confirm flow.
	r.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_decompose",
			Description: "Break a task into a sequence of independently-shippable sub-tasks (Sonnet). Inserts them as children of the original task on confirm.",
			InputSchema: objSchema(map[string]any{
				"task_id":      map[string]any{"type": "string"},
				"max_subtasks": map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
			}, "task_id"),
		},
		Mutating: true,
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			var in struct {
				TaskID      string `json:"task_id"`
				MaxSubtasks int    `json:"max_subtasks"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return chatErr(err)
			}
			if in.TaskID == "" {
				return chatErr(fmt.Errorf("task_id required"))
			}
			// RPCServer is the natural home of handleDecompose/handleDecomposeApply
			// (they need store + bus from the Daemon). Construct one on the fly so
			// this works even before Start() has wired d.rpc — chatRegistry() is
			// also used by tests that don't run Start.
			srv := NewRPCServer(d)
			decParams, _ := json.Marshal(map[string]any{
				"task_id":      in.TaskID,
				"max_subtasks": in.MaxSubtasks,
			})
			decRaw, rpcErr := srv.handleDecompose(ctx, decParams)
			if rpcErr != nil {
				return chatErr(fmt.Errorf("decompose: %s", rpcErr.Message))
			}
			var decRes DecomposeResult
			if err := json.Unmarshal(decRaw, &decRes); err != nil {
				return chatErr(err)
			}
			applyParams, _ := json.Marshal(map[string]any{
				"parent_task_id": in.TaskID,
				"subtasks":       decRes.Subtasks,
			})
			applyRaw, rpcErr := srv.handleDecomposeApply(ctx, applyParams)
			if rpcErr != nil {
				return chatErr(fmt.Errorf("decompose_apply: %s", rpcErr.Message))
			}
			var applyRes DecomposeApplyResult
			_ = json.Unmarshal(applyRaw, &applyRes)
			return chatJSON(map[string]any{
				"inserted_count":    len(applyRes.InsertedTaskIDs),
				"inserted_task_ids": applyRes.InsertedTaskIDs,
				"cost_usd":          decRes.CostUSD,
				"model":             decRes.Model,
				"subtask_titles":    subtaskTitles(decRes.Subtasks),
			})
		},
	})

	return r
}

// subtaskTitles is a small helper that pulls just the Title field from a
// slice of ProposedSubtask — used by the hive_decompose chat tool's compact
// result so the model can paraphrase the breakdown without re-serializing
// every field.
func subtaskTitles(subs []decompose.ProposedSubtask) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.Title)
	}
	return out
}

// chatJSON marshals v into a ToolResult; marshal failure becomes an error
// result rather than panicking.
func chatJSON(v any) chat.ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return chatErr(err)
	}
	return chat.ToolResult{Content: string(b)}
}

// chatErr builds an IsError ToolResult with a JSON {"error":...} body.
func chatErr(err error) chat.ToolResult {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return chat.ToolResult{Content: string(b), IsError: true}
}

// deriveSessionName turns the first user message into a short session
// name (~50 runes max, truncated at a word boundary when possible).
// Rune-safe (no UTF-8 cut hazard).
func deriveSessionName(message string) string {
	const maxRunes = 50
	rs := []rune(strings.TrimSpace(message))
	if len(rs) == 0 {
		return ""
	}
	if len(rs) <= maxRunes {
		return string(rs)
	}
	// Look for the last space within [maxRunes/2, maxRunes) so we cut
	// at a word boundary; fall back to maxRunes if no convenient space.
	cut := maxRunes
	for i := maxRunes - 1; i > maxRunes/2; i-- {
		if rs[i] == ' ' {
			cut = i
			break
		}
	}
	return string(rs[:cut]) + "…"
}

// ChatHistoryListParams is the params envelope for chat.history_list.
type ChatHistoryListParams struct {
	Limit int `json:"limit,omitempty"`
}

// ChatHistoryGetParams is the params envelope for chat.history_get.
type ChatHistoryGetParams struct {
	SessionID string `json:"session_id"`
}

// handleChatHistoryList returns the most recent chat sessions, newest first.
// limit defaults to 50, capped at 200.
func (s *RPCServer) handleChatHistoryList(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ChatHistoryListParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
		}
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	sessions, err := s.d.store.ListChatSessions(ctx, p.Limit)
	if err != nil {
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(map[string]any{"sessions": sessions})
	return out, nil
}

// handleChatHistoryGet returns all messages for one chat session in
// created_at ASC order (replay order).
func (s *RPCServer) handleChatHistoryGet(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ChatHistoryGetParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.SessionID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "session_id required"}
	}
	msgs, err := s.d.store.GetChatMessages(ctx, p.SessionID)
	if err != nil {
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(map[string]any{"messages": msgs})
	return out, nil
}

// ChatSetNameParams is the params envelope for chat.set_name.
type ChatSetNameParams struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

// handleChatSetName updates the human-readable name of a chat session.
// Empty SessionID is invalid; an over-long name is capped at 200 runes.
func (s *RPCServer) handleChatSetName(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ChatSetNameParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.SessionID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "session_id required"}
	}
	// Cap at 200 runes (defensive — long names break display layouts).
	if rs := []rune(p.Name); len(rs) > 200 {
		p.Name = string(rs[:200])
	}
	if err := s.d.store.SetChatSessionName(ctx, p.SessionID, p.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "session not found"}
		}
		return nil, internalErr(err)
	}
	out, _ := json.Marshal(map[string]any{"ok": true})
	return out, nil
}

// ChatDeleteParams is the params envelope for chat.delete.
type ChatDeleteParams struct {
	SessionID string `json:"session_id"`
}

// handleChatDelete removes a chat session and all its messages atomically,
// then evicts the in-memory scope cache + on-disk scratch dir if the
// chat agent exposes a session-eviction hook (only the claude-code agent
// does today; the SDK agent has nothing to evict).
//
// Returns {ok: true} on success, ErrInvalidParams with "session not found"
// when the session row is absent (idempotent: still tries cache eviction
// since the on-disk state may exist regardless).
func (s *RPCServer) handleChatDelete(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ChatDeleteParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.SessionID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "session_id required"}
	}
	dbErr := s.d.store.DeleteChatSession(ctx, p.SessionID)
	if dbErr != nil && !errors.Is(dbErr, store.ErrNotFound) {
		return nil, internalErr(dbErr)
	}
	// Evict scope cache + reap on-disk scratch even on a missing row —
	// scratch dirs can survive a row deletion via the existing
	// EndChatSession-after-every-turn semantics or daemon restart.
	if evictor, ok := s.d.chatAgent.(interface{ EvictSession(sessionID string) }); ok {
		evictor.EvictSession(p.SessionID)
	}
	if errors.Is(dbErr, store.ErrNotFound) {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "session not found"}
	}
	out, _ := json.Marshal(map[string]any{"ok": true})
	return out, nil
}

// ChatToolParams is the params envelope for the chat.tool RPC. The CC chat
// provider's MCP server forwards each read-tool call here so it executes
// against the same chat.Registry the SDK agent uses. SessionID is threaded
// from the chat.send stream so the gate can route tool_proposed frames back
// to the right session; empty means an orphan call (denied gracefully).
type ChatToolParams struct {
	Tool      string          `json:"tool"`
	SessionID string          `json:"session_id"`
	Input     json.RawMessage `json:"input"`
}

// handleChatTool runs a single tool from the chat registry on behalf of the
// CC chat provider's MCP forwarder. Unknown tools are an invalid-params error;
// Mutating tools go through the confirm gate (unless auto-confirmed); all
// others run directly. Handler-level errors are carried in is_error, not as
// an RPC error, so the model can recover.
func (s *RPCServer) handleChatTool(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ChatToolParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.Tool == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "tool required"}
	}
	reg := s.d.chatRegistryForTest
	if reg == nil {
		// Phase 8.A T6b: planner-kind sessions get a planner registry so
		// hive_save_roadmap / hive_list_specs / etc. are dispatchable
		// through the same chat.tool RPC the CC chat-tools MCP server
		// forwards every call to. We look up the session row to discover
		// (a) whether this is a planner session and (b) which project
		// it's bound to (planner write tools scope writes to <repo>/docs/).
		// A lookup failure falls through to the regular chat registry so a
		// missing session-row never blocks normal chat tools.
		if p.SessionID != "" {
			if sess, err := s.d.store.GetChatSession(ctx, p.SessionID); err == nil && sess != nil && sess.Kind == store.KindPlan {
				if planReg, perr := s.d.plannerRegistryForSession(ctx, sess); perr == nil {
					reg = planReg
				}
			}
		}
		if reg == nil {
			reg = s.d.chatRegistry()
		}
	}
	tool, ok := reg.Get(p.Tool)
	if !ok {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "unknown chat tool: " + p.Tool}
	}
	edited := false
	if tool.Mutating && !isAutoConfirmed(p.Tool, s.d.cfg.Cfg.Chat.AutoConfirm) {
		var gate chat.ConfirmGate
		if s.d.chatConfirmGateForTool != nil {
			gate = s.d.chatConfirmGateForTool
		} else {
			gate = newDaemonConfirmGate(s.d, s.d.cfg.Cfg.Chat.ConfirmTimeoutSeconds)
		}
		callID := fmt.Sprintf("cc-%d", time.Now().UnixNano())
		dec, _ := gate.Propose(ctx, p.SessionID, callID, p.Tool, p.Input)
		if !dec.Approve {
			errMsg := dec.DenyReason
			if errMsg == "" {
				errMsg = "declined by user"
			}
			body, _ := json.Marshal(map[string]string{"error": errMsg})
			out, _ := json.Marshal(map[string]any{"content": string(body), "is_error": true})
			return out, nil
		}
		if dec.EditedInput != nil {
			p.Input = dec.EditedInput
			edited = true
		}
	}
	res := tool.Handler(ctx, p.Input)
	if edited {
		res.Content = "[user edited args before running] " + res.Content
	}
	// Roadmap-mirror: when the planner successfully saves a roadmap for a
	// write-back project, mirror it to Linear (document + milestones + links).
	// Best-effort, runs in the background, never blocks the tool response.
	if p.Tool == "hive_save_roadmap" && !res.IsError {
		s.syncRoadmapMirrorAfterSave(p.Input)
		s.publishCanSequenceAfterRoadmapSave(p.Input)
	}
	out, _ := json.Marshal(map[string]any{"content": res.Content, "is_error": res.IsError})
	return out, nil
}

// syncRoadmapMirrorAfterSave fires syncRoadmapToLinear for the saved project in
// the background with a bounded timeout. Best-effort: failures are logged inside
// syncRoadmapToLinear; this never affects the chat-tool response. No-op for
// projects that aren't Linear write-back bound.
func (s *RPCServer) syncRoadmapMirrorAfterSave(input json.RawMessage) {
	var a struct {
		ProjectSlug string `json:"project_slug"`
	}
	if json.Unmarshal(input, &a) != nil || a.ProjectSlug == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		proj, err := s.d.store.GetProjectBySlug(ctx, a.ProjectSlug)
		if err != nil || proj == nil {
			return
		}
		if _, _, ok := linearWriteTarget(proj); !ok {
			return // not write-back bound → nothing to mirror
		}
		if err := s.d.syncRoadmapToLinear(ctx, proj); err != nil {
			log.Printf("roadmap mirror after save (%s): %v", a.ProjectSlug, err)
		}
	}()
}

// publishCanSequenceAfterRoadmapSave re-evaluates the sequencing enable gate for
// the just-saved project and broadcasts project.updated carrying the fresh
// can_sequence, so an open TUI un-greys the "sequenced" dispatch-mode option the
// moment `hive plan` writes a valid roadmap. The roadmap is a plain file write
// that emits no event of its own, so without this the TUI's cached CanSequence
// stays stale until the operator restarts it. Best-effort: a lookup/gate failure
// just skips the broadcast (the next project.list fetch recomputes it anyway).
func (s *RPCServer) publishCanSequenceAfterRoadmapSave(input json.RawMessage) {
	var a struct {
		ProjectSlug string `json:"project_slug"`
	}
	if json.Unmarshal(input, &a) != nil || a.ProjectSlug == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	proj, err := s.d.store.GetProjectBySlug(ctx, a.ProjectSlug)
	if err != nil || proj == nil {
		return
	}
	canSeq := s.checkEnableGate(ctx, proj) == nil
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventProjectUpdated,
		Data: map[string]any{
			"project_id":   proj.ID,
			"slug":         proj.Slug,
			"can_sequence": canSeq,
		},
	})
}

// isAutoConfirmed reports whether the named tool is in the auto-confirm list,
// meaning the confirm gate is skipped for it.
func isAutoConfirmed(name string, list []string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// streamChat handles the chat.send streaming RPC. It establishes (or
// resumes the session row for) a chat session, persists the user message,
// builds the agent, runs ONE turn, streams frames as line-delimited JSON,
// and persists assistant/tool messages best-effort. A one-shot fresh
// conversation is used in 6.1a; multi-turn history reload lands in 6.1b.
func (s *RPCServer) streamChat(ctx context.Context, conn net.Conn, reqID string, params json.RawMessage) {
	var in struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		// Kind + ProjectSlug are honored ONLY when this call creates a
		// new session (SessionID == ""). Resumed sessions read these
		// fields from the persisted row, ignoring the request values to
		// avoid mid-session kind changes silently re-routing the agent.
		Kind        string `json:"kind,omitempty"`
		ProjectSlug string `json:"project_slug,omitempty"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		chatWriteFrame(conn, reqID, chat.Frame{Kind: "error", Text: "invalid params: " + err.Error()})
		return
	}
	if in.Message == "" {
		chatWriteFrame(conn, reqID, chat.Frame{Kind: "error", Text: "message is required"})
		return
	}

	sessionID := in.SessionID
	newSession := sessionID == ""
	if sessionID == "" {
		// Validate Kind upfront so a typo ("plann") doesn't silently fall
		// through to the default chat agent (since Kind would be persisted
		// as the typo and never match KindPlan). Empty is allowed — the
		// store layer defaults it to KindChat.
		if in.Kind != "" && in.Kind != store.KindChat && in.Kind != store.KindPlan {
			chatWriteFrame(conn, reqID, chat.Frame{Kind: "error", Text: fmt.Sprintf("chat.send: unknown kind %q", in.Kind)})
			return
		}
		// Validate planner sessions reference a real project before we
		// persist the row, so a typo doesn't leave a dangling planner
		// session that streamChat can never run.
		if in.Kind == store.KindPlan && in.ProjectSlug == "" {
			chatWriteFrame(conn, reqID, chat.Frame{Kind: "error", Text: "planner session requires project_slug"})
			return
		}
		sessionID = newID("chat")
		// Planner sessions always carry a project_slug (validated above)
		// and the seed message is the literal "begin" sentinel — useless
		// as a session name. Use "<Project Name> Planning" instead so
		// the picker + history list scan cleanly. Fall back to the
		// derived-from-message name only if the project lookup fails.
		sessionName := deriveSessionName(in.Message)
		if in.Kind == store.KindPlan && in.ProjectSlug != "" {
			if proj, perr := s.d.store.GetProjectBySlug(ctx, in.ProjectSlug); perr == nil && proj != nil && proj.Name != "" {
				sessionName = proj.Name + " Planning"
			}
		}
		if err := s.d.store.InsertChatSession(ctx, &store.ChatSession{
			ID:          sessionID,
			Surface:     "cli", // existing default — TUI doesn't currently distinguish itself in the RPC, that's fine
			Kind:        in.Kind,
			Name:        sessionName,
			Provider:    s.d.chatProvider(),
			ProjectSlug: in.ProjectSlug,
		}); err != nil {
			chatWriteFrame(conn, reqID, chat.Frame{Kind: "error", Text: "create session: " + err.Error()})
			return
		}
	}

	// Emit the session id as the FIRST frame so a REPL client can capture it
	// and reuse it for subsequent turns (continuing the same session).
	chatWriteFrame(conn, reqID, chat.Frame{Kind: "session", Text: sessionID})

	// session_info carries session metadata for clients that want a
	// header bar (e.g. the TUI Chat tab); CLI clients fall through their
	// frame switch without handling it (silent skip).
	if sess, err := s.d.store.GetChatSession(ctx, sessionID); err == nil && sess != nil {
		infoBody, _ := json.Marshal(map[string]any{
			"name":     sess.Name,
			"provider": sess.Provider,
		})
		chatWriteFrame(conn, reqID, chat.Frame{
			Kind:   "session_info",
			Result: string(infoBody),
		})
	}

	s.d.RegisterChatStream(sessionID, conn)
	defer s.d.UnregisterChatStream(sessionID)

	// Build the conversation. For the SDK ("api") provider, rehydrate prior
	// user+assistant text turns so the model has multi-turn context. The CC
	// ("claude-code") provider gets continuity from `--resume` (keyed on
	// conv.SessionID), so we must NOT rehydrate Messages there or context
	// would be double-counted.
	conv := &chat.Conversation{SessionID: sessionID}
	if s.d.chatProvider() != "claude-code" {
		if prior, err := s.d.store.GetChatMessages(ctx, sessionID); err == nil {
			conv.Messages = rehydrateMessages(prior)
		}
	}

	// For brand-new plan sessions, prepend the project's existing open work so
	// the planner can annotate each roadmap phase with tasks that already exist.
	// Only the first turn gets the seed; resumed turns skip this entirely.
	if newSession {
		if seed := s.d.existingWorkSeed(ctx, in.Kind, in.ProjectSlug); seed != "" {
			in.Message = seed + "\n" + in.Message
		}
	}

	// Persist the user message (best-effort: a persistence failure must not
	// abort the turn). Done AFTER rehydration so the current message isn't
	// duplicated into the replayed history (SDKAgent.Send appends it itself).
	_ = s.d.store.AppendChatMessage(ctx, &store.ChatMessage{
		ID: newID("msg"), SessionID: sessionID, Role: "user", Content: in.Message,
	})

	agent := s.d.chatAgent

	// Phase 8.A T6: planner-kind sessions get a dedicated agent built with
	// the planner registry + planner system prompt + ForceSonnet router,
	// scoped to the bound project's repo_path. We re-read the session
	// (rather than trust the request `kind` field) so that resumed
	// sessions route by their persisted kind, not whatever the client
	// happened to send this turn.
	if sess, err := s.d.store.GetChatSession(ctx, sessionID); err == nil && sess != nil && sess.Kind == store.KindPlan {
		plannerAgent, perr := s.d.buildPlannerAgentForSession(ctx, sess)
		if perr != nil {
			chatWriteFrame(conn, reqID, chat.Frame{Kind: "error", Text: "planner agent: " + perr.Error()})
			return
		}
		agent = plannerAgent
	}

	if agent == nil {
		chatWriteFrame(conn, reqID, chat.Frame{Kind: "error", Text: "chat agent not configured"})
		return
	}

	runChatTurn(ctx, agent, conn, reqID, s.d.store, conv, sessionID, in.Message)
}

// existingWorkSeed returns an "EXISTING WORK" block for the first turn of a
// new plan-kind session, so the planner can annotate each roadmap phase with
// work that already exists. Returns "" for non-plan kinds, missing projects,
// or when there is no existing work.
func (d *Daemon) existingWorkSeed(ctx context.Context, kind string, slug string) string {
	if kind != store.KindPlan || slug == "" {
		return ""
	}
	proj, err := d.store.GetProjectBySlug(ctx, slug)
	if err != nil || proj == nil {
		return ""
	}
	items, err := d.gatherExistingWork(ctx, proj, "")
	if err != nil || len(items) == 0 {
		return ""
	}
	return "EXISTING WORK (open tasks + un-pulled Linear issues for this project):\n" + formatExistingWorkBlock(items)
}

// buildPlannerAgentForSession resolves the session's bound project to a
// repo_path, then calls the injected plannerAgentFor factory. Returns a
// clear error if the project is missing, has no repo_path, or the factory
// hasn't been wired at the composition root.
func (d *Daemon) buildPlannerAgentForSession(ctx context.Context, sess *store.ChatSession) (chat.Agent, error) {
	if d.plannerAgentFor == nil {
		return nil, fmt.Errorf("planner agent factory not registered (composition root must call SetPlannerAgentFor)")
	}
	if sess.ProjectSlug == "" {
		return nil, fmt.Errorf("session has no project_slug; planner sessions must bind a project")
	}
	proj, err := d.store.GetProjectBySlug(ctx, sess.ProjectSlug)
	if err != nil {
		return nil, fmt.Errorf("project %q: %w", sess.ProjectSlug, err)
	}
	if proj == nil || proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, fmt.Errorf("project %q has no repo_path", sess.ProjectSlug)
	}
	return d.plannerAgentFor(sess.ProjectSlug, *proj.RepoPath)
}

// rehydrateMessages maps stored chat messages to anth.MessageParam turns for
// SDK multi-turn context. To keep replay robust we map only user+assistant
// TEXT turns (tool_call/tool_result blocks would require faithful tool_use_id
// pairing — skipped here; the assistant's text summary of any tool work is
// still replayed, which preserves the conversational thread). Empty-content
// rows are skipped.
func rehydrateMessages(msgs []store.ChatMessage) []anth.MessageParam {
	out := make([]anth.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		if m.Content == "" {
			continue
		}
		switch m.Role {
		case "user":
			out = append(out, anth.NewUserMessage(anth.NewTextBlock(m.Content)))
		case "assistant":
			out = append(out, anth.NewAssistantMessage(anth.NewTextBlock(m.Content)))
		}
	}
	return out
}

// runChatTurn runs the agent for one user message against the supplied
// conversation (fresh for a new session, or rehydrated with prior turns for
// SDK multi-turn), streaming frames over conn and persisting assistant/tool
// messages. It is split out from streamChat so it can be tested offline with a
// fake-runner-backed agent (no API key / network required).
func runChatTurn(ctx context.Context, agent chat.Agent, conn net.Conn, reqID string, st *store.Store, conv *chat.Conversation, sessionID, message string) {
	if conv == nil {
		conv = &chat.Conversation{SessionID: sessionID}
	}

	sawErrFrame := false
	emit := func(f chat.Frame) {
		chatWriteFrame(conn, reqID, f)
		switch f.Kind {
		case "text":
			_ = st.AppendChatMessage(ctx, &store.ChatMessage{
				ID: newID("msg"), SessionID: sessionID, Role: "assistant", Content: f.Text,
				CostUSD: f.CostUSD,
			})
		case "tool_result":
			_ = st.AppendChatMessage(ctx, &store.ChatMessage{
				ID: newID("msg"), SessionID: sessionID, Role: "tool", Content: f.Result,
				ToolResults: f.Result,
			})
		case "turn_done":
			_ = st.EndChatSession(ctx, sessionID, f.CostUSD)
		case "error":
			sawErrFrame = true
			_ = st.AppendChatMessage(ctx, &store.ChatMessage{
				ID: newID("msg"), SessionID: sessionID, Role: "error", Content: f.Text,
			})
		}
	}

	if err := agent.Send(ctx, conv, message, emit); err != nil {
		if !sawErrFrame {
			// Send returned err without emitting an error frame; persist a
			// synthetic one so the turn isn't an orphan in chat_messages.
			content := "agent error (no detail): " + err.Error()
			const maxOrphanContent = 1024
			if len(content) > maxOrphanContent {
				content = content[:maxOrphanContent-3] + "..."
			}
			_ = st.AppendChatMessage(ctx, &store.ChatMessage{
				ID: newID("msg"), SessionID: sessionID, Role: "error",
				Content: content,
			})
		}
		// The session is left open (no EndChatSession) so a retry can continue it.
		return
	}
}

// chatWriteFrame writes one frame as a line-delimited JSON response
// envelope carrying the chat Frame as its result (mirroring streamEvents'
// envelope-per-line shape). reqID lets the client correlate the stream.
func chatWriteFrame(conn net.Conn, reqID string, f chat.Frame) {
	resp := rpc.Response[chat.Frame]{ID: reqID, Result: &f}
	payload, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(payload, '\n'))
}

// handleHivePredict implements the hive_predict chat tool. Looks up
// the task + project, picks a repoRoot (the project's main repo) +
// bundleDir (a tempdir we clean up at return), calls the predictor,
// and returns a compact JSON summary. When refresh=true AND a
// non-terminal run exists for the task, persists the full
// predictor.Result JSON to the run via PutPredictionJSON.
//
// Errors are returned as tool-level errors (IsError=true with a JSON
// {"error":...} body) so the model can react rather than seeing a
// protocol failure.
func (d *Daemon) handleHivePredict(ctx context.Context, taskID string, refresh bool) chat.ToolResult {
	if taskID == "" {
		return toolErr("task_id required")
	}
	task, err := d.store.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return toolErr("task not found")
		}
		return toolErr("get task: " + err.Error())
	}
	proj, err := d.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return toolErr("get project: " + err.Error())
	}
	if proj == nil || proj.RepoPath == nil || *proj.RepoPath == "" {
		return toolErr("project repo_path missing")
	}

	bundleDir, cleanup, err := makeTempBundleDir()
	if err != nil {
		return toolErr("create bundle dir: " + err.Error())
	}
	defer cleanup()

	if d.predictor == nil {
		return toolErr("predictor not configured")
	}
	result, predErr := d.predictor.Predict(ctx, task.Body, *proj.RepoPath, bundleDir)
	if predErr != nil {
		return toolErr("predict failed: " + predErr.Error())
	}

	body := compactPrediction(result)

	if refresh {
		run, err := d.store.LatestNonTerminalRunForTask(ctx, taskID)
		if err != nil {
			return toolErr("look up run: " + err.Error())
		}
		if run == nil || result == nil {
			body["refresh_noop"] = true
		} else {
			payload, _ := json.Marshal(result)
			if err := d.store.PutPredictionJSON(ctx, run.ID, payload); err != nil {
				return toolErr("persist prediction: " + err.Error())
			}
			body["persisted_to_run"] = run.ID
		}
	}

	raw, _ := json.Marshal(body)
	return chat.ToolResult{Content: string(raw), IsError: false}
}

// handleHiveResume implements the hive_resume chat tool. Delegates to
// Scheduler.Resume for the eligibility checks + launch. On any error
// returns a tool-level error so the model can recover gracefully; on
// success returns {"resumed":true,"run_id":<id>}.
func (d *Daemon) handleHiveResume(ctx context.Context, runID string) chat.ToolResult {
	if runID == "" {
		return toolErr("run_id required")
	}
	if err := d.scheduler.Resume(ctx, runID); err != nil {
		return toolErr(err.Error())
	}
	raw, _ := json.Marshal(map[string]any{"resumed": true, "run_id": runID})
	return chat.ToolResult{Content: string(raw), IsError: false}
}

// toolErr builds a {"error":"..."} chat.ToolResult with IsError=true.
func toolErr(msg string) chat.ToolResult {
	raw, _ := json.Marshal(map[string]string{"error": msg})
	return chat.ToolResult{Content: string(raw), IsError: true}
}

// makeTempBundleDir creates a per-call scratch dir for the predictor
// to write prefetch.md into. Returned cleanup removes the dir; safe to
// defer regardless of error path.
func makeTempBundleDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "hive-predict-*")
	if err != nil {
		return "", func() {}, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// compactPrediction reduces a predictor.Result to the JSON-able summary
// the chat model wants when calling hive_predict. Pure: no I/O, no
// daemon state. Symbol identifiers come from Capsule.Target.
//
// nil input produces a coherent empty summary (graceful-degrade path
// from Predictor.Predict).
func compactPrediction(r *predictor.Result) map[string]any {
	if r == nil {
		return map[string]any{
			"candidate_files":        []string{},
			"candidate_count":        0,
			"inline_capsule_symbols": []string{},
			"overflow_symbols":       []string{},
			"metrics": map[string]any{
				"haiku_latency_ms": int64(0),
				"fetch_latency_ms": int64(0),
				"inline_count":     0,
				"overflow_count":   0,
				"truncated":        false,
			},
		}
	}
	inlineSyms := make([]string, 0, len(r.InlineCapsules))
	for _, c := range r.InlineCapsules {
		inlineSyms = append(inlineSyms, c.Target)
	}
	overflowSyms := make([]string, 0, len(r.Overflow))
	for _, c := range r.Overflow {
		overflowSyms = append(overflowSyms, c.Symbol)
	}
	files := r.Files
	if files == nil {
		files = []string{}
	}
	return map[string]any{
		"candidate_files":        files,
		"candidate_count":        len(files),
		"inline_capsule_symbols": inlineSyms,
		"overflow_symbols":       overflowSyms,
		"metrics": map[string]any{
			"haiku_latency_ms": r.Metrics.HaikuLatency.Milliseconds(),
			"fetch_latency_ms": r.Metrics.FetchLatency.Milliseconds(),
			"inline_count":     r.Metrics.InlineCount,
			"overflow_count":   r.Metrics.OverflowCount,
			"truncated":        r.Metrics.Truncated,
		},
	}
}
