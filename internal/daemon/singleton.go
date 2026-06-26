package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

// acquireSingletonLock tries to acquire an exclusive non-blocking flock
// on pidPath. Before attempting flock, runs priorDaemonLivenessCheck
// against (pidPath, sockPath) to catch two known flock-evasion patterns:
// (a) stale pidfile with the daemon still alive (rare; mostly clean today),
// (b) pidfile-deleted-while-alive — operator rm'd the pidfile, the surviving
//     daemon's flock is on an orphaned inode; a fresh flock would land on
//     a new inode and "succeed" without catching the running daemon.
//     Detected via the socket-dial probe.
//
// On success, returns the open file handle (caller must keep it open
// for the daemon's lifetime — the OS releases the lock when the handle
// is closed or the process dies). The file's contents are truncated and
// the current PID is written so an operator can `cat ~/.hive/daemon.pid`
// to see which process holds it.
//
// On lock conflict (another daemon is alive), returns an error
// referencing the prior PID for easier debugging. The OS clears the
// lock automatically when the holding process exits (SIGKILL, panic,
// clean exit alike) — stale PID files never accumulate.
func acquireSingletonLock(pidPath, sockPath string) (*os.File, error) {
	if err := priorDaemonLivenessCheck(pidPath, sockPath); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(pidPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open pid file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		priorPID := readPriorPID(pidPath)
		if priorPID > 0 {
			return nil, fmt.Errorf("another hive daemon is already running (pid %d); refusing to start a second instance. If that process is dead, remove %s and retry", priorPID, pidPath)
		}
		return nil, fmt.Errorf("another hive daemon is already running; refusing to start a second instance. Check %s for the holding pid", pidPath)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("truncate pid file: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write pid: %w", err)
	}
	return f, nil
}

// priorDaemonLivenessCheck runs two checks BEFORE attempting flock so
// acquireSingletonLock can refuse early when another daemon is clearly
// alive. Returns nil if no signs of another daemon. Returns a non-nil
// error if any probe detects a live daemon.
//
// Check 1 — PID liveness: read pidfile's stored PID, syscall.Kill(pid, 0).
//
//	nil  → alive (refuse)
//	EPERM → alive but foreign uid; refuse defensively (another user's process
//	        on this PID, but we can't tell if it's a hive daemon — safer
//	        to refuse than risk a double-daemon scenario)
//	ESRCH → dead; proceed
//
// Check 2 — Socket dial: stat sockPath; if it exists, net.DialTimeout for
//
//	50ms on the UDS. If the dial succeeds, another daemon is listening;
//	refuse. Catches the "pidfile rm'd but daemon still alive" gap that
//	bit during 2b.5 smoke (pidfile flock landed on a fresh inode, so the
//	flock test passed; the live daemon's listener was the surviving signal).
func priorDaemonLivenessCheck(pidPath, sockPath string) error {
	// Check 1: PID liveness.
	if pid := readPriorPID(pidPath); pid > 0 {
		err := syscall.Kill(pid, 0)
		if err == nil {
			return fmt.Errorf("another hive daemon (pid %d) is alive; refusing to start a second instance", pid)
		}
		if errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("another process (pid %d) holds the pidfile but we lack permission to probe it; refusing to start", pid)
		}
		// syscall.ESRCH (or other) → treat as dead; continue.
	}

	// Check 2: socket dial.
	if _, statErr := os.Stat(sockPath); statErr == nil {
		conn, dialErr := net.DialTimeout("unix", sockPath, 50*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return fmt.Errorf("a daemon is listening on %s; refusing to start a second instance", sockPath)
		}
		// Dial failed (refused, EOF, timeout) → no daemon owns it; continue.
	}
	return nil
}

// readPriorPID reads the PID from pidPath best-effort. Returns 0 on
// any failure — the error message in acquireSingletonLock is just
// informational.
func readPriorPID(pidPath string) int {
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0
	}
	return pid
}
