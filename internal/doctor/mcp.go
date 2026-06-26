package doctor

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// runMCPChecks probes the MCP HTTP listener via GET /health when the
// daemon reports it as enabled. M1 distinguishes:
//   - daemon down → skip (we have no listener address to probe)
//   - listener disabled (MCPHTTPListenerOK=false) → skip (intended state)
//   - listener enabled but probe fails → error (drift; daemon thinks it's
//     up but the listener isn't responding)
//   - probe 200 → ok
func runMCPChecks(ctx context.Context, hiveDir string, client RPCClient) []Check {
	if client == nil {
		return []Check{skipCheck("mcp.http_listener", "mcp", "skipped — daemon not running")}
	}
	if err := client.Status(ctx); err != nil {
		return []Check{skipCheck("mcp.http_listener", "mcp", "skipped — daemon not running")}
	}
	health, hErr := client.Health(ctx)
	if hErr != nil {
		return []Check{skipCheck("mcp.http_listener", "mcp", "daemon.health rpc failed: "+hErr.Error())}
	}
	if !health.MCPHTTPListenerOK {
		return []Check{{Name: "mcp.http_listener", Subsystem: "mcp", Status: StatusSkip, Message: "skipped (http MCP not enabled)"}}
	}
	addr := health.MCPHTTPAddr
	if addr == "" {
		return []Check{{Name: "mcp.http_listener", Subsystem: "mcp", Status: StatusError, Message: "listener reported ok but no addr"}}
	}

	url := "http://" + addr + "/health"
	httpClient := &http.Client{Timeout: time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return []Check{{
			Name: "mcp.http_listener", Subsystem: "mcp", Status: StatusError,
			Message: "probe " + url + ": " + err.Error(),
			Hint:    "check daemon logs for mcp server failures",
		}}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return []Check{{
			Name: "mcp.http_listener", Subsystem: "mcp", Status: StatusError,
			Message: fmt.Sprintf("probe %s: status %d", url, resp.StatusCode),
		}}
	}
	return []Check{{Name: "mcp.http_listener", Subsystem: "mcp", Status: StatusOK, Message: addr + " responding"}}
}
