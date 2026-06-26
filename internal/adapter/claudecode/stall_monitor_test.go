package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStallStore captures Insert + Clear calls in-memory for assertions.
type fakeStallStore struct {
	mu      sync.Mutex
	inserts []fakeStallInsert
	clears  []fakeStallClear
	nextID  int64
}

type fakeStallInsert struct {
	RunID       string
	StageID     int64
	Layer       int
	DetectedAt  int64
	ClearedAt   int64
	ActionTaken string
	DetailsJSON string
}

type fakeStallClear struct {
	ID        int64
	ClearedAt int64
}

func (f *fakeStallStore) InsertStall(ctx context.Context, runID string, stageID int64, layer int, detectedAt, clearedAt int64, action, details string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.inserts = append(f.inserts, fakeStallInsert{runID, stageID, layer, detectedAt, clearedAt, action, details})
	return f.nextID, nil
}

func (f *fakeStallStore) ClearStall(ctx context.Context, id, clearedAt int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears = append(f.clears, fakeStallClear{id, clearedAt})
	return nil
}

func (f *fakeStallStore) snapshotInserts() []fakeStallInsert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeStallInsert, len(f.inserts))
	copy(out, f.inserts)
	return out
}

func (f *fakeStallStore) snapshotClears() []fakeStallClear {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeStallClear, len(f.clears))
	copy(out, f.clears)
	return out
}

// fakeSignaller captures Signal calls instead of actually killing a process.
type fakeSignaller struct {
	mu      sync.Mutex
	signals []string
}

func (f *fakeSignaller) Signal(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, name)
	return nil
}

func (f *fakeSignaller) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.signals))
	copy(out, f.signals)
	return out
}

