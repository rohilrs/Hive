package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// minimalAdapter satisfies adapter.Adapter with no-op behavior — used
// for streaming tests that don't exercise the pipeline.
type minimalAdapter struct{}

func (minimalAdapter) Name() string { return "minimal" }
func (minimalAdapter) Close() error { return nil }
func (minimalAdapter) ClassifyVerdict(_ context.Context, _ string) (*adapter.Verdict, error) {
	return &adapter.Verdict{Kind: adapter.VerdictChangesRequested}, nil
}
func (minimalAdapter) RunStage(_ context.Context, _ adapter.StageRequest) (*adapter.StageOutput, error) {
	return &adapter.StageOutput{}, nil
}

func TestRPCEventsSubscribeReceivesPublishedEvents(t *testing.T) {
	hiveDir := t.TempDir()
	cfg, err := config.Load(config.LoadOptions{ConfigDir: hiveDir, SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	// Disable heartbeats so the only event we see is the one we publish.
	cfg.TUI.HeartbeatSeconds = 0

	d, err := New(Config{HiveDir: hiveDir, Cfg: cfg, Adapter: minimalAdapter{}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	sockPath := filepath.Join(hiveDir, "daemon.sock")
	waitFor(t, func() bool { _, err := os.Stat(sockPath); return err == nil }, 3*time.Second)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := `{"id":"t1","method":"events.subscribe","params":{}}` + "\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	rdr := bufio.NewReader(conn)
	// Read the ack line.
	ackLine, err := rdr.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if !contains(string(ackLine), "subscribed") {
		t.Errorf("ack=%q missing 'subscribed'", ackLine)
	}

	// Publish a test event.
	d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventRunStarted,
		Data: map[string]any{"run_id": "synthetic"},
	})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := rdr.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var ev rpc.EventMessage
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != rpc.EventRunStarted {
		t.Errorf("got type=%v want run.started", ev.Type)
	}
}

func TestRPCEventsSubscribeReceivesHeartbeat(t *testing.T) {
	hiveDir := t.TempDir()
	cfg, err := config.Load(config.LoadOptions{ConfigDir: hiveDir, SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.TUI.HeartbeatSeconds = 1 // fast for the test

	d, err := New(Config{HiveDir: hiveDir, Cfg: cfg, Adapter: minimalAdapter{}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	sockPath := filepath.Join(hiveDir, "daemon.sock")
	waitFor(t, func() bool { _, err := os.Stat(sockPath); return err == nil }, 3*time.Second)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"id":"hb","method":"events.subscribe","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}

	rdr := bufio.NewReader(conn)
	_, _ = rdr.ReadBytes('\n') // ack

	// Within ~2.5s we should see at least one daemon.heartbeat.
	conn.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
	sawHeartbeat := false
	for !sawHeartbeat {
		line, err := rdr.ReadBytes('\n')
		if err != nil {
			break
		}
		var ev rpc.EventMessage
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == rpc.EventDaemonHeartbeat {
			sawHeartbeat = true
		}
	}
	if !sawHeartbeat {
		t.Error("never saw daemon.heartbeat event within 2.5s")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRPCAddProject(t *testing.T) {
	hiveDir := t.TempDir()
	cfg, _ := config.Load(config.LoadOptions{ConfigDir: hiveDir, SkipEnv: true})
	cfg.TUI.HeartbeatSeconds = 0
	d, err := New(Config{HiveDir: hiveDir, Cfg: cfg, Adapter: minimalAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	sockPath := filepath.Join(hiveDir, "daemon.sock")
	waitFor(t, func() bool { _, err := os.Stat(sockPath); return err == nil }, 3*time.Second)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := `{"id":"t1","method":"project.add","params":{"slug":"smoke","name":"Smoke","repo_path":"/tmp/smoke"}}` + "\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	rdr := bufio.NewReader(conn)
	line, err := rdr.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(line), `"slug":"smoke"`) {
		t.Errorf("response missing slug: %s", line)
	}
}
