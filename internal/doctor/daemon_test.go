package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/store"
)

func TestDaemonPidfileMissingIsWarn(t *testing.T) {
	hiveDir := t.TempDir()
	checks := runDaemonChecks(context.Background(), hiveDir, &stubRPCClient{statusErr: errSocketDown})
	c := findCheck(t, checks, "daemon.pidfile")
	if c.Status != StatusWarn {
		t.Errorf("missing pidfile: status=%s, want warn", c.Status)
	}
}

func TestDaemonPidfileAlivePassesIfPIDExists(t *testing.T) {
	hiveDir := t.TempDir()
	pidPath := filepath.Join(hiveDir, "daemon.pid")
	// Use current process PID — guaranteed alive.
	pid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	checks := runDaemonChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "daemon.pidfile")
	if c.Status != StatusOK {
		t.Errorf("live pidfile: status=%s, want ok; msg=%q", c.Status, c.Message)
	}
}

func TestDaemonPidfileStaleIsError(t *testing.T) {
	hiveDir := t.TempDir()
	pidPath := filepath.Join(hiveDir, "daemon.pid")
	deadPID := findDeadPID(t)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(deadPID)), 0o644); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	checks := runDaemonChecks(context.Background(), hiveDir, &stubRPCClient{statusErr: errSocketDown})
	c := findCheck(t, checks, "daemon.pidfile")
	if c.Status != StatusError {
		t.Errorf("stale pidfile: status=%s, want error", c.Status)
	}
}

func TestDaemonSocketDailyReturnsErrorWhenPidAlive(t *testing.T) {
	hiveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hiveDir, "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	checks := runDaemonChecks(context.Background(), hiveDir, &stubRPCClient{statusErr: errSocketDown})
	c := findCheck(t, checks, "daemon.socket")
	if c.Status != StatusError {
		t.Errorf("split brain (pid alive, socket down): status=%s, want error", c.Status)
	}
}

func TestDaemonSocketWarnWhenPidMissingAndSocketDown(t *testing.T) {
	hiveDir := t.TempDir()
	checks := runDaemonChecks(context.Background(), hiveDir, &stubRPCClient{statusErr: errSocketDown})
	c := findCheck(t, checks, "daemon.socket")
	if c.Status != StatusWarn {
		t.Errorf("daemon not running: status=%s, want warn", c.Status)
	}
}

func TestDaemonLastTickFreshIsOK(t *testing.T) {
	hiveDir := t.TempDir()
	client := &stubRPCClient{
		health: HealthSnapshot{LastTickUnix: time.Now().Unix() - 2},
	}
	checks := runDaemonChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "daemon.last_tick")
	if c.Status != StatusOK {
		t.Errorf("fresh tick: status=%s, want ok", c.Status)
	}
}

func TestDaemonLastTickStaleIsWarnOrError(t *testing.T) {
	hiveDir := t.TempDir()
	// 70s stale → error.
	client := &stubRPCClient{
		health: HealthSnapshot{LastTickUnix: time.Now().Unix() - 70},
	}
	checks := runDaemonChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "daemon.last_tick")
	if c.Status != StatusError {
		t.Errorf("70s stale: status=%s, want error", c.Status)
	}
}

func TestDaemonSchemaMatchPassesOnMatch(t *testing.T) {
	hiveDir := t.TempDir()
	client := &stubRPCClient{
		health: HealthSnapshot{SchemaVersionDB: store.MaxSchemaVersion, LastTickUnix: time.Now().Unix()},
	}
	checks := runDaemonChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "daemon.schema_match")
	if c.Status != StatusOK {
		t.Errorf("schema match: status=%s, want ok", c.Status)
	}
}

func TestDaemonPidfileGarbageContentsIsError(t *testing.T) {
	hiveDir := t.TempDir()
	pidPath := filepath.Join(hiveDir, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte("not a number"), 0o644); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	checks := runDaemonChecks(context.Background(), hiveDir, &stubRPCClient{statusErr: errSocketDown})
	c := findCheck(t, checks, "daemon.pidfile")
	if c.Status != StatusError {
		t.Errorf("garbage pidfile: status=%s, want error", c.Status)
	}
}

func TestDaemonSchemaMatchMismatchIsError(t *testing.T) {
	hiveDir := t.TempDir()
	// Pretend the DB has v1 but binary expects MaxSchemaVersion.
	client := &stubRPCClient{
		health: HealthSnapshot{SchemaVersionDB: 1, LastTickUnix: time.Now().Unix()},
	}
	checks := runDaemonChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "daemon.schema_match")
	if c.Status != StatusError {
		t.Errorf("schema mismatch: status=%s, want error", c.Status)
	}
}

// findCheck returns the first Check with the given Name, failing the
// test if none found.
func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in %v", name, checks)
	return Check{}
}

// findDeadPID forks a short-lived child (`true`), waits for it to
// exit (reaping it), and returns its PID. The PID is then guaranteed
// dead. Recycling is theoretically possible but the test uses the
// PID within microseconds, before the kernel reassigns.
func findDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
	// Verify the PID is dead — sanity, not a guarantee against recycle.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Skipf("PID %d unexpectedly alive immediately after Wait (recycle?); skipping", pid)
	} else if !errors.Is(err, syscall.ESRCH) {
		t.Skipf("kill(%d, 0) returned %v; skipping", pid, err)
	}
	return pid
}
