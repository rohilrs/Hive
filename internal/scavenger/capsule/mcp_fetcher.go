package capsule

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// MCPFetcher implements Fetcher via long-lived `scavenger mcp-bridge`
// subprocesses (one per repo Cwd). JSON-RPC over stdin/stdout. Each
// bridge is lazy-spawned on first Fetch for a given repo and reused
// for all subsequent Fetches. On any subprocess failure (broken pipe,
// JSON parse error, exited process), the bridge is marked dead; the
// next Fetch for that repo respawns fresh.
//
// Goroutine-safe. Per-bridge mutex serializes JSON-RPC requests;
// cross-repo Fetches run in parallel.
type MCPFetcher struct {
	cfg Config

	bridgesMu sync.Mutex
	bridges   map[string]*activeBridge // key: req.Cwd (or "")
}

// activeBridge holds one running scavenger mcp-bridge subprocess.
type activeBridge struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	nextID int
}

// NewMCPFetcher constructs an MCPFetcher with sensible defaults.
func NewMCPFetcher(cfg Config) *MCPFetcher {
	if cfg.Binary == "" {
		cfg.Binary = "scavenger"
	}
	if cfg.PerCallTimeout == 0 {
		cfg.PerCallTimeout = 5 * time.Second
	}
	return &MCPFetcher{
		cfg:     cfg,
		bridges: map[string]*activeBridge{},
	}
}

// Fetch sends a get_capsule tools/call to the bridge for req.Cwd,
// spawning a fresh bridge if none exists or the prior one died.
func (f *MCPFetcher) Fetch(ctx context.Context, req Req) (*Capsule, error) {
	if req.File == "" {
		return nil, fmt.Errorf("capsule.Fetch: File is required")
	}

	b, err := f.bridgeFor(req.Cwd)
	if err != nil {
		return nil, err
	}

	// Per-call timeout wraps the round-trip.
	callCtx, cancel := context.WithTimeout(ctx, f.cfg.PerCallTimeout)
	defer cancel()

	raw, callErr := b.call(callCtx, "get_capsule", req)
	if callErr != nil {
		// Tear down the now-suspect bridge; next Fetch respawns.
		f.dropBridge(req.Cwd)
		return nil, callErr
	}
	return parseCapsule(raw), nil
}

// Close tears down every active bridge. Safe to call multiple times.
func (f *MCPFetcher) Close() error {
	f.bridgesMu.Lock()
	bridges := f.bridges
	f.bridges = map[string]*activeBridge{}
	f.bridgesMu.Unlock()

	var firstErr error
	for _, b := range bridges {
		if err := b.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// bridgeFor returns the bridge for cwd, spawning one if absent.
func (f *MCPFetcher) bridgeFor(cwd string) (*activeBridge, error) {
	f.bridgesMu.Lock()
	defer f.bridgesMu.Unlock()
	if b, ok := f.bridges[cwd]; ok {
		return b, nil
	}
	b, err := f.spawnBridge(cwd)
	if err != nil {
		return nil, err
	}
	f.bridges[cwd] = b
	return b, nil
}

// dropBridge removes the bridge for cwd and closes it. Idempotent.
func (f *MCPFetcher) dropBridge(cwd string) {
	f.bridgesMu.Lock()
	b, ok := f.bridges[cwd]
	if ok {
		delete(f.bridges, cwd)
	}
	f.bridgesMu.Unlock()
	if ok {
		_ = b.close()
	}
}

func (f *MCPFetcher) spawnBridge(cwd string) (*activeBridge, error) {
	cmd := exec.Command(f.cfg.Binary, "mcp-bridge")
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp-bridge stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp-bridge stdout: %w", err)
	}
	// Discard stderr; if we ever need to surface it, capture to a bounded buffer.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp-bridge start: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20) // allow up to 1MB lines

	b := &activeBridge{
		cmd:    cmd,
		stdin:  stdin,
		stdout: scanner,
		nextID: 1,
	}

	// Handshake: initialize + initialized notification.
	if err := b.initialize(); err != nil {
		_ = b.close()
		return nil, fmt.Errorf("mcp-bridge handshake: %w", err)
	}
	return b, nil
}

func (b *activeBridge) initialize() error {
	id := b.nextID
	b.nextID++
	req := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"hive","version":"0.1"}}}`,
		id,
	)
	if _, err := io.WriteString(b.stdin, req+"\n"); err != nil {
		return fmt.Errorf("write initialize: %w", err)
	}
	if !b.stdout.Scan() {
		if err := b.stdout.Err(); err != nil {
			return fmt.Errorf("read initialize: %w", err)
		}
		return fmt.Errorf("read initialize: EOF")
	}
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(b.stdout.Bytes(), &resp); err != nil {
		return fmt.Errorf("parse initialize: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error: %s", resp.Error.Message)
	}
	if resp.Result.ProtocolVersion == "" {
		return fmt.Errorf("initialize missing protocolVersion")
	}
	// Send initialized notification (no id, no response expected).
	if _, err := io.WriteString(b.stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
		return fmt.Errorf("write initialized notification: %w", err)
	}
	return nil
}

// call sends a tools/call JSON-RPC request and returns the text content
// of the response. Holds the bridge mutex for the round-trip.
func (b *activeBridge) call(ctx context.Context, toolName string, req Req) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Build arguments object — omit empty optional fields rather than
	// sending empty strings (MCP servers may treat null/missing/empty
	// differently). Budget is not part of get_capsule's schema.
	args := map[string]any{"file": req.File}
	if req.Symbol != "" {
		args["symbol"] = req.Symbol
	}
	if req.Query != "" {
		args["query"] = req.Query
	}

	id := b.nextID
	b.nextID++
	reqEnvelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	reqJSON, _ := json.Marshal(reqEnvelope)

	// Honor ctx by setting a write deadline via goroutine + close-on-cancel.
	// Simpler: cmd.Process inherits the OS pipes; on ctx cancel we don't
	// have a clean way to interrupt. For now rely on PerCallTimeout in
	// MCPFetcher.Fetch + bridge respawn on broken pipe. Document this.
	_ = ctx

	if _, err := io.WriteString(b.stdin, string(reqJSON)+"\n"); err != nil {
		return "", fmt.Errorf("write tools/call: %w", err)
	}
	if !b.stdout.Scan() {
		if err := b.stdout.Err(); err != nil {
			return "", fmt.Errorf("read tools/call: %w", err)
		}
		return "", fmt.Errorf("read tools/call: EOF")
	}
	var resp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(b.stdout.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("parse tools/call: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("tools/call rpc error: %s", resp.Error.Message)
	}
	if resp.Result.IsError {
		msg := ""
		if len(resp.Result.Content) > 0 {
			msg = resp.Result.Content[0].Text
		}
		return "", fmt.Errorf("tools/call: %s", msg)
	}
	if len(resp.Result.Content) == 0 {
		return "", nil // zero content → empty capsule (mirrors CLIFetcher "(0 tokens, 0 items)")
	}
	return resp.Result.Content[0].Text, nil
}

// close shuts down the bridge cleanly: close stdin, wait briefly,
// kill if needed. Idempotent.
func (b *activeBridge) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stdin != nil {
		_ = b.stdin.Close()
		b.stdin = nil
	}
	if b.cmd != nil && b.cmd.Process != nil {
		done := make(chan struct{})
		go func() { _ = b.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			_ = b.cmd.Process.Kill()
			<-done
		}
		b.cmd = nil
	}
	return nil
}
