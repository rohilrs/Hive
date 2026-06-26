package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/chat"
)

// fakeChatSessionStore records SetChatProviderSession calls so the test can
// assert the claude session id was captured for --resume continuity.
type fakeChatSessionStore struct {
	mu       sync.Mutex
	provider map[string]string // sessionID -> providerSessionID
	sets     [][2]string       // ordered (sessionID, providerSessionID) Set calls
}

func newFakeChatSessionStore() *fakeChatSessionStore {
	return &fakeChatSessionStore{provider: map[string]string{}}
}

func (f *fakeChatSessionStore) GetChatProviderSession(_ context.Context, sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.provider[sessionID], nil
}

func (f *fakeChatSessionStore) SetChatProviderSession(_ context.Context, sessionID, providerSessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provider[sessionID] = providerSessionID
	f.sets = append(f.sets, [2]string{sessionID, providerSessionID})
	return nil
}

func TestChatAgentSend(t *testing.T) {
	fake := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/chat_turn.jsonl")

	store := newFakeChatSessionStore()
	agent := NewChatAgent(Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixture},
		HiveBinary: "/unused/hive",
		RealHome:   realHome,
	}, "be concise", t.TempDir(), store)

	var (
		mu     sync.Mutex
		frames []chat.Frame
	)
	collect := func(f chat.Frame) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conv := &chat.Conversation{SessionID: "hive-1"}
	if err := agent.Send(ctx, conv, "status?", collect); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Assert: text frames, a tool_result frame, and a turn_done.
	var (
		gotText       string
		sawToolResult bool
		sawTurnDone   bool
	)
	for _, f := range frames {
		switch f.Kind {
		case "text":
			gotText += f.Text
		case "tool_result":
			sawToolResult = true
			if f.Result != `{"pending_tasks":2}` {
				t.Errorf("tool_result Result=%q want %q", f.Result, `{"pending_tasks":2}`)
			}
		case "turn_done":
			sawTurnDone = true
		case "error":
			t.Errorf("unexpected error frame: %q", f.Text)
		}
	}
	if gotText != "Let me check the status. You have 2 pending tasks." {
		t.Errorf("accumulated text=%q", gotText)
	}
	if !sawToolResult {
		t.Error("no tool_result frame emitted")
	}
	if !sawTurnDone {
		t.Error("no turn_done frame emitted")
	}

	// Assert the assistant message was appended to conv for persistence.
	if len(conv.Messages) != 1 {
		t.Fatalf("conv.Messages len=%d want 1", len(conv.Messages))
	}

	// Assert the claude session id was captured for --resume.
	store.mu.Lock()
	sets := append([][2]string(nil), store.sets...)
	store.mu.Unlock()
	if len(sets) != 1 {
		t.Fatalf("SetChatProviderSession calls=%d want 1: %v", len(sets), sets)
	}
	if sets[0] != [2]string{"hive-1", "fake-chat-sess-1"} {
		t.Errorf("SetChatProviderSession=%v want [hive-1 fake-chat-sess-1]", sets[0])
	}
}

// TestChatAgentScopeCacheReusedAcrossTurns verifies that getOrMaterializeScope
// returns the same *ScopeInfo pointer (by identity) on the second call for the
// same stageDir. This is the unit-level guard for the stable-scratch fix: if the
// cache weren't working, MaterializeScope would nuke .claude/projects/... between
// turns, which would destroy claude's --resume session state.
func TestChatAgentScopeCacheReusedAcrossTurns(t *testing.T) {
	realHome := makeFakeRealHome(t)
	scratchRoot := t.TempDir()
	agent := NewChatAgent(Config{
		Binary:     "claude",
		HiveBinary: "hive",
		RealHome:   realHome,
	}, "sys", scratchRoot, nil)

	stageDir := filepath.Join(scratchRoot, "sess-1")
	scope1, err := agent.getOrMaterializeScope(stageDir)
	if err != nil {
		t.Fatalf("first getOrMaterializeScope: %v", err)
	}
	scope2, err := agent.getOrMaterializeScope(stageDir)
	if err != nil {
		t.Fatalf("second getOrMaterializeScope: %v", err)
	}
	// Must be the exact same pointer — cache hit, not a second materialize.
	if scope1 != scope2 {
		t.Errorf("expected identical *ScopeInfo from cache; got %p vs %p", scope1, scope2)
	}
}

