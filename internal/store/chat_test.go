package store

import (
	"context"
	"errors"
	"testing"
)

func TestChatSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sess := &ChatSession{ID: "sess-1", Surface: "cli"}
	if err := s.InsertChatSession(ctx, sess); err != nil {
		t.Fatalf("InsertChatSession: %v", err)
	}
	if sess.StartedAt == 0 {
		t.Errorf("StartedAt not stamped")
	}

	msgs := []*ChatMessage{
		{ID: "m1", SessionID: "sess-1", Role: "user", Content: "hello"},
		{
			ID: "m2", SessionID: "sess-1", Role: "assistant",
			Content: "calling a tool", ToolCalls: `[{"name":"list_runs"}]`,
			TokensIn: 100, TokensOut: 20, CostUSD: 0.004,
		},
		{
			ID: "m3", SessionID: "sess-1", Role: "tool",
			ToolResults: `[{"output":"ok"}]`,
		},
	}
	for _, m := range msgs {
		if err := s.AppendChatMessage(ctx, m); err != nil {
			t.Fatalf("AppendChatMessage %s: %v", m.ID, err)
		}
	}

	got, err := s.GetChatMessages(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetChatMessages: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if got[0].ID != "m1" || got[1].ID != "m2" || got[2].ID != "m3" {
		t.Errorf("messages out of order: %s %s %s", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Role != "user" || got[0].Content != "hello" {
		t.Errorf("m1 fields: role=%q content=%q", got[0].Role, got[0].Content)
	}
	if got[1].ToolCalls != `[{"name":"list_runs"}]` {
		t.Errorf("m2 tool_calls=%q", got[1].ToolCalls)
	}
	if got[1].TokensIn != 100 || got[1].TokensOut != 20 || got[1].CostUSD != 0.004 {
		t.Errorf("m2 usage: in=%d out=%d cost=%v", got[1].TokensIn, got[1].TokensOut, got[1].CostUSD)
	}
	if got[2].ToolResults != `[{"output":"ok"}]` {
		t.Errorf("m3 tool_results=%q", got[2].ToolResults)
	}
	if got[0].ToolCalls != "" || got[0].ToolResults != "" {
		t.Errorf("m1 should have empty tool fields, got calls=%q results=%q", got[0].ToolCalls, got[0].ToolResults)
	}

	if err := s.EndChatSession(ctx, "sess-1", 0.012); err != nil {
		t.Fatalf("EndChatSession: %v", err)
	}

	sessions, err := s.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].ID != "sess-1" || sessions[0].Surface != "cli" {
		t.Errorf("session fields: id=%q surface=%q", sessions[0].ID, sessions[0].Surface)
	}
	if sessions[0].EndedAt == 0 {
		t.Errorf("EndedAt should be set after EndChatSession")
	}
	if sessions[0].TotalCostUSD != 0.012 {
		t.Errorf("TotalCostUSD=%v want 0.012", sessions[0].TotalCostUSD)
	}
}

func TestInsertReadChatSessionWithNameProvider(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertChatSession(ctx, &ChatSession{
		ID: "s1", Surface: "cli", Name: "smoke run", Provider: "claude-code",
	}); err != nil {
		t.Fatal(err)
	}
	sessions, err := s.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].Name != "smoke run" || sessions[0].Provider != "claude-code" {
		t.Errorf("session=%+v", sessions[0])
	}
}

func TestSetChatSessionName(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertChatSession(ctx, &ChatSession{ID: "s1", Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChatSessionName(ctx, "s1", "renamed"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChatSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" {
		t.Errorf("Name=%q, want 'renamed'", got.Name)
	}
}

func TestSetChatSessionNameMissingIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.SetChatSessionName(ctx, "nope", "x")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v, want ErrNotFound", err)
	}
}

func TestGetChatSessionMissingIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, err = s.GetChatSession(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v, want ErrNotFound", err)
	}
}

func TestInsertChatSessionPersistsKind(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cs := &ChatSession{ID: "s-plan-1", Surface: "cli", Kind: KindPlan}
	if err := s.InsertChatSession(ctx, cs); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetChatSession(ctx, "s-plan-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != KindPlan {
		t.Fatalf("kind: got %q, want %q", got.Kind, KindPlan)
	}
}

func TestChatSessionKindDefaultsToChat(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cs := &ChatSession{ID: "s-default-1", Surface: "cli"} // Kind unset
	if err := s.InsertChatSession(ctx, cs); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetChatSession(ctx, "s-default-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != KindChat {
		t.Fatalf("default kind: got %q, want %q", got.Kind, KindChat)
	}
}

func TestChatProviderSession(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertChatSession(ctx, &ChatSession{ID: "sess-p", Surface: "cli"}); err != nil {
		t.Fatalf("InsertChatSession: %v", err)
	}

	// No provider session set yet -> "".
	got, err := s.GetChatProviderSession(ctx, "sess-p")
	if err != nil {
		t.Fatalf("GetChatProviderSession (empty): %v", err)
	}
	if got != "" {
		t.Errorf("provider session = %q, want empty", got)
	}

	if err := s.SetChatProviderSession(ctx, "sess-p", "claude-sess-1"); err != nil {
		t.Fatalf("SetChatProviderSession: %v", err)
	}
	got, err = s.GetChatProviderSession(ctx, "sess-p")
	if err != nil {
		t.Fatalf("GetChatProviderSession: %v", err)
	}
	if got != "claude-sess-1" {
		t.Errorf("provider session = %q, want claude-sess-1", got)
	}
}

