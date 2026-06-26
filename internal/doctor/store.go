package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/rohilrs/Hive/internal/store"
)

// runStoreChecks opens ~/.hive/db.sqlite read-only (filesystem-direct,
// no daemon required) and runs three checks: integrity_check, schema
// version vs. binary, and journal_mode == wal. Daemon-down-safe: works
// even if the daemon isn't running, because doctor's whole point is
// "is anything broken even when nothing else is up?".
func runStoreChecks(ctx context.Context, hiveDir string, client RPCClient) []Check {
	dbPath := filepath.Join(hiveDir, "db.sqlite")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return []Check{
			{
				Name: "store.db_readable", Subsystem: "store",
				Status:  StatusWarn,
				Message: "no db file at " + dbPath,
				Hint:    "daemon has not run here yet; start with: hive daemon",
			},
			skipCheck("store.schema_version", "store", "skipped — no db file"),
			skipCheck("store.wal_mode", "store", "skipped — no db file"),
		}
	}

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return []Check{
			{Name: "store.db_readable", Subsystem: "store", Status: StatusError, Message: "open: " + err.Error()},
			skipCheck("store.schema_version", "store", "skipped — db not openable"),
			skipCheck("store.wal_mode", "store", "skipped — db not openable"),
		}
	}
	defer db.Close()

	var out []Check
	out = append(out, checkDBReadable(ctx, db))
	out = append(out, checkSchemaVersion(ctx, db))
	out = append(out, checkWALMode(ctx, db))
	return out
}

// checkDBReadable runs PRAGMA integrity_check. SQLite returns "ok" on
// a healthy DB; anything else signals corruption.
func checkDBReadable(ctx context.Context, db *sql.DB) Check {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return Check{Name: "store.db_readable", Subsystem: "store", Status: StatusError, Message: "integrity_check: " + err.Error()}
	}
	if result != "ok" {
		return Check{Name: "store.db_readable", Subsystem: "store", Status: StatusError, Message: "integrity_check returned: " + result, Hint: "DB may be corrupted; back up before any repair"}
	}
	return Check{Name: "store.db_readable", Subsystem: "store", Status: StatusOK, Message: "integrity_check ok"}
}

// checkSchemaVersion compares the highest applied migration to
// store.MaxSchemaVersion (what the binary expects). Mismatch =
// pending migrations or binary↔DB drift.
func checkSchemaVersion(ctx context.Context, db *sql.DB) Check {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		return Check{Name: "store.schema_version", Subsystem: "store", Status: StatusError, Message: "read schema_migrations: " + err.Error(), Hint: "DB may be from a different binary or corrupted"}
	}
	if !v.Valid {
		return Check{Name: "store.schema_version", Subsystem: "store", Status: StatusError, Message: "schema_migrations empty"}
	}
	got := int(v.Int64)
	if got != store.MaxSchemaVersion {
		return Check{Name: "store.schema_version", Subsystem: "store", Status: StatusError, Message: fmt.Sprintf("db v%d != binary v%d", got, store.MaxSchemaVersion), Hint: "rebuild the binary or restart the daemon to apply pending migrations"}
	}
	return Check{Name: "store.schema_version", Subsystem: "store", Status: StatusOK, Message: fmt.Sprintf("v%d", got)}
}

// checkWALMode verifies journal_mode is "wal". A non-wal DB serializes
// readers (concurrent runs would block) — store.Open re-asserts WAL on
// every open, so any drift here is a write-side anomaly.
func checkWALMode(ctx context.Context, db *sql.DB) Check {
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return Check{Name: "store.wal_mode", Subsystem: "store", Status: StatusWarn, Message: "pragma journal_mode: " + err.Error()}
	}
	if mode != "wal" {
		return Check{Name: "store.wal_mode", Subsystem: "store", Status: StatusWarn, Message: "journal_mode=" + mode + " (expected wal)", Hint: "concurrent reads may be serialized; rerun with daemon to re-set"}
	}
	return Check{Name: "store.wal_mode", Subsystem: "store", Status: StatusOK, Message: "wal"}
}
