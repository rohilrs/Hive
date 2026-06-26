package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/scavenger/capsule"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// chatFakeRunner is a turnRunner backed by a scripted queue. It satisfies
// chat's unexported turnRunner interface (only RunTurn is required), so the
// daemon test can build a real *chat.Agent without an API key or network.
type chatFakeRunner struct {
	queue []*anthropic.TurnOutput
}

func (f *chatFakeRunner) RunTurn(ctx context.Context, in anthropic.TurnInput) (*anthropic.TurnOutput, error) {
	if len(f.queue) > 0 {
		out := f.queue[0]
		f.queue = f.queue[1:]
		return out, nil
	}
	return &anthropic.TurnOutput{StopReason: "end_turn", Assistant: anth.NewAssistantMessage(anth.NewTextBlock(""))}, nil
}

// TestHandleChatTool exercises the chat.tool RPC against an in-memory store:
// a known tool returns a non-error result, an unknown tool is invalid-params.
func TestHandleChatTool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	s := &RPCServer{d: &Daemon{store: st}}

	// Known tool: hive_status returns {pending_tasks, running} JSON, not an error.
	params, _ := json.Marshal(ChatToolParams{Tool: "hive_status", Input: json.RawMessage(`{}`)})
	out, rerr := s.handleChatTool(ctx, params)
	if rerr != nil {
		t.Fatalf("handleChatTool(hive_status) error: %+v", rerr)
	}
	var res struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.IsError {
		t.Errorf("hive_status is_error=true, content=%q", res.Content)
	}
	if res.Content == "" {
		t.Errorf("hive_status returned empty content")
	}

	// Unknown tool: invalid-params RPC error.
	bad, _ := json.Marshal(ChatToolParams{Tool: "hive_nonexistent", Input: json.RawMessage(`{}`)})
	if _, rerr := s.handleChatTool(ctx, bad); rerr == nil || rerr.Code != rpc.ErrInvalidParams {
		t.Errorf("unknown tool: got %+v, want ErrInvalidParams", rerr)
	}

	// Missing tool name: invalid-params too.
	empty, _ := json.Marshal(ChatToolParams{Input: json.RawMessage(`{}`)})
	if _, rerr := s.handleChatTool(ctx, empty); rerr == nil || rerr.Code != rpc.ErrInvalidParams {
		t.Errorf("empty tool: got %+v, want ErrInvalidParams", rerr)
	}
}

// TestStreamChatRunTurn exercises runChatTurn end-to-end with a fake-runner
// agent: it asserts the streamed frames (text -> tool_result -> text ->
// turn_done) reach the wire and that the session + messages are persisted.
func TestStreamChatRunTurn(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	sessionID := "chat-test-1"
	if err := st.InsertChatSession(ctx, &store.ChatSession{ID: sessionID, Surface: "cli"}); err != nil {
		t.Fatalf("InsertChatSession: %v", err)
	}

	// Agent: a tool-use turn then a final text turn.
	agentRunner := &chatFakeRunner{
		queue: []*anthropic.TurnOutput{
			{
				Text:       "checking",
				ToolCalls:  []anthropic.ToolCall{{ID: "t1", Name: "hive_status", Input: json.RawMessage(`{}`)}},
				StopReason: "tool_use",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("checking")),
				TokensIn:   100, TokensOut: 20,
			},
			{
				Text:       "all good",
				StopReason: "end_turn",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("all good")),
				TokensIn:   30, TokensOut: 10,
			},
		},
	}
	reg := chat.NewRegistry()
	reg.Register(chat.Tool{
		Def: anthropic.ToolDef{Name: "hive_status"},
		Handler: func(ctx context.Context, input json.RawMessage) chat.ToolResult {
			return chat.ToolResult{Content: `{"pending_tasks":0}`}
		},
	})
	// Router runner is separate so it never consumes the agent's queue.
	router := chat.NewRouter(&chatFakeRunner{}, "default-model", "reasoning-model")
	agent := chat.NewSDKAgent(agentRunner, reg, router,
		chat.Config{DefaultModel: "default-model", ReasoningModel: "reasoning-model"}, chatCostFn)

	server, client := net.Pipe()

	// Reader goroutine: decode line-delimited Frame response envelopes.
	type framesOrErr struct {
		frames []chat.Frame
		err    error
	}
	done := make(chan framesOrErr, 1)
	go func() {
		var got []chat.Frame
		sc := bufio.NewScanner(client)
		for sc.Scan() {
			var resp rpc.Response[chat.Frame]
			if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
				done <- framesOrErr{err: err}
				return
			}
			if resp.Result != nil {
				got = append(got, *resp.Result)
			}
		}
		done <- framesOrErr{frames: got}
	}()

	runChatTurn(ctx, agent, server, "req-1", st, &chat.Conversation{SessionID: sessionID}, sessionID, "what's the status?")
	server.Close() // signal the reader the stream is complete

	res := <-done
	if res.err != nil {
		t.Fatalf("decode frames: %v", res.err)
	}

	wantKinds := []string{"text", "tool_result", "text", "turn_done"}
	if len(res.frames) != len(wantKinds) {
		t.Fatalf("frames = %d, want %d: %+v", len(res.frames), len(wantKinds), res.frames)
	}
	for i, want := range wantKinds {
		if res.frames[i].Kind != want {
			t.Fatalf("frame[%d].Kind = %q, want %q", i, res.frames[i].Kind, want)
		}
	}
	if res.frames[1].Tool != "hive_status" || res.frames[1].Result != `{"pending_tasks":0}` {
		t.Errorf("tool_result frame = %+v", res.frames[1])
	}
	if res.frames[3].Model != "default-model" {
		t.Errorf("turn_done model = %q, want default-model", res.frames[3].Model)
	}

	// Persistence: assistant + tool messages appended (user msg is appended by
	// streamChat, not runChatTurn, so we don't assert it here).
	msgs, err := st.GetChatMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetChatMessages: %v", err)
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	// Expect: assistant("checking"), tool(result), assistant("all good").
	wantRoles := []string{"assistant", "tool", "assistant"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("persisted roles = %v, want %v", roles, wantRoles)
	}
	for i, want := range wantRoles {
		if roles[i] != want {
			t.Errorf("msg[%d].Role = %q, want %q", i, roles[i], want)
		}
	}

	// Session should be ended (ended_at stamped) after turn_done.
	sessions, err := st.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].EndedAt == 0 {
		t.Errorf("session not ended: %+v", sessions)
	}
}

// newTestStore opens an in-memory (temp-dir) SQLite store for tests and
// registers a t.Cleanup to close it.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestChatRegistryHasMutatingTools asserts that the three mutating tools are
// registered with Mutating=true.
func TestChatRegistryHasMutatingTools(t *testing.T) {
	st := newTestStore(t)
	d := &Daemon{store: st}
	r := d.chatRegistry()
	for _, name := range []string{"hive_add_task", "hive_run_now", "hive_abandon"} {
		tool, ok := r.Get(name)
		if !ok {
			t.Errorf("%s not registered", name)
			continue
		}
		if !tool.Mutating {
			t.Errorf("%s.Mutating=false, want true", name)
		}
	}
}

