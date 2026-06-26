// Package pipeline owns Hive's pipeline state machines. Phase 1c ships the
// Build pipeline with implement<->review iteration loop only; test/validate/
// document stages arrive in Phase 3 + 4.
package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// Run is the in-memory representation of a pipeline invocation. Fields
// roughly mirror store.Run plus the working state the FSM needs (worktree
// path, runtime/scratch path).
type Run struct {
	ID           string
	Task         *store.Task
	Project      *store.Project
	WorktreePath string
	RuntimeDir   string
	BranchName   string
	Pipeline     string

	// TargetBranch is the integration branch this run targets, resolved
	// per-project at dispatch. The finish-branch pipeline substitutes it
	// into the {{target_branch}} token of its create-pr command. Empty
	// resolves to "main".
	TargetBranch string

	// Commands carries the per-project-resolved pipeline shell commands +
	// timeouts for this run, overriding the boot-time pipeline Cfg defaults.
	// nil = use the pipeline's Cfg defaults (legacy callers / unit tests).
	// Populated at dispatch from the per-project effectiveCfg.
	Commands *RunCommands

	// Prediction is the predictor result populated by the scheduler
	// at dispatch time (Phase 2b.3). nil when the predictor is
	// disabled, errors, or degrades gracefully. The implement-stage
	// prompt enumerates inline capsules + overflow pointers + bundle
	// path when this is non-nil.
	Prediction *predictor.Result
}

// RunCommands carries per-run pipeline shell commands + stage timeouts, resolved
// per-project at dispatch. An empty string for a command means "skip that stage"
// (resolved per project via the config overlay).
type RunCommands struct {
	// build pipeline
	Test, Validate               string
	TestTimeout, ValidateTimeout time.Duration
	// finish-branch pipeline
	Format, Typecheck, Lint, FinishTest, CreatePR, CIMonitor string
	FinishStageTimeout, CIMonitorTimeout                     time.Duration
	// Prepare installs/prepares deps before the shippability gates. Only
	// consumed by `hive project graduate` Stage 3 (it runs the gates on a fresh
	// detached worktree with no build stage to install deps). Empty = skip.
	Prepare string
	// CI auto-fix (Integration): when AutoFixCI is set, a ci-monitor failure
	// triggers ONE child fix + a push (CIFixPushCommand) before re-checking CI.
	AutoFixCI        bool
	CIFixPushCommand string
}

// EffTest returns the effective build test command + timeout: the per-run
// override when Commands is set, else the supplied pipeline defaults.
func (r *Run) EffTest(defCmd string, defTO time.Duration) (string, time.Duration) {
	if r.Commands != nil {
		return r.Commands.Test, r.Commands.TestTimeout
	}
	return defCmd, defTO
}

// EffValidate mirrors EffTest for the validate stage.
func (r *Run) EffValidate(defCmd string, defTO time.Duration) (string, time.Duration) {
	if r.Commands != nil {
		return r.Commands.Validate, r.Commands.ValidateTimeout
	}
	return defCmd, defTO
}

// Result is what Pipeline.Run returns on success.
type Result struct {
	Status       string
	Summary      string
	Iterations   int
	TotalCostUSD float64
	EndedAt      time.Time

	// PRURL/PRNumber are populated by the finish-branch pipeline from the
	// create-pr stage output. Zero values when no PR was opened.
	PRURL    string
	PRNumber int

	// Phase 4.4: documenter outcome (Build pipeline). Skipped=true means
	// the documenter was enabled but its stage failed; the run still
	// completes "done". The daemon persists these to runs.
	DocumentationSkipped    bool
	DocumentationSkipReason string

	// FinalFeedback is the last iteration's feedback when the build pipeline
	// gives up (needs_attention). nil on "done" or when no feedback was
	// captured before give-up.
	FinalFeedback *Feedback

	// ExhaustReason is the human-readable reason the loop gave up.
	// Empty on "done".
	ExhaustReason string
}

// ModelLadder describes the per-iteration model escalation.
type ModelLadder struct {
	Worker   []string
	Reviewer []string
}

func (l ModelLadder) ModelsForIter(iter int) (worker, reviewer string) {
	return lastOr(l.Worker, iter), lastOr(l.Reviewer, iter)
}

// WorkerAt returns the worker model at ladder index i, clamped to
// the last entry. Empty when the ladder is empty.
func (l ModelLadder) WorkerAt(i int) string { return lastOr(l.Worker, i) }

// ReviewerAt returns the reviewer model at ladder index i, clamped
// to the last entry. Empty when the ladder is empty.
func (l ModelLadder) ReviewerAt(i int) string { return lastOr(l.Reviewer, i) }

func lastOr(xs []string, i int) string {
	if len(xs) == 0 {
		return ""
	}
	if i < len(xs) {
		return xs[i]
	}
	return xs[len(xs)-1]
}

// Feedback is the per-iteration feedback handed to the next implement stage:
// the reviewer's holistic Summary (empty for shell-failure feedback) plus the
// file-anchored FileRefs (review comments, or the synthetic test/validate ref).
type Feedback struct {
	// Summary is rendered into the re-implement prompt by implementPrompt (wired in the following task).
	Summary  string            `json:"summary"`
	FileRefs []verdict.FileRef `json:"file_refs"`
}

// FeedbackStore is the pipeline's typed view onto reviewer FileRefs
// persistence. The daemon composition root supplies an implementation
// backed by *store.Store (see internal/daemon/feedback_adapter.go).
// Tests use an in-memory fake.
type FeedbackStore interface {
	Put(ctx context.Context, runID string, iter int, fb Feedback) error
	Get(ctx context.Context, runID string, iter int) (Feedback, error)
}

// StageStore is the pipeline's typed view onto stages + tool_calls
// persistence. The daemon composition root supplies an implementation
// backed by *store.Store; tests use an in-memory fake.
type StageStore interface {
	BeginStage(ctx context.Context, runID, name string, iter int, model string) (stageID int64, err error)
	EndStage(ctx context.Context, stageID int64, verdict string, verdictConfidence *float64, tokensIn, tokensOut, cacheHit int, costUSD *float64) error
	PutToolCalls(ctx context.Context, runID string, stageID int64, calls []ToolCallRecord) error
}

// EventPublisher is the pipeline's typed view onto event emission.
// nil disables event publishing (pre-3.5a behavior). The daemon
// composition root supplies an implementation backed by eventbus.Bus.
//
// Publish must not block — implementations drop on overflow rather
// than slow the pipeline. Best-effort.
type EventPublisher interface {
	Publish(ev rpc.EventMessage)
}

// StallRecorder is the pipeline's typed view onto stalls persistence.
// Phase 3.3 uses it for L3 (loop) rows; future sub-phases may add
// more layers. Single method keeps the interface narrow.
//
// Implementation by daemon composition root (internal/daemon/
// stall_pipeline_adapter.go), backed by *store.Store. Tests use an
// in-memory fake.
type StallRecorder interface {
	RecordStall(ctx context.Context, runID string, stageID int64, layer int, detectedAt int64, action, details string) error
}

// ToolCallRecord is the pipeline's mirror of adapter.ToolCallRecord
// so callers (StageStore implementations) don't import internal/adapter
// directly. Field names match. Conversion happens in build.go.
type ToolCallRecord struct {
	Name      string
	ArgsJSON  []byte
	StartedAt time.Time
	EndedAt   time.Time
	Success   bool
}

// ErrFeedbackNotFound mirrors store.ErrNotFound at the pipeline-layer
// abstraction so consumers don't import store directly.
var ErrFeedbackNotFound = errors.New("pipeline: feedback not found")
