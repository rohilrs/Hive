package mcphttp

import (
	"testing"
)

func TestMatchChatRoute(t *testing.T) {
	rctx, ok := MatchRoute("/mcp/chat/sess-abc123")
	if !ok {
		t.Fatal("did not match")
	}
	if rctx.Kind != "chat" || rctx.SessionID != "sess-abc123" {
		t.Errorf("got %+v", rctx)
	}
}

func TestMatchStageRoute(t *testing.T) {
	rctx, ok := MatchRoute("/mcp/stage/run-42/implement")
	if !ok || rctx.Kind != "stage" || rctx.RunID != "run-42" || rctx.Stage != "implement" {
		t.Errorf("got %+v ok=%v", rctx, ok)
	}
}

func TestMatchPermRoute(t *testing.T) {
	rctx, ok := MatchRoute("/mcp/perm/run-42/review")
	if !ok || rctx.Kind != "perm" || rctx.RunID != "run-42" || rctx.Stage != "review" {
		t.Errorf("got %+v ok=%v", rctx, ok)
	}
}

func TestMatchTolerateTrailingSlash(t *testing.T) {
	rctx, ok := MatchRoute("/mcp/chat/sess-abc/")
	if !ok || rctx.SessionID != "sess-abc" {
		t.Errorf("trailing slash rejected: %+v ok=%v", rctx, ok)
	}
}

func TestMatchRejectMissingParam(t *testing.T) {
	for _, path := range []string{
		"/mcp/chat/",
		"/mcp/chat",
		"/mcp/stage/run-42",
		"/mcp/stage/run-42/",
		"/mcp/perm//implement",
	} {
		if _, ok := MatchRoute(path); ok {
			t.Errorf("path %q wrongly matched", path)
		}
	}
}

func TestMatchRejectExtraSegments(t *testing.T) {
	for _, path := range []string{
		"/mcp/chat/sess-abc/extra",
		"/mcp/stage/run-42/implement/extra",
	} {
		if _, ok := MatchRoute(path); ok {
			t.Errorf("path %q wrongly matched", path)
		}
	}
}

func TestMatchRejectUnknownPrefix(t *testing.T) {
	for _, path := range []string{
		"",
		"/",
		"/mcp",
		"/mcp/",
		"/mcp/unknown/x",
		"/other/chat/sess-abc",
	} {
		if _, ok := MatchRoute(path); ok {
			t.Errorf("path %q wrongly matched", path)
		}
	}
}

func TestMatchRejectDoubleSlash(t *testing.T) {
	if _, ok := MatchRoute("/mcp//chat/sess-abc"); ok {
		t.Error("double slash wrongly matched")
	}
}
