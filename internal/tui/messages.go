package tui

import (
	"time"

	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// eventMsg wraps a streamed event for delivery into tea.Update.
type eventMsg struct{ Event rpc.EventMessage }

// roadmapContentLoadedMsg carries the result of a branch-aware
// roadmap.content fetch fired when the viewer's working-tree read missed.
type roadmapContentLoadedMsg struct {
	slug    string
	content string
	err     error
}

// initialStateMsg carries the result of the initial-state fetch.
// Sent once after subscribe, and again after any resync / reconnect.
type initialStateMsg struct {
	Projects []rpc.ProjectView
	Tasks    []rpc.TaskView
	// Status is daemon.status's raw payload (running/recent runs +
	// counts). Not yet typed in pkg/rpc; the TUI normalizes on apply.
	Status map[string]any
	// SequenceStatus carries per-project sequence.status views (keyed by
	// project ID), fetched for every sequenced project during the initial
	// state fetch. Empty for non-sequenced setups.
	SequenceStatus map[string]*rpc.SeqStatusView
}

// sequenceStatusMsg carries a refreshed sequence.status view for one
// project, fetched in response to a sequence.* event. The root model
// folds View into snapshot.SequenceStatus[ProjectID].
type sequenceStatusMsg struct {
	ProjectID string
	View      *rpc.SeqStatusView
}

// heartbeatTickMsg fires periodically so we can recompute the
// daemon-down banner state.
type heartbeatTickMsg time.Time

// daemonDownMsg signals the event stream disconnected.
type daemonDownMsg struct{ Err error }

// daemonReconnectedMsg signals a successful reconnect; recipient
// should refetch initial state.
type daemonReconnectedMsg struct{}

// costSummaryMsg carries a CostSummaryView fetched via the cost.summary
// RPC. Sent in response to a CostRefreshRequest from the Costs tab.
type costSummaryMsg struct{ Summary rpc.CostSummaryView }

// rpcResultMsg wraps an RPC call's outcome for forwarding to a modal.
type rpcResultMsg struct {
	Kind string
	Err  error
	Data map[string]any
}

// taskDecomposeProposedMsg carries the result of the synchronous
// task.decompose call fired off the UI thread (mirrors the CLI's first
// RPC). The proposals are held as a typed *decompose.Result so the
// confirm modal renders ProposedSubtask fields directly — same in-memory
// typed-handoff pattern as runDoctorCmd's doctor.Report. On Err != nil the
// root forwards the error to the open task_detail modal (it drops its
// spinner and shows the error inline); on success the root builds the
// TaskDecomposeConfirmModal seeded with TaskID + Result.Subtasks.
type taskDecomposeProposedMsg struct {
	TaskID string
	Result *decompose.Result
	Err    error
}

// runStagesMsg carries the result of run.stages for drill-in
// hydration when a run was dispatched before the TUI subscribed
// (or events haven't fully populated the snapshot's Stages map yet).
type runStagesMsg struct {
	RunID  string
	Stages []rpc.StageRow
}
