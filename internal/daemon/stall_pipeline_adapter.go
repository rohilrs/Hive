package daemon

import (
	"context"

	"github.com/rohilrs/Hive/internal/daemon/eventbus"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// pipelineStallAdapter implements pipeline.StallRecorder over
// *store.Store. Mirrors the stage_adapter / feedback_adapter pattern;
// the cmd-side daemonStallAdapter (which satisfies the adapter-side
// claudecode.StallStore interface) is independent.
//
// Phase 3.5a: also publishes stall.detected events to the bus so
// subscribers (TUI, hive events) see stalls live. Only L3 stalls
// (pipeline-side) hit this path; L1/L2 stalls written by the
// claudecode adapter go through the cmd-side adapter which doesn't
// have the bus reference (documented as deferred).
type pipelineStallAdapter struct {
	s   *store.Store
	bus *eventbus.Bus
}

func newPipelineStallAdapter(s *store.Store, bus *eventbus.Bus) *pipelineStallAdapter {
	return &pipelineStallAdapter{s: s, bus: bus}
}

// RecordStall inserts a stalls row with cleared_at = detected_at
// (single-shot L3 events; not a long-running surface). Best-effort:
// caller logs on error and proceeds. Also publishes a stall.detected
// event to the bus (best-effort; nil bus = no publish).
func (a *pipelineStallAdapter) RecordStall(ctx context.Context, runID string, stageID int64, layer int, detectedAt int64, action, details string) error {
	row := &store.Stall{
		RunID:       runID,
		Layer:       layer,
		DetectedAt:  detectedAt,
		ClearedAt:   &detectedAt,
		ActionTaken: action,
		DetailsJSON: details,
	}
	if stageID != 0 {
		row.StageID = &stageID
	}
	_, err := a.s.InsertStall(ctx, row)
	if a.bus != nil {
		a.bus.Publish(rpc.EventMessage{
			Type: rpc.EventStallDetected,
			Data: map[string]any{
				"run_id":   runID,
				"stage_id": stageID,
				"layer":    layer,
				"action":   action,
			},
		})
	}
	return err
}