func TestChatRegistryHasEditTaskMutating(t *testing.T) {
	st := newTestStore(t)
	d := &Daemon{store: st}
	r := d.chatRegistry()
	tool, ok := r.Get("hive_edit_task")
	if !ok {
		t.Fatal("hive_edit_task not registered")
	}
	if !tool.Mutating {
		t.Errorf("hive_edit_task.Mutating=false, want true")
	}
}

func TestChatRegistryHasApproveDenyMutating(t *testing.T) {
	st := newTestStore(t)
	d := &Daemon{store: st}
	r := d.chatRegistry()
	for _, name := range []string{"hive_approve", "hive_deny"} {
		tool, ok := r.Get(name)
		if !ok {
			t.Errorf("%s not registered", name)
			continue
		}
		if !tool.Mutating {
			t.Errorf("%s.Mutating=false, want true", name)
		}
	}
}

func TestChatRegistryHasResumeRegistered(t *testing.T) {
	// hive_resume is now a real handler (wired to Scheduler.Resume in T2).
	// hive_predict is also a real handler — tested by TestHivePredictToolRegisteredAndDispatchable.
	st := newTestStore(t)
	d := &Daemon{store: st}
	r := d.chatRegistry()

	tool, ok := r.Get("hive_resume")
	if !ok {
		t.Fatal("hive_resume not registered")
	}
	if !tool.Mutating {
		t.Errorf("hive_resume.Mutating=false; want true so the confirm gate fires before launching a worker")
	}
	// Missing run_id should still return an error (run_id required), not a "not available" stub.
	res := tool.Handler(context.Background(), json.RawMessage(`{}`))
	if !res.IsError {
		t.Errorf("hive_resume with empty run_id did not return IsError=true (Content=%s)", res.Content)
	}
	if strings.Contains(res.Content, "not available in this version") {
		t.Errorf("hive_resume still returning old stub message: %s", res.Content)
	}
}

