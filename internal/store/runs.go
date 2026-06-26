package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) InsertRun(ctx context.Context, r *Run) error {
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.Status == "" {
		r.Status = "pending"
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, task_id, project_id, pipeline, status,
		                  total_cost_usd, summary, created_at, parent_run_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.ID, r.TaskID, r.ProjectID, r.Pipeline, r.Status,
		r.TotalCostUSD, r.Summary, r.CreatedAt.Unix(), nullString(r.ParentRunID))
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelectClause+` WHERE id = ?`, id))
}

// ListRunningRuns returns ROOT runs (parent_run_id IS NULL) with
// status='running'. EXCLUDES child fix runs (parent_run_id set) so
// capacity accounting counts only actual subprocess-worker holders —
// a finish-branch parent with an in-flight child fix occupies ONE slot
// total, not two. Use ListAllRunningRuns when the all-inclusive view
// is needed (recovery sweeps, status panels, chat surfaces).
func (s *Store) ListRunningRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelectClause+` WHERE status = 'running' AND parent_run_id IS NULL ORDER BY started_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAllRunningRuns returns ALL runs with status='running', including
// child fix runs (parent_run_id != NULL). Use this for recovery sweeps
// and observability surfaces that need every running row (recoverStaleRuns,
// status/health RPCs, chat tools surfacing active runs).
func (s *Store) ListAllRunningRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelectClause+` WHERE status = 'running' ORDER BY started_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRecentRuns returns the most recently ended runs (terminal statuses
// done/needs_attention/abandoned), newest first. Used by `hive status`
// so the user can find run IDs after the daemon auto-dispatched and
// completed a task without them having captured the ID at dispatch time.
func (s *Store) ListRecentRuns(ctx context.Context, limit int) ([]*Run, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.QueryContext(ctx,
		runSelectClause+` WHERE status != 'pending' AND status != 'running'
		                  ORDER BY ended_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRecentDoneTasks returns exactly one row per task_id — the single done
// run with the latest ended_at (ties broken by id DESC), newest-first,
// limited to N distinct tasks. Used by the TUI recent-done section so a task
// with build + finish-branch (+ child) runs appears once.
func (s *Store) ListRecentDoneTasks(ctx context.Context, limit int) ([]*Run, error) {
	if limit <= 0 {
		limit = 10
	}
	// The correlated subquery picks exactly one run ID per task (the latest
	// done run by ended_at; id DESC breaks same-second ties because run IDs
	// are "run-<timestamp>" strings of equal length). This prevents the
	// correlated-MAX form from returning two rows when two done runs of the
	// same task share an ended_at second.
	rows, err := s.db.QueryContext(ctx,
		runSelectClause+`
		WHERE status = 'done'
		  AND id = (SELECT r2.id FROM runs r2
		             WHERE r2.task_id = runs.task_id AND r2.status = 'done'
		             ORDER BY r2.ended_at DESC, r2.id DESC LIMIT 1)
		ORDER BY ended_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAllRuns returns every run row (any status), newest first. Used by the
// cleanup planner to decide which runs' artifacts are reclaimable.
func (s *Store) ListAllRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelectClause+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRunsByTask returns all runs for a task (any status, including
// abandoned and children), newest-first. Used by DeriveTaskStatus via
// refreshTaskStatus.
func (s *Store) ListRunsByTask(ctx context.Context, taskID string) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		runSelectClause+` WHERE task_id = ? ORDER BY COALESCE(ended_at, started_at, created_at) DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MarkRunStarted(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = 'running', started_at = ? WHERE id = ?`,
		time.Now().Unix(), runID,
	)
	return err
}

func (s *Store) MarkRunEnded(ctx context.Context, runID, status, summary string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, ended_at = ?, summary = ?
		WHERE id = ?
	`, status, time.Now().Unix(), summary, runID)
	return err
}

func (s *Store) MarkDocumentationSkipped(ctx context.Context, runID, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET documentation_skipped = 1, documentation_skip_reason = ?
		WHERE id = ?
	`, reason, runID)
	return err
}

// ClearDocumentationSkipped resets the skip flag after a successful manual
// re-run (`hive document <run-id>`).
func (s *Store) ClearDocumentationSkipped(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET documentation_skipped = 0, documentation_skip_reason = ''
		WHERE id = ?
	`, runID)
	return err
}

func (s *Store) AddRunCost(ctx context.Context, runID string, deltaUSD float64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET total_cost_usd = total_cost_usd + ? WHERE id = ?`,
		deltaUSD, runID,
	)
	return err
}

