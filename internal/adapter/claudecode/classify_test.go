package claudecode

import (
	"context"
	"errors"
	"testing"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/anthropic"
)

type fakeSDK struct {
	response *anthropic.VerdictResult
	err      error
}

func (f *fakeSDK) ClassifyVerdict(_ context.Context, _ string) (*anthropic.VerdictResult, error) {
	return f.response, f.err
}

func TestClassifyApprovedText(t *testing.T) {
	sdk := &fakeSDK{response: &anthropic.VerdictResult{Verdict: "APPROVE", Confidence: 88}}
	v, err := ClassifyText(context.Background(), sdk, "looks good")
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != adapter.VerdictApprove {
		t.Errorf("kind=%s", v.Kind)
	}
	if v.FromTool {
		t.Error("FromTool should be false")
	}
}

func TestClassifyErrorReturnsChangesRequested(t *testing.T) {
	sdk := &fakeSDK{err: errors.New("network down")}
	v, err := ClassifyText(context.Background(), sdk, "anything")
	if err != nil {
		t.Fatal("error should be masked")
	}
	if v.Kind != adapter.VerdictChangesRequested {
		t.Errorf("kind=%s want CHANGES_REQUESTED", v.Kind)
	}
}