func TestChatHistoryListReturnsRecent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// Insert 3 sessions with explicitly monotonically increasing StartedAt so
	// the DESC order is deterministic even within a single second.
	for i := 0; i < 3; i++ {
		_ = st.InsertChatSession(ctx, &store.ChatSession{
			ID:        fmt.Sprintf("s%d", i),
			Surface:   "cli",
			StartedAt: int64(i + 1), // 1, 2, 3 — stable ordering
		})
	}
	d := &Daemon{store: st}
	s := &RPCServer{d: d}
	out, rerr := s.handleChatHistoryList(ctx, json.RawMessage(`{"limit":2}`))
	if rerr != nil {
		t.Fatalf("rpc err: %v", rerr)
	}
	var resp struct {
		Sessions []store.ChatSession `json:"sessions"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != 2 {
		t.Errorf("got %d sessions, want 2", len(resp.Sessions))
		return
	}
	// ListChatSessions orders DESC by started_at, so s2 (StartedAt=3) comes
	// first, then s1 (StartedAt=2).
	if resp.Sessions[0].ID != "s2" {
		t.Errorf("first session ID=%q, want s2 (newest first)", resp.Sessions[0].ID)
	}
	if resp.Sessions[1].ID != "s1" {
		t.Errorf("second session ID=%q, want s1", resp.Sessions[1].ID)
	}
}

func TestChatRegistryHasListProjects(t *testing.T) {
	st := newTestStore(t)
	d := &Daemon{store: st}
	r := d.chatRegistry()
	tool, ok := r.Get("hive_list_projects")
	if !ok {
		t.Fatal("hive_list_projects not registered")
	}
	if tool.Mutating {
		t.Errorf("hive_list_projects.Mutating=true, want false (read tool)")
	}
}

// TestListProjectsIncludesTaskCounts asserts the hive_list_projects handler
// attaches task_counts to projects that have tasks and omits the key for
// projects without any. The shape must be []map (not the bare store.Project
// struct) so the model sees task_counts as a first-class field.
func TestListProjectsIncludesTaskCounts(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// p1 has tasks; p2 has none → p2 should have no task_counts key.
	if err := st.InsertProject(ctx, &store.Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertProject(ctx, &store.Project{ID: "p2", Slug: "b", Name: "B", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Title: "x", Pipeline: "build", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.Task{ID: "t2", ProjectID: "p1", Title: "y", Pipeline: "build", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{store: st}
	tool, _ := d.chatRegistry().Get("hive_list_projects")
	res := tool.Handler(ctx, json.RawMessage(`{}`))
	if res.IsError {
		t.Fatalf("handler returned error: %s", res.Content)
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(res.Content), &rows); err != nil {
		t.Fatalf("unmarshal result: %v (content=%s)", err, res.Content)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}

	// Build a slug→row index — order is by slug (alphabetical), so a, then b.
	bySlug := map[string]map[string]any{}
	for _, r := range rows {
		slug, _ := r["slug"].(string)
		bySlug[slug] = r
	}
	pA, ok := bySlug["a"]
	if !ok {
		t.Fatalf("missing slug=a; rows=%+v", rows)
	}
	tc, ok := pA["task_counts"].(map[string]any)
	if !ok {
		t.Fatalf("a.task_counts missing or wrong type: %+v", pA["task_counts"])
	}
	// JSON unmarshal of int → float64 (default), so cast accordingly.
	if got, _ := tc["pending"].(float64); got != 1 {
		t.Errorf("a.task_counts.pending=%v, want 1", tc["pending"])
	}
	if got, _ := tc["running"].(float64); got != 1 {
		t.Errorf("a.task_counts.running=%v, want 1", tc["running"])
	}

	pB := bySlug["b"]
	if _, ok := pB["task_counts"]; ok {
		t.Errorf("b should not have task_counts (no tasks); got %+v", pB["task_counts"])
	}
}

func TestChatHistoryGetReturnsMessages(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	_ = st.InsertChatSession(ctx, &store.ChatSession{ID: "sx", Surface: "cli"})
	_ = st.AppendChatMessage(ctx, &store.ChatMessage{ID: "m1", SessionID: "sx", Role: "user", Content: "hi"})
	_ = st.AppendChatMessage(ctx, &store.ChatMessage{ID: "m2", SessionID: "sx", Role: "assistant", Content: "hello"})
	d := &Daemon{store: st}
	s := &RPCServer{d: d}
	out, rerr := s.handleChatHistoryGet(ctx, json.RawMessage(`{"session_id":"sx"}`))
	if rerr != nil {
		t.Fatalf("rpc err: %v", rerr)
	}
	var resp struct {
		Messages []store.ChatMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Messages) != 2 {
		t.Errorf("got %d messages, want 2", len(resp.Messages))
	}
	if resp.Messages[0].Content != "hi" {
		t.Errorf("first message content=%q, want 'hi'", resp.Messages[0].Content)
	}
}

func TestRunChatTurnPersistsErrorFrame(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.InsertChatSession(ctx, &store.ChatSession{ID: "s1", Surface: "test"}); err != nil {
		t.Fatal(err)
	}
	// Stub agent that emits an error frame then returns an error.
	stub := chatStubAgent{frames: []chat.Frame{{Kind: "error", Text: "claude subprocess: exit status 137"}}, err: errors.New("subprocess died")}
	// Use a net.Pipe so emit's writes don't fail.
	cConn, sConn := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, cConn) }()
	defer cConn.Close()
	defer sConn.Close()
	conv := &chat.Conversation{SessionID: "s1"}
	runChatTurn(ctx, stub, sConn, "rid", st, conv, "s1", "what's up")
	// Now query chat_messages for the session; expect at least one row with
	// role="error" containing the subprocess error text.
	msgs, err := st.GetChatMessages(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for _, m := range msgs {
		if m.Role == "error" && strings.Contains(m.Content, "subprocess") {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("error frame not persisted; messages=%+v", msgs)
	}
}

func TestRunChatTurnSyntheticOrphanWhenNoErrorFrame(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.InsertChatSession(ctx, &store.ChatSession{ID: "s-orphan", Surface: "test"}); err != nil {
		t.Fatal(err)
	}
	// Stub agent that returns an error WITHOUT emitting any frames.
	stub := chatStubAgent{frames: nil, err: errors.New("timeout: deadline exceeded")}
	cConn, sConn := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, cConn) }()
	defer cConn.Close()
	defer sConn.Close()
	conv := &chat.Conversation{SessionID: "s-orphan"}
	runChatTurn(ctx, stub, sConn, "rid", st, conv, "s-orphan", "ping")

	msgs, err := st.GetChatMessages(ctx, "s-orphan")
	if err != nil {
		t.Fatal(err)
	}
	var sawSynthetic bool
	for _, m := range msgs {
		if m.Role == "error" && strings.Contains(m.Content, "agent error (no detail)") && strings.Contains(m.Content, "timeout") {
			sawSynthetic = true
		}
	}
	if !sawSynthetic {
		t.Errorf("synthetic orphan row not persisted; messages=%+v", msgs)
	}
}

// chatStubAgent emits a scripted list of frames then returns err.
type chatStubAgent struct {
	frames []chat.Frame
	err    error
}

func (a chatStubAgent) Send(ctx context.Context, conv *chat.Conversation, msg string, emit func(chat.Frame)) error {
	for _, f := range a.frames {
		emit(f)
	}
	return a.err
}

func TestDeriveSessionNameShortPassthrough(t *testing.T) {
	got := deriveSessionName("hello world")
	if got != "hello world" {
		t.Errorf("got %q, want passthrough", got)
	}
}

func TestDeriveSessionNameTruncatesAtWordBoundary(t *testing.T) {
	long := "this is a very long message that goes well past fifty characters and beyond"
	got := deriveSessionName(long)
	// Should be no longer than ~50 runes + ellipsis
	if len([]rune(got)) > 55 {
		t.Errorf("got %q (%d runes), want truncated", got, len([]rune(got)))
	}
	// Should end with ellipsis
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want trailing ellipsis", got)
	}
	// Should not end mid-word — check that the char before ellipsis is a letter, not a space
	rs := []rune(strings.TrimSuffix(got, "…"))
	if len(rs) > 0 && rs[len(rs)-1] == ' ' {
		t.Errorf("truncated with trailing space: %q", got)
	}
}

func TestDeriveSessionNameTrimsWhitespace(t *testing.T) {
	got := deriveSessionName("   hello   ")
	if got != "hello" {
		t.Errorf("got %q, want 'hello'", got)
	}
}

func TestDeriveSessionNameEmpty(t *testing.T) {
	if got := deriveSessionName(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := deriveSessionName("   "); got != "" {
		t.Errorf("whitespace-only: got %q, want empty", got)
	}
}

func TestChatSetNameUpdatesSession(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.InsertChatSession(ctx, &store.ChatSession{ID: "s1", Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: st}
	s := &RPCServer{d: d}
	params := json.RawMessage(`{"session_id":"s1","name":"renamed via rpc"}`)
	_, rerr := s.handleChatSetName(ctx, params)
	if rerr != nil {
		t.Fatalf("rpc err: %v", rerr)
	}
	got, _ := st.GetChatSession(ctx, "s1")
	if got.Name != "renamed via rpc" {
		t.Errorf("Name=%q, want 'renamed via rpc'", got.Name)
	}
}

func TestChatSetNameEmptySessionIDRejected(t *testing.T) {
	st := newTestStore(t)
	s := &RPCServer{d: &Daemon{store: st}}
	_, rerr := s.handleChatSetName(context.Background(), json.RawMessage(`{"session_id":"","name":"x"}`))
	if rerr == nil || rerr.Code != rpc.ErrInvalidParams {
		t.Errorf("rerr=%v, want ErrInvalidParams", rerr)
	}
}

func TestChatSetNameMissingSessionRejected(t *testing.T) {
	st := newTestStore(t)
	s := &RPCServer{d: &Daemon{store: st}}
	_, rerr := s.handleChatSetName(context.Background(), json.RawMessage(`{"session_id":"nope","name":"x"}`))
	if rerr == nil {
		t.Errorf("expected error for missing session")
	}
}

func TestChatSetNameCapsLongName(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	_ = st.InsertChatSession(ctx, &store.ChatSession{ID: "s1", Surface: "cli"})
	s := &RPCServer{d: &Daemon{store: st}}
	longName := strings.Repeat("a", 500)
	params, _ := json.Marshal(map[string]any{"session_id": "s1", "name": longName})
	_, rerr := s.handleChatSetName(ctx, params)
	if rerr != nil {
		t.Fatalf("rpc err: %v", rerr)
	}
	got, _ := st.GetChatSession(ctx, "s1")
	if len([]rune(got.Name)) != 200 {
		t.Errorf("name not capped at 200 runes; got %d", len([]rune(got.Name)))
	}
}

// TestExistingWorkSeed_PlanOnly verifies that existingWorkSeed injects an
// EXISTING WORK block for plan sessions and returns "" for non-plan kinds.
func TestExistingWorkSeed_PlanOnly(t *testing.T) {
	d := newTestDaemon(t)
	proj := insertWriteBackProject(t, d, "demo", "HBA", "proj-uuid")
	_ = d.store.InsertTask(context.Background(), &store.Task{
		ID: "t1", ProjectID: proj.ID, Source: "inbox", Title: "x", Status: "pending",
		Pipeline: "build", Priority: "P1",
	})
	d.RegisterSource(&fakeSource{name: "linear"})

	seed := d.existingWorkSeed(context.Background(), store.KindPlan, "demo")
	if !strings.Contains(seed, "EXISTING WORK") || !strings.Contains(seed, "hive:t1") {
		t.Errorf("plan seed missing existing work: %q", seed)
	}
	if d.existingWorkSeed(context.Background(), store.KindChat, "demo") != "" {
		t.Error("non-plan kind must get empty seed")
	}
}

func TestStreamChatAutoNamesNewSession(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{Chat: config.Chat{Provider: "claude-code"}}}}
	d.chatAgent = chatStubAgent{} // no frames, no err — turn ends immediately
	s := &RPCServer{d: d}

	cConn, sConn := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, cConn) }()
	defer cConn.Close()
	defer sConn.Close()

	params := json.RawMessage(`{"message":"list all my pending tasks for me kthx","session_id":""}`)
	go s.streamChat(ctx, sConn, "rid", params)
	time.Sleep(100 * time.Millisecond)

	sessions, _ := st.ListChatSessions(ctx, 10)
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].Name == "" {
		t.Errorf("name not auto-derived; session=%+v", sessions[0])
	}
	if sessions[0].Provider != "claude-code" {
		t.Errorf("provider=%q, want claude-code", sessions[0].Provider)
	}
}

// TestStreamChatPlannerSessionNamedFromProject pins the 8.C.2 T6 fix:
// planner-mode sessions are seeded with the literal "begin" message,
// which deriveSessionName would turn into a useless "begin" name.
// Instead the session name should be "<Project Name> Planning" so it
// scans in the picker + chat history list.
func TestStreamChatPlannerSessionNamedFromProject(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.InsertProject(ctx, &store.Project{ID: "p1", Slug: "my-app", Name: "My App", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{Chat: config.Chat{Provider: "claude-code"}}}}
	d.chatAgent = chatStubAgent{}
	// Planner sessions need the planner factory wired or streamChat
	// fails before reaching the InsertChatSession path we're testing.
	d.SetPlannerAgentFor(func(_, _ string) (chat.Agent, error) {
		return chatStubAgent{}, nil
	})
	s := &RPCServer{d: d}

	cConn, sConn := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, cConn) }()
	defer cConn.Close()
	defer sConn.Close()

	params := json.RawMessage(`{"message":"begin","session_id":"","kind":"plan","project_slug":"my-app"}`)
	go s.streamChat(ctx, sConn, "rid", params)
	time.Sleep(150 * time.Millisecond)

	sessions, _ := st.ListChatSessions(ctx, 10)
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].Name != "My App Planning" {
		t.Errorf("planner session name=%q, want 'My App Planning' (project Name + ' Planning')", sessions[0].Name)
	}
	if sessions[0].Kind != store.KindPlan {
		t.Errorf("kind=%q, want plan", sessions[0].Kind)
	}
}

// TestStreamChatPlannerSessionFallsBackToDerivedNameWhenProjectMissing
// guards against the unlikely case where the project_slug validates
// (non-empty) at session-create time but a concurrent delete removes
// the row before GetProjectBySlug runs. We don't want the session to
// have an empty name in that race.
func TestStreamChatPlannerSessionFallsBackToDerivedNameWhenProjectMissing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{Chat: config.Chat{Provider: "claude-code"}}}}
	d.chatAgent = chatStubAgent{}
	d.SetPlannerAgentFor(func(_, _ string) (chat.Agent, error) {
		return chatStubAgent{}, nil
	})
	s := &RPCServer{d: d}

	cConn, sConn := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, cConn) }()
	defer cConn.Close()
	defer sConn.Close()

	// Note: no project inserted; the slug exists in the request but
	// GetProjectBySlug will return ErrNotFound.
	params := json.RawMessage(`{"message":"begin","session_id":"","kind":"plan","project_slug":"ghost"}`)
	go s.streamChat(ctx, sConn, "rid", params)
	time.Sleep(150 * time.Millisecond)

	sessions, _ := st.ListChatSessions(ctx, 10)
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	// Fall back to deriveSessionName("begin") → "begin". Not ideal but
	// at least non-empty so the session is recognizable in the picker.
	if sessions[0].Name == "" {
		t.Errorf("name should fall back to derived form when project missing; got empty")
	}
}

// fakeGate is a minimal chat.ConfirmGate that returns a canned decision.
// Captures the input arg for assertions when needed.
type fakeGate struct {
	decision chat.ConfirmDecision
	gotInput json.RawMessage
}

func (g *fakeGate) Propose(_ context.Context, _, _, _ string, input json.RawMessage) (chat.ConfirmDecision, error) {
	g.gotInput = input
	return g.decision, nil
}

func TestCCChatToolDenyContentUsesDenyReason(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{}}}
	srv := &RPCServer{d: d}

	// Register a Mutating chat tool whose handler must not run on deny.
	reg := chat.NewRegistry()
	handlerCalled := false
	reg.Register(chat.Tool{
		Def:      anthropic.ToolDef{Name: "fake_mutating_tool"},
		Mutating: true,
		Handler: func(_ context.Context, _ json.RawMessage) chat.ToolResult {
			handlerCalled = true
			return chat.ToolResult{}
		},
	})
	d.chatRegistryForTest = reg
	d.chatConfirmGateForTool = &fakeGate{
		decision: chat.ConfirmDecision{Approve: false, DenyReason: "user cancelled, do not retry"},
	}

	params, _ := json.Marshal(ChatToolParams{Tool: "fake_mutating_tool", Input: json.RawMessage(`{}`)})
	raw, rpcErr := srv.handleChatTool(ctx, params)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc err: %v", rpcErr)
	}
	if handlerCalled {
		t.Errorf("handler should not run on deny")
	}
	var out struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !out.IsError {
		t.Errorf("expected is_error=true")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out.Content), &body); err != nil {
		t.Fatalf("content not JSON: %s", out.Content)
	}
	if body["error"] != "user cancelled, do not retry" {
		t.Errorf("error=%v, want 'user cancelled, do not retry'", body["error"])
	}
	if _, has := body["reason"]; has {
		t.Errorf("tool_result should not have 'reason' field anymore; got %s", out.Content)
	}
}

func TestCCChatToolDenyContentDefaultsToDeclinedByUser(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{}}}
	srv := &RPCServer{d: d}

	reg := chat.NewRegistry()
	reg.Register(chat.Tool{
		Def:      anthropic.ToolDef{Name: "fake_mutating_tool"},
		Mutating: true,
		Handler:  func(_ context.Context, _ json.RawMessage) chat.ToolResult { return chat.ToolResult{} },
	})
	d.chatRegistryForTest = reg
	d.chatConfirmGateForTool = &fakeGate{decision: chat.ConfirmDecision{Approve: false}}

	params, _ := json.Marshal(ChatToolParams{Tool: "fake_mutating_tool", Input: json.RawMessage(`{}`)})
	raw, _ := srv.handleChatTool(ctx, params)
	var out struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	_ = json.Unmarshal(raw, &out)
	var body map[string]any
	_ = json.Unmarshal([]byte(out.Content), &body)
	if body["error"] != "declined by user" {
		t.Errorf("error=%v, want 'declined by user'", body["error"])
	}
}

func TestCCChatToolEditedInputResultGetsPrefix(t *testing.T) {
	ctx := context.Background()
	var seenInput json.RawMessage
	st := newTestStore(t)
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{}}}
	srv := &RPCServer{d: d}

	reg := chat.NewRegistry()
	reg.Register(chat.Tool{
		Def:      anthropic.ToolDef{Name: "fake_mutating_tool"},
		Mutating: true,
		Handler: func(_ context.Context, in json.RawMessage) chat.ToolResult {
			seenInput = in
			return chat.ToolResult{Content: `{"task_id":"t-1"}`}
		},
	})
	d.chatRegistryForTest = reg
	d.chatConfirmGateForTool = &fakeGate{
		decision: chat.ConfirmDecision{Approve: true, EditedInput: json.RawMessage(`{"title":"edited"}`)},
	}

	params, _ := json.Marshal(ChatToolParams{Tool: "fake_mutating_tool", Input: json.RawMessage(`{"title":"original"}`)})
	raw, _ := srv.handleChatTool(ctx, params)
	var out struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(raw, &out)

	if string(seenInput) != `{"title":"edited"}` {
		t.Errorf("handler received %s, want edited input", seenInput)
	}
	if !strings.HasPrefix(out.Content, "[user edited args before running] ") {
		t.Errorf("content missing edit prefix: %q", out.Content)
	}
	if !strings.Contains(out.Content, `"task_id":"t-1"`) {
		t.Errorf("content missing original handler output: %q", out.Content)
	}
}

func TestCompactPredictionPopulatesFields(t *testing.T) {
	res := &predictor.Result{
		Files: []string{"a.go", "b.go"},
		InlineCapsules: []capsule.Capsule{
			{Target: "PkgA.FuncA"},
			{Target: "PkgB.TypeB"},
		},
		Overflow: []anthropic.Candidate{
			{File: "c.go", Symbol: "PkgC.OverflowSym"},
		},
		Metrics: predictor.Metrics{
			HaikuLatency:   421 * time.Millisecond,
			FetchLatency:   87 * time.Millisecond,
			CandidateCount: 5,
			InlineCount:    2,
			OverflowCount:  1,
			Truncated:      false,
		},
	}
	got := compactPrediction(res)

	files, _ := got["candidate_files"].([]string)
	if len(files) != 2 || files[0] != "a.go" || files[1] != "b.go" {
		t.Errorf("candidate_files=%v, want [a.go b.go]", files)
	}
	if got["candidate_count"] != 2 {
		t.Errorf("candidate_count=%v, want 2", got["candidate_count"])
	}
	syms, _ := got["inline_capsule_symbols"].([]string)
	if len(syms) != 2 || syms[0] != "PkgA.FuncA" || syms[1] != "PkgB.TypeB" {
		t.Errorf("inline_capsule_symbols=%v, want [PkgA.FuncA PkgB.TypeB]", syms)
	}
	over, _ := got["overflow_symbols"].([]string)
	if len(over) != 1 || over[0] != "PkgC.OverflowSym" {
		t.Errorf("overflow_symbols=%v, want [PkgC.OverflowSym]", over)
	}
	metrics, _ := got["metrics"].(map[string]any)
	if metrics["haiku_latency_ms"] != int64(421) {
		t.Errorf("haiku_latency_ms=%v, want 421", metrics["haiku_latency_ms"])
	}
	if metrics["fetch_latency_ms"] != int64(87) {
		t.Errorf("fetch_latency_ms=%v, want 87", metrics["fetch_latency_ms"])
	}
	if metrics["inline_count"] != 2 || metrics["overflow_count"] != 1 || metrics["truncated"] != false {
		t.Errorf("metrics misc fields wrong: %+v", metrics)
	}
}

func TestCompactPredictionHandlesNilResult(t *testing.T) {
	got := compactPrediction(nil)

	files, _ := got["candidate_files"].([]string)
	if files == nil || len(files) != 0 {
		t.Errorf("candidate_files=%v, want empty slice (not nil)", files)
	}
	if got["candidate_count"] != 0 {
		t.Errorf("candidate_count=%v, want 0", got["candidate_count"])
	}
	metrics, _ := got["metrics"].(map[string]any)
	if metrics == nil {
		t.Fatal("metrics map missing")
	}
	if metrics["haiku_latency_ms"] != int64(0) {
		t.Errorf("haiku_latency_ms=%v, want 0", metrics["haiku_latency_ms"])
	}
}

func TestCompactPredictionMarshalsToValidJSON(t *testing.T) {
	res := &predictor.Result{Files: []string{"a.go"}, Metrics: predictor.Metrics{InlineCount: 1}}
	got := compactPrediction(res)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("compactPrediction output must marshal cleanly: %v", err)
	}
	var rt map[string]any
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatal(err)
	}
	if rt["candidate_count"].(float64) != 1 {
		t.Errorf("round-trip candidate_count=%v, want 1", rt["candidate_count"])
	}
}

// chatFakePredictor is a test double for predictorIface that captures call
// arguments and returns a scripted result/error. Named chatFakePredictor to
// avoid collision with the fakePredictor type in scheduler_test.go (same
// package) which has a different field set.
type chatFakePredictor struct {
	result    *predictor.Result
	err       error
	callCount int
	lastTask  string
	lastRoot  string
}

func (f *chatFakePredictor) Predict(_ context.Context, task, repoRoot, _ string) (*predictor.Result, error) {
	f.callCount++
	f.lastTask = task
	f.lastRoot = repoRoot
	return f.result, f.err
}

// hivePredictTestSetup constructs a fresh Daemon with a fake predictor
// and returns it for store seeding and handleHivePredict invocation.
func hivePredictTestSetup(t *testing.T, fp *chatFakePredictor) *Daemon {
	t.Helper()
	hiveDir := t.TempDir()
	cfg := config.Default()
	d, err := New(Config{
		HiveDir:   hiveDir,
		Cfg:       cfg,
		Adapter:   noopAdapter{},
		Predictor: fp,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHivePredictUnknownTaskReturnsError(t *testing.T) {
	fp := &chatFakePredictor{result: &predictor.Result{}}
	d := hivePredictTestSetup(t, fp)

	res := d.handleHivePredict(context.Background(), "ghost-task", false)
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown task")
	}
	if !strings.Contains(res.Content, "task not found") {
		t.Errorf("content=%q, want substring 'task not found'", res.Content)
	}
	if fp.callCount != 0 {
		t.Errorf("predictor called %d times for unknown task; want 0", fp.callCount)
	}
}

func TestHivePredictPendingTaskUsesProjectRepoPath(t *testing.T) {
	fp := &chatFakePredictor{result: &predictor.Result{Files: []string{"a.go"}}}
	d := hivePredictTestSetup(t, fp)
	ctx := context.Background()

	repoPath := "/fake/repo"
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "do thing", Status: "pending"}); err != nil {
		t.Fatal(err)
	}

	res := d.handleHivePredict(ctx, "t1", false)
	if res.IsError {
		t.Fatalf("expected no error: %s", res.Content)
	}
	if fp.callCount != 1 {
		t.Errorf("predictor called %d times; want 1", fp.callCount)
	}
	if fp.lastTask != "do thing" || fp.lastRoot != "/fake/repo" {
		t.Errorf("predictor received task=%q root=%q; want 'do thing' / '/fake/repo'", fp.lastTask, fp.lastRoot)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("response not JSON: %s err=%v", res.Content, err)
	}
	if _, has := body["persisted_to_run"]; has {
		t.Errorf("persisted_to_run should be absent on refresh=false; got %v", body["persisted_to_run"])
	}
}

func TestHivePredictRefreshTrueOnPendingTaskMarksRefreshNoop(t *testing.T) {
	fp := &chatFakePredictor{result: &predictor.Result{Files: []string{"a.go"}}}
	d := hivePredictTestSetup(t, fp)
	ctx := context.Background()
	repoPath := "/r"
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath, Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "x", Status: "pending"})

	res := d.handleHivePredict(ctx, "t1", true)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Content), &body)
	if body["refresh_noop"] != true {
		t.Errorf("refresh_noop=%v, want true (no non-terminal run)", body["refresh_noop"])
	}
	if _, has := body["persisted_to_run"]; has {
		t.Errorf("persisted_to_run must be absent when refresh is a no-op; got %v", body["persisted_to_run"])
	}
}

func TestHivePredictRefreshTrueOnNonTerminalRunPersists(t *testing.T) {
	fp := &chatFakePredictor{result: &predictor.Result{Files: []string{"a.go", "b.go"}}}
	d := hivePredictTestSetup(t, fp)
	ctx := context.Background()
	repoPath := "/r"
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath, Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "x", Status: "running"})
	if err := d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	res := d.handleHivePredict(ctx, "t1", true)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Content), &body)
	if body["persisted_to_run"] != "r1" {
		t.Errorf("persisted_to_run=%v, want 'r1'", body["persisted_to_run"])
	}
	if body["refresh_noop"] == true {
		t.Errorf("refresh_noop=true; want false (run r1 was updated)")
	}
	// Verify the prediction was persisted via GetPredictionJSON (stored in a
	// separate run_predictions-style column, not on the Run struct itself).
	got, err := d.store.GetPredictionJSON(ctx, "r1")
	if err != nil {
		t.Fatalf("GetPredictionJSON: %v", err)
	}
	if len(got) == 0 {
		t.Errorf("prediction not persisted; got empty bytes")
	}
}

func TestHivePredictGracefulDegradeOnNilResult(t *testing.T) {
	fp := &chatFakePredictor{result: nil, err: nil}
	d := hivePredictTestSetup(t, fp)
	ctx := context.Background()
	repoPath := "/r"
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath, Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "x", Status: "pending"})

	res := d.handleHivePredict(ctx, "t1", false)
	if res.IsError {
		t.Errorf("graceful degrade should not set IsError; got %s", res.Content)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Content), &body)
	if body["candidate_count"] != float64(0) {
		t.Errorf("candidate_count=%v, want 0 on degrade", body["candidate_count"])
	}
}

func TestHivePredictPredictorErrorSurfaces(t *testing.T) {
	fp := &chatFakePredictor{result: nil, err: errors.New("bundle dir not writable")}
	d := hivePredictTestSetup(t, fp)
	ctx := context.Background()
	repoPath := "/r"
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath, Status: "active"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "x", Status: "pending"})

	res := d.handleHivePredict(ctx, "t1", false)
	if !res.IsError {
		t.Errorf("expected IsError=true on predictor failure")
	}
	if !strings.Contains(res.Content, "predict failed") {
		t.Errorf("content=%q, want 'predict failed' substring", res.Content)
	}
}

func TestHivePredictToolRegisteredAndDispatchable(t *testing.T) {
	// End-to-end through the chat registry: registry lookup + Handler
	// dispatches to handleHivePredict. Catches a regression where the
	// stub Handler is still wired.
	fp := &chatFakePredictor{result: &predictor.Result{Files: []string{"x.go"}}}
	d := hivePredictTestSetup(t, fp)
	ctx := context.Background()
	repoPath := "/r"
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "x", Status: "pending"})

	reg := d.chatRegistry()
	tool, ok := reg.Get("hive_predict")
	if !ok {
		t.Fatal("hive_predict tool not registered")
	}
	result := tool.Handler(ctx, json.RawMessage(`{"task_id":"t1"}`))
	if result.IsError {
		t.Errorf("hive_predict should not error for valid task: %s", result.Content)
	}
	if strings.Contains(result.Content, "not available in this version") {
		t.Errorf("hive_predict still returning stub error: %s", result.Content)
	}
}

func TestChatSystemPromptDoesNotMarkPredictUnimplemented(t *testing.T) {
	// chatSystemPrefix should NOT list hive_predict as unimplemented.
	// hive_resume is also now a real handler (T2), not a stub.
	lines := strings.Split(chatSystemPrefix, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Not yet implemented") || strings.Contains(line, "not available in this version") {
			if strings.Contains(line, "hive_predict") {
				t.Errorf("system prompt still marks hive_predict as unimplemented: %q", line)
			}
		}
	}
}

func TestHiveResumeToolRegisteredAndDispatchable(t *testing.T) {
	// End-to-end through the chat registry. Catches a regression
	// where the stub Handler is still wired.
	d := newTestDaemon(t)
	t.Cleanup(func() { d.wg.Wait() }) // resume spawns a goroutine
	ctx := context.Background()
	repoPath := t.TempDir()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "x", Status: "needs_attention"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "needs_attention"}); err != nil {
		t.Fatal(err)
	}
	// Create the worktree dir on disk so the os.Stat eligibility check passes.
	wt := filepath.Join(d.HiveDir(), "worktrees", "r1")
	_ = os.MkdirAll(wt, 0700)

	reg := d.chatRegistry()
	tool, ok := reg.Get("hive_resume")
	if !ok {
		t.Fatal("hive_resume tool not registered")
	}
	result := tool.Handler(ctx, json.RawMessage(`{"run_id":"r1"}`))
	if result.IsError {
		t.Errorf("hive_resume should not error for eligible run: %s", result.Content)
	}
	if strings.Contains(result.Content, "not available in this version") {
		t.Errorf("hive_resume still returning stub error: %s", result.Content)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(result.Content), &body)
	if body["resumed"] != true || body["run_id"] != "r1" {
		t.Errorf("response body wrong: %s", result.Content)
	}
}

func TestHiveResumeToolIsMutating(t *testing.T) {
	d := newTestDaemon(t)
	reg := d.chatRegistry()
	tool, ok := reg.Get("hive_resume")
	if !ok {
		t.Fatal("hive_resume tool not registered")
	}
	if !tool.Mutating {
		t.Errorf("hive_resume.Mutating=false; want true so the confirm gate fires")
	}
}

func TestHiveResumeToolReturnsToolErrOnIneligible(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	repoPath := t.TempDir()
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "x", Status: "done"})
	_ = d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "done"})

	reg := d.chatRegistry()
	tool, _ := reg.Get("hive_resume")
	result := tool.Handler(ctx, json.RawMessage(`{"run_id":"r1"}`))
	if !result.IsError {
		t.Errorf("expected IsError=true on ineligible run")
	}
	if !strings.Contains(result.Content, "not resumable") {
		t.Errorf("content=%q, want 'not resumable' substring", result.Content)
	}
}

func TestChatSystemPromptHasNoNotYetImplementedLine(t *testing.T) {
	// Both hive_predict (item #4) and hive_resume (item #3) are now
	// real. The "Not yet implemented" line in the system prompts is
	// dead text and should be gone.
	if strings.Contains(chatSystemPrefix, "Not yet implemented") {
		t.Errorf("chatSystemPrefix still contains 'Not yet implemented' line")
	}
}

// recordingPlannerAgent is a chat.Agent stub that records what its factory
// was called with so the planner-routing test can assert the daemon picked
// the planner agent over the regular chat agent.
type recordingPlannerAgent struct {
	gotSlug string
	gotCwd  string
}

func (a *recordingPlannerAgent) Send(_ context.Context, _ *chat.Conversation, _ string, emit func(chat.Frame)) error {
	emit(chat.Frame{Kind: "text", Text: "planner-ack:" + a.gotSlug + ":" + a.gotCwd})
	emit(chat.Frame{Kind: "turn_done"})
	return nil
}

// TestChatSessionCreatedWithKindAndSlugFromStartParams asserts streamChat
// persists chat_sessions.kind + project_slug from the chat.send envelope on
// first-call session creation (Phase 8.A T6).
func TestChatSessionCreatedWithKindAndSlugFromStartParams(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	repoPath := t.TempDir()
	if err := st.InsertProject(ctx, &store.Project{ID: "proj-1", Slug: "myapp", Name: "MyApp", RepoPath: &repoPath, Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{Chat: config.Chat{Provider: "api"}}}}
	// Inject a stub planner agent so streamChat doesn't fail looking for one.
	d.SetPlannerAgentFor(func(slug, cwd string) (chat.Agent, error) {
		return &recordingPlannerAgent{gotSlug: slug, gotCwd: cwd}, nil
	})
	s := &RPCServer{d: d}

	cConn, sConn := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, cConn) }()
	defer cConn.Close()
	defer sConn.Close()

	params := json.RawMessage(`{"message":"begin","session_id":"","kind":"plan","project_slug":"myapp"}`)
	go s.streamChat(ctx, sConn, "rid", params)
	// Wait long enough for InsertChatSession + a streamed frame.
	time.Sleep(150 * time.Millisecond)

	sessions, err := st.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].Kind != store.KindPlan {
		t.Errorf("Kind=%q, want %q", sessions[0].Kind, store.KindPlan)
	}
	if sessions[0].ProjectSlug != "myapp" {
		t.Errorf("ProjectSlug=%q, want %q", sessions[0].ProjectSlug, "myapp")
	}
}

// TestChatSendRoutesPlannerSessionToPlannerAgent asserts a session whose
// row has Kind="plan" routes streamChat through the injected planner
// factory (not the default chatAgent), with the project's repo_path as cwd.
func TestChatSendRoutesPlannerSessionToPlannerAgent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	repoPath := t.TempDir()
	if err := st.InsertProject(ctx, &store.Project{ID: "proj-1", Slug: "myapp", Name: "MyApp", RepoPath: &repoPath, Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	sessionID := "chat-planner-1"
	if err := st.InsertChatSession(ctx, &store.ChatSession{ID: sessionID, Surface: "cli", Kind: store.KindPlan, ProjectSlug: "myapp"}); err != nil {
		t.Fatalf("InsertChatSession: %v", err)
	}

	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{Chat: config.Chat{Provider: "api"}}}}
	// Default chat agent should NOT be invoked for a planner session.
	d.chatAgent = chatStubAgent{frames: []chat.Frame{{Kind: "text", Text: "default-agent-leaked"}}}

	captured := &recordingPlannerAgent{}
	d.SetPlannerAgentFor(func(slug, cwd string) (chat.Agent, error) {
		captured.gotSlug = slug
		captured.gotCwd = cwd
		return captured, nil
	})

	s := &RPCServer{d: d}
	cConn, sConn := net.Pipe()

	type framesOrErr struct {
		frames []chat.Frame
		err    error
	}
	done := make(chan framesOrErr, 1)
	go func() {
		var got []chat.Frame
		sc := bufio.NewScanner(cConn)
		for sc.Scan() {
			var resp rpc.Response[chat.Frame]
			if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
				done <- framesOrErr{err: err}
				return
			}
			if resp.Result != nil {
				got = append(got, *resp.Result)
			}
		}
		done <- framesOrErr{frames: got}
	}()

	params, _ := json.Marshal(map[string]any{"session_id": sessionID, "message": "hi"})
	go func() {
		s.streamChat(ctx, sConn, "rid", json.RawMessage(params))
		sConn.Close()
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("decode frames: %v", res.err)
		}
		// Look for the planner-ack frame proving captured agent ran.
		foundAck := false
		leaked := false
		for _, f := range res.frames {
			if f.Kind == "text" && strings.HasPrefix(f.Text, "planner-ack:") {
				foundAck = true
			}
			if f.Kind == "text" && f.Text == "default-agent-leaked" {
				leaked = true
			}
		}
		if !foundAck {
			t.Errorf("planner agent was not invoked; frames=%+v", res.frames)
		}
		if leaked {
			t.Errorf("default chat agent ran on a planner session")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for streamChat frames")
	}

	if captured.gotSlug != "myapp" {
		t.Errorf("planner factory slug=%q, want %q", captured.gotSlug, "myapp")
	}
	if captured.gotCwd != repoPath {
		t.Errorf("planner factory cwd=%q, want %q", captured.gotCwd, repoPath)
	}
}

// TestBuildPlannerSDKAgentUsesPlannerRegistryAndForceSonnet asserts the
// composition-root SDK planner builder wires the planner registry (so
// hive_save_roadmap is callable) and a ForceSonnet router (so every turn
// goes to Sonnet without classify). System prompt is asserted to contain
// the planner header so the model knows its mode.
func TestBuildPlannerSDKAgentUsesPlannerRegistryAndForceSonnet(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key-not-used") // BuildPlannerSDKAgent reads it but never calls
	d := &Daemon{
		store: newTestStore(t),
		cfg: Config{HiveDir: t.TempDir(), Cfg: &config.Config{Chat: config.Chat{
			Provider:       "api",
			DefaultModel:   "claude-haiku-4-5",
			ReasoningModel: "claude-sonnet-4-5",
			MaxIters:       8,
		}}},
	}
	// BuildPlannerSDKAgent now resolves the project's integration feature branch
	// via d.scheduler.effectiveFeatureBranchForProject — wire a scheduler so the
	// lookup (empty branch here) doesn't nil-panic.
	d.scheduler = NewScheduler(d)
	cwd := t.TempDir()
	a, err := d.BuildPlannerSDKAgent("myapp", cwd)
	if err != nil {
		t.Fatalf("BuildPlannerSDKAgent: %v", err)
	}
	sdk, ok := a.(*chat.SDKAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *chat.SDKAgent", a)
	}
	// Reflect-free assertions via the agent's exported config + registry
	// shape (use chat package helpers where available).
	prompt := sdk.SystemPrefixForTest()
	if !strings.Contains(prompt, "Hive Roadmap Planner") {
		t.Errorf("planner system prompt missing 'Hive Roadmap Planner' header; got: %s", prompt)
	}
	if !strings.Contains(prompt, "myapp") {
		t.Errorf("planner system prompt missing project slug 'myapp'; got: %s", prompt)
	}
	reg := sdk.RegistryForTest()
	if _, ok := reg.Get("hive_save_roadmap"); !ok {
		t.Errorf("planner registry missing hive_save_roadmap tool")
	}
	if _, ok := reg.Get("hive_list_specs"); !ok {
		t.Errorf("planner registry missing hive_list_specs tool")
	}
	// Inherited from base chat registry (composition):
	if _, ok := reg.Get("hive_status"); !ok {
		t.Errorf("planner registry should inherit base chat read tools (hive_status missing)")
	}
	router := sdk.RouterForTest()
	if !router.ForceSonnet {
		t.Errorf("planner router ForceSonnet=false; want true")
	}
}

// TestHandleChatToolDispatchesPlannerToolForPlanSession asserts that
// chat.tool RPC routes planner-named tools through the planner registry
// when the session's Kind is "plan" (Phase 8.A T6b). Without this routing
// the CC chat-tools MCP server would forward hive_list_specs / hive_read_doc
// / hive_save_roadmap / hive_save_spec to the regular chat registry and get
// "unknown chat tool" back.
func TestHandleChatToolDispatchesPlannerToolForPlanSession(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	repoPath := t.TempDir()
	if err := st.InsertProject(ctx, &store.Project{ID: "p1", Slug: "myapp", Name: "MyApp", RepoPath: &repoPath, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	sessionID := "chat-planner-tool-1"
	if err := st.InsertChatSession(ctx, &store.ChatSession{ID: sessionID, Surface: "cli", Kind: store.KindPlan, ProjectSlug: "myapp"}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: st, cfg: Config{HiveDir: t.TempDir(), Cfg: &config.Config{}}}
	// plannerRegistryForSession resolves the project's integration feature branch
	// via d.scheduler — wire one so the (empty-branch) lookup doesn't nil-panic.
	d.scheduler = NewScheduler(d)
	s := &RPCServer{d: d}

	// hive_list_specs is a planner READ tool that scans <cwd>/docs/superpowers/specs/.
	// The fresh repo path has no specs dir, so the handler returns an empty
	// list — that's a successful, non-error tool result. The point of this
	// test is to prove the tool DISPATCHES (not "unknown chat tool" rejection).
	params, _ := json.Marshal(ChatToolParams{
		Tool:      "hive_list_specs",
		SessionID: sessionID,
		Input:     json.RawMessage(`{"project_slug":"myapp"}`),
	})
	raw, rerr := s.handleChatTool(ctx, params)
	if rerr != nil {
		t.Fatalf("handleChatTool(hive_list_specs) returned RPC error: %+v", rerr)
	}
	var res struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.IsError {
		t.Errorf("hive_list_specs is_error=true; content=%q", res.Content)
	}
	if !strings.Contains(res.Content, "specs") && !strings.Contains(res.Content, "[]") {
		// Handler returns either {"specs":[...]} or similar; any non-error JSON is fine.
		t.Errorf("hive_list_specs content suggests handler didn't run: %q", res.Content)
	}
}

// TestHandleChatToolPlannerToolUnknownInChatSession asserts that planner-only
// tool names are NOT dispatchable when the session is KindChat (or has no
// session row). This prevents planner write tools from being callable from a
// regular chat session — keeping the planner palette gated by session kind.
func TestHandleChatToolPlannerToolUnknownInChatSession(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	sessionID := "chat-regular-1"
	if err := st.InsertChatSession(ctx, &store.ChatSession{ID: sessionID, Surface: "cli", Kind: store.KindChat}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{}}}
	s := &RPCServer{d: d}

	params, _ := json.Marshal(ChatToolParams{
		Tool:      "hive_save_roadmap",
		SessionID: sessionID,
		Input:     json.RawMessage(`{"project_slug":"x","content":"y"}`),
	})
	_, rerr := s.handleChatTool(ctx, params)
	if rerr == nil {
		t.Fatal("expected RPC error for planner tool in chat session; got nil")
	}
	if !strings.Contains(rerr.Message, "unknown chat tool") {
		t.Errorf("error message=%q, want 'unknown chat tool'", rerr.Message)
	}
}

// TestChatSendRejectsUnknownKind asserts streamChat refuses to create a
// session whose Kind is neither "" nor one of the known constants
// (KindChat/KindPlan). Without this guard a typo ("plann") would be
// persisted as-is and the session would silently route to the default
// chat agent (Kind != KindPlan fails the planner-routing check).
func TestChatSendRejectsUnknownKind(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	d := &Daemon{store: st, cfg: Config{Cfg: &config.Config{Chat: config.Chat{Provider: "api"}}}}
	s := &RPCServer{d: d}

	cConn, sConn := net.Pipe()

	type framesOrErr struct {
		frames []chat.Frame
		err    error
	}
	done := make(chan framesOrErr, 1)
	go func() {
		var got []chat.Frame
		sc := bufio.NewScanner(cConn)
		for sc.Scan() {
			var resp rpc.Response[chat.Frame]
			if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
				done <- framesOrErr{err: err}
				return
			}
			if resp.Result != nil {
				got = append(got, *resp.Result)
			}
		}
		done <- framesOrErr{frames: got}
	}()

	params := json.RawMessage(`{"message":"hi","session_id":"","kind":"plann","project_slug":"x"}`)
	go func() {
		s.streamChat(ctx, sConn, "rid", params)
		sConn.Close()
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("decode frames: %v", res.err)
		}
		var sawErr bool
		for _, f := range res.frames {
			if f.Kind == "error" && strings.Contains(f.Text, "unknown kind") && strings.Contains(f.Text, "plann") {
				sawErr = true
			}
		}
		if !sawErr {
			t.Fatalf("expected an error frame mentioning 'unknown kind' and 'plann'; got frames=%+v", res.frames)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error frame")
	}

	// Session must NOT have been persisted.
	sessions, err := st.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("unknown-kind session persisted; sessions=%+v", sessions)
	}
}

// TestHiveDecomposeChatToolRegistered verifies the hive_decompose chat tool
// is registered with Mutating=true (so the existing confirm gate fires
// before the handler runs) and a non-nil Handler. The handler itself runs
// both task.decompose AND task.decompose_apply atomically — the user's y/n
// on the tool_proposed frame approves the IDEA of decomposing, not a
// specific breakdown.
func TestHiveDecomposeChatToolRegistered(t *testing.T) {
	d := newTestDaemon(t)
	reg := d.chatRegistry()
	tool, ok := reg.Get("hive_decompose")
	if !ok {
		t.Fatal("hive_decompose tool not registered")
	}
	if !tool.Mutating {
		t.Errorf("hive_decompose should be Mutating=true so the confirm gate fires")
	}
	if tool.Handler == nil {
		t.Error("hive_decompose Handler is nil")
	}
}
