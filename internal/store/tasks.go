package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

func (s *Store) InsertTask(ctx context.Context, t *Task) error {
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = "pending"
	}
	if t.Priority == "" {
		t.Priority = "P3"
	}
	if t.Pipeline == "" {
		t.Pipeline = "build"
	}
	if t.GateState == "" {
		t.GateState = "none"
	}

	if t.Metadata == nil {
		t.Metadata = map[string]any{}
	}

	predictedJSON, _ := json.Marshal(t.PredictedFiles)
	conflictJSON, _ := json.Marshal(t.ConflictSet)
	metadataJSON, _ := json.Marshal(t.Metadata)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks
			(id, project_id, source, source_id, title, body, priority, status, pipeline,
			 predicted_files, conflict_set, predict_confidence, metadata,
			 created_at, updated_at, parent_task_id, gate_state, linear_synced_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		t.ID, t.ProjectID, t.Source, nullString(t.SourceID), t.Title, t.Body,
		t.Priority, t.Status, t.Pipeline, string(predictedJSON), string(conflictJSON),
		t.PredictConfidence, string(metadataJSON),
		t.CreatedAt.Unix(), t.UpdatedAt.Unix(), t.ParentTaskID, t.GateState, t.LinearSyncedState,
	)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, taskSelectClause+` WHERE id = ?`, id)
	return scanTask(row)
}

