package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestPutGetConfigSnapshot(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// FK requires project + task before run.
	_ = s.InsertProject(ctx, &Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "T", Status: "pending"})
	if err := s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "pending"}); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"Predictor":{"HaikuModel":"claude-haiku-4-5","Enabled":true}}`)
	if err := s.PutConfigSnapshot(ctx, "r1", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.GetConfigSnapshot(ctx, "r1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("round-trip mismatch: got=%s want=%s", got, payload)
	}
}

func TestGetConfigSnapshotMissing(t *testing.T) {
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	_, err := s.GetConfigSnapshot(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}

func TestGetConfigSnapshotNullColumn(t *testing.T) {
	// A run row that exists but has never had a snapshot written returns
	// ErrNotFound (NULL column == "no snapshot recorded").
	s, _ := Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	defer s.Close()
	ctx := context.Background()
	_ = s.InsertProject(ctx, &Project{ID: "p", Slug: "p", Name: "P", Status: "active"})
	_ = s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "T", Status: "pending"})
	_ = s.InsertRun(ctx, &Run{ID: "r1", TaskID: "t1", ProjectID: "p", Pipeline: "build", Status: "pending"})

	_, err := s.GetConfigSnapshot(ctx, "r1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound for NULL snapshot column", err)
	}
}
