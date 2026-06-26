package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type SubprocessConfig struct {
	Binary string
	Args   []string
	Env    []string // nil = inherit caller env (caller usually allowlists)
	Cwd    string

	// OnEvent, when non-nil, is invoked synchronously for every parsed
	// event before the event is appended to the result slice. The
	// timestamp is captured by the subprocess wrapper at the moment the
	// event arrives. Used by the stall monitor (Phase 3.2).
	OnEvent func(ev Event, when time.Time)

	// OnStarted, if non-nil, is invoked after cmd.Start() returns with
	// the subprocess PID. The daemon uses this to stamp runs.worker_pid
	// for restart-recovery. A non-nil error aborts the run: the worker
	// is SIGKILLed and the error is returned from Subprocess.Run.
	OnStarted func(pid int) error

	// OnExited, if non-nil, is invoked exactly once after the worker
	// has exited (graceful or via kill). The daemon uses this to
	// clear runs.worker_pid. The callback's return value is discarded.
	OnExited func(pid int)
}

type Subprocess struct {
	cfg SubprocessConfig

	mu  sync.Mutex
	cmd *exec.Cmd // set after Start(); nil before / after
}

func NewSubprocess(cfg SubprocessConfig) *Subprocess { return &Subprocess{cfg: cfg} }

type SubprocessResult struct {
	Events   []Event
	Stderr   string
	ExitCode int
}

// Signal sends a named signal to the running subprocess. Names: "SIGTERM",
// "SIGKILL", "SIGINT". Returns error if the subprocess hasn't started yet
// or has already exited.
func (s *Subprocess) Signal(name string) error {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("subprocess not running")
	}
	var sig syscall.Signal
	switch name {
	case "SIGTERM":
		sig = syscall.SIGTERM
	case "SIGKILL":
		sig = syscall.SIGKILL
	case "SIGINT":
		sig = syscall.SIGINT
	default:
		return fmt.Errorf("unknown signal %q", name)
	}
	// Target the whole process group (pgid == leader pid) so the worker's
	// descendants (MCP stage server, nested helpers) die with it rather
	// than orphaning. Falls back to the leader if the group send fails.
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		return cmd.Process.Signal(sig)
	}
	return nil
}

func (s *Subprocess) Run(ctx context.Context) (*SubprocessResult, error) {
	cmd := exec.CommandContext(ctx, s.cfg.Binary, s.cfg.Args...)
	if s.cfg.Env != nil {
		cmd.Env = s.cfg.Env
	}
	if s.cfg.Cwd != "" {
		cmd.Dir = s.cfg.Cwd
	}

	// Put the worker in its own process group so we can kill it AND every
	// descendant it spawns (the MCP stage server, nested claude helpers).
	// exec.CommandContext's default Cancel only kills the leader, leaving
	// grandchildren orphaned — so override Cancel to signal the whole group
	// (negative pid). WaitDelay bounds how long Wait blocks on pipe-holding
	// children after the group is signaled.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the whole group (pgid == leader pid),
		// killing the worker and every descendant it spawned.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cmd = nil
		s.mu.Unlock()
	}()

	pid := cmd.Process.Pid
	if s.cfg.OnStarted != nil {
		if startErr := s.cfg.OnStarted(pid); startErr != nil {
			// Stamp failed — kill the worker we just started + propagate.
			// Negative pid targets the whole process group (Setpgid=true
			// above), matching the existing Signal()/Cancel paths.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			// Reap so we don't leak a zombie.
			_ = cmd.Wait()
			if s.cfg.OnExited != nil {
				s.cfg.OnExited(pid)
			}
			return nil, fmt.Errorf("on_started callback: %w", startErr)
		}
	}
	// Defer registered AFTER the OnStarted-error branch so OnExited
	// fires exactly once: either via this defer (success path) or via
	// the explicit call in the error branch above.
	defer func() {
		if s.cfg.OnExited != nil {
			s.cfg.OnExited(pid)
		}
	}()

	var (
		events   []Event
		stderr   string
		eventsCh = make(chan Event, 64)
		stderrCh = make(chan string, 1)
		wg       sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(eventsCh)
		parseJSONL(stdout, eventsCh)
	}()
	go func() {
		defer wg.Done()
		b, _ := io.ReadAll(stderrPipe)
		stderrCh <- string(b)
		close(stderrCh)
	}()

	for ev := range eventsCh {
		if s.cfg.OnEvent != nil {
			s.cfg.OnEvent(ev, time.Now())
		}
		events = append(events, ev)
	}
	stderr = <-stderrCh

	waitErr := cmd.Wait()
	wg.Wait()

	res := &SubprocessResult{Events: events, Stderr: stderr}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if waitErr != nil {
		// Surface the actual failure reason, not just "exit status N". claude
		// emits real errors (auth 401s, API overload, max-turns) on STDOUT as
		// the terminal `result` frame or a trailing assistant text block, while
		// STDERR often holds only benign warnings (e.g. a stale --disallowedTools
		// deny rule). Prefer the stdout-derived reason so the surfaced error
		// isn't a red herring; fall back to stderr, then to the bare exit code.
		reason := failureReason(events)
		stderrStr := capRunes(strings.TrimSpace(stderr), 500)
		switch {
		case reason != "" && stderrStr != "":
			return res, fmt.Errorf("claude subprocess: %w (%s; stderr: %s)", waitErr, reason, stderrStr)
		case reason != "":
			return res, fmt.Errorf("claude subprocess: %w (%s)", waitErr, reason)
		case stderrStr != "":
			return res, fmt.Errorf("claude subprocess: %w (stderr: %s)", waitErr, stderrStr)
		default:
			return res, fmt.Errorf("claude subprocess: %w", waitErr)
		}
	}
	return res, nil
}

// failureReason mines the parsed event stream for the human-readable reason a
// turn failed. claude reports failures on stdout, not stderr: either as the
// terminal `result` frame (with an error subtype and/or a "result" text) or as
// the last assistant text block (e.g. "Failed to authenticate. API Error: 401
// ..."). Returns "" when nothing useful is found. Capped to keep error strings
// readable in chat_messages rows.
func failureReason(events []Event) string {
	var resultText, resultSubtype, lastAssistant string
	for _, ev := range events {
		switch ev.Type {
		case EventResult:
			if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
				resultSubtype = ev.Subtype
			}
			if ev.Result != "" {
				resultText = ev.Result
			}
		case EventText:
			if t := ev.Delta; t != "" {
				lastAssistant = t
			} else if ev.Text != "" {
				lastAssistant = ev.Text
			}
		}
		// Real claude nests assistant text inside message.content[] blocks.
		for _, block := range ev.Message.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				lastAssistant = block.Text
			}
		}
	}
	switch {
	case resultText != "":
		return capRunes(strings.TrimSpace(resultText), 500)
	case strings.TrimSpace(lastAssistant) != "":
		return capRunes(strings.TrimSpace(lastAssistant), 500)
	case resultSubtype != "":
		return resultSubtype
	default:
		return ""
	}
}

// capRunes truncates s to at most max runes, appending an ellipsis when cut.
func capRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}

func parseJSONL(r io.Reader, out chan<- Event) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		ev.Raw = json.RawMessage(line)
		out <- ev
	}
}
