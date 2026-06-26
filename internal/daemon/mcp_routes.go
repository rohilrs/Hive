package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/rohilrs/Hive/internal/approval"
	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/internal/mcphttp"
	"github.com/rohilrs/Hive/internal/verdict"
)

// SessionKindLookup is the closure buildChatRoute uses to discover whether a
// chat session is a planner-kind session (and which project it's bound to).
// kind is the ChatSession.Kind ("chat" | "plan"); projectSlug + cwd are
// populated when kind == "plan" so the route can build a planner registry
// scoped to the project's repo. An empty kind (or any error) means the
// caller should fall back to the default chat registry.
type SessionKindLookup func(ctx context.Context, sessionID string) (kind, projectSlug, cwd string, err error)

// PlannerRegistryFor builds a planner chat.Registry for a given project. The
// chat route invokes this when SessionKindLookup returns kind=="plan" so the
// planner tools (hive_list_specs, hive_read_doc, hive_save_roadmap,
// hive_save_spec) are advertised + dispatched for that session.
type PlannerRegistryFor func(slug, cwd string) (*chat.Registry, error)

// buildChatRoute adapts the in-process chat.Registry to the mcphttp.Route
// shape. Tool names registered with the chat agent become advertised
// tools; tools/call dispatches via the same chat.Tool.Handler the SDK
// agent uses.
//
// Per-session advertisement (Phase 8.A T8): when sessionKindLookup +
// plannerRegistryFor are non-nil, Route.ToolsFor consults them on each
// tools/list and tools/call: planner-kind sessions see the planner
// registry's tools, every other session sees the default chat tools. The
// static Route.Tools fallback is the chat registry's tools so a request
// for an unbound or unknown session still gets a sane tools/list.
//
// Snapshot semantics: Route.Tools (and the per-call dynamic tools list)
// are computed from reg.Defs() at build/lookup time. Route.Handler does a
// live reg.Get(toolName) lookup against the per-session registry, so
// tools added to a registry after buildChatRoute remain dispatchable.
//
// sessionKindLookup or plannerRegistryFor may be nil — in which case the
// route degenerates to static chat-only behavior (pre-8.A semantics).
// This is useful for tests that don't care about per-session routing.
func buildChatRoute(
	chatReg *chat.Registry,
	sessionKindLookup SessionKindLookup,
	plannerRegistryFor PlannerRegistryFor,
) mcphttp.Route {
	chatTools := registryToToolSpecs(chatReg)

	// Cache planner registries by session ID. Chat sessions don't change
	// kind after creation and planner sessions are short-lived, so a
	// permanent per-session entry is fine — no eviction needed.
	var cache sync.Map // sessionID -> *chat.Registry

	// resolveRegistry picks the live chat.Registry for this session: the
	// planner registry for kind=="plan", otherwise the default chat
	// registry. Falls back to chatReg on any lookup error so a missing
	// session row never breaks default chat.
	resolveRegistry := func(ctx context.Context, sessionID string) *chat.Registry {
		if sessionID == "" || sessionKindLookup == nil || plannerRegistryFor == nil {
			return chatReg
		}
		if cached, ok := cache.Load(sessionID); ok {
			if reg, ok := cached.(*chat.Registry); ok && reg != nil {
				return reg
			}
		}
		kind, slug, cwd, err := sessionKindLookup(ctx, sessionID)
		if err != nil || kind != "plan" {
			return chatReg
		}
		planReg, perr := plannerRegistryFor(slug, cwd)
		if perr != nil || planReg == nil {
			return chatReg
		}
		cache.Store(sessionID, planReg)
		return planReg
	}

	return mcphttp.Route{
		Tools: chatTools,
		ToolsFor: func(rctx mcphttp.RouteContext) []mcphttp.ToolSpec {
			reg := resolveRegistry(context.Background(), rctx.SessionID)
			if reg == chatReg {
				return chatTools
			}
			return registryToToolSpecs(reg)
		},
		Handler: func(ctx context.Context, rctx mcphttp.RouteContext, toolName string, input json.RawMessage) (string, bool, error) {
			reg := resolveRegistry(ctx, rctx.SessionID)
			t, ok := reg.Get(toolName)
			if !ok {
				return "", false, fmt.Errorf("unknown chat tool: %s", toolName)
			}
			r := t.Handler(ctx, input)
			return r.Content, r.IsError, nil
		},
	}
}

// registryToToolSpecs converts a chat.Registry's Defs into mcphttp.ToolSpec
// shape (the wire form advertised by tools/list).
func registryToToolSpecs(reg *chat.Registry) []mcphttp.ToolSpec {
	if reg == nil {
		return nil
	}
	defs := reg.Defs()
	out := make([]mcphttp.ToolSpec, 0, len(defs))
	for _, d := range defs {
		schema, _ := json.Marshal(d.InputSchema)
		out = append(out, mcphttp.ToolSpec{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: schema,
		})
	}
	return out
}

