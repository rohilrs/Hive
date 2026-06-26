package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/approval"
	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/internal/mcphttp"
	"github.com/rohilrs/Hive/internal/verdict"
)

func TestBuildChatRouteDispatchesToRegistry(t *testing.T) {
	reg := chat.NewRegistry()
	reg.Register(chat.Tool{
		Def: anthropic.ToolDef{Name: "hive_status", Description: "status"},
		Handler: func(_ context.Context, _ json.RawMessage) chat.ToolResult {
			return chat.ToolResult{Content: `{"ok":true}`}
		},
	})
	route := buildChatRoute(reg, nil, nil)
	content, _, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "chat", SessionID: "s1"},
		"hive_status",
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if !strings.Contains(content, `"ok":true`) {
		t.Errorf("content=%q", content)
	}
}

func TestBuildChatRouteUnknownToolErrors(t *testing.T) {
	reg := chat.NewRegistry()
	route := buildChatRoute(reg, nil, nil)
	_, _, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "chat", SessionID: "s1"},
		"hive_nope",
		json.RawMessage(`{}`))
	if err == nil {
		t.Errorf("expected error for unknown tool")
	}
}

func TestBuildChatRouteAdvertisesToolSpecs(t *testing.T) {
	reg := chat.NewRegistry()
	reg.Register(chat.Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_list_tasks",
			Description: "list tasks",
			InputSchema: map[string]any{"type": "object"},
		},
		Handler: func(_ context.Context, _ json.RawMessage) chat.ToolResult {
			return chat.ToolResult{Content: `[]`}
		},
	})
	route := buildChatRoute(reg, nil, nil)
	if len(route.Tools) != 1 {
		t.Fatalf("expected 1 tool spec, got %d", len(route.Tools))
	}
	if route.Tools[0].Name != "hive_list_tasks" {
		t.Errorf("tool name=%q", route.Tools[0].Name)
	}
	if len(route.Tools[0].InputSchema) == 0 {
		t.Errorf("InputSchema is empty")
	}
}

// TestBuildChatRoutePlannerSessionSeesPlannerTools pins the Phase 8.A T8
// fix: when sessionKindLookup returns kind="plan", the route's ToolsFor
// returns the planner registry's tools (not the chat registry's tools) —
// so the model sees hive_list_specs / hive_read_doc / hive_save_roadmap /
// hive_save_spec on tools/list, and tools/call dispatches them through
// the planner registry's handlers.
func TestBuildChatRoutePlannerSessionSeesPlannerTools(t *testing.T) {
	chatReg := chat.NewRegistry()
	chatReg.Register(chat.Tool{
		Def:     anthropic.ToolDef{Name: "hive_list_tasks", InputSchema: map[string]any{"type": "object"}},
		Handler: func(_ context.Context, _ json.RawMessage) chat.ToolResult { return chat.ToolResult{Content: `[]`} },
	})

	lookup := func(_ context.Context, sessionID string) (string, string, string, error) {
		if sessionID == "plan-sess" {
			return "plan", "my-app", "/tmp/repo", nil
		}
		return "chat", "", "", nil
	}
	plannerCallCount := 0
	factory := func(slug, cwd string) (*chat.Registry, error) {
		plannerCallCount++
		return chat.NewPlannerRegistry(cwd, chatReg, "", nil), nil
	}

	route := buildChatRoute(chatReg, lookup, factory)

	// Planner-kind session: tools/list returns planner + inherited chat tools.
	planTools := route.ToolsFor(mcphttp.RouteContext{Kind: "chat", SessionID: "plan-sess"})
	names := toolNames(planTools)
	for _, want := range []string{"hive_list_specs", "hive_read_doc", "hive_save_roadmap", "hive_save_spec"} {
		if !containsName(names, want) {
			t.Errorf("planner ToolsFor missing %q; got %v", want, names)
		}
	}

	// Chat-kind session: tools/list returns only chat tools.
	chatTools := route.ToolsFor(mcphttp.RouteContext{Kind: "chat", SessionID: "chat-sess"})
	chatNames := toolNames(chatTools)
	if containsName(chatNames, "hive_list_specs") {
		t.Errorf("chat-kind session unexpectedly saw planner tool; got %v", chatNames)
	}
	if !containsName(chatNames, "hive_list_tasks") {
		t.Errorf("chat-kind session missing chat tool; got %v", chatNames)
	}

	// Handler dispatches planner tool on planner session.
	content, isErr, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "chat", SessionID: "plan-sess"},
		"hive_list_specs",
		json.RawMessage(`{"project_slug":"my-app"}`))
	if err != nil {
		t.Fatalf("planner-tool Handler err: %v", err)
	}
	// hive_list_specs returns either a JSON list or a tool-level error
	// content (the filesystem at /tmp/repo doesn't exist) — both are
	// fine as long as we didn't get an unknown-tool error.
	_ = content
	_ = isErr

	// Planner factory is cached: a second tools/list for the same session
	// must not invoke the factory again.
	_ = route.ToolsFor(mcphttp.RouteContext{Kind: "chat", SessionID: "plan-sess"})
	if plannerCallCount != 1 {
		t.Errorf("planner factory called %d times for same session, want 1 (caching broken)", plannerCallCount)
	}
}

