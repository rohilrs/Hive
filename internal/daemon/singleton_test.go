package daemon

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireSingletonLockBlocksSecond(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "daemon.pid")
	sockPath := filepath.Join(tmpDir, "daemon.sock")

	f1, err := acquireSingletonLock(pidPath, sockPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer f1.Close()

	_, err = acquireSingletonLock(pidPath, sockPath)
	if err == nil {
		t.Fatalf("second acquire should fail")
	}
	if !strings.Contains(err.Error(), "alive") && !strings.Contains(err.Error(), "already running") {
		t.Errorf("error should mention 'alive' or 'already running'; got %q", err.Error())
	}
}

func TestAcquireSingletonLockReleaseAllowsReacquire(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "daemon.pid")
	sockPath := filepath.Join(tmpDir, "daemon.sock")

	f1, err := acquireSingletonLock(pidPath, sockPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_ = f1.Close() // closing releases the flock

	// Note: the pidfile still contains our (alive) PID, so the prior-PID
	// liveness probe in priorDaemonLivenessCheck will refuse. Clear the
	// pidfile to simulate the "previous daemon exited cleanly" case the
	// original test was modelling.
	if err := os.Remove(pidPath); err != nil {
		t.Fatal(err)
	}

	f2, err := acquireSingletonLock(pidPath, sockPath)
	if err != nil {
		t.Fatalf("re-acquire after close should succeed: %v", err)
	}
	defer f2.Close()
}

func TestAcquireSingletonLockWritesPID(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "daemon.pid")
	sockPath := filepath.Join(tmpDir, "daemon.sock")
	f, err := acquireSingletonLock(pidPath, sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	pid := readPriorPID(pidPath)
	if pid <= 0 {
		t.Errorf("expected non-zero pid in file, got %d", pid)
	}
}

func TestAcquireSingletonLockErrorMentionsPriorPID(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "daemon.pid")
	sockPath := filepath.Join(tmpDir, "daemon.sock")
	f1, err := acquireSingletonLock(pidPath, sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f1.Close()

	_, err = acquireSingletonLock(pidPath, sockPath)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "pid ") {
		t.Errorf("error should reference prior pid; got %q", err.Error())
	}
}

func TestSingletonLockRejectsWhenPriorPIDAlive(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "daemon.pid")
	sockPath := filepath.Join(tmpDir, "daemon.sock")
	// Write OUR pid to the pidfile (we're guaranteed alive).
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := acquireSingletonLock(pidPath, sockPath)
	if err == nil {
		t.Fatal("expected refusal when prior PID is our own (alive)")
	}
	if !strings.Contains(err.Error(), "alive") {
		t.Errorf("error doesn't mention alive: %v", err)
	}
}

func TestSingletonLockAcceptsDeadPriorPID(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "daemon.pid")
	sockPath := filepath.Join(tmpDir, "daemon.sock")
	// Fork+reap a child; its PID is guaranteed dead post-Wait.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadPID := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(deadPID)), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := acquireSingletonLock(pidPath, sockPath)
	if err != nil {
		t.Fatalf("expected to acquire lock with dead PID; got %v", err)
	}
	f.Close()
}

func TestSingletonLockRejectsWhenSocketDialable(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "daemon.pid") // doesn't exist; no pid probe
	sockPath := filepath.Join(tmpDir, "daemon.sock")

	// Bind a UDS listener on the socket path (simulating a live daemon).
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Accept connections in a goroutine so dial succeeds.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	_, err = acquireSingletonLock(pidPath, sockPath)
	if err == nil {
		t.Fatal("expected refusal when socket is dialable")
	}
	if !strings.Contains(err.Error(), "listening") {
		t.Errorf("error doesn't mention listening: %v", err)
	}
}

func TestSingletonLockAcceptsWhenSocketFileExistsButUnboundShouldDialFail(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "daemon.pid")
	sockPath := filepath.Join(tmpDir, "daemon.sock")
	// Create the socket file but DON'T listen on it — touch only.
	if err := os.WriteFile(sockPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := acquireSingletonLock(pidPath, sockPath)
	if err != nil {
		t.Fatalf("expected to acquire lock when sock file exists but no listener; got %v", err)
	}
	f.Close()
}
