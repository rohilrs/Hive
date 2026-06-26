package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/rohilrs/Hive/internal/verdict"
)

type fakeLoopDetector struct {
	sim float64
	err error
}

func (f *fakeLoopDetector) ClassifyLoopSimilarity(ctx context.Context, prev, curr Iteration) (float64, error) {
	return f.sim, f.err
}

func TestEscalateLadderIdxAdvancesWhenBelowTop(t *testing.T) {
	idx, escalated := escalateLadderIdx(0, 3)
	if idx != 1 || !escalated {
		t.Errorf("got idx=%d escalated=%v want 1, true", idx, escalated)
	}
	// Escalating to the top index is still "escalated=true" — the new
	// tier gets a chance.
	idx, escalated = escalateLadderIdx(1, 3)
	if idx != 2 || !escalated {
		t.Errorf("got idx=%d escalated=%v want 2, true", idx, escalated)
	}
}

func TestEscalateLadderIdxStopsAtTop(t *testing.T) {
	idx, escalated := escalateLadderIdx(2, 3)
	if idx != 2 || escalated {
		t.Errorf("got idx=%d escalated=%v want 2, false", idx, escalated)
	}
	_, escalated = escalateLadderIdx(5, 3)
	if escalated {
		t.Errorf("escalated=%v want false for idx=5/len=3", escalated)
	}
}

func TestEscalateLadderIdxHandlesSingleEntryLadder(t *testing.T) {
	idx, escalated := escalateLadderIdx(0, 1)
	if idx != 0 || escalated {
		t.Errorf("got idx=%d escalated=%v want 0, false (single-entry ladder)", idx, escalated)
	}
}

func TestFakeLoopDetectorReturnsConfigured(t *testing.T) {
	d := &fakeLoopDetector{sim: 0.92}
	sim, err := d.ClassifyLoopSimilarity(context.Background(), Iteration{}, Iteration{})
	if err != nil || sim != 0.92 {
		t.Errorf("got (%v, %v) want (0.92, nil)", sim, err)
	}
}

func TestFakeLoopDetectorPropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	d := &fakeLoopDetector{err: wantErr}
	_, err := d.ClassifyLoopSimilarity(context.Background(), Iteration{}, Iteration{})
	if !errors.Is(err, wantErr) {
		t.Errorf("got err=%v want %v", err, wantErr)
	}
}

func TestIterationStructHasExpectedFields(t *testing.T) {
	it := Iteration{
		Diff:     "diff text",
		FileRefs: []verdict.FileRef{{Path: "foo.go"}},
	}
	if it.Diff != "diff text" || len(it.FileRefs) != 1 {
		t.Error("Iteration fields not set correctly")
	}
}
