package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMCPHTTPEndToEndChatRoute starts a real daemon, reads the mcp.url
// it wrote, hits the chat route with a JSON-RPC tools/call for
// hive_status, and confirms the response carries the daemon's status
// payload. This is the all-the-way-through smoke for Phase 6.3A.
func TestMCPHTTPEndToEndChatRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	d := newTestDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startCh := make(chan error, 1)
	go func() { startCh <- d.Start(ctx) }()
	defer d.Stop()
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	urlPath := filepath.Join(d.HiveDir(), "mcp.url")
	deadline := time.Now().Add(5 * time.Second)
	var base string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(urlPath); err == nil {
			base = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if base == "" {
		select {
		case err := <-startCh:
			t.Fatalf("mcp.url never written; Start returned: %v", err)
		default:
			t.Fatalf("mcp.url never written within 5s")
		}
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_status","arguments":{}}}`
	resp, err := http.Post(base+"/mcp/chat/sess-test", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, buf.String())
	}
	var parsed struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("decode response: %v body=%s", err, buf.String())
	}
	if parsed.Error != nil {
		t.Errorf("response carried an error: %s", buf.String())
	}
	if len(parsed.Result.Content) == 0 {
		t.Fatalf("hive_status returned no content: %s", buf.String())
	}
	// The hive_status tool returns a JSON status payload. Verify it
	// at least decodes as JSON — the exact fields aren't part of this
	// task's contract.
	var statusPayload map[string]any
	if err := json.Unmarshal([]byte(parsed.Result.Content[0].Text), &statusPayload); err != nil {
		t.Errorf("hive_status content not JSON: %v: %q", err, parsed.Result.Content[0].Text)
	}
}
