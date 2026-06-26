// fake-claude is a test substitute for the real `claude -p` binary.
//
// Usage: fake-claude [flags] ARGS...
//
// The fixture file is selected from one of (in order):
//   1. -fixture <path> flag
//   2. HIVE_FAKE_CLAUDE_FIXTURE env var
//   3. Default fixture (./fixtures/approve_immediately.jsonl relative to binary)
//
// The fixture is a JSONL file. Each line is one event emitted on stdout.
// Lines may include a "delay_ms" field which fake-claude honors before
// emitting the next line (the delay_ms field is stripped from the emitted
// event itself).
//
// fake-claude tolerates and ignores Claude-CLI-style flags like
// --plugin-dir, --mcp-config, -p, --allowed-tools, --max-output-tokens.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	var (
		fixture            string
		mcpConfig          string
		pluginDir          string
		allowed            string
		printPrompt        string
		outputFormat       string
		model              string
		appendSystemPrompt string
		maxTokens          int
		maxTurns           int
		verbose            bool
		skipPermissions    bool
	)
	flag.StringVar(&fixture, "fixture", "", "fixture JSONL path")
	flag.StringVar(&mcpConfig, "mcp-config", "", "(ignored)")
	flag.StringVar(&pluginDir, "plugin-dir", "", "(ignored)")
	flag.StringVar(&allowed, "allowed-tools", "", "(ignored)")
	// Real-claude uses --allowedTools / --disallowedTools (camelCase) and
	// --strict-mcp-config; the chat CC provider passes these. Tolerate them
	// so chat agent tests don't fail with "flag provided but not defined".
	var allowedTools string
	flag.StringVar(&allowedTools, "allowedTools", "", "(ignored: real-claude camelCase form)")
	var disallowedTools string
	flag.StringVar(&disallowedTools, "disallowedTools", "", "(ignored: real-claude deny-list)")
	var strictMCP bool
	flag.BoolVar(&strictMCP, "strict-mcp-config", false, "(ignored)")
	var tools string
	flag.StringVar(&tools, "tools", "", "(ignored)")
	flag.StringVar(&printPrompt, "p", "", "(ignored: print mode prompt)")
	// Real claude exposes --print as a bool ("non-interactive print mode").
	// Tolerate it so oneshot/decompose tests don't fail on flag parse.
	var printMode bool
	flag.BoolVar(&printMode, "print", false, "(ignored: print mode bool)")
	flag.StringVar(&outputFormat, "output-format", "", "(ignored)")
	flag.StringVar(&model, "model", "", "(ignored)")
	flag.StringVar(&appendSystemPrompt, "append-system-prompt", "", "(ignored)")
	flag.IntVar(&maxTokens, "max-output-tokens", 0, "(ignored)")
	flag.IntVar(&maxTurns, "max-turns", 0, "(ignored)")
	flag.BoolVar(&verbose, "verbose", false, "(ignored)")
	flag.BoolVar(&skipPermissions, "dangerously-skip-permissions", false, "(ignored)")
	flag.Parse()

	// Write argv to <cwd>/.fake-claude-argv.json so integration tests can
	// inspect which flags the adapter passed to the claude binary.
	if argvJSON, err := json.Marshal(os.Args); err == nil {
		if cwd, err := os.Getwd(); err == nil {
			_ = os.WriteFile(filepath.Join(cwd, ".fake-claude-argv.json"), argvJSON, 0o644)
		}
	}

	if fixture == "" {
		// Phase 3.3 hook: if the system prompt identifies a loop-similarity
		// classifier call and HIVE_FAKE_CLAUDE_LOOP_FIXTURE is set, use
		// that fixture instead of the default. Lets smoke tests inject
		// deterministic loop-similarity responses for the L3 detector
		// without affecting the implement/review fixtures.
		if loop := os.Getenv("HIVE_FAKE_CLAUDE_LOOP_FIXTURE"); loop != "" &&
			strings.Contains(appendSystemPrompt, "loop-detection heuristic") {
			fixture = loop
		} else if v := os.Getenv("HIVE_FAKE_CLAUDE_FIXTURE"); v != "" {
			fixture = v
		} else {
			exe, _ := os.Executable()
			fixture = filepath.Join(filepath.Dir(exe), "fixtures", "approve_immediately.jsonl")
		}
	}

	if err := emit(fixture); err != nil {
		fmt.Fprintf(os.Stderr, "fake-claude: %v\n", err)
		os.Exit(2)
	}

	// Phase 3.4 hook: when HIVE_FAKE_CLAUDE_DELIVER_VERDICT is set
	// AND --mcp-config points at an hive-stage MCP entry, parse the
	// notify-sock arg and forward a verdict frame so smoke tests can
	// trigger APPROVE deterministically (the listener path) instead of
	// relying on the classifier fallback (Haiku may return UNCLEAR).
	if v := os.Getenv("HIVE_FAKE_CLAUDE_DELIVER_VERDICT"); v != "" && mcpConfig != "" {
		if err := deliverVerdict(mcpConfig, v); err != nil {
			fmt.Fprintf(os.Stderr, "fake-claude: deliver verdict: %v\n", err)
			// non-fatal; the events were emitted, just no verdict frame
		}
	}
}

