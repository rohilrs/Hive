package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohilrs/Hive/internal/store"
)

func TestStoreDBReadableOnFreshOpenIsOK(t *testing.T) {
	hiveDir := t.TempDir()
	// Use store.Open to create a real WAL DB at hiveDir/db.sqlite.
	s, err := store.Open(context.Background(), filepath.Join(hiveDir, "db.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	s.Close()

	checks := runStoreChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "store.db_readable")
	if c.Status != StatusOK {
		t.Errorf("fresh DB: status=%s, want ok; msg=%q", c.Status, c.Message)
	}
}

func TestStoreSchemaVersionMatchesMaxAfterFreshOpen(t *testing.T) {
	hiveDir := t.TempDir()
	s, err := store.Open(context.Background(), filepath.Join(hiveDir, "db.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	s.Close()

	checks := runStoreChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "store.schema_version")
	if c.Status != StatusOK {
		t.Errorf("fresh DB schema: status=%s, want ok; msg=%q", c.Status, c.Message)
	}
}

func TestStoreWALModeOKAfterFreshOpen(t *testing.T) {
	hiveDir := t.TempDir()
	s, err := store.Open(context.Background(), filepath.Join(hiveDir, "db.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	s.Close()

	checks := runStoreChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "store.wal_mode")
	if c.Status != StatusOK {
		t.Errorf("fresh DB wal: status=%s, want ok; msg=%q", c.Status, c.Message)
	}
}

func TestStoreDBReadableNoFileIsWarn(t *testing.T) {
	hiveDir := t.TempDir()
	// No DB file at all.
	if _, err := os.Stat(filepath.Join(hiveDir, "db.sqlite")); err == nil {
		t.Fatal("expected no DB file at start")
	}

	checks := runStoreChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "store.db_readable")
	if c.Status != StatusWarn {
		t.Errorf("missing DB: status=%s, want warn; msg=%q", c.Status, c.Message)
	}
}
