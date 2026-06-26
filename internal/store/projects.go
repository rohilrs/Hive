package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a row lookup fails.
var ErrNotFound = errors.New("store: not found")

func (s *Store) InsertProject(ctx context.Context, p *Project) error {
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now

	if p.Sources == nil {
		p.Sources = map[string]any{}
	}
	if p.Config == nil {
		p.Config = map[string]any{}
	}

	sourcesJSON, _ := json.Marshal(p.Sources)
	configJSON, _ := json.Marshal(p.Config)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, slug, name, repo_path, sources, config, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		p.ID, p.Slug, p.Name, p.RepoPath, string(sourcesJSON), string(configJSON),
		p.Status, p.CreatedAt.Unix(), p.UpdatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

func (s *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, repo_path, sources, config, status, created_at, updated_at
		FROM projects WHERE id = ?
	`, id)
	return scanProject(row)
}

func (s *Store) GetProjectBySlug(ctx context.Context, slug string) (*Project, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, repo_path, sources, config, status, created_at, updated_at
		FROM projects WHERE slug = ?
	`, slug)
	return scanProject(row)
}

func (s *Store) ListProjects(ctx context.Context, status string) ([]*Project, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if status == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, slug, name, repo_path, sources, config, status, created_at, updated_at
			FROM projects ORDER BY slug
		`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, slug, name, repo_path, sources, config, status, created_at, updated_at
			FROM projects WHERE status = ? ORDER BY slug
		`, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TaskCountsByProject returns a per-project map of status → count, built
// from a single GROUP BY query so cost stays O(distinct (project,status))
// rather than N projects × M statuses. The outer map is keyed by
// project_id; the inner map is keyed by status string (e.g. "pending",
// "running", "done"). Projects with zero tasks are absent from the outer
// map — callers should treat a missing entry as "no tasks".
func (s *Store) TaskCountsByProject(ctx context.Context) (map[string]map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, status, COUNT(*) FROM tasks GROUP BY project_id, status
	`)
	if err != nil {
		return nil, fmt.Errorf("task counts by project: %w", err)
	}
	defer rows.Close()

	out := map[string]map[string]int{}
	for rows.Next() {
		var (
			projectID string
			status    string
			count     int
		)
		if err := rows.Scan(&projectID, &status, &count); err != nil {
			return nil, err
		}
		if out[projectID] == nil {
			out[projectID] = map[string]int{}
		}
		out[projectID][status] = count
	}
	return out, rows.Err()
}

// UpdateProjectSources replaces a project's sources binding JSON.
func (s *Store) UpdateProjectSources(ctx context.Context, projectID string, sources map[string]any) error {
	raw, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET sources = ?, updated_at = ? WHERE id = ?`,
		string(raw), time.Now().Unix(), projectID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateProject applies the non-nil fields to a project (name/repo_path/status).
// A non-nil repo_path of "" clears it. Returns ErrNotFound if absent.
func (s *Store) UpdateProject(ctx context.Context, id string, name, repoPath, status *string) error {
	p, err := s.GetProject(ctx, id)
	if err != nil {
		return err // ErrNotFound propagates
	}
	if name != nil {
		p.Name = *name
	}
	if repoPath != nil {
		if *repoPath == "" {
			p.RepoPath = nil
		} else {
			rp := *repoPath
			p.RepoPath = &rp
		}
	}
	if status != nil {
		p.Status = *status
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE projects SET name = ?, repo_path = ?, status = ?, updated_at = ?
		WHERE id = ?
	`, p.Name, p.RepoPath, p.Status, time.Now().Unix(), id)
	return err
}

// DeleteProject removes a project + its tasks + runs in a transaction.
// Refuses if the project has a running run (abandon it first). Returns
// ErrNotFound if the slug doesn't exist.
func (s *Store) DeleteProject(ctx context.Context, slug string) error {
	p, err := s.GetProjectBySlug(ctx, slug)
	if err != nil {
		return err
	}
	var running int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE project_id = ? AND status = 'running'`, p.ID,
	).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return fmt.Errorf("project %s has %d running run(s); abandon them first", slug, running)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE project_id = ?`, p.ID); err != nil {
		return fmt.Errorf("delete project runs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE project_id = ?`, p.ID); err != nil {
		return fmt.Errorf("delete project tasks: %w", err)
	}
	// Sequenced-dispatcher row (Phase 2a) FKs to projects(id); remove it before
	// the project row or the delete fails the FK constraint. No-op if absent.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sequence_dispatchers WHERE project_id = ?`, p.ID); err != nil {
		return fmt.Errorf("delete project sequence dispatcher: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, p.ID); err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	return tx.Commit()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(r rowScanner) (*Project, error) {
	var (
		p           Project
		repoPath    sql.NullString
		sourcesJSON string
		configJSON  string
		createdAt   int64
		updatedAt   int64
	)
	err := r.Scan(&p.ID, &p.Slug, &p.Name, &repoPath, &sourcesJSON, &configJSON, &p.Status, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	if repoPath.Valid {
		rp := repoPath.String
		p.RepoPath = &rp
	}
	if err := json.Unmarshal([]byte(sourcesJSON), &p.Sources); err != nil {
		return nil, fmt.Errorf("unmarshal project sources: %w", err)
	}
	if err := json.Unmarshal([]byte(configJSON), &p.Config); err != nil {
		return nil, fmt.Errorf("unmarshal project config: %w", err)
	}
	p.CreatedAt = time.Unix(createdAt, 0).UTC()
	p.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &p, nil
}