// buildStageRoute adapts the per-stage verdict.Listener (looked up via
// the stage registry) and the documenter-submit RPC into the stage Route.
// Tools advertised: hive_submit_review_verdict, hive_submit_documentation.
//
// docSubmit may be nil if the daemon doesn't have a documentation submit
// path wired — the route will return an error for that tool name.
func buildStageRoute(reg *verdict.StageRegistry, docSubmit func(context.Context, json.RawMessage) error) mcphttp.Route {
	return mcphttp.Route{
		Tools: []mcphttp.ToolSpec{
			{
				Name:        "hive_submit_review_verdict",
				Description: "Submit the review verdict for the current stage.",
				InputSchema: json.RawMessage(`{"type":"object","required":["run_id","stage","verdict","confidence"],"properties":{"run_id":{"type":"string"},"stage":{"type":"string"},"verdict":{"type":"string","enum":["APPROVE","CHANGES_REQUESTED"]},"confidence":{"type":"integer"},"file_refs":{"type":"array"}}}`),
			},
			{
				Name:        "hive_submit_documentation",
				Description: "Submit a documentation summary for the current run.",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
		Handler: func(ctx context.Context, rctx mcphttp.RouteContext, toolName string, input json.RawMessage) (string, bool, error) {
			switch toolName {
			case "hive_submit_review_verdict":
				l, ok := reg.Get(rctx.RunID, rctx.Stage)
				if !ok {
					// Tool-level error (isError=true) instead of protocol
					// error: the model can see and react to "stage not
					// active" content the same way it sees a verdict
					// rejection (REVIEW_FEEDBACK_MISSING). A -32603
					// would surface as "server broke" with no context.
					content, _ := json.Marshal(map[string]any{
						"ok":    false,
						"error": fmt.Sprintf("stage not active: %s/%s", rctx.RunID, rctx.Stage),
					})
					return string(content), true, nil
				}
				var frame verdict.Frame
				if err := json.Unmarshal(input, &frame); err != nil {
					return "", false, fmt.Errorf("bad frame: %w", err)
				}
				ack, err := l.Submit(frame)
				if err != nil {
					return "", false, err
				}
				out, _ := json.Marshal(ack)
				return string(out), !ack.OK, nil
			case "hive_submit_documentation":
				if docSubmit == nil {
					return "", false, fmt.Errorf("documentation submit not wired")
				}
				if err := docSubmit(ctx, input); err != nil {
					return "", false, err
				}
				return `{"ok":true}`, false, nil
			}
			return "", false, fmt.Errorf("unknown stage tool: %s", toolName)
		},
	}
}

// buildPermRoute adapts approval.Engine.Evaluate into the perm Route.
// One tool: hive_permission_check. Input is the standard --permission-
// prompt-tool input shape: {tool_name, input, tool_use_id}. Returns
// {behavior:"allow",updatedInput} or {behavior:"deny",message}.
//
// Actual approval types (internal/approval/types.go):
//   - ToolUseRequest: RunID, Stage, ToolName, Project, ToolInput map[string]any, Reasoning
//   - Decision: Kind DecisionKind ("approve"|"deny"), Reason, RuleID
//
// The plan assumed "Input"/"Allow" field names; the real names are
// "ToolInput" and "Kind" (DecisionKind). buildPermRoute uses the real
// field names.
func buildPermRoute(engine approval.Engine) mcphttp.Route {
	return mcphttp.Route{
		Tools: []mcphttp.ToolSpec{
			{
				Name:        "hive_permission_check",
				Description: "Authorize a tool use via Hive's approval engine.",
				InputSchema: json.RawMessage(`{"type":"object","required":["tool_name","input","tool_use_id"],"properties":{"tool_name":{"type":"string"},"input":{"type":"object"},"tool_use_id":{"type":"string"}}}`),
			},
		},
		Handler: func(ctx context.Context, rctx mcphttp.RouteContext, _ string, input json.RawMessage) (string, bool, error) {
			// Parse the permission-prompt-tool input shape.
			var p struct {
				ToolName  string         `json:"tool_name"`
				Input     map[string]any `json:"input"`
				ToolUseID string         `json:"tool_use_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", false, fmt.Errorf("bad perm input: %w", err)
			}

			req := approval.ToolUseRequest{
				RunID:     rctx.RunID,
				Stage:     rctx.Stage,
				ToolName:  p.ToolName,
				ToolInput: p.Input,
			}
			decision, err := engine.Evaluate(ctx, req)
			if err != nil {
				return "", false, fmt.Errorf("approval engine: %w", err)
			}

			// Map Decision.Kind ("approve"/"deny") to behavior ("allow"/"deny")
			// per claude's --permission-prompt-tool response shape.
			if decision.Kind == approval.DecisionApprove {
				out, _ := json.Marshal(map[string]any{
					"behavior":     "allow",
					"updatedInput": p.Input,
				})
				return string(out), false, nil
			}
			out, _ := json.Marshal(map[string]any{
				"behavior": "deny",
				"message":  decision.Reason,
			})
			return string(out), false, nil
		},
	}
}
