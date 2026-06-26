package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
)

func TestFeedbackAdapterRoundTrip(t *testing.T) {
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	adp := feedbackAdapter{S: s}
	want := pipeline.Feedback{
		Summary: "error handling is inconsistent throughout the package",
		FileRefs: []verdict.FileRef{
			{Path: "a.go", Line: 5, Comment: "fix", Reasoning: "because"},
		},
	}
	if err := adp.Put(context.Background(), "r", 0, want); err != nil {
		t.Fatal(err)
	}
	got, err := adp.Get(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != want.Summary {
		t.Errorf("Summary: got=%q want=%q", got.Summary, want.Summary)
	}
	if len(got.FileRefs) != 1 || got.FileRefs[0] != want.FileRefs[0] {
		t.Errorf("FileRefs: got=%+v want=%+v", got.FileRefs, want.FileRefs)
	}

	_, err = adp.Get(context.Background(), "missing", 0)
	if !errors.Is(err, pipeline.ErrFeedbackNotFound) {
		t.Errorf("expected ErrFeedbackNotFound, got %v", err)
	}
}

// TestFeedbackAdapterOldRowTolerance verifies that a bare []FileRef row
// (written by a pre-Feedback-record daemon) decodes without error into a
// Feedback with the refs intact and an empty Summary.
func TestFeedbackAdapterOldRowTolerance(t *testing.T) {
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Write a bare []FileRef directly (simulates an old-format row).
	rawOld := []byte(`[{"path":"old.go","line":3,"comment":"old comment","reasoning":"old reason"}]`)
	if err := s.PutFeedbackJSON(context.Background(), "r", 0, rawOld); err != nil {
		t.Fatal(err)
	}

	adp := feedbackAdapter{S: s}
	got, err := adp.Get(context.Background(), "r", 0)
	if err != nil {
		t.Fatalf("Get old row: %v", err)
	}
	if got.Summary != "" {
		t.Errorf("Summary for old row should be empty, got %q", got.Summary)
	}
	if len(got.FileRefs) != 1 || got.FileRefs[0].Path != "old.go" {
		t.Errorf("FileRefs for old row: got=%+v", got.FileRefs)
	}
}
