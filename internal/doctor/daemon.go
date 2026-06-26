package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rohilrs/Hive/internal/store"
)

// runDaemonChecks evaluates the daemon subsystem: pidfile presence +
// liveness, socket reachability, scheduler tick freshness, and binary↔DB
// schema agreement. Each sub-check emits one Check.
func runDaemonChecks(ctx context.Context, hiveDir string, client RPCClient) []Check {
	var out []Check

	pidfileCheck, pidAlive := checkPidfile(hiveDir)
	out = append(out, pidfileCheck)

	out = append(out, checkSocket(ctx, client, pidAlive))

	// last_tick + schema_match depend on the daemon being reachable.
	if client == nil {
		out = append(out, skipCheck("daemon.last_tick", "daemon", "skipped — daemon not running"))
		out = append(out, skipCheck("daemon.schema_match", "daemon", "skipped — daemon not running"))
		return out
	}
	if err := client.Status(ctx); err != nil {
		out = append(out, skipCheck("daemon.last_tick", "daemon", "skipped — daemon not running"))
		out = append(out, skipCheck("daemon.schema_match", "daemon", "skipped — daemon not running"))
		return out
	}

	health, hErr := client.Health(ctx)
	if hErr != nil {
		out = append(out, skipCheck("daemon.last_tick", "daemon", "daemon.health rpc failed: "+hErr.Error()))
		out = append(out, skipCheck("daemon.schema_match", "daemon", "daemon.health rpc failed: "+hErr.Error()))
		return out
	}

	out = append(out, checkLastTick(health))
	out = append(out, checkSchemaMatch(health))
	return out
}

// checkPidfile reads ~/.hive/daemon.pid and signals the PID with 0 to
// distinguish: missing pidfile (warn), unreadable/garbage (error),
// dead PID (stale; error), live PID (ok). Returns the Check plus
// whether the PID is alive so the socket check can disambiguate
// split-brain (pid alive but socket dead) from clean "daemon down".
func checkPidfile(hiveDir string) (Check, bool) {
	pidPath := filepath.Join(hiveDir, "daemon.pid")
	data, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return Check{
			Name: "daemon.pidfile", Subsystem: "daemon",
			Status:  StatusWarn,
			Message: "no pidfile (" + pidPath + ")",
			Hint:    "daemon may not be running; start with: hive daemon",
		}, false
	}
	if err != nil {
		return Check{
			Name: "daemon.pidfile", Subsystem: "daemon",
			Status:  StatusError,
			Message: "read pidfile: " + err.Error(),
			Hint:    "fix filesystem permissions or remove the file",
		}, false
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil {
		return Check{
			Name: "daemon.pidfile", Subsystem: "daemon",
			Status:  StatusError,
			Message: "pidfile contents not numeric: " + perr.Error(),
			Hint:    "remove the file: rm " + pidPath,
		}, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return Check{
				Name: "daemon.pidfile", Subsystem: "daemon",
				Status:  StatusError,
				Message: fmt.Sprintf("pid %d dead (stale pidfile)", pid),
				Hint:    "remove the file: rm " + pidPath,
			}, false
		}
		// EPERM means the process exists but we lack permission to
		// signal it — treat as alive.
		return Check{
			Name: "daemon.pidfile", Subsystem: "daemon",
			Status:  StatusOK,
			Message: fmt.Sprintf("pid %d alive", pid),
		}, true
	}
	return Check{
		Name: "daemon.pidfile", Subsystem: "daemon",
		Status:  StatusOK,
		Message: fmt.Sprintf("pid %d alive", pid),
	}, true
}

// checkSocket attempts an RPC Status call. Distinguishes "daemon
// genuinely down" (warn) from "pidfile says alive but socket is dead"
// (split-brain, error) using the pidAlive hint from checkPidfile.
func checkSocket(ctx context.Context, client RPCClient, pidAlive bool) Check {
	if client == nil {
		return Check{
			Name: "daemon.socket", Subsystem: "daemon",
			Status:  StatusWarn,
			Message: "no client configured",
		}
	}
	err := client.Status(ctx)
	if err == nil {
		return Check{
			Name: "daemon.socket", Subsystem: "daemon",
			Status:  StatusOK,
			Message: "responding",
		}
	}
	if pidAlive {
		return Check{
			Name: "daemon.socket", Subsystem: "daemon",
			Status:  StatusError,
			Message: "pidfile alive but socket unreachable: " + err.Error(),
			Hint:    "split-brain — try: kill <pid> && rm ~/.hive/daemon.{sock,pid} && hive daemon",
		}
	}
	return Check{
		Name: "daemon.socket", Subsystem: "daemon",
		Status:  StatusWarn,
		Message: "socket unreachable: " + err.Error(),
		Hint:    "daemon not running; start with: hive daemon",
	}
}

// checkLastTick classifies HealthSnapshot.LastTickUnix into freshness
// bands: <=15s ok, <=60s warn, >60s error.
func checkLastTick(h HealthSnapshot) Check {
	if h.LastTickUnix == 0 {
		return Check{
			Name: "daemon.last_tick", Subsystem: "daemon",
			Status:  StatusWarn,
			Message: "no tick recorded yet (daemon just booted?)",
		}
	}
	age := time.Now().Unix() - h.LastTickUnix
	switch {
	case age <= 15:
		return Check{
			Name: "daemon.last_tick", Subsystem: "daemon",
			Status:  StatusOK,
			Message: fmt.Sprintf("last tick %ds ago", age),
		}
	case age <= 60:
		return Check{
			Name: "daemon.last_tick", Subsystem: "daemon",
			Status:  StatusWarn,
			Message: fmt.Sprintf("last tick %ds ago (warn > 15s)", age),
			Hint:    "scheduler may be busy with a long check; investigate active runs",
		}
	default:
		return Check{
			Name: "daemon.last_tick", Subsystem: "daemon",
			Status:  StatusError,
			Message: fmt.Sprintf("last tick %ds ago (error > 60s)", age),
			Hint:    "scheduler loop wedged — try restarting daemon",
		}
	}
}

// checkSchemaMatch compares HealthSnapshot.SchemaVersionDB (what the
// daemon sees in the DB) to store.MaxSchemaVersion (what the binary
// was built against). A mismatch means a binary↔DB drift that needs a
// daemon restart.
func checkSchemaMatch(h HealthSnapshot) Check {
	if h.SchemaVersionDB == store.MaxSchemaVersion {
		return Check{
			Name: "daemon.schema_match", Subsystem: "daemon",
			Status:  StatusOK,
			Message: fmt.Sprintf("db schema v%d matches binary", h.SchemaVersionDB),
		}
	}
	return Check{
		Name: "daemon.schema_match", Subsystem: "daemon",
		Status:  StatusError,
		Message: fmt.Sprintf("db schema v%d != binary v%d", h.SchemaVersionDB, store.MaxSchemaVersion),
		Hint:    "the daemon binary expects a different schema; restart the daemon after rebuild",
	}
}

// skipCheck builds a StatusSkip Check. Shared by daemon + store (and
// future Tasks 7-8) for "subsystem unreachable, can't evaluate".
func skipCheck(name, subsystem, message string) Check {
	return Check{Name: name, Subsystem: subsystem, Status: StatusSkip, Message: message}
}
