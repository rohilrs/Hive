package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/pkg/rpc"
)

var chatCmd = &cobra.Command{
	Use:   "chat [message]",
	Short: "Ask Hive's chat agent (one-shot with a message, or an interactive REPL with none)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return runChatOneShot(args[0])
		}
		return runChatREPLWithSession("")
	},
}

// runChatOneShot dials the daemon, issues a single chat.send streaming RPC,
// and renders the line-delimited frames as they arrive until the daemon closes
// the connection at end of turn. The session frame is ignored (one-shot has no
// next turn to continue).
func runChatOneShot(message string) error {
	conn, err := dialDaemon()
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = streamChatTurn(conn, "", message)
	return err
}

// runChatREPLWithSession reads lines from stdin and runs each as a chat turn,
// threading the session id through subsequent turns so the daemon continues the
// same session (and, for the SDK provider, rehydrates prior context). Pass ""
// to start a new session (the first turn's session frame will populate it for
// subsequent turns); pass an existing session id to resume. A fresh connection
// is dialed per turn since the daemon closes the conn at end of turn. Loops
// until EOF (Ctrl-D) or "exit"/"quit".
func runChatREPLWithSession(sessionID string) error {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	fmt.Fprintln(os.Stderr, "hive chat — type a message (Ctrl-D or 'exit' to quit)")
	fmt.Fprint(os.Stderr, "hive> ")
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			fmt.Fprint(os.Stderr, "hive> ")
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}

		conn, err := dialDaemon()
		if err != nil {
			fmt.Fprintln(os.Stderr, "hive chat:", err)
			fmt.Fprint(os.Stderr, "hive> ")
			continue
		}
		newSession, err := streamChatTurn(conn, sessionID, line)
		conn.Close()
		if err != nil {
			// A turn error is reported but the REPL continues; the session id
			// is preserved so the next turn can retry on the same session.
			fmt.Fprintln(os.Stderr, "hive chat:", err)
		} else if newSession != "" {
			sessionID = newSession
		}
		fmt.Fprint(os.Stderr, "hive> ")
	}
	if err := in.Err(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr) // newline after the trailing prompt on EOF
	return nil
}

// dialDaemon connects to the daemon's unix socket with a short timeout.
func dialDaemon() (net.Conn, error) {
	conn, err := net.DialTimeout("unix", daemonSocketPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w (is `hive daemon` running?)", err)
	}
	return conn, nil
}

