package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SequenceDispatcher is the per-project sequenced-dispatch state. One row per
// project running in sequenced mode. Phase 2a.
type SequenceDispatcher struct {
	ProjectID         string
	Status            string   // active | paused | completed
	AdvancementPolicy string   // pr_opened (only policy implemented in Phase 2)
	CompletedPhases   []string // parsed from comma-joined completed_phases
	CreatedAt         int64
	UpdatedAt         int64
}

// UpsertSequenceDispatcher inserts or updates the dispatcher row for a project.
// On update, created_at is preserved and updated_at is bumped.
func (s *Store) UpsertSequenceDispatcher(ctx context.Context, d *SequenceDispatcher) error {
	now := time.Now().Unix()
	if d.Status == "" {
		d.Status = "active"
	}
	if d.AdvancementPolicy == "" {
		d.AdvancementPolicy = "pr_opened"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sequence_dispatchers (project_id, status, advancement_policy, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			status = excluded.status,
			advancement_policy = excluded.advancement_policy,
			updated_at = excluded.updated_at`,
		d.ProjectID, d.Status, d.AdvancementPolicy, now, now)
	if err != nil {
		return fmt.Errorf("upsert sequence dispatcher: %w", err)
	}
	return nil
}

// GetSequenceDispatcher returns the dispatcher row for a project, or ErrNotFound.
func (s *Store) GetSequenceDispatcher(ctx context.Context, projectID string) (*SequenceDispatcher, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT project_id, status, advancement_policy, completed_phases, created_at, updated_at
		FROM sequence_dispatchers WHERE project_id = ?`, projectID)
	var d SequenceDispatcher
	var completed string
	if err := row.Scan(&d.ProjectID, &d.Status, &d.AdvancementPolicy, &completed, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.CompletedPhases = splitCSV(completed)
	return &d, nil
}

// DeleteSequenceDispatcher removes a project's dispatcher row. No error if absent.
func (s *Store) DeleteSequenceDispatcher(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sequence_dispatchers WHERE project_id = ?`, projectID)
	if err != nil {
		return fmt.Errorf("delete sequence dispatcher: %w", err)
	}
	return nil
}

// MarkPhaseComplete records a roadmap phase as operator-completed for the
// project's dispatcher (idempotent). Derive treats completed phases as
// resolved so the active phase can advance past a shipped/0-task phase.
func (s *Store) MarkPhaseComplete(ctx context.Context, projectID, phase string) error {
	d, err := s.GetSequenceDispatcher(ctx, projectID)
	if err != nil {
		return fmt.Errorf("mark phase complete: %w", err)
	}
	for _, p := range d.CompletedPhases {
		if p == phase {
			return nil
		}
	}
	joined := strings.Join(append(d.CompletedPhases, phase), ",")
	_, err = s.db.ExecContext(ctx,
		`UPDATE sequence_dispatchers SET completed_phases = ?, updated_at = ? WHERE project_id = ?`,
		joined, time.Now().Unix(), projectID)
	if err != nil {
		return fmt.Errorf("mark phase complete: %w", err)
	}
	return nil
}

// splitCSV splits a comma-joined string into a slice, dropping empty entries.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
