package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var (
	eventsFormat    string
	eventsTypes     string
	eventsHeartbeat bool
)

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Tail the daemon's event stream",
	Long: `Subscribes to the daemon's event-subscription RPC and prints each event as it arrives.

Default output is one human-readable line per event. Use --format json for raw
line-delimited EventMessage JSON (pipe-friendly).

Filters apply client-side: --type accepts a comma-separated list of EventType
values to print; events not matching are silently dropped.

The connection holds until the daemon shuts down or Ctrl-C is pressed. The
client treats daemon.stopping events as a clean exit signal.`,
	RunE: runEvents,
}

func init() {
	eventsCmd.Flags().StringVar(&eventsFormat, "format", "human", "Output format: human or json")
	eventsCmd.Flags().StringVar(&eventsTypes, "type", "", "Comma-separated event types to keep (default: all)")
	eventsCmd.Flags().BoolVar(&eventsHeartbeat, "heartbeat", false, "Include daemon.heartbeat events (default: filter out)")
}

func runEvents(cmd *cobra.Command, args []string) error {
	sockPath := daemonSocketPath()
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial daemon: %w (is `hive daemon` running?)", err)
	}
	defer conn.Close()

	req := fmt.Sprintf(`{"id":"events-%d","method":"events.subscribe","params":{}}%s`,
		time.Now().UnixNano(), "\n")
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("write subscribe: %w", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		conn.Close()
	}()

	var keep map[string]bool
	if eventsTypes != "" {
		keep = map[string]bool{}
		for _, t := range strings.Split(eventsTypes, ",") {
			keep[strings.TrimSpace(t)] = true
		}
	}

	rdr := bufio.NewReader(conn)
	if _, err := rdr.ReadBytes('\n'); err != nil {
		return fmt.Errorf("read subscribe ack: %w", err)
	}

	for {
		line, err := rdr.ReadBytes('\n')
		if err != nil {
			return nil
		}
		var ev rpc.EventMessage
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if !eventsHeartbeat && ev.Type == rpc.EventDaemonHeartbeat {
			continue
		}
		if keep != nil && !keep[string(ev.Type)] {
			continue
		}
		if ev.Type == rpc.EventDaemonStopping {
			fmt.Fprintln(os.Stderr, "daemon stopping; exiting")
			return nil
		}
		switch eventsFormat {
		case "json":
			os.Stdout.Write(line)
		default:
			fmt.Printf("[%s] %s %s\n",
				time.Now().Format("15:04:05"),
				ev.Type,
				compactJSON(ev.Data),
			)
		}
	}
}

func compactJSON(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
