// Package claudecode is the v1 Adapter implementation backed by the
// Claude Code CLI (`claude -p`). It encapsulates everything specific to
// CC: HOME-redirect skill scoping, JSONL stream parsing, stdio MCP
// bridge to the verdict UDS listener.
package claudecode

import "encoding/json"

type EventType string

const (
	EventSystem     EventType = "system"
	EventText       EventType = "text"
	EventToolUse    EventType = "tool_use"
	EventToolResult EventType = "tool_result"
	EventResult     EventType = "result"
	EventError      EventType = "error"
)

// ContentBlock is one nested content block inside a Message. Type is
// "text", "thinking", "tool_use", or "tool_result". The other fields
// apply per-type; unknown types leave them zero.
type ContentBlock struct {
	Type string `json:"type"`
	// text block field — real claude nests assistant prose here (mirrors
	// internal/llm/claudecli/client.go's blk.Text walk).
	Text string `json:"text,omitempty"`
	// tool_use fields
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// Content carries a tool_result block's output in the nested shape
	// ({"type":"tool_result",...,"content":...}). It is raw because real
	// claude emits it either as a JSON string or as an array of blocks.
	Content json.RawMessage `json:"content,omitempty"`
}

// Message is the nested Anthropic Messages API payload that real claude
// emits on assistant/user events. Tool calls in real claude live HERE,
// not at the top level — top-level Tool* fields cover the legacy
// fake-claude fixture shape.
type Message struct {
	Role    string         `json:"role,omitempty"`
	Content []ContentBlock `json:"content,omitempty"`
}

// Event is a tolerant parse of the JSONL events claude -p emits on
// stdout. Unknown shapes still produce a usable Event (Type + Raw set).
type Event struct {
	Type    EventType `json:"type"`
	Subtype string    `json:"subtype,omitempty"`

	SessionID string `json:"session_id,omitempty"`
	Model     string `json:"model,omitempty"`

	Delta string `json:"delta,omitempty"`
	Text  string `json:"text,omitempty"`

	ToolName  string          `json:"name,omitempty"`
	ToolInput json.RawMessage `json:"input,omitempty"`
	ToolID    string          `json:"id,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Output    string `json:"output,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	StopReason string `json:"stop_reason,omitempty"`
	Error      string `json:"error,omitempty"`

	// Result holds the final assistant text on a `result` event — claude's
	// stream-json terminal frame carries it in a top-level "result" field,
	// not "text". On a failed turn this is where the real reason often lands
	// (e.g. "Failed to authenticate. API Error: 401 ...").
	Result string `json:"result,omitempty"`

	// Message holds the nested Anthropic Messages API payload — set on
	// assistant/user events from real claude.
	Message Message `json:"message,omitempty"`

	// Usage holds token counts when the provider reports them on a
	// system/init or result event.
	Usage struct {
		InputTokens     int `json:"input_tokens,omitempty"`
		OutputTokens    int `json:"output_tokens,omitempty"`
		CacheReadTokens int `json:"cache_read_input_tokens,omitempty"`
	} `json:"usage,omitempty"`

	Raw json.RawMessage `json:"-"`
}
