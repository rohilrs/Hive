package store

import (
	"context"
	"testing"
)

func TestMaxSchemaVersionMatchesLastMigration(t *testing.T) {
	migs := migrations()
	if len(migs) == 0 {
		t.Fatal("no migrations registered")
	}
	last := migs[len(migs)-1].version
	if MaxSchemaVersion != last {
		t.Errorf("MaxSchemaVersion=%d, last migration=%d (must equal)", MaxSchemaVersion, last)
	}
}

func TestSchemaVersionReturnsMigrationVersionAfterOpen(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	v, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != MaxSchemaVersion {
		t.Errorf("SchemaVersion=%d, MaxSchemaVersion=%d (must match after fresh Open)", v, MaxSchemaVersion)
	}
}
