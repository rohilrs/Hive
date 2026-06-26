package main

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rohilrs/Hive/internal/verdict"
)

func buildHive(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "hive")
	cmd := exec.Command("go", "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build hive: %v\n%s", err, b)
	}
	return out
}

func TestMCPStageServerForwardsVerdict(t *testing.T) {
	hive := buildHive(t)
	sockPath := filepath.Join(t.TempDir(), "verdict.sock")
	listener, err := verdict.Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, hive,
		"mcp-stage-server",
		"--notify-sock", sockPath,
		"--stage", "review",
		"--run-id", "run-test",
		"--tool", "hive_submit_review_verdict",
	)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	transport := &mcp.CommandTransport{Command: cmd}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	_, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "hive_submit_review_verdict",
		Arguments: map[string]any{
			"verdict": "APPROVE", "confidence": 95,
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	select {
	case f := <-listener.Frames():
		if f.Verdict != "APPROVE" {
			t.Errorf("verdict=%s", f.Verdict)
		}
		if f.RunID != "run-test" {
			t.Errorf("run_id=%s", f.RunID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener didn't receive frame")
	}
}

func TestForwardDocumentation(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan map[string]any, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		dec := json.NewDecoder(conn)
		_ = dec.Decode(&req)
		got <- map[string]any{"method": req.Method, "params": req.Params}
		_, _ = conn.Write([]byte(`{"id":"x","result":{"ok":true}}` + "\n"))
	}()

	ok := forwardDocumentation(sock, "run-1", "document", DocSubmitInput{
		Summary:        "did docs",
		FilesChanged:   []string{"CHANGELOG.md"},
		ChangelogEntry: "- x",
	})
	if !ok {
		t.Error("forwardDocumentation returned false")
	}
	select {
	case m := <-got:
		if m["method"] != "documentation.submit" {
			t.Errorf("method=%v want documentation.submit", m["method"])
		}
		p := m["params"].(map[string]any)
		if p["run_id"] != "run-1" || p["summary"] != "did docs" {
			t.Errorf("params missing fields: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not receive the request")
	}
}

func TestForwardChatTool(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan map[string]any, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		dec := json.NewDecoder(conn)
		_ = dec.Decode(&req)
		got <- map[string]any{"method": req.Method, "params": req.Params}
		_, _ = conn.Write([]byte(`{"id":"x","result":{"content":"{\"pending_tasks\":3}","is_error":false}}` + "\n"))
	}()

	content, isErr := forwardChatTool(sock, "hive_status", "", map[string]any{"limit": 5})
	if isErr {
		t.Errorf("forwardChatTool isErr=true, content=%q", content)
	}
	if content != `{"pending_tasks":3}` {
		t.Errorf("content=%q want %q", content, `{"pending_tasks":3}`)
	}
	select {
	case m := <-got:
		if m["method"] != "chat.tool" {
			t.Errorf("method=%v want chat.tool", m["method"])
		}
		p := m["params"].(map[string]any)
		if p["tool"] != "hive_status" {
			t.Errorf("params.tool=%v want hive_status", p["tool"])
		}
		in, ok := p["input"].(map[string]any)
		if !ok || in["limit"] != float64(5) {
			t.Errorf("params.input=%+v want {limit:5}", p["input"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not receive the request")
	}
}

func TestForwardChatToolIncludesSessionID(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	captured := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req struct {
			Params struct {
				SessionID string `json:"session_id"`
			} `json:"params"`
		}
		dec := json.NewDecoder(conn)
		_ = dec.Decode(&req)
		captured <- req.Params.SessionID
		_, _ = conn.Write([]byte(`{"id":"x","result":{"content":"{}","is_error":false}}` + "\n"))
	}()

	content, isErr := forwardChatTool(sock, "hive_status", "sess-123", map[string]any{})
	if isErr {
		t.Errorf("forwardChatTool isErr=true, content=%q", content)
	}
	select {
	case got := <-captured:
		if got != "sess-123" {
			t.Errorf("session_id=%q, want %q", got, "sess-123")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not receive the request")
	}
}

func TestForwardChatToolTransportError(t *testing.T) {
	// Dialing a nonexistent socket must yield an error result, not a panic.
	content, isErr := forwardChatTool(filepath.Join(t.TempDir(), "nope.sock"), "hive_status", "", nil)
	if !isErr {
		t.Error("forwardChatTool isErr=false on dial failure; want true")
	}
	if content == "" {
		t.Error("forwardChatTool returned empty content on error")
	}
}

// TestRunChatToolsServerWithModePlanAdvertisesPlannerTools spawns the
// chat-tools MCP server with --mode plan and asserts the tools/list response
// includes exactly the 4 planner tool names — and none of the regular chat
// tool names. Phase 8.A T6b.
func TestRunChatToolsServerWithModePlanAdvertisesPlannerTools(t *testing.T) {
	hive := buildHive(t)
	sockPath := filepath.Join(t.TempDir(), "d.sock")
	// The chat-tools server only DIALS this socket when a tool is invoked
	// (forwarding chat.tool). tools/list never touches it, so we don't need
	// a real listener for this test.

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, hive,
		"mcp-stage-server",
		"--chat-tools",
		"--daemon-sock", sockPath,
		"--mode", "plan",
	)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	transport := &mcp.CommandTransport{Command: cmd}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	gotNames := map[string]bool{}
	for _, tl := range tools.Tools {
		gotNames[tl.Name] = true
	}
	wantPlanner := []string{"hive_list_specs", "hive_read_doc", "hive_save_roadmap", "hive_save_spec"}
	for _, name := range wantPlanner {
		if !gotNames[name] {
			t.Errorf("tools/list missing planner tool %q; got %v", name, gotNames)
		}
	}
	// And the regular chat tools must NOT be advertised in plan mode.
	for _, name := range []string{"hive_list_tasks", "hive_status", "hive_add_task"} {
		if gotNames[name] {
			t.Errorf("tools/list advertises chat tool %q in plan mode; should be planner-only", name)
		}
	}
}

// TestRunChatToolsServerDefaultModeAdvertisesChatTools asserts the existing
// (mode-flag-absent) behavior is unchanged: tools/list returns the regular
// chat tool palette. Pins the backward-compat invariant for Phase 8.A T6b.
func TestRunChatToolsServerDefaultModeAdvertisesChatTools(t *testing.T) {
	hive := buildHive(t)
	sockPath := filepath.Join(t.TempDir(), "d.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, hive,
		"mcp-stage-server",
		"--chat-tools",
		"--daemon-sock", sockPath,
		// No --mode flag — must default to chat tools.
	)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	transport := &mcp.CommandTransport{Command: cmd}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	gotNames := map[string]bool{}
	for _, tl := range tools.Tools {
		gotNames[tl.Name] = true
	}
	// Spot-check several known chat tools are present.
	for _, name := range []string{"hive_list_tasks", "hive_status", "hive_add_task"} {
		if !gotNames[name] {
			t.Errorf("default-mode tools/list missing chat tool %q; got %v", name, gotNames)
		}
	}
	// Planner tools must NOT leak into default mode.
	for _, name := range []string{"hive_list_specs", "hive_save_roadmap"} {
		if gotNames[name] {
			t.Errorf("default-mode tools/list advertises planner tool %q; should be chat-only", name)
		}
	}
}

func TestForwardDocumentationFailSoft(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		dec := json.NewDecoder(conn)
		_ = dec.Decode(&req)
		_, _ = conn.Write([]byte(`{"id":"x","error":{"message":"boom"}}` + "\n"))
	}()

	ok := forwardDocumentation(sock, "run-1", "document", DocSubmitInput{Summary: "x"})
	if ok {
		t.Error("forwardDocumentation returned true on RPC error; want false (fail-soft)")
	}
}
