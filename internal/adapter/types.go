// Package adapter defines Hive's provider abstraction (spec §5.3). An
// Adapter encapsulates one LLM provider's specifics: how to run a stage
// (subprocess vs SDK), how to scope skills/tools, how to capture verdicts.
//
// v1 ships exactly one Adapter: internal/adapter/claudecode (using the
// claude CLI). Future adapters (anthropicsdk, openai, codex, local)
// implement the same interface as sibling packages.
package adapter

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rohilrs/Hive/internal/verdict"
)

// Adapter is the provider-agnostic contract used by pipeline FSMs and
// the daemon. Implementations live in sibling packages.
type Adapter interface {
	Name() string
	RunStage(ctx context.Context, req StageRequest) (*StageOutput, error)
	ClassifyVerdict(ctx context.Context, text string) (*Verdict, error)
	Close() error
}

// StageRequest is the provider-agnostic stage execution request.
type StageRequest struct {
	RunID        string
	StageName    string
	Iter         int
	Model        string
	Skills       []string // skill identifiers
	AllowedTools []string // generic tool names (Read, Edit, Bash, etc.)
	SystemPrompt string
	UserPrompt   string

	Cwd      string // working directory for the worker
	StageDir string // per-stage scratch directory; caller pre-creates with 0700

	// RunDir is the run-level scratch directory (parent of StageDir).
	// Adapters that need run-scoped artifacts shared across stages
	// (e.g., the Hive-owned scavenger plugin dir from Phase 2b.1) write
	// here. Earlier phases left it empty; field is additive.
	RunDir string

	// OriginalRepoPath is the user's actual repo path (not the
	// worktree). It is the canonical repo path that pipeline stages use
	// for repo-relative operations that must resolve against the real
	// project tree rather than the per-run worktree (Cwd). Earlier
	// phases left it empty and the field is additive.
	OriginalRepoPath string

	Timeout         time.Duration
	MaxOutputTokens int

	// VerdictToolName is the tool the assistant must call to complete.
	// Empty = no verdict required (e.g., implement stages).
	VerdictToolName string

	// DocToolName, when set, advertises an optional structured-output tool
	// (hive_submit_documentation) on a "hive_docs" MCP server that forwards
	// to the daemon. Used by the non-blocking Build documenter stage.
	DocToolName string

	// StageID is the row ID returned by the pipeline's StageStore.BeginStage,
	// passed through so adapter-side observers (Phase 3.2 stall monitor)
	// can attribute their rows to the stage. Zero when the caller didn't
	// supply one (older adapters / tests).
	StageID int64
}

// StageOutput is the provider-agnostic stage execution result.
type StageOutput struct {
	RawEvents []byte // adapter-specific event stream, captured for logs
	Stderr    string
	ExitCode  int
	Verdict   *Verdict
	Tokens    TokenUsage
	ToolCalls []ToolCallRecord

	StartedAt time.Time
	EndedAt   time.Time
}

// TokenUsage is the per-stage token accounting reported by the provider.
type TokenUsage struct {
	Input    int
	Output   int
	CacheHit int
}

// ToolCallRecord is one provider-agnostic tool invocation observed
// during a stage. Adapters reconstruct these from their native event
// streams (tool_use / tool_result for claudecode) and return them on
// StageOutput.ToolCalls. The pipeline persists them after the stage
// completes; Phase 3.2's stall monitor reads them live from the
// adapter's stream (not from this slice).
type ToolCallRecord struct {
	Name      string          // e.g. "Read", "Edit", "Bash"
	ArgsJSON  json.RawMessage // raw tool input as the provider reported
	StartedAt time.Time       // when the adapter saw the tool_use event
	EndedAt   time.Time       // when the matching tool_result arrived
	Success   bool            // !is_error on tool_result
}

// VerdictKind is the canonical verdict label.
type VerdictKind string

const (
	VerdictApprove          VerdictKind = "APPROVE"
	VerdictChangesRequested VerdictKind = "CHANGES_REQUESTED"
	VerdictNone             VerdictKind = ""
)

// Verdict carries the structured outcome of a stage that requested one.
// FileRefs is populated for CHANGES_REQUESTED verdicts coming from the
// reviewer tool; classifier-fallback verdicts (FromTool=false) leave it
// empty.
//
// Summary is the reviewer's optional holistic finding — a one-paragraph
// cross-cutting observation that is not anchored to any single file.
// Empty when the verdict came from the classifier fallback (FromTool=false).
type Verdict struct {
	Kind       VerdictKind
	Confidence int
	FileRefs   []verdict.FileRef
	Summary    string

	// FromTool is true if the verdict came from the provider's native
	// tool-use mechanism (preferred); false if from the classifier fallback.
	FromTool bool
}
