package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestIterFeedbackRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	payload := []byte(`[{"path":"a.go","line":12,"comment":"nil check","reasoning":"panics"}]`)

	if err := s.PutFeedbackJSON(ctx, "run-1", 0, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.GetFeedbackJSON(ctx, "run-1", 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("got=%s want=%s", got, payload)
	}
}

func TestIterFeedbackMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, err = s.GetFeedbackJSON(ctx, "missing", 0)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}

func TestIterFeedbackUpsert(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	first := []byte(`[{"path":"a.go","comment":"first"}]`)
	second := []byte(`[{"path":"a.go","comment":"second"}]`)

	if err := s.PutFeedbackJSON(ctx, "run-2", 0, first); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFeedbackJSON(ctx, "run-2", 0, second); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, _ := s.GetFeedbackJSON(ctx, "run-2", 0)
	if string(got) != string(second) {
		t.Errorf("got=%s want second", got)
	}
}
