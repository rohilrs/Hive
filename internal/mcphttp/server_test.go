package mcphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestRoute(toolName, content string) Route {
	return Route{
		Tools: []ToolSpec{{
			Name:        toolName,
			Description: "test tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		Handler: func(_ context.Context, _ RouteContext, _ string, _ json.RawMessage) (string, bool, error) {
			return content, false, nil
		},
	}
}

func postJSON(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func TestServerInitialize(t *testing.T) {
	s := NewServer()
	s.RegisterChat(newTestRoute("hive_status", "{}"))
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	var resp struct {
		Result struct {
			ServerInfo   map[string]string `json:"serverInfo"`
			Capabilities map[string]any    `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.ServerInfo["name"] == "" {
		t.Errorf("missing serverInfo.name: %s", body)
	}
	if _, ok := resp.Result.Capabilities["tools"]; !ok {
		t.Errorf("missing capabilities.tools: %s", body)
	}
}

func TestServerToolsList(t *testing.T) {
	s := NewServer()
	s.RegisterChat(newTestRoute("hive_status", "{}"))
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if !strings.Contains(body, "hive_status") {
		t.Errorf("tools/list missing tool: %s", body)
	}
}

func TestServerToolsCallDispatches(t *testing.T) {
	called := false
	r := Route{
		Tools: []ToolSpec{{Name: "hive_status", InputSchema: json.RawMessage(`{}`)}},
		Handler: func(_ context.Context, rctx RouteContext, _ string, _ json.RawMessage) (string, bool, error) {
			called = true
			if rctx.SessionID != "sess-1" {
				t.Errorf("rctx.SessionID=%q, want sess-1", rctx.SessionID)
			}
			return `{"ok":true}`, false, nil
		},
	}
	s := NewServer()
	s.RegisterChat(r)
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hive_status","arguments":{}}}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if !called {
		t.Errorf("handler not invoked")
	}
	// The handler content is a JSON string value in the MCP content block.
	// The wire format encodes inner quotes as \", so we check for the
	// escaped key fragment that appears verbatim in the response bytes.
	if !strings.Contains(body, `\"ok\":true`) {
		t.Errorf("response missing handler content: %s", body)
	}
}

func TestServerToolsCallUnknownTool(t *testing.T) {
	s := NewServer()
	s.RegisterChat(newTestRoute("hive_status", "{}"))
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if !strings.Contains(body, `"code":-32602`) {
		t.Errorf("expected invalid-params error code: %s", body)
	}
}

func TestServerUnknownMethod(t *testing.T) {
	s := NewServer()
	s.RegisterChat(newTestRoute("hive_status", "{}"))
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":5,"method":"some/other","params":{}}`)
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if !strings.Contains(body, `"code":-32601`) {
		t.Errorf("expected method-not-found error: %s", body)
	}
}

func TestServerPromptsAndResourcesReturnEmpty(t *testing.T) {
	s := NewServer()
	s.RegisterChat(newTestRoute("hive_status", "{}"))
	ts := httptest.NewServer(s)
	defer ts.Close()

	for _, m := range []string{"prompts/list", "resources/list"} {
		req := `{"jsonrpc":"2.0","id":6,"method":"` + m + `","params":{}}`
		code, body := postJSON(t, ts, "/mcp/chat/sess-1", req)
		if code != 200 {
			t.Fatalf("%s: status=%d", m, code)
		}
		if strings.Contains(body, "error") {
			t.Errorf("%s should not error: %s", m, body)
		}
	}
}

func TestServerUnknownRouteReturns404(t *testing.T) {
	s := NewServer()
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, _ := postJSON(t, ts, "/mcp/chat/sess-1", `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}`)
	if code != 404 {
		t.Errorf("status=%d, want 404 (no route registered)", code)
	}
}

func TestServerNotificationsReturn202(t *testing.T) {
	// JSON-RPC 2.0 §4.1: notifications (no id) get HTTP 202 Accepted
	// with empty body, NOT a -32601 error response. MCP clients
	// send notifications/initialized after initialize; replying
	// would break the handshake.
	s := NewServer()
	s.RegisterChat(newTestRoute("hive_status", "{}"))
	ts := httptest.NewServer(s)
	defer ts.Close()

	// No "id" field → notification per JSON-RPC §4.1.
	body := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`
	resp, err := http.Post(ts.URL+"/mcp/chat/sess-1", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status=%d, want 202 Accepted", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("notification reply must be empty body, got %q", buf.String())
	}
}

func TestServerKnownMethodWithIDStillResponds(t *testing.T) {
	// Sanity: a request WITH id still gets a normal JSON-RPC response
	// (the notification short-circuit must not swallow regular calls).
	s := NewServer()
	s.RegisterChat(newTestRoute("hive_status", "{}"))
	ts := httptest.NewServer(s)
	defer ts.Close()
	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":42,"method":"tools/list","params":{}}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"id":42`) {
		t.Errorf("response missing id echo: %s", body)
	}
}

func TestServerStageRouteParamsReachHandler(t *testing.T) {
	captured := RouteContext{}
	r := Route{
		Tools: []ToolSpec{{Name: "hive_submit_review_verdict", InputSchema: json.RawMessage(`{}`)}},
		Handler: func(_ context.Context, rctx RouteContext, _ string, _ json.RawMessage) (string, bool, error) {
			captured = rctx
			return `{}`, false, nil
		},
	}
	s := NewServer()
	s.RegisterStage(r)
	ts := httptest.NewServer(s)
	defer ts.Close()

	postJSON(t, ts, "/mcp/stage/run-99/review",
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"hive_submit_review_verdict","arguments":{}}}`)
	if captured.Kind != "stage" || captured.RunID != "run-99" || captured.Stage != "review" {
		t.Errorf("captured rctx=%+v", captured)
	}
}

func TestServerHealthRouteReturns200(t *testing.T) {
	srv := NewServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "ok" {
		t.Errorf("body=%q, want \"ok\"", string(body))
	}
}

// TestServerToolsListUsesToolsForWhenSet pins that Route.ToolsFor takes
// precedence over the static Route.Tools list for tools/list responses —
// this is the Phase 8.A T8 mechanism letting the chat route advertise
// planner tools for planner-kind sessions.
func TestServerToolsListUsesToolsForWhenSet(t *testing.T) {
	staticTool := ToolSpec{Name: "static_tool", InputSchema: json.RawMessage(`{}`)}
	dynamicTool := ToolSpec{Name: "dynamic_tool", InputSchema: json.RawMessage(`{}`)}
	r := Route{
		Tools: []ToolSpec{staticTool},
		ToolsFor: func(_ RouteContext) []ToolSpec {
			return []ToolSpec{dynamicTool}
		},
		Handler: func(_ context.Context, _ RouteContext, _ string, _ json.RawMessage) (string, bool, error) {
			return `{}`, false, nil
		},
	}
	s := NewServer()
	s.RegisterChat(r)
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if !strings.Contains(body, "dynamic_tool") {
		t.Errorf("tools/list missing dynamic tool: %s", body)
	}
	if strings.Contains(body, "static_tool") {
		t.Errorf("tools/list returned static tool but ToolsFor was set: %s", body)
	}
}

// TestServerToolsCallKnownCheckUsesToolsFor pins that the tools/call
// known-name validation also consults ToolsFor — without this, a planner
// tool whose name is in ToolsFor but NOT in the static Tools list would
// be rejected with "unknown tool" before reaching the handler.
func TestServerToolsCallKnownCheckUsesToolsFor(t *testing.T) {
	called := false
	r := Route{
		Tools: []ToolSpec{{Name: "static_tool", InputSchema: json.RawMessage(`{}`)}},
		ToolsFor: func(_ RouteContext) []ToolSpec {
			return []ToolSpec{{Name: "planner_tool", InputSchema: json.RawMessage(`{}`)}}
		},
		Handler: func(_ context.Context, _ RouteContext, name string, _ json.RawMessage) (string, bool, error) {
			called = true
			if name != "planner_tool" {
				t.Errorf("handler got tool name %q, want planner_tool", name)
			}
			return `{"ok":true}`, false, nil
		},
	}
	s := NewServer()
	s.RegisterChat(r)
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"planner_tool","arguments":{}}}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if !called {
		t.Errorf("handler not invoked; body=%s", body)
	}
	// Conversely, a tool that's in the static Tools list but NOT in
	// ToolsFor should be rejected.
	code, body = postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"static_tool","arguments":{}}}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"code":-32602`) {
		t.Errorf("expected unknown-tool error for static_tool when ToolsFor is set: %s", body)
	}
}