func (s *Store) ListPendingTasks(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectClause+`
		WHERE status = 'pending'
		ORDER BY
			CASE priority
				WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2
				WHEN 'P3' THEN 3 ELSE 9
			END,
			created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			log.Printf("store: skipping unscannable task row in ListPendingTasks: %v", err)
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTasksByStatuses returns tasks whose status is in the given set, ordered
// like ListPendingTasks (priority then created_at). Used by the TUI initial
// state to seed BOTH the queued (pending) and needs-attention lanes — without
// it a task that went needs_attention before the TUI subscribed never enters
// the snapshot. Empty statuses → no rows.
func (s *Store) ListTasksByStatuses(ctx context.Context, statuses []string) ([]*Task, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	ph := make([]string, len(statuses))
	args := make([]any, len(statuses))
	for i, st := range statuses {
		ph[i] = "?"
		args[i] = st
	}
	rows, err := s.db.QueryContext(ctx, taskSelectClause+`
		WHERE status IN (`+strings.Join(ph, ",")+`)
		ORDER BY
			CASE priority
				WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2
				WHEN 'P3' THEN 3 ELSE 9
			END,
			created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			log.Printf("store: skipping unscannable task row in ListTasksByStatuses: %v", err)
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTaskPrediction(ctx context.Context, taskID string,
	predicted, conflictSet []string, confidence int,
) error {
	predictedJSON, _ := json.Marshal(predicted)
	conflictJSON, _ := json.Marshal(conflictSet)
	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET predicted_files = ?, conflict_set = ?, predict_confidence = ?, updated_at = ?
		WHERE id = ?
	`, string(predictedJSON), string(conflictJSON), confidence, time.Now().Unix(), taskID)
	return err
}

func (s *Store) UpdateTaskStatus(ctx context.Context, taskID, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), taskID,
	)
	return err
}

// UpdateTaskContent updates a task's title + body. Phase 3.7.4 (TUI
// task editing). Returns ErrNotFound if the task doesn't exist.
func (s *Store) UpdateTaskContent(ctx context.Context, taskID, title, body string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET title = ?, body = ?, updated_at = ? WHERE id = ?`,
		title, body, time.Now().Unix(), taskID,
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

// DeleteTask removes a task and its runs in a transaction. The
// runs.task_id → tasks.id foreign key means a task with any run
// (incl. abandoned) can't be deleted directly (SQLite error 787), so
// we delete the task's runs first. Stages / tool_calls / stalls for
// those runs lost their runs-FK in migration 0007, so they orphan
// harmlessly (observability-data convention). Returns ErrNotFound if
// the task doesn't exist.
func (s *Store) DeleteTask(ctx context.Context, taskID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("delete task runs: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, taskID)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// ClaimTask atomically transitions a task from "pending" to "running".
// Returns (true, nil) if this caller won the claim; (false, nil) if the
// task was in some other state (another dispatcher won the race, or the
// task is done/abandoned). Conditional UPDATE ensures concurrent callers
// can't both succeed.
func (s *Store) ClaimTask(ctx context.Context, taskID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'running', updated_at = ?
		 WHERE id = ? AND status = 'pending'`,
		time.Now().Unix(), taskID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ListTaskSourceIDsForProject returns the set of non-empty source_id strings
// for tasks belonging to projectID with the given source kind (e.g. "github"
// or "linear"). Used by the syncer to dedup new items against tasks already
// imported from a different source — e.g. skip a Linear issue whose linked
// GitHub issue is already a task via the gh source for this same project.
func (s *Store) ListTaskSourceIDsForProject(ctx context.Context, projectID, source string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source_id FROM tasks WHERE project_id = ? AND source = ? AND source_id IS NOT NULL`,
		projectID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sid sql.NullString
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		if sid.Valid && sid.String != "" {
			out[sid.String] = true
		}
	}
	return out, rows.Err()
}

// UpdateTaskMetadata replaces a task's metadata column with the given map.
// Use MergeTaskMetadata when you want to preserve unmentioned keys (sync
// updates that bring only a subset of fields).
func (s *Store) UpdateTaskMetadata(ctx context.Context, taskID string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET metadata = ?, updated_at = ? WHERE id = ?`,
		string(raw), time.Now().Unix(), taskID,
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

// MergeTaskMetadata merges kv into the task's existing metadata. New keys are
// added; existing keys with names in kv get overwritten; other existing keys
// are preserved. kv is string→string because SourceItem.Metadata carries
// provider-shaped string scalars today; values are stored as any so the
// resulting JSON shape matches direct InsertTask metadata.
func (s *Store) MergeTaskMetadata(ctx context.Context, taskID string, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Metadata == nil {
		t.Metadata = map[string]any{}
	}
	for k, v := range kv {
		t.Metadata[k] = v
	}
	return s.UpdateTaskMetadata(ctx, taskID, t.Metadata)
}

// ListTasksBySource returns all tasks for a (project, source), for sync
// reconciliation (any status — the reconciler decides what to touch).
func (s *Store) ListTasksBySource(ctx context.Context, projectID, source string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectClause+`
		WHERE project_id = ? AND source = ?`, projectID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			log.Printf("store: skipping unscannable task row in ListTasksBySource: %v", err)
			continue
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// MarkTaskSourceClosed marks a task whose upstream source item was closed
// or deleted. source_closed is a SOFT terminal state: the scheduler never
// auto-dispatches it, but an explicit `hive run <id>` (RunNow) still resets
// it to pending and runs it — re-opening a task whose source is gone is a
// deliberate user action, not a bug.
func (s *Store) MarkTaskSourceClosed(ctx context.Context, taskID string) error {
	return s.UpdateTaskStatus(ctx, taskID, "source_closed")
}

const taskSelectClause = `
	SELECT id, project_id, source, source_id, title, body, priority, status, pipeline,
	       predicted_files, conflict_set, predict_confidence, metadata,
	       created_at, updated_at, parent_task_id, gate_state, linear_synced_state,
	       last_failure_feedback
	FROM tasks
`

func scanTask(r rowScanner) (*Task, error) {
	var (
		t             Task
		sourceID      sql.NullString
		predictedJSON string
		conflictJSON  string
		metadataJSON  string
		createdAt     any
		updatedAt     any
	)
	err := r.Scan(&t.ID, &t.ProjectID, &t.Source, &sourceID, &t.Title, &t.Body,
		&t.Priority, &t.Status, &t.Pipeline, &predictedJSON, &conflictJSON, &t.PredictConfidence,
		&metadataJSON, &createdAt, &updatedAt, &t.ParentTaskID, &t.GateState, &t.LinearSyncedState,
		&t.LastFailureFeedback)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	if sourceID.Valid {
		t.SourceID = sourceID.String
	}
	if err := json.Unmarshal([]byte(predictedJSON), &t.PredictedFiles); err != nil {
		return nil, fmt.Errorf("unmarshal task predicted_files: %w", err)
	}
	if err := json.Unmarshal([]byte(conflictJSON), &t.ConflictSet); err != nil {
		return nil, fmt.Errorf("unmarshal task conflict_set: %w", err)
	}
	if err := json.Unmarshal([]byte(metadataJSON), &t.Metadata); err != nil {
		return nil, fmt.Errorf("unmarshal task metadata: %w", err)
	}
	t.CreatedAt = parseTimestamp(createdAt)
	t.UpdatedAt = parseTimestamp(updatedAt)
	return &t, nil
}

// parseTimestamp tolerates both integer Unix-epoch (the canonical form) and
// RFC3339 text in created_at/updated_at columns, so a single malformed row
// can't fail the scan. Unparseable values produce a zero time.
func parseTimestamp(v any) time.Time {
	switch x := v.(type) {
	case int64:
		return time.Unix(x, 0).UTC()
	case []byte:
		return parseTimestamp(string(x))
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return time.Unix(n, 0).UTC()
		}
		if ts, err := time.Parse(time.RFC3339, x); err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SubtaskInput is the per-child payload for InsertSubtasks. Decoupled
// from internal/decompose.ProposedSubtask so the store has no upward
// dependency.
type SubtaskInput struct {
	Title    string
	Body     string
	Priority string
	Pipeline string
}

// InsertSubtasks transactionally inserts a batch of child tasks for
// parentID. All children inherit the parent's project_id. Each row's
// parent_task_id is set to parentID. Returns the new task IDs in
// insertion order. If any insert fails (or the parent is missing) all
// inserts roll back and an error is returned.
func (s *Store) InsertSubtasks(ctx context.Context, parentID string, items []SubtaskInput) ([]string, error) {
	if parentID == "" {
		return nil, fmt.Errorf("parent_task_id required")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("at least one subtask required")
	}

	parent, err := s.GetTask(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("get parent: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Unix()
	ids := make([]string, 0, len(items))
	for i, it := range items {
		id := fmt.Sprintf("task-%d-%d", time.Now().UnixNano(), i)
		_, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (
				id, project_id, source, source_id, title, body, priority,
				status, pipeline, predicted_files, conflict_set, predict_confidence,
				metadata, created_at, updated_at, parent_task_id
			) VALUES (?, ?, 'decompose', NULL, ?, ?, ?, 'pending', ?, 'null', 'null', 0, '{}', ?, ?, ?)`,
			id, parent.ProjectID, it.Title, it.Body, it.Priority, it.Pipeline, now, now, parentID,
		)
		if err != nil {
			return nil, fmt.Errorf("insert subtask %d: %w", i, err)
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return ids, nil
}

// UpdateTaskGateState sets a task's sequenced-dispatcher gate state.
func (s *Store) UpdateTaskGateState(ctx context.Context, taskID, gateState string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET gate_state = ?, updated_at = ? WHERE id = ?`,
		gateState, time.Now().Unix(), taskID,
	)
	if err != nil {
		return fmt.Errorf("update task gate_state: %w", err)
	}
	return nil
}

// UpdateTaskLinearSyncedState records the logical Linear workflow state last
// pushed for this task's mirrored issue. Column-scoped (writes only
// linear_synced_state + updated_at) so it never clobbers concurrent ingest
// updates to title/body/status. Linear write-back Phase 1.
func (s *Store) UpdateTaskLinearSyncedState(ctx context.Context, taskID, state string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET linear_synced_state = ?, updated_at = ? WHERE id = ?`,
		state, time.Now().Unix(), taskID,
	)
	if err != nil {
		return fmt.Errorf("update task linear_synced_state: %w", err)
	}
	return nil
}

// SetTaskLinearMirror marks an existing task as mirrored to a Linear issue,
// used by the reconciler backfill path. Column-scoped: it writes only
// source='linear', source_id (the Linear issue UUID), and updated_at, so it
// never clobbers concurrent ingest updates to title/body/status. The Linear
// human identifier (external_id) is cosmetic and is NOT persisted in Phase 1;
// the identifier parameter is accepted for call-site symmetry but ignored.
func (s *Store) SetTaskLinearMirror(ctx context.Context, taskID, issueID, identifier string) error {
	_ = identifier // cosmetic; not persisted in Phase 1
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET source = 'linear', source_id = ?, updated_at = ? WHERE id = ?`,
		issueID, time.Now().Unix(), taskID,
	)
	if err != nil {
		return fmt.Errorf("set task linear mirror: %w", err)
	}
	return nil
}

// ListTasksByProject returns all tasks for a project regardless of status,
// ordered by priority then created_at (same order as ListPendingTasks).
func (s *Store) ListTasksByProject(ctx context.Context, projectID string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectClause+`
		WHERE project_id = ?
		ORDER BY
			CASE priority
				WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2
				WHEN 'P3' THEN 3 ELSE 9
			END,
			created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			log.Printf("store: skipping unscannable task row in ListTasksByProject: %v", err)
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListPendingTasksByProject returns only the pending tasks for a project,
// priority-ordered. Used by existing-work reconciliation: pending tasks are the
// candidates a decompose can rewrite/merge into (running/done are in-flight or
// finished and must not be touched).
func (s *Store) ListPendingTasksByProject(ctx context.Context, projectID string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectClause+`
		WHERE project_id = ? AND status = 'pending'
		ORDER BY
			CASE priority
				WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2
				WHEN 'P3' THEN 3 ELSE 9
			END,
			created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			log.Printf("store: skipping unscannable task row in ListPendingTasksByProject: %v", err)
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListChildTasks returns all tasks whose parent_task_id matches the
// given ID, ordered by creation time ascending. Siblings inserted via
// a single InsertSubtasks call share a second-resolution created_at, so
// id ASC is appended as a deterministic tiebreaker — IDs are formatted
// as task-<unixnano>-<i>, nano-monotonic within the call.
func (s *Store) ListChildTasks(ctx context.Context, parentID string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectClause+`
		WHERE parent_task_id = ?
		ORDER BY created_at ASC, id ASC`,
		parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			log.Printf("store: skipping unscannable task row in ListChildTasks: %v", err)
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTaskLastFailureFeedback persists a JSON blob of the run's final feedback
// (summary + file_refs + exhaust_reason) for the given task. Called when a
// build run gives up (transitions to needs_attention). The blob is injected
// into the iter-0 implement prompt of the next run for this task.
func (s *Store) SetTaskLastFailureFeedback(ctx context.Context, taskID, jsonBlob string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET last_failure_feedback = ?, updated_at = ? WHERE id = ?`,
		jsonBlob, time.Now().Unix(), taskID)
	if err != nil {
		return fmt.Errorf("set last_failure_feedback: %w", err)
	}
	return nil
}

// ClearTaskLastFailureFeedback resets last_failure_feedback to empty, called
// on a successful run so stale failure context is not injected into future runs.
func (s *Store) ClearTaskLastFailureFeedback(ctx context.Context, taskID string) error {
	return s.SetTaskLastFailureFeedback(ctx, taskID, "")
}

// ListTasksByGateState returns every task in the given gate_state across all
// projects, ordered by created_at for determinism. Used by the merge-poller to
// find awaiting_merge tasks.
func (s *Store) ListTasksByGateState(ctx context.Context, gate string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelectClause+`
		WHERE gate_state = ?
		ORDER BY created_at ASC`, gate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			log.Printf("store: skipping unscannable task row in ListTasksByGateState: %v", err)
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
