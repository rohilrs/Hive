// fake-scavenger is a test stub for the real scavenger CLI. It implements
// the subset of subcommands Hive's lifecycle wrapper invokes:
//
//	fake-scavenger daemon start                -> exits 0 immediately
//	fake-scavenger daemon stop                 -> exits 0
//	fake-scavenger doctor --format json        -> prints {"ok":true} to stdout
//	fake-scavenger mcp-bridge                  -> reads stdin until EOF, exits 0
//	fake-scavenger capsule <file> [symbol]    -> prints fixture or env-controlled output
//	fake-scavenger index                       -> creates .scavenger/indexes/ in cwd
//	fake-scavenger init --plugin-only          -> creates .scavenger/claude-plugin/ in cwd
//	fake-scavenger --version                   -> prints version, exits 0
//
// Unknown subcommands exit non-zero.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "fake-scavenger: missing subcommand")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		// daemon start | stop | status — all no-ops
		os.Exit(0)
	case "doctor":
		fmt.Println(`{"ok":true,"version":"fake-0.0.1"}`)
	case "mcp-bridge":
		runMCPBridge()
	case "capsule":
		switch os.Getenv("FAKE_SCAVENGER_CAPSULE") {
		case "":
			fmt.Println(`[TARGET]
func (p *BuildPipeline) Run(ctx context.Context, run *Run) (*Result, error)

[CALLEES]
func derefRepoPath(p *store.Project) string

[BODY] Run
func (p *BuildPipeline) Run(ctx context.Context, run *Run) (*Result, error) {
	// ... body elided ...
}`)
		case "empty":
			fmt.Println("(0 tokens, 0 items)")
		default:
			fmt.Println(os.Getenv("FAKE_SCAVENGER_CAPSULE"))
		}
	case "index":
		// Simulate `scavenger index`: create .scavenger/indexes/ in cwd so
		// callers observing the directory see the same effect as the real CLI.
		// Honor FAKE_SCAVENGER_INDEX_FAIL=1 for failure-path tests.
		if os.Getenv("FAKE_SCAVENGER_INDEX_FAIL") == "1" {
			fmt.Fprintln(os.Stderr, "fake-scavenger: forced index failure")
			os.Exit(1)
		}
		_ = os.MkdirAll(".scavenger/indexes", 0o755)
		fmt.Println("Indexed: 1 files, 1 symbols, 0 edges")
	case "init":
		// Only --plugin-only is exercised by Hive. Create the plugin marker
		// file the adapter looks for; never touch .mcp.json/.cursor.
		_ = os.MkdirAll(".scavenger/claude-plugin/.claude-plugin", 0o755)
		_ = os.MkdirAll(".scavenger/claude-plugin/hooks", 0o755)
		_ = os.WriteFile(".scavenger/claude-plugin/.claude-plugin/plugin.json",
			[]byte(`{"name":"scavenger","version":"fake"}`), 0o644)
		_ = os.WriteFile(".scavenger/claude-plugin/hooks/hooks.json",
			[]byte(`{"hooks":{}}`), 0o644)
		fmt.Println("Created: .scavenger/claude-plugin/ (plugin-only)")
	case "--version":
		fmt.Println("fake-scavenger 0.0.1")
	default:
		fmt.Fprintf(os.Stderr, "fake-scavenger: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runMCPBridge() {
	// Optional spawn log so tests can count spawns.
	if path := os.Getenv("FAKE_SCAVENGER_SPAWN_LOG"); path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			_, _ = fmt.Fprintln(f, "spawn")
			_ = f.Close()
		}
	}
	exitAfter := 0
	if v := os.Getenv("FAKE_SCAVENGER_EXIT_AFTER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			exitAfter = n
		}
	}
	failHandshake := os.Getenv("FAKE_SCAVENGER_FAIL_HANDSHAKE") == "1"
	toolError := os.Getenv("FAKE_SCAVENGER_TOOL_ERROR")
	capsuleText := os.Getenv("FAKE_SCAVENGER_CAPSULE")
	if capsuleText == "" {
		capsuleText = "[TARGET]\nfake target\n\n[BODY] sym\nfake body content"
	}

	scanner := bufio.NewScanner(os.Stdin)
	// Allow larger JSON-RPC frames than the 64KB default.
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	toolCalls := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			if failHandshake {
				writeJSONRPCError(req.ID, -32603, "fake handshake failure")
				continue
			}
			writeJSONRPCResult(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-scavenger", "version": "0.0.1"},
			})
		case "notifications/initialized":
			// No response.
		case "tools/call":
			toolCalls++
			if toolError != "" {
				writeJSONRPCResult(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": toolError}},
					"isError": true,
				})
			} else {
				writeJSONRPCResult(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": capsuleText}},
					"isError": false,
				})
			}
			if exitAfter > 0 && toolCalls >= exitAfter {
				return
			}
		default:
			// Unknown method — JSON-RPC method-not-found.
			writeJSONRPCError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func writeJSONRPCResult(id json.RawMessage, result any) {
	if id == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
	fmt.Println(string(body))
}

func writeJSONRPCError(id json.RawMessage, code int, message string) {
	if id == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
	fmt.Println(string(body))
}