// TestChatAgentEvictSessionRemovesScopeAndScratch confirms EvictSession
// drops the per-session entry from scopeCache AND removes the on-disk
// dir. Idempotent on missing/unknown sessionIDs.
func TestChatAgentEvictSessionRemovesScopeAndScratch(t *testing.T) {
	realHome := makeFakeRealHome(t)
	scratchRoot := t.TempDir()
	agent := NewChatAgent(Config{
		Binary:     "claude",
		HiveBinary: "hive",
		RealHome:   realHome,
	}, "sys", scratchRoot, nil)

	stageDir := filepath.Join(scratchRoot, "sess-evict")
	if _, err := agent.getOrMaterializeScope(stageDir); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, ok := agent.scopeCache[stageDir]; !ok {
		t.Fatal("scopeCache miss after materialize")
	}
	if _, err := os.Stat(stageDir); err != nil {
		t.Fatalf("scratch dir not created: %v", err)
	}

	agent.EvictSession("sess-evict")

	if _, ok := agent.scopeCache[stageDir]; ok {
		t.Errorf("scopeCache entry survived EvictSession")
	}
	if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
		t.Errorf("scratch dir survived EvictSession: %v", err)
	}

	// Idempotent — second call on a now-gone session is a no-op.
	agent.EvictSession("sess-evict")
	agent.EvictSession("never-existed")
}

// TestChatAgentReapOrphansRemovesUnknownDirs confirms the startup reaper
// keeps known sessions, removes orphans, and silently handles a missing
// scratchRoot (the first-ever startup case).
func TestChatAgentReapOrphansRemovesUnknownDirs(t *testing.T) {
	realHome := makeFakeRealHome(t)
	scratchRoot := t.TempDir()
	agent := NewChatAgent(Config{
		Binary:     "claude",
		HiveBinary: "hive",
		RealHome:   realHome,
	}, "sys", scratchRoot, nil)

	// Seed: one "known" session, two orphans, one stray non-dir file
	// (must not crash the reaper).
	for _, sid := range []string{"keep", "orphan-1", "orphan-2"} {
		if err := os.MkdirAll(filepath.Join(scratchRoot, sid), 0700); err != nil {
			t.Fatal(err)
		}
	}
	strayFile := filepath.Join(scratchRoot, "stray.txt")
	if err := os.WriteFile(strayFile, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	known := map[string]bool{"keep": true}
	removed, err := agent.ReapOrphans(context.Background(), known)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(removed) != 2 {
		t.Errorf("removed=%v, want 2 orphans", removed)
	}
	if _, err := os.Stat(filepath.Join(scratchRoot, "keep")); err != nil {
		t.Errorf("known session reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scratchRoot, "orphan-1")); !os.IsNotExist(err) {
		t.Errorf("orphan-1 survived: %v", err)
	}
	if _, err := os.Stat(strayFile); err != nil {
		t.Errorf("stray non-dir file should not be touched: %v", err)
	}

	// Missing scratchRoot → no error, no removals.
	emptyAgent := NewChatAgent(Config{RealHome: realHome}, "", filepath.Join(t.TempDir(), "never-created"), nil)
	rem, err := emptyAgent.ReapOrphans(context.Background(), nil)
	if err != nil {
		t.Errorf("missing scratchRoot should not error: %v", err)
	}
	if len(rem) != 0 {
		t.Errorf("unexpected removals on missing scratchRoot: %v", rem)
	}
}

// TestChatAgentSendNestedShape is the regression guard for the REAL claude
// stream-json shape: assistant text + tool results nested inside
// {"type":"assistant","message":{"content":[...]}} rather than top-level
// {"type":"text",...} events. Before the fix the nested text never reached the
// user (no text frames, empty synthetic assistant message).
func TestChatAgentSendNestedShape(t *testing.T) {
	fake := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)
	fixture, _ := filepath.Abs("../../../scripts/fake-claude/fixtures/chat_turn_nested.jsonl")

	store := newFakeChatSessionStore()
	agent := NewChatAgent(Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixture},
		HiveBinary: "/unused/hive",
		RealHome:   realHome,
	}, "be concise", t.TempDir(), store)

	var (
		mu     sync.Mutex
		frames []chat.Frame
	)
	collect := func(f chat.Frame) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conv := &chat.Conversation{SessionID: "hive-nested"}
	if err := agent.Send(ctx, conv, "status?", collect); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var (
		gotText       string
		textFrames    int
		sawToolResult bool
		sawTurnDone   bool
	)
	for _, f := range frames {
		switch f.Kind {
		case "text":
			gotText += f.Text
			textFrames++
		case "tool_result":
			sawToolResult = true
			if f.Result != `{"pending_tasks":2}` {
				t.Errorf("tool_result Result=%q want %q", f.Result, `{"pending_tasks":2}`)
			}
			if f.Tool != "hive_list_tasks" {
				t.Errorf("tool_result Tool=%q want %q (should be resolved name, not id)", f.Tool, "hive_list_tasks")
			}
		case "turn_done":
			sawTurnDone = true
		case "error":
			t.Errorf("unexpected error frame: %q", f.Text)
		}
	}

	// The core regression assertion: nested assistant text DID reach the user.
	if textFrames == 0 {
		t.Fatal("no text frames emitted from nested assistant shape (regression)")
	}
	if gotText != "Let me check the status. You have 2 pending tasks." {
		t.Errorf("accumulated text=%q", gotText)
	}
	if !sawToolResult {
		t.Error("no tool_result frame emitted from nested shape")
	}
	if !sawTurnDone {
		t.Error("no turn_done frame emitted")
	}

	// The synthetic assistant message must be non-empty so persistence sees
	// a complete turn.
	if len(conv.Messages) != 1 {
		t.Fatalf("conv.Messages len=%d want 1", len(conv.Messages))
	}

	// The session id from the nested system/init was captured for --resume.
	store.mu.Lock()
	sets := append([][2]string(nil), store.sets...)
	store.mu.Unlock()
	if len(sets) != 1 {
		t.Fatalf("SetChatProviderSession calls=%d want 1: %v", len(sets), sets)
	}
	if sets[0] != [2]string{"hive-nested", "nested-chat-sess-1"} {
		t.Errorf("SetChatProviderSession=%v want [hive-nested nested-chat-sess-1]", sets[0])
	}
}