// streamChatTurn issues one chat.send streaming RPC over conn (continuing
// sessionID if non-empty), renders frames as they arrive, and returns the
// session id reported by the session frame so callers can continue the
// session on the next turn.
//
// Wire shape (verified against internal/daemon/chat_rpc.go chatWriteFrame):
// the daemon writes one `rpc.Response[chat.Frame]` per line. chat.Frame carries
// NO json tags, so its fields marshal capitalized: Kind, Text, Tool, Result,
// Model, CostUSD. The decode struct below mirrors that exactly.
func streamChatTurn(conn net.Conn, sessionID, message string) (newSessionID string, err error) {
	params := map[string]any{"message": message}
	if sessionID != "" {
		params["session_id"] = sessionID
	}
	req := rpc.Request[map[string]any]{
		ID:     fmt.Sprintf("chat-%d", time.Now().UnixNano()),
		Method: rpc.MethodChatSend,
		Params: params,
	}
	raw, _ := json.Marshal(req)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return "", fmt.Errorf("write chat request: %w", err)
	}

	rdr := bufio.NewReader(conn)
	stdinReader := bufio.NewReader(os.Stdin)
	for {
		line, readErr := rdr.ReadBytes('\n')
		if len(line) > 0 {
			var env struct {
				Result *chat.Frame `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if jerr := json.Unmarshal(line, &env); jerr == nil {
				if env.Error != nil {
					fmt.Fprintln(os.Stderr, "hive chat:", env.Error.Message)
					return newSessionID, errors.New(env.Error.Message)
				}
				if env.Result != nil {
					f := env.Result
					switch f.Kind {
					case "session":
						newSessionID = f.Text
					case "text":
						fmt.Println(f.Text)
					case "tool_result":
						fmt.Fprintf(os.Stderr, "· [%s] %s\n", f.Tool, truncate(f.Result, 80))
					case "turn_done":
						fmt.Fprintf(os.Stderr, "(%s · $%.4f)\n", f.Model, f.CostUSD)
					case "tool_proposed":
						// f.Result is JSON: {"tool_call_id":"...","input":{...}}
						var p struct {
							ToolCallID string          `json:"tool_call_id"`
							Input      json.RawMessage `json:"input"`
						}
						if err := json.Unmarshal([]byte(f.Result), &p); err != nil {
							fmt.Fprintln(os.Stderr, "hive chat: bad tool_proposed frame:", err)
							continue
						}
						fmt.Fprintf(os.Stderr, "\n· [proposed] %s with input: %s\n  Approve? [y/n]: ", f.Tool, string(p.Input))
						answer, _ := stdinReader.ReadString('\n')
						approve := strings.HasPrefix(strings.TrimSpace(strings.ToLower(answer)), "y")
						confirmReq := map[string]any{
							"id":     fmt.Sprintf("confirm-%d", time.Now().UnixNano()),
							"method": "chat.confirm",
							"params": map[string]any{
								"session_id":   sessionID,
								"tool_call_id": p.ToolCallID,
								"approve":      approve,
							},
						}
						cb, _ := json.Marshal(confirmReq)
						// chat.confirm MUST go on a separate conn — the chat.send
						// streaming RPC is holding this conn one-way (the daemon
						// only writes frames; its request reader isn't consuming
						// more requests until streamChat returns). Writing on this
						// conn would queue in the kernel buffer until the stream
						// ends — i.e. always after the gate's timeout.
						confirmConn, derr := dialDaemon()
						if derr != nil {
							fmt.Fprintln(os.Stderr, "hive chat: confirm dial:", derr)
							continue
						}
						if _, err := confirmConn.Write(append(cb, '\n')); err != nil {
							fmt.Fprintln(os.Stderr, "hive chat: confirm write:", err)
							confirmConn.Close()
							continue
						}
						// Drain the one-line response so the conn closes cleanly.
						_ = confirmConn.SetReadDeadline(time.Now().Add(5 * time.Second))
						_, _ = bufio.NewReader(confirmConn).ReadBytes('\n')
						confirmConn.Close()
					case "error":
						fmt.Fprintln(os.Stderr, "hive chat:", f.Text)
						return newSessionID, errors.New(f.Text)
					}
				}
			}
		}
		if readErr != nil {
			// EOF (or any read error) means the daemon closed the conn at end
			// of turn. The turn_done frame was already rendered.
			return newSessionID, nil
		}
	}
}

// truncate shortens s to at most n runes, appending an ellipsis when cut.
// Single-line collapse keeps multi-line tool payloads to one compact line.
func truncate(s string, n int) string {
	for i, r := range s {
		if r == '\n' {
			s = s[:i]
			break
		}
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

var chatHistoryCmd = &cobra.Command{
	Use:   "history [session-id]",
	Short: "List recent chat sessions, or show messages for one session",
	Long:  "Without args: list the most recent chat sessions (id, surface, started, cost). With a session-id arg: show all messages in that session in order.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if len(args) == 0 {
			return runHistoryList(ctx)
		}
		return runHistoryShow(ctx, args[0])
	},
}

func runHistoryList(_ context.Context) error {
	conn, err := dialDaemon()
	if err != nil {
		return err
	}
	defer conn.Close()
	limit := chatHistoryLimit
	if limit <= 0 {
		limit = 50
	}
	req := map[string]any{
		"id":     fmt.Sprintf("hist-%d", time.Now().UnixNano()),
		"method": "chat.history_list",
		"params": map[string]any{"limit": limit},
	}
	body, _ := json.Marshal(req)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Sessions []struct {
				ID           string  `json:"id"`
				Surface      string  `json:"surface"`
				StartedAt    int64   `json:"started_at"`
				EndedAt      int64   `json:"ended_at"`
				TotalCostUSD float64 `json:"total_cost_usd"`
				Name         string  `json:"name,omitempty"`
				Provider     string  `json:"provider,omitempty"`
			} `json:"sessions"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return errors.New(resp.Error.Message)
	}
	if len(resp.Result.Sessions) == 0 {
		fmt.Println("(no chat sessions)")
		return nil
	}
	for _, s := range resp.Result.Sessions {
		started := time.Unix(s.StartedAt, 0).Format("2006-01-02 15:04")
		status := "open"
		if s.EndedAt > 0 {
			status = "ended " + time.Unix(s.EndedAt, 0).Format("15:04")
		}
		prov := s.Provider
		if prov == "claude-code" {
			prov = "cc"
		}
		if prov == "" {
			prov = "?"
		}
		name := s.Name
		if name == "" {
			name = "(unnamed)"
		}
		// Cap name at 40 runes for column alignment
		if rs := []rune(name); len(rs) > 40 {
			name = string(rs[:37]) + "..."
		}
		fmt.Printf("%-30s  %-3s  %-3s  %s  %s  $%.4f  %s\n",
			s.ID, s.Surface, prov, started, status, s.TotalCostUSD, name)
	}
	return nil
}

func runHistoryShow(_ context.Context, sessionID string) error {
	conn, err := dialDaemon()
	if err != nil {
		return err
	}
	defer conn.Close()
	req := map[string]any{
		"id":     fmt.Sprintf("hist-%d", time.Now().UnixNano()),
		"method": "chat.history_get",
		"params": map[string]any{"session_id": sessionID},
	}
	body, _ := json.Marshal(req)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Messages []struct {
				Role      string  `json:"role"`
				Content   string  `json:"content"`
				CostUSD   float64 `json:"cost_usd"`
				CreatedAt int64   `json:"created_at"`
			} `json:"messages"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return errors.New(resp.Error.Message)
	}
	if len(resp.Result.Messages) == 0 {
		fmt.Println("(no messages)")
		return nil
	}
	for _, m := range resp.Result.Messages {
		ts := time.Unix(m.CreatedAt, 0).Format("15:04:05")
		fmt.Printf("[%s] %s:\n%s\n\n", ts, m.Role, m.Content)
	}
	return nil
}

var chatResumeCmd = &cobra.Command{
	Use:   "resume <session-id>",
	Short: "Resume a chat session as a REPL",
	Long:  "Continues an existing chat session. The session id can be obtained from `hive chat history`. The REPL behaves identically to `hive chat` (Ctrl-D / `exit` to quit), but each turn carries the existing session id so the agent has prior multi-turn context.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runChatREPLWithSession(args[0])
	},
}

var chatNameCmd = &cobra.Command{
	Use:   "name <session-id> <new-name>",
	Short: "Rename a chat session",
	Long:  "Sets a human-readable name for the chat session. The id can be obtained from `hive chat history`.",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runChatSetName(cmd.Context(), args[0], args[1])
	},
}

func runChatSetName(ctx context.Context, sessionID, name string) error {
	conn, err := dialDaemon()
	if err != nil {
		return err
	}
	defer conn.Close()
	req := map[string]any{
		"id":     fmt.Sprintf("setname-%d", time.Now().UnixNano()),
		"method": "chat.set_name",
		"params": map[string]any{
			"session_id": sessionID,
			"name":       name,
		},
	}
	body, _ := json.Marshal(req)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			OK bool `json:"ok"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return errors.New(resp.Error.Message)
	}
	fmt.Println("ok")
	return nil
}

var chatDeleteCmd = &cobra.Command{
	Use:   "delete <session-id>",
	Short: "Delete a chat session and all its messages",
	Long: `Removes a chat session, its messages, and any on-disk per-session
scratch directory under <hive-dir>/chat/<session-id>/. Irreversible.
The session id can be obtained from "hive chat history".`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runChatDelete(cmd.Context(), args[0])
	},
}

func runChatDelete(ctx context.Context, sessionID string) error {
	conn, err := dialDaemon()
	if err != nil {
		return err
	}
	defer conn.Close()
	req := map[string]any{
		"id":     fmt.Sprintf("chatdelete-%d", time.Now().UnixNano()),
		"method": rpc.MethodChatDelete,
		"params": map[string]any{
			"session_id": sessionID,
		},
	}
	body, _ := json.Marshal(req)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			OK bool `json:"ok"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return errors.New(resp.Error.Message)
	}
	fmt.Println("ok")
	return nil
}

var chatHistoryLimit int

func init() {
	chatCmd.AddCommand(chatHistoryCmd, chatResumeCmd, chatNameCmd, chatDeleteCmd)
	chatHistoryCmd.Flags().IntVar(&chatHistoryLimit, "limit", 50, "max number of sessions to list (max 200)")
}
