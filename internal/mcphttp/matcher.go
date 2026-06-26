package mcphttp

import "strings"

// MatchRoute parses an MCP HTTP URL path and returns the populated
// RouteContext. Returns ok=false for any path that doesn't conform to
// one of the three route templates:
//
//	/mcp/chat/<session-id>
//	/mcp/stage/<run-id>/<stage>
//	/mcp/perm/<run-id>/<stage>
//
// A single trailing slash is tolerated. Empty path params (e.g. "//")
// and extra segments past the template fail to match — keeps the route
// surface tight and makes typos in mcp.json fail fast.
func MatchRoute(path string) (RouteContext, bool) {
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}
	parts := strings.Split(path, "/")
	if len(parts) < 4 || parts[0] != "" || parts[1] != "mcp" {
		return RouteContext{}, false
	}
	switch parts[2] {
	case "chat":
		if len(parts) != 4 || parts[3] == "" {
			return RouteContext{}, false
		}
		return RouteContext{Kind: "chat", SessionID: parts[3]}, true
	case "stage":
		if len(parts) != 5 || parts[3] == "" || parts[4] == "" {
			return RouteContext{}, false
		}
		return RouteContext{Kind: "stage", RunID: parts[3], Stage: parts[4]}, true
	case "perm":
		if len(parts) != 5 || parts[3] == "" || parts[4] == "" {
			return RouteContext{}, false
		}
		return RouteContext{Kind: "perm", RunID: parts[3], Stage: parts[4]}, true
	}
	return RouteContext{}, false
}
