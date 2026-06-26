package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// StallStore is the persistence contract the Monitor depends on. The
// daemon's composition root supplies an implementation backed by
// *store.Store (see internal/daemon/stall_adapter.go).
//
// InsertStall: layer is 1 (heartbeat) or 2 (tool-call timeout).
// clearedAt is unix seconds; zero means "still active" (the
// implementation maps to a SQL NULL). stageID zero is mapped to NULL.
//
// ClearStall: sets cleared_at on the active row referenced by id.
type StallStore interface {
	InsertStall(ctx context.Context, runID string, stageID int64, layer int, detectedAt, clearedAt int64, action, details string) (int64, error)
	ClearStall(ctx context.Context, id, clearedAt int64) error
}

// Signaller is the kill contract. *Subprocess satisfies it. Defined
// separately so tests can substitute a fake.
type Signaller interface {
	Signal(name string) error
}

// MonitorConfig wires the monitor to a single stage execution. All
// duration fields accept zero meaning "disable this layer."
type MonitorConfig struct {
	RunID            string
	StageID          int64 // 0 means "no stage context" (writes NULL in SQL)
	HeartbeatTimeout time.Duration
	ToolCallTimeout  time.Duration
	PollInterval     time.Duration
	Store            StallStore // nil = log-only; SIGTERM still fires on L2
	Signaller        Signaller  // nil disables the kill on L2 (still records)
}

// Monitor is a per-stage stall watcher. One per RunStage call. Caller
// invokes OnEvent synchronously from the subprocess event loop and runs
// Run in a separate goroutine; both are safe under concurrent calls.
//
// L1 (heartbeat): tracks last-event timestamp. On poll, if delta >
// HeartbeatTimeout AND no L1 row is active, insert one and remember
// its ID for later clearing. When a new event arrives, the next
// OnEvent call records the timestamp and the next poll observes
// delta <= HeartbeatTimeout, triggering a ClearStall on the active row.
//
// L2 (tool-call timeout): tracks per-tool-call started_at. On poll,
// if any inflight tool started > ToolCallTimeout ago, insert a row
// (action=killed_subprocess), call Signaller.Signal("SIGTERM"), set
// killed flag, stop the loop. L2 fires once per stage (single-shot).
type Monitor struct {
	cfg MonitorConfig

	mu          sync.Mutex
	lastEventTs time.Time
	inflight    map[string]inflightTool // by tool_use ID
	activeL1ID  int64                   // 0 = no active L1 row
	killed      bool
	culpritTool string
}

type inflightTool struct {
	Name      string
	Args      json.RawMessage
	StartedAt time.Time
}

// NewMonitor constructs a Monitor. Caller must invoke Run for layers
// to actually fire; OnEvent updates state safely whether Run is running
// or not.
func NewMonitor(cfg MonitorConfig) *Monitor {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	return &Monitor{
		cfg:         cfg,
		lastEventTs: time.Now(),
		inflight:    map[string]inflightTool{},
	}
}

// OnEvent updates monitor state for one observed event. Safe to call
// concurrently with Run.
func (m *Monitor) OnEvent(ev Event, when time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastEventTs = when

	recordUse := func(id, name string, input json.RawMessage) {
		if id == "" {
			return
		}
		m.inflight[id] = inflightTool{Name: name, Args: input, StartedAt: when}
	}
	recordResult := func(useID string) {
		if useID == "" {
			return
		}
		delete(m.inflight, useID)
	}

	// Shape 1: top-level tool_use / tool_result (fake-claude fixtures).
	switch ev.Type {
	case EventToolUse:
		recordUse(ev.ToolID, ev.ToolName, ev.ToolInput)
	case EventToolResult:
		recordResult(ev.ToolUseID)
	}
	// Shape 2: nested in assistant/user message.content (real claude).
	for _, block := range ev.Message.Content {
		switch block.Type {
		case "tool_use":
			recordUse(block.ID, block.Name, block.Input)
		case "tool_result":
			recordResult(block.ToolUseID)
		}
	}
}

// SetSignaller assigns the signaller used by L2. Used by the adapter
// once the subprocess is constructed (which happens after the monitor
// since OnEvent must be installed on the subprocess config).
func (m *Monitor) SetSignaller(s Signaller) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Signaller = s
}

// Run drives the poll loop until ctx is canceled or L2 fires. Both
// outcomes leave Killed() consistent.
func (m *Monitor) Run(ctx context.Context) {
	// Skip entirely when both layers disabled.
	if m.cfg.HeartbeatTimeout <= 0 && m.cfg.ToolCallTimeout <= 0 {
		return
	}
	t := time.NewTicker(m.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if m.tick(ctx, now) {
				return // L2 fired; stop monitoring.
			}
		}
	}
}

