package store

import (
	"database/sql"
	"time"
)

type Project struct {
	ID        string         `json:"id"`
	Slug      string         `json:"slug"`
	Name      string         `json:"name"`
	RepoPath  *string        `json:"repo_path,omitempty"`
	Sources   map[string]any `json:"sources,omitempty"`
	Config    map[string]any `json:"config,omitempty"`
	Status    string         `json:"status"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type Task struct {
	ID                string
	ProjectID         string
	Source            string
	SourceID          string
	Title             string
	Body              string
	Priority          string
	Status            string
	Pipeline          string
	PredictedFiles    []string
	ConflictSet       []string
	PredictConfidence int
	Metadata          map[string]any
	CreatedAt         time.Time
	UpdatedAt         time.Time
	// ParentTaskID points to the task that decomposed into this one.
	// NULL for top-level tasks. Phase 7 hive decompose introduced this.
	ParentTaskID sql.NullString
	GateState    string // sequenced-dispatcher gate (none|built|pr_open|awaiting_merge|satisfied|skipped); Phase 2a
	// LinearSyncedState is the last logical Linear workflow state pushed for
	// this task's mirrored issue (Linear write-back Phase 1). Empty = unpushed.
	LinearSyncedState string
	// LastFailureFeedback is a JSON blob of a build run's final feedback
	// (summary + file_refs + exhaust_reason), written when the run gives up
	// (needs_attention) and injected into the iter-0 implement prompt of the
	// next run for this task. Cleared on success. Empty = no prior failure.
	LastFailureFeedback string
}

type Run struct {
	ID                      string
	TaskID                  string
	ProjectID               string
	Pipeline                string
	ParentRunID             string // empty = root run; set = child fix run (Phase 4.3.1)
	Status                  string
	StartedAt               *time.Time
	EndedAt                 *time.Time
	TotalCostUSD            float64
	Summary                 string
	DocumentationSkipped    bool
	DocumentationSkipReason string
	CreatedAt               time.Time
	// WorkerPID is the OS PID of the claude subprocess servicing this
	// run. Set on subprocess start; cleared on exit. NULL for runs
	// without an active worker. Used by daemon restart-recovery to
	// kill orphaned subprocesses from a crashed daemon.
	WorkerPID sql.NullInt64
	// BranchName is the git branch this run's worktree was created on
	// (set at worktree-create). PRURL/PRNumber are the PR opened by the
	// finish-branch create-pr stage (NULL until then). Populated by the
	// sequenced-dispatcher foundations (Phase 1).
	BranchName string
	PRURL      string
	PRNumber   sql.NullInt64
}

type Stage struct {
	ID                int64
	RunID             string
	Name              string
	Iter              int
	Model             string
	StartedAt         int64
	EndedAt           int64
	TokensIn          int
	TokensOut         int
	CacheHitTokens    int
	Verdict           string
	VerdictConfidence *float64
	CostUSD           *float64
}

// Stall is one row from the stalls table. Phase 3.2 + future stall
// layers populate it. Nullable columns use pointer types.
type Stall struct {
	ID          int64
	RunID       string
	StageID     *int64 // nil = run-level stall (no stage context)
	Layer       int    // 1 = event heartbeat, 2 = tool-call timeout, 3 = iteration loop
	DetectedAt  int64
	ClearedAt   *int64 // nil while active
	ActionTaken string // "surfaced" / "killed_subprocess" / "escalated_model" / "marked_needs_attention"
	DetailsJSON string // free-form per-layer JSON; empty when no metadata
}

type Event struct {
	ID        int64
	RunID     string
	StageID   string
	Timestamp time.Time
	Type      string
	Message   string
	Payload   map[string]any
}