func TestMonitor_L1FireAndClear(t *testing.T) {
	store := &fakeStallStore{}
	sig := &fakeSignaller{}
	mon := NewMonitor(MonitorConfig{
		RunID:            "run-1",
		StageID:          7,
		HeartbeatTimeout: 50 * time.Millisecond,
		ToolCallTimeout:  0,
		PollInterval:     10 * time.Millisecond,
		Store:            store,
		Signaller:        sig,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mon.OnEvent(Event{Type: EventText}, time.Now())

	go mon.Run(ctx)

	// Wait long enough for the monitor to detect (heartbeat 50ms + poll 10ms).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(store.snapshotInserts()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ins := store.snapshotInserts()
	if len(ins) == 0 {
		t.Fatal("expected at least one L1 insert")
	}
	if ins[0].Layer != 1 || ins[0].ActionTaken != "surfaced" {
		t.Errorf("got layer=%d action=%q want 1/surfaced", ins[0].Layer, ins[0].ActionTaken)
	}
	if ins[0].StageID != 7 {
		t.Errorf("StageID=%d want 7", ins[0].StageID)
	}

	// Now emit a fresh event — L1 should clear on next tick.
	mon.OnEvent(Event{Type: EventText}, time.Now())
	deadline = time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(store.snapshotClears()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(store.snapshotClears()) == 0 {
		t.Error("expected ClearStall after event resumed")
	}

	if len(sig.snapshot()) != 0 {
		t.Errorf("L1 must not Signal; got %v", sig.snapshot())
	}
}

func TestMonitor_L2FireAndKill(t *testing.T) {
	store := &fakeStallStore{}
	sig := &fakeSignaller{}
	mon := NewMonitor(MonitorConfig{
		RunID:           "run-1",
		StageID:         7,
		ToolCallTimeout: 50 * time.Millisecond,
		PollInterval:    10 * time.Millisecond,
		Store:           store,
		Signaller:       sig,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mon.OnEvent(Event{
		Type:      EventToolUse,
		ToolID:    "tool-1",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"sleep 9999"}`),
	}, time.Now())

	go mon.Run(ctx)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mon.Killed() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !mon.Killed() {
		t.Fatal("expected mon.Killed() = true")
	}
	if got := sig.snapshot(); len(got) != 1 || got[0] != "SIGTERM" {
		t.Errorf("got signals %v want [SIGTERM]", got)
	}
	ins := store.snapshotInserts()
	if len(ins) != 1 {
		t.Fatalf("len(inserts)=%d want 1", len(ins))
	}
	if ins[0].Layer != 2 || ins[0].ActionTaken != "killed_subprocess" {
		t.Errorf("got layer=%d action=%q want 2/killed_subprocess", ins[0].Layer, ins[0].ActionTaken)
	}
	if !strings.Contains(ins[0].DetailsJSON, "Bash") {
		t.Errorf("DetailsJSON=%q missing tool name", ins[0].DetailsJSON)
	}

	tool, ok := mon.CulpritTool()
	if !ok || tool != "Bash" {
		t.Errorf("CulpritTool = %q,%v want Bash,true", tool, ok)
	}
}

func TestMonitor_L2DoesNotFireAfterToolResult(t *testing.T) {
	store := &fakeStallStore{}
	sig := &fakeSignaller{}
	mon := NewMonitor(MonitorConfig{
		RunID:           "run-1",
		StageID:         7,
		ToolCallTimeout: 50 * time.Millisecond,
		PollInterval:    10 * time.Millisecond,
		Store:           store,
		Signaller:       sig,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now()
	mon.OnEvent(Event{Type: EventToolUse, ToolID: "tool-1", ToolName: "Bash"}, now)
	mon.OnEvent(Event{Type: EventToolResult, ToolUseID: "tool-1"}, now.Add(10*time.Millisecond))

	go mon.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	if mon.Killed() {
		t.Error("monitor killed after tool_result; expected no L2")
	}
	if len(sig.snapshot()) != 0 {
		t.Errorf("got signals %v want []", sig.snapshot())
	}
}

func TestMonitor_BothDisabledNoWork(t *testing.T) {
	store := &fakeStallStore{}
	sig := &fakeSignaller{}
	mon := NewMonitor(MonitorConfig{
		RunID:            "run-1",
		HeartbeatTimeout: 0,
		ToolCallTimeout:  0,
		PollInterval:     10 * time.Millisecond,
		Store:            store,
		Signaller:        sig,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go mon.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	if len(store.snapshotInserts()) != 0 {
		t.Errorf("inserts=%v want none", store.snapshotInserts())
	}
}

func TestErrToolCallStallSentinel(t *testing.T) {
	wrapped := errToolCallStallWith("Bash")
	if !errors.Is(wrapped, ErrToolCallStall) {
		t.Errorf("errors.Is mismatch: wrapped=%v sentinel=%v", wrapped, ErrToolCallStall)
	}
	if got := wrapped.Error(); !strings.Contains(got, "Bash") {
		t.Errorf("err message %q missing tool name", got)
	}
	tool, ok := StallToolFromError(wrapped)
	if !ok || tool != "Bash" {
		t.Errorf("StallToolFromError = %q,%v want Bash,true", tool, ok)
	}
}

func TestMonitor_L1IgnoresToolUseTrackingWhenL2Disabled(t *testing.T) {
	// Edge: a tool_use is emitted but L2 is disabled. The monitor still
	// tracks it (in case L2 turns on mid-stage in some future config),
	// but won't fire from it. L1 should still see the lastEventTs update.
	store := &fakeStallStore{}
	mon := NewMonitor(MonitorConfig{
		RunID:            "r",
		HeartbeatTimeout: 50 * time.Millisecond,
		ToolCallTimeout:  0,
		PollInterval:     10 * time.Millisecond,
		Store:            store,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mon.OnEvent(Event{Type: EventToolUse, ToolID: "t", ToolName: "Bash"}, time.Now())
	go mon.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// L1 should have surfaced (tool_use event was the last one; no
	// further events; gap > 50ms).
	ins := store.snapshotInserts()
	if len(ins) == 0 || ins[0].Layer != 1 {
		t.Errorf("expected L1 row; got %+v", ins)
	}
}
