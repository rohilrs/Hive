package main

import (
	"context"

	"github.com/rohilrs/Hive/internal/store"
)

// daemonStallAdapter wires *store.Store to claudecode.StallStore. Lives
// in cmd because cmd is the composition root for the adapter; placing
// it here avoids internal/daemon importing internal/adapter/claudecode
// for the interface contract.
type daemonStallAdapter struct{ s *store.Store }

func newDaemonStallAdapter(s *store.Store) *daemonStallAdapter { return &daemonStallAdapter{s: s} }

func (a *daemonStallAdapter) InsertStall(ctx context.Context, runID string, stageID int64, layer int, detectedAt, clearedAt int64, action, details string) (int64, error) {
	row := &store.Stall{
		RunID:       runID,
		Layer:       layer,
		DetectedAt:  detectedAt,
		ActionTaken: action,
		DetailsJSON: details,
	}
	if stageID != 0 {
		row.StageID = &stageID
	}
	if clearedAt != 0 {
		row.ClearedAt = &clearedAt
	}
	return a.s.InsertStall(ctx, row)
}

func (a *daemonStallAdapter) ClearStall(ctx context.Context, id, clearedAt int64) error {
	return a.s.ClearStall(ctx, id, clearedAt)
}
