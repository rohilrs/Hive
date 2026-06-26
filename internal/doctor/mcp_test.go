package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPHTTPListenerOKWhenProbeReturns200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	hiveDir := t.TempDir()
	client := &stubRPCClient{health: HealthSnapshot{MCPHTTPListenerOK: true, MCPHTTPAddr: addr}}
	checks := runMCPChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "mcp.http_listener")
	if c.Status != StatusOK {
		t.Errorf("http listener responsive: status=%s, want ok; msg=%q", c.Status, c.Message)
	}
}

func TestMCPHTTPListenerSkippedWhenNotEnabled(t *testing.T) {
	hiveDir := t.TempDir()
	client := &stubRPCClient{health: HealthSnapshot{MCPHTTPListenerOK: false}}
	checks := runMCPChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "mcp.http_listener")
	if c.Status != StatusSkip {
		t.Errorf("http not enabled: status=%s, want skip; msg=%q", c.Status, c.Message)
	}
}

func TestMCPHTTPListenerErrorWhenProbeFails(t *testing.T) {
	hiveDir := t.TempDir()
	// Bind a port that nothing is listening on. 127.0.0.1:1 is a
	// privileged port so dialing it from userland refuses fast.
	client := &stubRPCClient{health: HealthSnapshot{MCPHTTPListenerOK: true, MCPHTTPAddr: "127.0.0.1:1"}}
	checks := runMCPChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "mcp.http_listener")
	if c.Status != StatusError {
		t.Errorf("probe fail: status=%s, want error; msg=%q", c.Status, c.Message)
	}
}

func TestMCPSkipsWhenDaemonDown(t *testing.T) {
	hiveDir := t.TempDir()
	client := &stubRPCClient{statusErr: errSocketDown}
	checks := runMCPChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "mcp.http_listener")
	if c.Status != StatusSkip {
		t.Errorf("daemon down: status=%s, want skip", c.Status)
	}
}
