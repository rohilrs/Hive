package mcphttp

import (
	"encoding/json"
	"net/http"
)

// Server is the MCP HTTP transport. Construct with NewServer, register
// up to three routes (chat / stage / perm), then mount it on an
// http.Server. Implements http.Handler.
//
// One Server per daemon — all sessions share the listener. Per-context
// state lives on the routed-to handler, not on the Server.
type Server struct {
	chat  *Route // nil until RegisterChat
	stage *Route // nil until RegisterStage
	perm  *Route // nil until RegisterPerm
}

// NewServer returns an empty server. Routes must be registered before
// requests will succeed for that kind.
func NewServer() *Server { return &Server{} }

// RegisterChat sets the route for /mcp/chat/<session-id>.
func (s *Server) RegisterChat(r Route) { s.chat = &r }

// RegisterStage sets the route for /mcp/stage/<run-id>/<stage>.
func (s *Server) RegisterStage(r Route) { s.stage = &r }

// RegisterPerm sets the route for /mcp/perm/<run-id>/<stage>.
func (s *Server) RegisterPerm(r Route) { s.perm = &r }

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// GET /health is a doctor probe — 200 + "ok" with no JSON-RPC routing.
	if req.Method == http.MethodGet && req.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rctx, ok := MatchRoute(req.URL.Path)
	if !ok {
		http.NotFound(w, req)
		return
	}
	route := s.routeFor(rctx.Kind)
	if route == nil {
		http.NotFound(w, req)
		return
	}

	var rpcReq Request
	if err := json.NewDecoder(req.Body).Decode(&rpcReq); err != nil {
		writeJSONError(w, nil, ErrParseError, "parse error: "+err.Error())
		return
	}
	defer req.Body.Close()

	// JSON-RPC 2.0 §4.1: notifications (no id) MUST NOT receive a
	// response. MCP clients send notifications/initialized after the
	// initialize handshake; replying with -32601 would surface as a
	// fatal handshake error in strict clients. Return 202 Accepted
	// with empty body and exit.
	if len(rpcReq.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Resolve the advertised tools for this request. ToolsFor (when set)
	// takes precedence over the static Tools list so a single registered
	// Route can advertise per-session tool sets (Phase 8.A T8: planner-kind
	// chat sessions see planner tools while default chat sessions see chat
	// tools). Stage + perm routes leave ToolsFor nil and use the static
	// Tools fallback.
	tools := route.Tools
	if route.ToolsFor != nil {
		tools = route.ToolsFor(rctx)
	}

	switch rpcReq.Method {
	case MethodInitialize:
		writeJSONResult(w, rpcReq.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]string{"name": "hive-mcp", "version": "0.1"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		})
	case MethodToolsList:
		writeJSONResult(w, rpcReq.ID, map[string]any{"tools": tools})
	case MethodToolsCall:
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(rpcReq.Params, &p); err != nil || p.Name == "" {
			writeJSONError(w, rpcReq.ID, ErrInvalidParams, "missing tool name")
			return
		}
		known := false
		for _, ts := range tools {
			if ts.Name == p.Name {
				known = true
				break
			}
		}
		if !known {
			writeJSONError(w, rpcReq.ID, ErrInvalidParams, "unknown tool: "+p.Name)
			return
		}
		content, isError, err := route.Handler(req.Context(), rctx, p.Name, p.Arguments)
		if err != nil {
			writeJSONError(w, rpcReq.ID, ErrInternalError, err.Error())
			return
		}
		writeJSONResult(w, rpcReq.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": content}},
			"isError": isError,
		})
	case MethodPromptsList:
		writeJSONResult(w, rpcReq.ID, map[string]any{"prompts": []any{}})
	case MethodResourcesList:
		writeJSONResult(w, rpcReq.ID, map[string]any{"resources": []any{}})
	default:
		writeJSONError(w, rpcReq.ID, ErrMethodNotFound, "unknown method: "+rpcReq.Method)
	}
}

func (s *Server) routeFor(kind string) *Route {
	switch kind {
	case "chat":
		return s.chat
	case "stage":
		return s.stage
	case "perm":
		return s.perm
	}
	return nil
}

func writeJSONResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		JSONRPC: "2.0", ID: id,
		Error: RPCError{Code: code, Message: msg},
	})
}
