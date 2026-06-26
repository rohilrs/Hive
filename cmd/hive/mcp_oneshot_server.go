package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runOneshotToolServer serves a single configurable tool over stdio MCP.
// When the tool is called, it atomically writes the input args (as raw
// JSON) to captureFile and returns success. After one successful call
// the server exits 0 (its purpose is done — the caller spawned it for
// exactly one tool_use turn).
//
// This is the server side of internal/llm/claudecli.OneshotToolRunner:
// the runner spawns `claude -p` with --mcp-config pointing at this
// binary in --oneshot mode, then reads captureFile after claude exits.
//
// Spawned by the runner with:
//
//	hive mcp-stage-server --oneshot \
//	  --tool-name <name> \
//	  --tool-description <desc> \
//	  --tool-input-schema-file <path/to/schema.json> \
//	  --capture-args-file <path/to/captured.json>
//
// Phase 8.B T2.
func runOneshotToolServer(ctx context.Context, toolName, toolDescription, schemaFile, captureFile string) error {
	if toolName == "" {
		return fmt.Errorf("--tool-name is required in --oneshot mode")
	}
	if schemaFile == "" {
		return fmt.Errorf("--tool-input-schema-file is required in --oneshot mode")
	}
	if captureFile == "" {
		return fmt.Errorf("--capture-args-file is required in --oneshot mode")
	}

	// Read the input schema verbatim. The runner marshaled it; we
	// re-emit it on tools/list. Validate it's parseable JSON.
	schemaBytes, err := os.ReadFile(schemaFile)
	if err != nil {
		return fmt.Errorf("read tool-input-schema-file: %w", err)
	}
	var inputSchema map[string]any
	if err := json.Unmarshal(schemaBytes, &inputSchema); err != nil {
		return fmt.Errorf("parse tool-input-schema-file: %w", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "hive-oneshot-mcp", Version: "0.1.0"}, nil)

	// Track whether the tool was successfully called so the server can
	// exit after one round-trip. A bare bool is fine — the mcp-go SDK
	// dispatches one handler at a time per connection.
	done := make(chan struct{}, 1)

	mcp.AddTool(server, &mcp.Tool{
		Name:        toolName,
		Description: toolDescription,
		InputSchema: inputSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, struct{}, error) {
		// Marshal the input back to JSON (we accept it as map[string]any
		// for permissiveness, but the runner consumes the raw JSON).
		raw, err := json.Marshal(input)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "marshal input: " + err.Error()}},
				IsError: true,
			}, struct{}{}, nil
		}
		// Atomic write: write to .tmp then rename so the reader (the
		// runner) never sees a partial file even if we crash mid-write.
		tmpPath := captureFile + ".tmp"
		if err := os.WriteFile(tmpPath, raw, 0o644); err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "write capture: " + err.Error()}},
				IsError: true,
			}, struct{}{}, nil
		}
		if err := os.Rename(tmpPath, captureFile); err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "rename capture: " + err.Error()}},
				IsError: true,
			}, struct{}{}, nil
		}
		// Signal the outer loop to exit after this response flushes.
		// Non-blocking send so a duplicate call (shouldn't happen, but
		// defensive) doesn't deadlock.
		select {
		case done <- struct{}{}:
		default:
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "submitted"}},
		}, struct{}{}, nil
	})

	// Run the server until either: (a) the tool was successfully called
	// (and the response has been sent), or (b) the parent context is
	// canceled, or (c) the underlying stdio transport closes (claude
	// exits / dies). A nested context lets us cancel server.Run from
	// the done signal without leaking the goroutine.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Run(runCtx, &mcp.StdioTransport{})
	}()
	select {
	case <-done:
		// Tool was called successfully. Give the SDK a brief window to
		// flush the response, then cancel and drain the run goroutine.
		cancel()
		<-errCh
		return nil
	case err := <-errCh:
		// stdio closed (claude exited) or the transport errored before
		// we got a tools/call. Surface the error; the runner will
		// report "no capture file" which is the right operator message.
		return err
	}
}