// TestChatAgentResolvesToolNameFromUseID is the targeted regression guard for
// the bug where tool_result frames carried block.ToolUseID (e.g. "toolu_abc")
// instead of the resolved tool name (e.g. "hive_list_projects"). The wire shape
// for tool_result blocks only carries the id; the name arrives on the earlier
// tool_use block. The fix maintains a per-Send id→name map and resolves on emit.
func TestChatAgentResolvesToolNameFromUseID(t *testing.T) {
	fake := buildFakeClaude(t)
	realHome := makeFakeRealHome(t)

	// Write a fixture that mimics real claude's nested shape: an assistant
	// tool_use (with both id AND name) followed by a user tool_result (id only).
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "tool_name_resolution.jsonl")
	fixtureContent := `{"type":"system","subtype":"init","session_id":"resolve-sess-1","model":"claude-sonnet-4-6"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_abc123","name":"hive_list_projects","input":{}}]}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc123","is_error":false,"content":"[]"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"No projects found."}]}}` + "\n" +
		`{"type":"result","subtype":"stop","stop_reason":"end_turn"}` + "\n"
	if err := os.WriteFile(fixturePath, []byte(fixtureContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := newFakeChatSessionStore()
	agent := NewChatAgent(Config{
		Binary:     fake,
		ExtraArgs:  []string{"-fixture", fixturePath},
		HiveBinary: "/unused/hive",
		RealHome:   realHome,
	}, "be concise", t.TempDir(), store)

	var (
		mu     sync.Mutex
		frames []chat.Frame
	)
	collect := func(f chat.Frame) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conv := &chat.Conversation{SessionID: "resolve-1"}
	if err := agent.Send(ctx, conv, "list projects", collect); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var sawNamedToolResult bool
	for _, f := range frames {
		if f.Kind != "tool_result" {
			continue
		}
		if f.Tool == "toolu_abc123" {
			t.Errorf("tool_result emitted with raw tool_use_id %q instead of resolved name", f.Tool)
		}
		if f.Tool == "hive_list_projects" {
			sawNamedToolResult = true
		}
	}
	if !sawNamedToolResult {
		t.Errorf("no tool_result frame had the resolved tool name %q", "hive_list_projects")
	}
}
