package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func (s *Store) AppendEvent(ctx context.Context, e *Event) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	payloadJSON, _ := json.Marshal(e.Payload)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO events (run_id, stage_id, ts, type, message, payload)
		VALUES (?, ?, ?, ?, ?, ?)
	`, e.RunID, nullString(e.StageID), e.Timestamp.Unix(), e.Type, e.Message, string(payloadJSON))
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}

	id, _ := res.LastInsertId()
	e.ID = id
	return nil
}

func (s *Store) ListEventsForRun(ctx context.Context, runID string, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, COALESCE(stage_id, ''), ts, type, message, payload
		FROM events WHERE run_id = ?
		ORDER BY ts ASC, id ASC
		LIMIT ?
	`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (s *Store) SearchEvents(ctx context.Context, query string, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.run_id, COALESCE(e.stage_id, ''), e.ts, e.type, e.message, e.payload
		FROM events_fts f
		JOIN events e ON e.id = f.rowid
		WHERE f.message MATCH ?
		ORDER BY e.ts DESC, e.id DESC
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]*Event, error) {
	var out []*Event
	for rows.Next() {
		var (
			e           Event
			ts          int64
			payloadJSON string
		)
		if err := rows.Scan(&e.ID, &e.RunID, &e.StageID, &ts, &e.Type, &e.Message, &payloadJSON); err != nil {
			return nil, err
		}
		e.Timestamp = time.Unix(ts, 0).UTC()
		_ = json.Unmarshal([]byte(payloadJSON), &e.Payload)
		out = append(out, &e)
	}
	return out, rows.Err()
}