func TestDeleteChatSessionRemovesSessionAndMessages(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Two sessions; only the first gets deleted. Confirms the WHERE
	// clauses don't over-delete.
	if err := s.InsertChatSession(ctx, &ChatSession{ID: "keep", Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChatSession(ctx, &ChatSession{ID: "doomed", Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"keep", "doomed"} {
		for _, mid := range []string{sid + "-m1", sid + "-m2"} {
			if err := s.AppendChatMessage(ctx, &ChatMessage{
				ID: mid, SessionID: sid, Role: "user", Content: "hi",
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := s.DeleteChatSession(ctx, "doomed"); err != nil {
		t.Fatalf("DeleteChatSession: %v", err)
	}

	// Session row gone
	if _, err := s.GetChatSession(ctx, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for deleted session, got %v", err)
	}
	// Messages gone
	got, err := s.GetChatMessages(ctx, "doomed")
	if err != nil {
		t.Fatalf("GetChatMessages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("messages survived delete: %d", len(got))
	}
	// Kept session intact
	if _, err := s.GetChatSession(ctx, "keep"); err != nil {
		t.Errorf("kept session lost: %v", err)
	}
	keptMsgs, _ := s.GetChatMessages(ctx, "keep")
	if len(keptMsgs) != 2 {
		t.Errorf("kept session lost messages: %d", len(keptMsgs))
	}
}

// TestEndChatSessionAddsCostDelta verifies that EndChatSession accumulates
// per-turn deltas rather than overwriting the total. A session pre-seeded with
// total_cost_usd = 0.5 should reach 0.8 after a 0.3 delta, not 0.3.
func TestEndChatSessionAddsCostDelta(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Insert a session with a pre-existing cost (simulates a resumed session
	// that already accumulated cost across prior turns).
	if err := s.InsertChatSession(ctx, &ChatSession{ID: "sess-delta", Surface: "cli", TotalCostUSD: 0.5}); err != nil {
		t.Fatalf("InsertChatSession: %v", err)
	}

	// Simulate two subsequent turns: first adds 0.3, second adds 0.2.
	if err := s.EndChatSession(ctx, "sess-delta", 0.3); err != nil {
		t.Fatalf("EndChatSession (turn 1): %v", err)
	}
	if err := s.EndChatSession(ctx, "sess-delta", 0.2); err != nil {
		t.Fatalf("EndChatSession (turn 2): %v", err)
	}

	got, err := s.GetChatSession(ctx, "sess-delta")
	if err != nil {
		t.Fatalf("GetChatSession: %v", err)
	}
	const want = 1.0 // 0.5 + 0.3 + 0.2
	if got.TotalCostUSD != want {
		t.Errorf("TotalCostUSD = %v, want %v (additive semantics broken — got overwrite?)", got.TotalCostUSD, want)
	}
}

func TestDeleteChatSessionMissingIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.DeleteChatSession(ctx, "never-existed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestReapStaleChatSessions verifies that ReapStaleChatSessions closes open
// sessions older than the threshold while leaving newer open sessions and
// already-ended sessions untouched.
func TestReapStaleChatSessions(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := int64(1_000_000)
	staleThreshold := now - 3600 // 1 hour ago

	// stale-open: started 2 hours ago, still open → should be reaped
	staleOpen := &ChatSession{ID: "stale-open", Surface: "cli", StartedAt: now - 7200}
	if err := s.InsertChatSession(ctx, staleOpen); err != nil {
		t.Fatal(err)
	}

	// fresh-open: started 30 minutes ago, still open → must NOT be reaped
	freshOpen := &ChatSession{ID: "fresh-open", Surface: "cli", StartedAt: now - 1800}
	if err := s.InsertChatSession(ctx, freshOpen); err != nil {
		t.Fatal(err)
	}

	// already-ended: started 2 hours ago but already ended → must NOT be touched
	alreadyEnded := &ChatSession{ID: "already-ended", Surface: "cli", StartedAt: now - 7200}
	if err := s.InsertChatSession(ctx, alreadyEnded); err != nil {
		t.Fatal(err)
	}
	if err := s.EndChatSession(ctx, "already-ended", 0.0); err != nil {
		t.Fatal(err)
	}

	n, err := s.ReapStaleChatSessions(ctx, staleThreshold)
	if err != nil {
		t.Fatalf("ReapStaleChatSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped %d rows, want 1", n)
	}

	// stale-open should now be ended
	got, err := s.GetChatSession(ctx, "stale-open")
	if err != nil {
		t.Fatalf("GetChatSession stale-open: %v", err)
	}
	if got.EndedAt == 0 {
		t.Errorf("stale-open: EndedAt still 0 after reap")
	}

	// fresh-open must still be open
	got, err = s.GetChatSession(ctx, "fresh-open")
	if err != nil {
		t.Fatalf("GetChatSession fresh-open: %v", err)
	}
	if got.EndedAt != 0 {
		t.Errorf("fresh-open: EndedAt=%d, want 0 (should not have been reaped)", got.EndedAt)
	}

	// already-ended must not have changed its ended_at
	got, err = s.GetChatSession(ctx, "already-ended")
	if err != nil {
		t.Fatalf("GetChatSession already-ended: %v", err)
	}
	if got.EndedAt == 0 {
		t.Errorf("already-ended: EndedAt is 0, should have been set by EndChatSession")
	}
}
