package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
)

// feedbackAdapter satisfies pipeline.FeedbackStore by marshaling
// pipeline.Feedback to/from JSON and delegating to *store.Store.
type feedbackAdapter struct {
	S *store.Store
}

func (a feedbackAdapter) Put(ctx context.Context, runID string, iter int, fb pipeline.Feedback) error {
	payload, err := json.Marshal(fb)
	if err != nil {
		return fmt.Errorf("marshal feedback: %w", err)
	}
	return a.S.PutFeedbackJSON(ctx, runID, iter, payload)
}

func (a feedbackAdapter) Get(ctx context.Context, runID string, iter int) (pipeline.Feedback, error) {
	raw, err := a.S.GetFeedbackJSON(ctx, runID, iter)
	if errors.Is(err, store.ErrNotFound) {
		return pipeline.Feedback{}, pipeline.ErrFeedbackNotFound
	}
	if err != nil {
		return pipeline.Feedback{}, err
	}
	var fb pipeline.Feedback
	if err := json.Unmarshal(raw, &fb); err != nil {
		// Tolerance for old rows written as bare []FileRef arrays (pre-Feedback-record).
		// Try to decode as []verdict.FileRef and wrap into the new shape.
		var refs []verdict.FileRef
		if aerr := json.Unmarshal(raw, &refs); aerr != nil {
			return pipeline.Feedback{}, fmt.Errorf("unmarshal feedback (struct: %w; array: %v)", err, aerr)
		}
		return pipeline.Feedback{FileRefs: refs}, nil
	}
	return fb, nil
}