// tick runs one pass. Returns true if L2 fired (caller should stop).
// Designed callable from tests for determinism.
func (m *Monitor) tick(ctx context.Context, now time.Time) bool {
	m.mu.Lock()
	heartbeat := m.cfg.HeartbeatTimeout
	toolTimeout := m.cfg.ToolCallTimeout
	lastTs := m.lastEventTs
	activeL1 := m.activeL1ID
	var stuck *inflightTool
	var stuckID string
	if toolTimeout > 0 {
		for id, p := range m.inflight {
			if now.Sub(p.StartedAt) > toolTimeout {
				cp := p
				stuck = &cp
				stuckID = id
				break
			}
		}
	}
	m.mu.Unlock()

	if stuck != nil {
		m.fireL2(ctx, now, *stuck, stuckID)
		return true
	}

	if heartbeat > 0 {
		elapsed := now.Sub(lastTs)
		if elapsed > heartbeat && activeL1 == 0 {
			m.fireL1(ctx, now, elapsed)
		} else if elapsed <= heartbeat && activeL1 != 0 {
			m.clearL1(ctx, now)
		}
	}
	return false
}

func (m *Monitor) fireL1(ctx context.Context, now time.Time, elapsed time.Duration) {
	details := fmt.Sprintf(`{"last_event_at":%d,"elapsed_seconds":%d}`,
		now.Add(-elapsed).Unix(), int(elapsed.Seconds()))
	if m.cfg.Store == nil {
		log.Printf("stall_monitor: L1 surfaced for run %s (no store; elapsed=%s)", m.cfg.RunID, elapsed)
		return
	}
	id, err := m.cfg.Store.InsertStall(ctx, m.cfg.RunID, m.cfg.StageID, 1, now.Unix(), 0, "surfaced", details)
	if err != nil {
		log.Printf("stall_monitor: L1 insert for run %s: %v", m.cfg.RunID, err)
		return
	}
	m.mu.Lock()
	m.activeL1ID = id
	m.mu.Unlock()
}

func (m *Monitor) clearL1(ctx context.Context, now time.Time) {
	m.mu.Lock()
	id := m.activeL1ID
	m.activeL1ID = 0
	m.mu.Unlock()
	if id == 0 || m.cfg.Store == nil {
		return
	}
	if err := m.cfg.Store.ClearStall(ctx, id, now.Unix()); err != nil {
		log.Printf("stall_monitor: L1 clear for run %s: %v", m.cfg.RunID, err)
	}
}

func (m *Monitor) fireL2(ctx context.Context, now time.Time, stuck inflightTool, stuckID string) {
	details := fmt.Sprintf(`{"tool":%q,"tool_id":%q,"args":%s,"elapsed_seconds":%d}`,
		stuck.Name, stuckID, jsonOr(stuck.Args, "null"), int(now.Sub(stuck.StartedAt).Seconds()))
	clearedAt := now.Unix()
	if m.cfg.Store != nil {
		if _, err := m.cfg.Store.InsertStall(ctx, m.cfg.RunID, m.cfg.StageID, 2,
			now.Unix(), clearedAt, "killed_subprocess", details); err != nil {
			log.Printf("stall_monitor: L2 insert for run %s: %v", m.cfg.RunID, err)
		}
	}
	m.mu.Lock()
	sig := m.cfg.Signaller
	m.mu.Unlock()
	if sig != nil {
		if err := sig.Signal("SIGTERM"); err != nil {
			log.Printf("stall_monitor: SIGTERM for run %s: %v", m.cfg.RunID, err)
		}
	}
	m.mu.Lock()
	m.killed = true
	m.culpritTool = stuck.Name
	m.mu.Unlock()
}

// Killed returns true after L2 fired. The adapter checks this post-Run
// to decide whether to return ErrToolCallStall.
func (m *Monitor) Killed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.killed
}

// CulpritTool returns the name of the tool that triggered L2, if any.
// Caller wraps it into the typed error for executePipeline.
func (m *Monitor) CulpritTool() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.culpritTool == "" {
		return "", false
	}
	return m.culpritTool, true
}

// jsonOr returns the raw JSON bytes as a string, or fallback if empty
// / invalid. Used to inline raw args into details_json.
func jsonOr(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fallback
	}
	return string(raw)
}

// ErrToolCallStall is returned by the adapter when the monitor SIGTERM'd
// the subprocess due to an L2 detection. Carries the offending tool
// name via Unwrap+Error so executePipeline can include it in the
// run summary.
var ErrToolCallStall = errors.New("tool-call stall: subprocess SIGTERM'd")

// toolCallStallError wraps ErrToolCallStall with the tool name. The
// sentinel is exported so callers can use errors.Is; the wrapper struct
// is internal.
type toolCallStallError struct {
	tool string
}

func (e *toolCallStallError) Error() string {
	return fmt.Sprintf("tool-call stall: subprocess SIGTERM'd (tool=%s)", e.tool)
}
func (e *toolCallStallError) Unwrap() error { return ErrToolCallStall }
func (e *toolCallStallError) Tool() string  { return e.tool }

// errToolCallStallWith constructs the wrapped error. Adapter calls this
// when monitor.CulpritTool() reports a non-empty tool.
func errToolCallStallWith(tool string) error { return &toolCallStallError{tool: tool} }

// StallToolFromError extracts the tool name from an ErrToolCallStall
// error wrapper. Returns ("", false) if err isn't a stall wrapper.
func StallToolFromError(err error) (string, bool) {
	var w *toolCallStallError
	if errors.As(err, &w) {
		return w.tool, true
	}
	return "", false
}