// TestBuildChatRouteLookupErrorFallsBackToChat pins that a sessionKindLookup
// error (e.g. session row missing) falls back to default chat tools — a
// missing row must never break default chat.
func TestBuildChatRouteLookupErrorFallsBackToChat(t *testing.T) {
	chatReg := chat.NewRegistry()
	chatReg.Register(chat.Tool{
		Def:     anthropic.ToolDef{Name: "hive_status", InputSchema: map[string]any{"type": "object"}},
		Handler: func(_ context.Context, _ json.RawMessage) chat.ToolResult { return chat.ToolResult{Content: `{}`} },
	})

	lookup := func(_ context.Context, _ string) (string, string, string, error) {
		return "", "", "", fmt.Errorf("session not found")
	}
	factory := func(_, _ string) (*chat.Registry, error) {
		t.Fatal("planner factory should not be called when lookup errors")
		return nil, nil
	}
	route := buildChatRoute(chatReg, lookup, factory)
	tools := route.ToolsFor(mcphttp.RouteContext{Kind: "chat", SessionID: "anything"})
	if !containsName(toolNames(tools), "hive_status") {
		t.Errorf("expected chat fallback, got tools=%v", toolNames(tools))
	}
}

// TestBuildChatRouteNilClosuresFallBackToChat pins the back-compat path:
// nil lookup + nil factory means the route degenerates to chat-only,
// just like before T8.
func TestBuildChatRouteNilClosuresFallBackToChat(t *testing.T) {
	chatReg := chat.NewRegistry()
	chatReg.Register(chat.Tool{
		Def:     anthropic.ToolDef{Name: "hive_status", InputSchema: map[string]any{"type": "object"}},
		Handler: func(_ context.Context, _ json.RawMessage) chat.ToolResult { return chat.ToolResult{Content: `{}`} },
	})
	route := buildChatRoute(chatReg, nil, nil)
	tools := route.ToolsFor(mcphttp.RouteContext{Kind: "chat", SessionID: "x"})
	if !containsName(toolNames(tools), "hive_status") {
		t.Errorf("nil-closures fallback failed: %v", toolNames(tools))
	}
}

func toolNames(specs []mcphttp.ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

func containsName(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestBuildStageRouteRoutesVerdict(t *testing.T) {
	reg := verdict.NewStageRegistry()
	sockPath := filepath.Join(t.TempDir(), "v.sock")
	l, err := verdict.Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	reg.Register("run-7", "review", l)

	route := buildStageRoute(reg, nil)
	content, isErr, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "stage", RunID: "run-7", Stage: "review"},
		"hive_submit_review_verdict",
		json.RawMessage(`{"run_id":"run-7","stage":"review","verdict":"APPROVE","confidence":95}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if isErr {
		t.Errorf("isError=true, content=%q", content)
	}
	if !strings.Contains(content, `"ok":true`) {
		t.Errorf("content=%q", content)
	}
}

func TestBuildStageRouteUnregisteredStageReturnsToolError(t *testing.T) {
	// A submit against a stage with no registered Listener returns a
	// tool-level error (isError=true) rather than a protocol-level
	// error — keeps the model's recovery path intact.
	reg := verdict.NewStageRegistry()
	route := buildStageRoute(reg, nil)
	content, isErr, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "stage", RunID: "ghost", Stage: "implement"},
		"hive_submit_review_verdict",
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("expected nil err (tool-level error, not protocol), got %v", err)
	}
	if !isErr {
		t.Errorf("expected isError=true on stage-not-active")
	}
	if !strings.Contains(content, "stage not active") {
		t.Errorf("content missing context: %q", content)
	}
}

func TestBuildStageRouteDocSubmitNilErrors(t *testing.T) {
	reg := verdict.NewStageRegistry()
	route := buildStageRoute(reg, nil)
	_, _, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "stage", RunID: "run-1", Stage: "review"},
		"hive_submit_documentation",
		json.RawMessage(`{}`))
	if err == nil {
		t.Errorf("expected error when docSubmit is nil")
	}
}

func TestBuildStageRouteDocSubmitCallsHandler(t *testing.T) {
	reg := verdict.NewStageRegistry()
	called := false
	docSubmit := func(_ context.Context, _ json.RawMessage) error {
		called = true
		return nil
	}
	route := buildStageRoute(reg, docSubmit)
	content, isErr, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "stage", RunID: "run-1", Stage: "review"},
		"hive_submit_documentation",
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if isErr {
		t.Errorf("isError=true")
	}
	if !called {
		t.Errorf("docSubmit not called")
	}
	if !strings.Contains(content, `"ok":true`) {
		t.Errorf("content=%q", content)
	}
}

func TestBuildPermRouteAllows(t *testing.T) {
	engine := approval.NewStub() // stub approves all
	route := buildPermRoute(engine)
	content, isErr, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "perm", RunID: "run-3", Stage: "implement"},
		"hive_permission_check",
		json.RawMessage(`{"tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"tc-1"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if isErr {
		t.Errorf("isError=true, content=%q", content)
	}
	if !strings.Contains(content, `"behavior":"allow"`) {
		t.Errorf("expected allow behavior, content=%q", content)
	}
}

func TestBuildPermRouteDenies(t *testing.T) {
	// RealEngine with no rules → fail-closed → deny
	engine := &denyAllEngine{}
	route := buildPermRoute(engine)
	content, isErr, err := route.Handler(context.Background(),
		mcphttp.RouteContext{Kind: "perm", RunID: "run-4", Stage: "implement"},
		"hive_permission_check",
		json.RawMessage(`{"tool_name":"Bash","input":{"command":"rm -rf /"},"tool_use_id":"tc-2"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if isErr {
		t.Errorf("isError=true, content=%q", content)
	}
	if !strings.Contains(content, `"behavior":"deny"`) {
		t.Errorf("expected deny behavior, content=%q", content)
	}
}

// denyAllEngine always returns DecisionDeny for testing.
type denyAllEngine struct{}

func (denyAllEngine) Evaluate(_ context.Context, _ approval.ToolUseRequest) (approval.Decision, error) {
	return approval.Decision{Kind: approval.DecisionDeny, Reason: "test deny"}, nil
}