// deliverVerdict reads the MCP config, extracts the hive-stage server's
// --notify-sock arg, and writes a verdict frame to it.
//
// frameSpec: "APPROVE" or "CHANGES_REQUESTED:path:line:comment"
// (the second form lets smokes craft synthetic FileRefs).
func deliverVerdict(mcpConfigPath, frameSpec string) error {
	b, err := os.ReadFile(mcpConfigPath)
	if err != nil {
		return fmt.Errorf("read mcp config: %w", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("parse mcp config: %w", err)
	}
	srv, ok := cfg.MCPServers["hive-stage"]
	if !ok {
		return fmt.Errorf("no hive-stage entry in mcp config")
	}
	var sock string
	for i, a := range srv.Args {
		if a == "--notify-sock" && i+1 < len(srv.Args) {
			sock = srv.Args[i+1]
		}
	}
	if sock == "" {
		return fmt.Errorf("no --notify-sock in hive-stage args")
	}

	// Build the verdict frame. APPROVE has empty FileRefs; otherwise
	// parse "CHANGES_REQUESTED:path:line:comment" into one FileRef.
	type fileRef struct {
		Path    string `json:"path"`
		Line    int    `json:"line,omitempty"`
		Comment string `json:"comment"`
	}
	type frame struct {
		RunID      string    `json:"run_id"`
		Stage      string    `json:"stage"`
		Verdict    string    `json:"verdict"`
		Confidence int       `json:"confidence"`
		FileRefs   []fileRef `json:"file_refs,omitempty"`
	}
	f := frame{Verdict: "APPROVE", Confidence: 90}
	parts := strings.SplitN(frameSpec, ":", 4)
	if parts[0] == "CHANGES_REQUESTED" && len(parts) >= 4 {
		var line int
		_, _ = fmt.Sscanf(parts[2], "%d", &line)
		f.Verdict = "CHANGES_REQUESTED"
		f.FileRefs = []fileRef{{Path: parts[1], Line: line, Comment: parts[3]}}
	}

	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", sock, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	payload, _ := json.Marshal(f)
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	// Read ack (drain; don't act on it).
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	sc.Scan()
	return nil
}

func emit(fixturePath string) error {
	f, err := os.Open(fixturePath)
	if err != nil {
		return fmt.Errorf("open fixture: %w", err)
	}
	defer f.Close()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var obj map[string]any
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			return fmt.Errorf("bad fixture line: %w", err)
		}
		var delay int
		if v, ok := obj["delay_ms"]; ok {
			if f, isFloat := v.(float64); isFloat {
				delay = int(f)
			}
			delete(obj, "delay_ms")
		}
		if delay > 0 {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}

		raw, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		out.Write(raw)
		out.WriteByte('\n')
		out.Flush()
	}
	return sc.Err()
}
