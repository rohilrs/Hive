// Package mcphttp implements the MCP (Model Context Protocol) Streamable
// HTTP transport for the Hive daemon. It exposes JSON-RPC 2.0 over POST
// with optional SSE upgrade on Accept: text/event-stream.
//
// Three MCP methods are supported: initialize, tools/list, tools/call.
// Empty replies are returned for prompts/list and resources/list so
// probing clients don't error.
package mcphttp

import (
	"context"
	"encoding/json"
)

// JSON-RPC error codes used in MCP. The MCP-specific codes (-32000+)
// are server-defined per the JSON-RPC 2.0 spec.
const (
	ErrParseError       = -32700
	ErrInvalidRequest   = -32600
	ErrMethodNotFound   = -32601
	ErrInvalidParams    = -32602
	ErrInternalError    = -32603
	ErrServerCustomBase = -32000 // first custom server error code
)

// MCP method names. Constants keep callers free of typos.
const (
	MethodInitialize    = "initialize"
	MethodToolsList     = "tools/list"
	MethodToolsCall     = "tools/call"
	MethodPromptsList   = "prompts/list"
	MethodResourcesList = "resources/list"
)

// Request is a JSON-RPC 2.0 request envelope. The id field is a raw
// message so we can echo it verbatim — JSON-RPC allows string, number,
// or null ids.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IntID returns the request id as an int64 when it's numeric; returns 0
// when the id is a string, null, or missing. Used only when callers
// don't need full echo fidelity (most don't — they pass req.ID through).
func (r *Request) IntID() int64 {
	if len(r.ID) == 0 {
		return 0
	}
	var n int64
	if err := json.Unmarshal(r.ID, &n); err == nil {
		return n
	}
	return 0
}

// Response is a JSON-RPC 2.0 success response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

// ErrorResponse is a JSON-RPC 2.0 error response. Exactly one of
// Result / Error is set in a real reply — kept as separate types to
// make that invariant explicit.
type ErrorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   RPCError        `json:"error"`
}

// RPCError is the JSON-RPC error payload.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ToolSpec is the MCP shape advertised in tools/list. The Hive registry
// types (chat.Tool, etc.) map into this shape at registration time.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// RouteContext carries URL path parameters into the tool handler. Kind
// distinguishes the three route shapes; the other fields are populated
// per route ("" when not applicable for that kind).
type RouteContext struct {
	Kind      string // "chat" | "stage" | "perm"
	SessionID string // for chat
	RunID     string // for stage / perm
	Stage     string // for stage / perm
}

// ToolHandler executes a tool call. Input is the raw JSON payload from
// the model. The handler returns either a content string (JSON-serialized
// to text content per MCP) or an error.
//
// IsError signals tool-level errors (which MCP returns as a content
// block with isError=true) vs protocol errors (which become RPCError).
// Most handlers return IsError=false; set true when the tool itself
// reports a failure to the model.
type ToolHandler func(ctx context.Context, rctx RouteContext, toolName string, input json.RawMessage) (content string, isError bool, err error)

// Route is one mounted route's tool advertisement + dispatch fan-out.
// The Handler switches on tool name internally; we keep the signature
// flat so adding a new tool is a one-line registration.
//
// Tools is the static fallback advertised when no ToolsFor closure is set
// (the stage + perm routes use this path).
//
// ToolsFor, when non-nil, takes precedence: it is invoked per-request with
// the RouteContext so the chat route can advertise different tools for
// different session kinds (e.g. planner-kind chat sessions see planner
// tools instead of the default chat tools — Phase 8.A T8). Both tools/list
// and the tools/call known-name check use ToolsFor when set.
type Route struct {
	Tools    []ToolSpec
	ToolsFor func(rctx RouteContext) []ToolSpec
	Handler  ToolHandler
}
