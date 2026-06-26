package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// chat_sessions.kind discriminator values.
const (
	KindChat = "chat" // regular Hive chat session
	KindPlan = "plan" // Phase 8.A roadmap planner session
)

type ChatSession struct {
	ID           string  `json:"id"`
	Surface      string  `json:"surface"`        // "cli" | "tui"
	Kind         string  `json:"kind"`           // KindChat | KindPlan
	StartedAt    int64   `json:"started_at"`
	EndedAt      int64   `json:"ended_at"` // 0 = open
	TotalCostUSD float64 `json:"total_cost_usd"`
	Name         string  `json:"name,omitempty"`
	Provider     string  `json:"provider,omitempty"`
	// ProjectSlug is set for planner sessions (Kind=KindPlan) so the daemon
	// can re-derive the project's repo_path as cwd when the session is
	// resumed. Empty for regular chat sessions.
	ProjectSlug string `json:"project_slug,omitempty"`
}

type ChatMessage struct {
	ID          string  `json:"id"`
	SessionID   string  `json:"session_id"`
	Role        string  `json:"role"` // "user" | "assistant" | "tool"
	Content     string  `json:"content"`
	ToolCalls   string  `json:"tool_calls,omitempty"`   // JSON, "" = none
	ToolResults string  `json:"tool_results,omitempty"` // JSON, "" = none
	TokensIn    int     `json:"tokens_in,omitempty"`
	TokensOut   int     `json:"tokens_out,omitempty"`
	CostUSD     float64 `json:"cost_usd"`
	CreatedAt   int64   `json:"created_at"`
}

// nullableInt returns nil for a zero value so NULL is stored, else the int.
func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func (s *Store) InsertChatSession(ctx context.Context, cs *ChatSession) error {
	if cs.StartedAt == 0 {
		cs.StartedAt = time.Now().Unix()
	}
	// Mirror SQL default for in-process callers that don't re-read after insert.
	if cs.Kind == "" {
		cs.Kind = KindChat
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_sessions (id, surface, kind, started_at, ended_at, total_cost_usd, name, provider, project_slug) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cs.ID, cs.Surface, cs.Kind, cs.StartedAt, nullableInt(cs.EndedAt), cs.TotalCostUSD,
		nullString(cs.Name), nullString(cs.Provider), nullString(cs.ProjectSlug))
	return err
}

// EndChatSession closes the session and adds deltaCostUSD to the running
// total. The parameter is a PER-TURN delta, not the cumulative total — the
// daemon creates a fresh Conversation (conv.CostUSD = 0) for every chat.send
// RPC, so f.CostUSD in the turn_done frame is always per-turn. Using
// total_cost_usd = total_cost_usd + ? ensures resumed / multi-turn sessions
// accumulate correctly rather than clobbering the prior total.
func (s *Store) EndChatSession(ctx context.Context, id string, deltaCostUSD float64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_sessions SET ended_at = ?, total_cost_usd = total_cost_usd + ? WHERE id = ?`,
		time.Now().Unix(), deltaCostUSD, id)
	return err
}

// SetChatProviderSession records the provider-specific session handle (e.g.
// the Claude-Code session id used for --resume) for a chat session.
func (s *Store) SetChatProviderSession(ctx context.Context, sessionID, providerSessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_sessions SET provider_session_id = ? WHERE id = ?`,
		providerSessionID, sessionID)
	return err
}

// GetChatProviderSession returns the provider session handle for a session,
// or "" if none is set (or the session does not exist).
func (s *Store) GetChatProviderSession(ctx context.Context, sessionID string) (string, error) {
	var pid string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(provider_session_id, '') FROM chat_sessions WHERE id = ?`,
		sessionID).Scan(&pid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return pid, err
}

func (s *Store) AppendChatMessage(ctx context.Context, m *ChatMessage) error {
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_messages
			(id, session_id, role, content, tool_calls, tool_results, tokens_in, tokens_out, cost_usd, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.SessionID, m.Role, m.Content,
		nullString(m.ToolCalls), nullString(m.ToolResults),
		m.TokensIn, m.TokensOut, m.CostUSD, m.CreatedAt)
	return err
}

func (s *Store) GetChatMessages(ctx context.Context, sessionID string) ([]ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, role, content,
		       COALESCE(tool_calls,''), COALESCE(tool_results,''),
		       tokens_in, tokens_out, cost_usd, created_at
		FROM chat_messages WHERE session_id = ? ORDER BY created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content,
			&m.ToolCalls, &m.ToolResults, &m.TokensIn, &m.TokensOut, &m.CostUSD, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListChatSessions(ctx context.Context, limit int) ([]ChatSession, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, surface, kind, started_at, COALESCE(ended_at,0), total_cost_usd,
		       COALESCE(name,''), COALESCE(provider,''), COALESCE(project_slug,'')
		FROM chat_sessions ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatSession
	for rows.Next() {
		var cs ChatSession
		if err := rows.Scan(&cs.ID, &cs.Surface, &cs.Kind, &cs.StartedAt, &cs.EndedAt, &cs.TotalCostUSD,
			&cs.Name, &cs.Provider, &cs.ProjectSlug); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// GetChatSession returns the session with the given ID, or ErrNotFound.
func (s *Store) GetChatSession(ctx context.Context, sessionID string) (*ChatSession, error) {
	const q = `SELECT id, surface, kind, started_at, COALESCE(ended_at,0), total_cost_usd,
		COALESCE(name,''), COALESCE(provider,''), COALESCE(project_slug,'')
		FROM chat_sessions WHERE id = ?`
	row := s.db.QueryRowContext(ctx, q, sessionID)
	var cs ChatSession
	if err := row.Scan(&cs.ID, &cs.Surface, &cs.Kind, &cs.StartedAt, &cs.EndedAt, &cs.TotalCostUSD,
		&cs.Name, &cs.Provider, &cs.ProjectSlug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &cs, nil
}

// DeleteChatSession removes a chat session and all its messages atomically.
// Returns ErrNotFound when the session row doesn't exist (and rolls back
// before any messages are deleted, so a no-op stays a no-op).
//
// Used by the chat.delete RPC (TUI picker `d` keybind, hive chat delete
// CLI). The on-disk per-session scratch dir under <scratchRoot>/<id>/
// is reaped separately by the daemon handler so the store stays free
// of filesystem concerns.
func (s *Store) DeleteChatSession(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Confirm the session exists first so we can return ErrNotFound
	// cleanly without doing any half-work.
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM chat_sessions WHERE id = ?`, sessionID).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_sessions WHERE id = ?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReapStaleChatSessions marks open sessions (ended_at = 0) whose started_at
// is older than staleBefore (Unix seconds) as ended with the current wall
// time. Returns the number of rows updated.
//
// This is called at daemon startup to close sessions that were left open by
// a crash, context cancellation, or daemon restart mid-turn. The staleBefore
// threshold is typically now - OpenSessionStaleHours*3600.
func (s *Store) ReapStaleChatSessions(ctx context.Context, staleBefore int64) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE chat_sessions
		SET ended_at = strftime('%s', 'now')
		WHERE ended_at IS NULL AND started_at < ?`,
		staleBefore)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// SetChatSessionName updates the human-readable name of a chat session.
// Empty string is preserved (stored as ""), so callers can clear a
// previously-set name without it becoming SQL NULL — useful when
// distinguishing "never named" (NULL on insert) from "cleared".
// Returns ErrNotFound when the session doesn't exist.
func (s *Store) SetChatSessionName(ctx context.Context, sessionID, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_sessions SET name = ? WHERE id = ?`,
		name, sessionID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
