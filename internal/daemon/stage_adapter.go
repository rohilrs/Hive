package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/store"
)

// stageAdapter implements pipeline.StageStore over *store.Store.
// Mirrors the feedback_adapter.go pattern from Phase 2b.0.
type stageAdapter struct {
	s *store.Store
}

func newStageAdapter(s *store.Store) *stageAdapter { return &stageAdapter{s: s} }

func (a *stageAdapter) BeginStage(ctx context.Context, runID, name string, iter int, model string) (int64, error) {
	return a.s.InsertStage(ctx, &store.Stage{
		RunID: runID, Name: name, Iter: iter, Model: model,
	})
}

func (a *stageAdapter) EndStage(ctx context.Context, stageID int64, verdict string, vc *float64, tIn, tOut, cache int, cost *float64) error {
	return a.s.UpdateStageEnd(ctx, &store.StageEndUpdate{
		ID: stageID, Verdict: verdict, VerdictConfidence: vc,
		TokensIn: tIn, TokensOut: tOut, CacheHitTokens: cache, CostUSD: cost,
	})
}

func (a *stageAdapter) PutToolCalls(ctx context.Context, runID string, stageID int64, calls []pipeline.ToolCallRecord) error {
	for _, c := range calls {
		started := c.StartedAt.Unix()
		var ended *int64
		var dur *int
		var success *int
		if !c.EndedAt.IsZero() {
			e := c.EndedAt.Unix()
			ended = &e
			d := int(c.EndedAt.Sub(c.StartedAt).Milliseconds())
			dur = &d
		}
		if c.Success {
			one := 1
			success = &one
		} else {
			zero := 0
			success = &zero
		}
		if _, err := a.s.InsertToolCall(ctx, &store.ToolCall{
			RunID:      runID,
			StageID:    stageID,
			Name:       c.Name,
			ArgsHash:   shortHash(c.ArgsJSON),
			ArgsJSON:   string(c.ArgsJSON),
			StartedAt:  started,
			EndedAt:    ended,
			DurationMS: dur,
			Success:    success,
		}); err != nil {
			// Return on first error; caller logs and proceeds.
			return err
		}
	}
	return nil
}

// shortHash returns the first 16 hex chars of sha256(payload). Used as
// args_hash for tool_call rows — Phase 4 approval rules match against
// it instead of scanning args_json.
func shortHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:8])
}
