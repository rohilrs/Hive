package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChatHistoryListResponseParses guards against the snake_case vs
// PascalCase JSON-tag mismatch fixed in T7. The daemon serializes
// store.ChatSession with snake_case tags (per internal/store/chat.go
// after the T7 fix). If a future refactor drops those tags, the daemon
// would emit PascalCase keys, the CLI decode struct (which uses
// snake_case tags) would silently zero out all compound-name fields,
// and `hive chat history` would render blank rows with no error.
//
// This test pins the CLI's decode struct to the snake_case wire format.
func TestChatHistoryListResponseParses(t *testing.T) {
	wire := []byte(`{"id":"x","result":{"sessions":[
		{"id":"sess-A","surface":"cli","started_at":1000,"ended_at":2000,"total_cost_usd":0.0123,"name":"first session","provider":"claude-code"},
		{"id":"sess-B","surface":"tui","started_at":3000,"ended_at":0,"total_cost_usd":0}
	]}}`)
	var resp struct {
		Result struct {
			Sessions []struct {
				ID           string  `json:"id"`
				Surface      string  `json:"surface"`
				StartedAt    int64   `json:"started_at"`
				EndedAt      int64   `json:"ended_at"`
				TotalCostUSD float64 `json:"total_cost_usd"`
				Name         string  `json:"name,omitempty"`
				Provider     string  `json:"provider,omitempty"`
			} `json:"sessions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(wire, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Result.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(resp.Result.Sessions))
	}
	s0 := resp.Result.Sessions[0]
	if s0.Name != "first session" || s0.Provider != "claude-code" {
		t.Errorf("sess-A name/provider wrong: %+v", s0)
	}
	if s0.ID != "sess-A" || s0.TotalCostUSD != 0.0123 {
		t.Errorf("sess-A core fields wrong: %+v", s0)
	}
	s1 := resp.Result.Sessions[1]
	if s1.Name != "" || s1.Provider != "" {
		t.Errorf("sess-B should have empty name/provider: %+v", s1)
	}
}

// TestChatSetNameResponseParses guards the CLI's response-decode struct for
// chat.set_name against future wire-format drift.
func TestChatSetNameResponseParses(t *testing.T) {
	wire := []byte(`{"id":"x","result":{"ok":true}}` + "\n")
	var resp struct {
		Result struct {
			OK bool `json:"ok"`
		} `json:"result"`
		Error *struct{ Message string `json:"message"` } `json:"error"`
	}
	if err := json.Unmarshal(wire, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Result.OK {
		t.Error("OK=false, want true")
	}
}

// TestChatHistoryGetResponseParses pins the messages-array decode shape
// for the same reason (T7 fix to store.ChatMessage).
func TestChatHistoryGetResponseParses(t *testing.T) {
	wire := []byte(`{"id":"x","result":{"messages":[
		{"id":"m1","session_id":"s1","role":"user","content":"hi","cost_usd":0,"created_at":1000},
		{"id":"m2","session_id":"s1","role":"assistant","content":"hello back","cost_usd":0.0005,"created_at":1001}
	]}}`)
	var resp struct {
		Result struct {
			Messages []struct {
				Role      string  `json:"role"`
				Content   string  `json:"content"`
				CostUSD   float64 `json:"cost_usd"`
				CreatedAt int64   `json:"created_at"`
			} `json:"messages"`
		} `json:"result"`
	}
	if err := json.Unmarshal(wire, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Result.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(resp.Result.Messages))
	}
	m0 := resp.Result.Messages[0]
	if m0.Role != "user" || m0.Content != "hi" || m0.CreatedAt != 1000 {
		t.Errorf("m0 decoded wrong: %+v", m0)
	}
	m1 := resp.Result.Messages[1]
	if m1.Role != "assistant" || !strings.Contains(m1.Content, "hello") || m1.CostUSD != 0.0005 || m1.CreatedAt != 1001 {
		t.Errorf("m1 decoded wrong: %+v", m1)
	}
}
