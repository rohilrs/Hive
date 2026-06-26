package rpc

import "time"

// TaskStatus enumerates terminal and in-flight states for a task.
type TaskStatus string

const (
	TaskStatusPending        TaskStatus = "pending"
	TaskStatusRunning        TaskStatus = "running"
	TaskStatusDone           TaskStatus = "done"
	TaskStatusNeedsAttention TaskStatus = "needs_attention"
	TaskStatusAbandoned      TaskStatus = "abandoned"
	TaskStatusBlocked        TaskStatus = "blocked"
)

// RunStatus enumerates pipeline run lifecycle states.
type RunStatus string

const (
	RunStatusPending        RunStatus = "pending"
	RunStatusRunning        RunStatus = "running"
	RunStatusDone           RunStatus = "done"
	RunStatusNeedsAttention RunStatus = "needs_attention"
	RunStatusAbandoned      RunStatus = "abandoned"
)

// StageVerdict is the structured outcome of a stage that requested one.
// The "not applicable" case is represented by the zero value ("") combined
// with `omitempty` on the field tag — no explicit constant needed.
type StageVerdict string

const (
	VerdictApprove          StageVerdict = "APPROVE"
	VerdictChangesRequested StageVerdict = "CHANGES_REQUESTED"
	VerdictUnclear          StageVerdict = "UNCLEAR"
)

// TaskView is the wire-format projection of a task row.
//
// PredictConfidence is a pointer so the JSON layer can distinguish
// "no confidence reported" (nil → omitted) from "confidence is 0"
// (real low-confidence signal — the conflict-guard kill-switch fires
// at <60, so 0 is meaningful).
type TaskView struct {
	ID                string         `json:"id"`
	ProjectID         string         `json:"project_id"`
	Source            string         `json:"source"`
	SourceID          string         `json:"source_id,omitempty"`
	Title             string         `json:"title"`
	Body              string         `json:"body,omitempty"`
	Priority          string         `json:"priority"`
	Status            TaskStatus     `json:"status"`
	Pipeline          string         `json:"pipeline"`
	PredictedFiles    []string       `json:"predicted_files,omitempty"`
	ConflictSet       []string       `json:"conflict_set,omitempty"`
	PredictConfidence *int           `json:"predict_confidence,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

// RunView is the wire-format projection of a run row.
type RunView struct {
	ID                      string     `json:"id"`
	TaskID                  string     `json:"task_id"`
	ProjectID               string     `json:"project_id"`
	Pipeline                string     `json:"pipeline"`
	Status                  RunStatus  `json:"status"`
	StartedAt               time.Time  `json:"started_at"`
	EndedAt                 *time.Time `json:"ended_at,omitempty"`
	TotalCostUSD            float64    `json:"total_cost_usd"`
	Summary                 string     `json:"summary,omitempty"`
	DocumentationSkipped    bool       `json:"documentation_skipped"`
	DocumentationSkipReason string     `json:"documentation_skip_reason,omitempty"`
}

// StageView is the wire-format projection of a stage row.
//
// VerdictConfidence is a pointer for the same reason as TaskView.PredictConfidence:
// the verdict-fallback path triggers at <70, so a reported 0 must be
// distinguishable from "no verdict confidence reported".
type StageView struct {
	ID                string       `json:"id"`
	RunID             string       `json:"run_id"`
	Name              string       `json:"name"`
	Iter              int          `json:"iter"`
	Model             string       `json:"model"`
	StartedAt         time.Time    `json:"started_at"`
	EndedAt           *time.Time   `json:"ended_at,omitempty"`
	TokensIn          int          `json:"tokens_in"`
	TokensOut         int          `json:"tokens_out"`
	CacheHitTokens    int          `json:"cache_hit_tokens"`
	Verdict           StageVerdict `json:"verdict,omitempty"`
	VerdictConfidence *int         `json:"verdict_confidence,omitempty"`
	CostUSD           float64      `json:"cost_usd"`
}

// StageRow is the wire shape returned by run.stages — minimal fields
// the TUI drill-in needs. Distinct from StageView (which is for a
// future stage detail RPC).
type StageRow struct {
	ID        int64  `json:"id"`
	RunID     string `json:"run_id"`
	Name      string `json:"name"`
	Iter      int    `json:"iter"`
	Model     string `json:"model"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
}

// CostBucket is one row in a cost rollup (per day / model / pipeline /
// project). Count is the number of stages contributing to TotalUSD.
type CostBucket struct {
	Key      string  `json:"key"`
	TotalUSD float64 `json:"total_usd"`
	Count    int     `json:"count"`
}

// CostSummaryView is the cost.summary RPC response: four pre-computed
// rollups of stages.cost_usd. Phase 3.6 Costs tab consumes these.
type CostSummaryView struct {
	Daily       []CostBucket `json:"daily"`     // last 14 days, newest first
	Models      []CostBucket `json:"models"`    // per-model
	Pipelines   []CostBucket `json:"pipelines"` // per-pipeline
	Projects    []CostBucket `json:"projects"`  // per-project (key=slug)
	GeneratedAt int64        `json:"generated_at"`
}

// ProjectView is the wire-format projection of a project row. Phase
// 3.5b TUI's Projects tab consumes these via the project.list RPC.
type ProjectView struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	RepoPath     string `json:"repo_path,omitempty"`
	Status       string `json:"status"`
	DispatchMode string `json:"dispatch_mode,omitempty"`
	CreatedAt    int64  `json:"created_at"`

	FeatureBranch string `json:"feature_branch,omitempty"`
	TargetBranch  string `json:"target_branch,omitempty"`
	AutoIntegrate bool   `json:"auto_integrate,omitempty"`
	MergeMethod   string `json:"merge_method,omitempty"`
	AutoFixCI     bool   `json:"auto_fix_ci,omitempty"`

	// CanSequence reports whether sequenced dispatch could be enabled for this
	// project right now (the roadmap/spec enable-gate passes). The TUI uses it to
	// grey out the "sequenced" option in the edit modal where it isn't yet valid.
	CanSequence bool `json:"can_sequence,omitempty"`
}

// SeqStatusView / SeqPhaseView / SeqTaskView are the wire-format projection of
// the sequence.status RPC response. The daemon producer (internal/daemon's
// seqStatusView etc.) marshals matching JSON; the TUI (Projects tab badge +
// Sequence modal) unmarshals into these. Shared here so both the tabs and
// modals packages can consume them without importing the tui package.
type SeqStatusView struct {
	Slug        string         `json:"slug"`
	Status      string         `json:"status,omitempty"`
	Policy      string         `json:"policy,omitempty"`
	Target      string         `json:"target,omitempty"`
	ActivePhase string         `json:"active_phase"`
	Complete    bool           `json:"complete"`
	Phases      []SeqPhaseView `json:"phases"`
	Unsequenced []SeqTaskView  `json:"unsequenced,omitempty"`
}

type SeqPhaseView struct {
	Number   string        `json:"number"`
	Title    string        `json:"title"`
	Complete bool          `json:"complete"`
	Tasks    []SeqTaskView `json:"tasks"`
	Blocked  []SeqTaskView `json:"blocked,omitempty"`
}

type SeqTaskView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	GateState string `json:"gate_state"`
}

// EventView is the wire-format projection of an event row.
type EventView struct {
	ID        int64          `json:"id"`
	RunID     string         `json:"run_id"`
	StageID   string         `json:"stage_id,omitempty"`
	Timestamp time.Time      `json:"ts"`
	Type      string         `json:"type"`
	Message   string         `json:"message,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}