// TestServerToolsListFallsBackToStaticTools pins that ToolsFor=nil still
// uses the static Tools list — the existing stage + perm routes rely on
// this behavior.
func TestServerToolsListFallsBackToStaticTools(t *testing.T) {
	s := NewServer()
	s.RegisterChat(newTestRoute("static_only_tool", "{}"))
	ts := httptest.NewServer(s)
	defer ts.Close()

	code, body := postJSON(t, ts, "/mcp/chat/sess-1",
		`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if !strings.Contains(body, "static_only_tool") {
		t.Errorf("static fallback failed: %s", body)
	}
}

// TestServerToolsForReceivesRouteContext pins that ToolsFor receives the
// RouteContext (so the chat route can switch on rctx.SessionID).
func TestServerToolsForReceivesRouteContext(t *testing.T) {
	gotCtx := RouteContext{}
	r := Route{
		ToolsFor: func(rctx RouteContext) []ToolSpec {
			gotCtx = rctx
			return []ToolSpec{{Name: "ctx_tool", InputSchema: json.RawMessage(`{}`)}}
		},
		Handler: func(_ context.Context, _ RouteContext, _ string, _ json.RawMessage) (string, bool, error) {
			return `{}`, false, nil
		},
	}
	s := NewServer()
	s.RegisterChat(r)
	ts := httptest.NewServer(s)
	defer ts.Close()

	postJSON(t, ts, "/mcp/chat/abc-123",
		`{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}`)
	if gotCtx.Kind != "chat" || gotCtx.SessionID != "abc-123" {
		t.Errorf("ToolsFor got rctx=%+v", gotCtx)
	}
}

func TestServerHealthRouteRejectsPost(t *testing.T) {
	// /health is GET-only; POST should NOT silently 200 (would confuse
	// doctor's probe semantics — POST to /health hits the JSON-RPC
	// path which would 404 the route or fail to parse).
	srv := NewServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("POST /health returned 200; expected 4xx (route should fall through to JSON-RPC routing which would fail to match /health)")
	}
}