// LatestNonTerminalRunForTask returns the most recently started run
// for the task whose status is in {pending, running, needs_attention}.
// Returns (nil, nil) when no such run exists — callers should treat
// nil as "no run to update," NOT as an error.
//
// Used by the chat-tool hive_predict handler to decide whether
// refresh=true has somewhere to persist the new prediction.
func (s *Store) LatestNonTerminalRunForTask(ctx context.Context, taskID string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, runSelectClause+`
		WHERE task_id = ?
		  AND status IN ('pending', 'running', 'needs_attention')
		ORDER BY COALESCE(started_at, created_at) DESC
		LIMIT 1`, taskID)
	r, err := scanRun(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// LatestDoneBuildRunForTask returns the most recent successful build run for a
// task (pipeline='build', status='done'), or (nil, nil) when none exists.
func (s *Store) LatestDoneBuildRunForTask(ctx context.Context, taskID string) (*Run, error) {
	// parent_run_id IS NULL excludes auto-fix child build runs (RunChildFix):
	// those are done builds too, but they reuse the parent's worktree and never
	// record their own branch_name, so finishing one would fail with "no recorded
	// branch". The integration target is always the top-level build run.
	row := s.db.QueryRowContext(ctx, runSelectClause+`
		WHERE task_id = ?
		  AND pipeline = 'build'
		  AND status = 'done'
		  AND parent_run_id IS NULL
		ORDER BY COALESCE(ended_at, started_at, created_at) DESC
		LIMIT 1`, taskID)
	r, err := scanRun(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

const runSelectClause = `
	SELECT id, task_id, project_id, pipeline, status,
	       started_at, ended_at, total_cost_usd, summary,
	       documentation_skipped, documentation_skip_reason, created_at,
	       parent_run_id, worker_pid, branch_name, pr_url, pr_number
	FROM runs
`

func scanRun(r rowScanner) (*Run, error) {
	var (
		run         Run
		startedAt   sql.NullInt64
		endedAt     sql.NullInt64
		skipReason  sql.NullString
		skipped     int64
		createdAt   any // tolerates integer epoch or RFC3339 text
		parentRunID sql.NullString
		branchName  sql.NullString
		prURL       sql.NullString
	)
	err := r.Scan(&run.ID, &run.TaskID, &run.ProjectID, &run.Pipeline, &run.Status,
		&startedAt, &endedAt, &run.TotalCostUSD, &run.Summary,
		&skipped, &skipReason, &createdAt, &parentRunID, &run.WorkerPID,
		&branchName, &prURL, &run.PRNumber)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	if startedAt.Valid {
		t := time.Unix(startedAt.Int64, 0).UTC()
		run.StartedAt = &t
	}
	if endedAt.Valid {
		t := time.Unix(endedAt.Int64, 0).UTC()
		run.EndedAt = &t
	}
	if skipReason.Valid {
		run.DocumentationSkipReason = skipReason.String
	}
	if parentRunID.Valid {
		run.ParentRunID = parentRunID.String
	}
	if branchName.Valid {
		run.BranchName = branchName.String
	}
	if prURL.Valid {
		run.PRURL = prURL.String
	}
	run.DocumentationSkipped = skipped == 1
	run.CreatedAt = parseTimestamp(createdAt)
	return &run, nil
}

// SetRunWorkerPID stamps the worker subprocess PID for a run.
// Called from the dispatch path immediately after cmd.Start() returns.
// Used by Phase 7 restart-recovery: after a daemon crash, the boot-path
// sweep reads non-NULL worker_pid rows and SIGKILLs each.
func (s *Store) SetRunWorkerPID(ctx context.Context, runID string, pid int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET worker_pid = ? WHERE id = ?`,
		pid, runID,
	)
	if err != nil {
		return fmt.Errorf("set worker pid: %w", err)
	}
	return nil
}

// ClearRunWorkerPID nulls the worker PID column. Called from the
// subprocess exit path (graceful or kill). Idempotent — no error if
// the row no longer exists or the column was already NULL.
func (s *Store) ClearRunWorkerPID(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET worker_pid = NULL WHERE id = ?`,
		runID,
	)
	if err != nil {
		return fmt.Errorf("clear worker pid: %w", err)
	}
	return nil
}

// ListRunsWithWorkerPID returns every run with a non-NULL worker_pid.
// Used by the restart-recovery sweep; after a clean shutdown this is
// always empty. After a crash, these are the survivors to kill.
func (s *Store) ListRunsWithWorkerPID(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		runSelectClause+` WHERE worker_pid IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("list runs with worker pid: %w", err)
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRunBranch records the git branch a run's worktree was created on.
// Called from the dispatch path right after worktree.Create returns.
func (s *Store) SetRunBranch(ctx context.Context, runID, branch string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET branch_name = ? WHERE id = ?`,
		branch, runID,
	)
	if err != nil {
		return fmt.Errorf("set run branch: %w", err)
	}
	return nil
}

// SetRunPR records the PR a finish-branch run opened. number<=0 stores a
// NULL pr_number (url still recorded) so a parse miss doesn't poison the row.
func (s *Store) SetRunPR(ctx context.Context, runID, url string, number int) error {
	var num sql.NullInt64
	if number > 0 {
		num = sql.NullInt64{Int64: int64(number), Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET pr_url = ?, pr_number = ? WHERE id = ?`,
		url, num, runID,
	)
	if err != nil {
		return fmt.Errorf("set run pr: %w", err)
	}
	return nil
}

// PRForTask returns the PR URL and number recorded for a task's most recent
// run that opened one (the chained finish-branch run). url=="" / number==0 when
// the task has no PR yet. Non-fatal: a missing row is ("",0,nil).
func (s *Store) PRForTask(ctx context.Context, taskID string) (string, int, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT pr_url, pr_number FROM runs
		WHERE task_id = ? AND pr_url IS NOT NULL AND pr_url <> ''
		ORDER BY created_at DESC LIMIT 1`, taskID)
	var url sql.NullString
	var num sql.NullInt64
	switch err := row.Scan(&url, &num); {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, nil
	case err != nil:
		return "", 0, err
	}
	return url.String, int(num.Int64), nil
}
