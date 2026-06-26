package claudecode

import (
	"context"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/anthropic"
)

// ClassifierSDK is the local interface the claudecode adapter depends on
// for the verdict-fallback path (spec §5.6). Defined here so tests can
// inject a fake without touching internal/anthropic. Production wiring
// passes a real *anthropic.SDK, which implements this interface.
type ClassifierSDK interface {
	ClassifyVerdict(ctx context.Context, reviewerText string) (*anthropic.VerdictResult, error)
}

// ClassifyText runs the fallback classifier against free-form reviewer
// text and returns a Verdict marked FromTool=false. Errors are masked
// to CHANGES_REQUESTED to keep the pipeline conservative when Haiku is
// unreachable (spec §5.6: classifier failures must not abort the run;
// the next iteration will re-review).
func ClassifyText(ctx context.Context, sdk ClassifierSDK, text string) (*adapter.Verdict, error) {
	res, err := sdk.ClassifyVerdict(ctx, text)
	if err != nil {
		return &adapter.Verdict{Kind: adapter.VerdictChangesRequested, FromTool: false}, nil
	}
	kind := adapter.VerdictKind(res.Verdict)
	if kind != adapter.VerdictApprove && kind != adapter.VerdictChangesRequested {
		kind = adapter.VerdictChangesRequested
	}
	return &adapter.Verdict{Kind: kind, Confidence: res.Confidence, FromTool: false}, nil
}
