package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSequenceDispatcherCRUD(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetSequenceDispatcher(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent: got %v, want ErrNotFound", err)
	}

	if err := s.UpsertSequenceDispatcher(ctx, &SequenceDispatcher{
		ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSequenceDispatcher(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || got.AdvancementPolicy != "pr_opened" {
		t.Fatalf("got %+v", got)
	}
	if got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Errorf("timestamps not set: %+v", got)
	}

	if err := s.UpsertSequenceDispatcher(ctx, &SequenceDispatcher{
		ProjectID: "p1", Status: "paused", AdvancementPolicy: "pr_opened",
	}); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetSequenceDispatcher(ctx, "p1")
	if got2.Status != "paused" {
		t.Errorf("status = %q, want paused", got2.Status)
	}
	if got2.CreatedAt != got.CreatedAt {
		t.Errorf("created_at changed on update: %d -> %d", got.CreatedAt, got2.CreatedAt)
	}

	if err := s.DeleteSequenceDispatcher(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSequenceDispatcher(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: got %v, want ErrNotFound", err)
	}
}

func TestMarkPhaseComplete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p2", Slug: "p2", Name: "P2", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpsertSequenceDispatcher(ctx, &SequenceDispatcher{
		ProjectID: "p2", Status: "active", AdvancementPolicy: "pr_opened",
	}); err != nil {
		t.Fatal(err)
	}

	// Mark phase "1" complete.
	if err := s.MarkPhaseComplete(ctx, "p2", "1"); err != nil {
		t.Fatalf("MarkPhaseComplete(1): %v", err)
	}

	// Mark phase "1" again — must be idempotent (no error, no duplicate).
	if err := s.MarkPhaseComplete(ctx, "p2", "1"); err != nil {
		t.Fatalf("MarkPhaseComplete(1) idempotent: %v", err)
	}

	// Mark phase "2a" complete.
	if err := s.MarkPhaseComplete(ctx, "p2", "2a"); err != nil {
		t.Fatalf("MarkPhaseComplete(2a): %v", err)
	}

	d, err := s.GetSequenceDispatcher(ctx, "p2")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(d.CompletedPhases, ",")
	if got != "1,2a" {
		t.Errorf("CompletedPhases = %q, want %q", got, "1,2a")
	}
}
