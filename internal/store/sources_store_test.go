package store

import (
	"context"
	"testing"
)

func TestListTasksBySource(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t2", ProjectID: "p1", Title: "y", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t3", ProjectID: "p1", Title: "z", Source: "github"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListTasksBySource(ctx, "p1", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTasksBySource returned %d tasks, want 2", len(got))
	}
	for _, tk := range got {
		if tk.Source != "inbox" {
			t.Errorf("task %s has source %q, want inbox", tk.ID, tk.Source)
		}
	}
}

func TestMarkTaskSourceClosed(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTask(ctx, &Task{ID: "t1", ProjectID: "p1", Title: "x", Source: "inbox"}); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkTaskSourceClosed(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "source_closed" {
		t.Errorf("Status=%q want source_closed", got.Status)
	}
}

func TestUpdateProjectSources(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateProjectSources(ctx, "p1", map[string]any{"inbox": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProject(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Sources["inbox"]; !ok {
		t.Errorf("Sources=%v missing key inbox", got.Sources)
	}
}

func TestListBoundSourcesNoneWhenNoProjects(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.ListBoundSources(ctx)
	if err != nil {
		t.Fatalf("ListBoundSources: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d bound sources, want 0", len(got))
	}
}

func TestListBoundSourcesEnumeratesBindings(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, &Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, &Project{ID: "p2", Slug: "b", Name: "B", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProjectSources(ctx, "p1", map[string]any{
		"inbox":  map[string]any{},
		"github": map[string]any{"repo": "owner/name"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProjectSources(ctx, "p2", map[string]any{
		"linear": map[string]any{"team": "HBA"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListBoundSources(ctx)
	if err != nil {
		t.Fatalf("ListBoundSources: %v", err)
	}
	// p1 has 2 bindings, p2 has 1.
	if len(got) != 3 {
		t.Fatalf("got %d bound sources, want 3: %+v", len(got), got)
	}
	// Confirm every binding has Kind + ProjectSlug populated.
	for _, b := range got {
		if b.Kind == "" {
			t.Errorf("bound source missing Kind: %+v", b)
		}
		if b.ProjectSlug == "" {
			t.Errorf("bound source missing ProjectSlug: %+v", b)
		}
	}
}
