package rpc

// EventType enumerates daemon → subscriber event kinds.
type EventType string

const (
	// Task lifecycle
	EventTaskCreated     EventType = "task.created"
	EventTaskUpdated     EventType = "task.updated"
	EventTaskIntegrating EventType = "task.integrating" // finish-branch chain started
	EventPROpened        EventType = "task.pr_opened"   // create-pr succeeded; data: pr_url, pr_number
	EventTaskMerged      EventType = "task.merged"      // PR auto-merged into the feature branch

	// Project lifecycle. project.updated carries a "deleted": true flag
	// when the project was removed (mirrors the task.updated deleted
	// pattern); otherwise it's an edit (name / repo_path / status).
	EventProjectCreated EventType = "project.created"
	EventProjectUpdated EventType = "project.updated"

	// Sequenced dispatcher lifecycle (Phase 2a). gate_changed/phase_advanced/
	// blocked/completed land with the Phase 2b engine.
	EventSequenceCreated     EventType = "sequence.created"
	EventSequenceUpdated     EventType = "sequence.updated"
	EventSequenceGateChanged EventType = "sequence.gate_changed"

	// Run lifecycle
	EventRunStarted EventType = "run.started"
	EventRunUpdated EventType = "run.updated"
	EventRunEnded   EventType = "run.ended"

	// Stage lifecycle
	EventStageStarted EventType = "stage.started"
	EventStageEnded   EventType = "stage.ended"

	// Approvals
	EventApprovalRequested EventType = "approval.requested"
	EventApprovalResolved  EventType = "approval.resolved"
	// EventToolDecision fires for EVERY gated tool-use evaluation (allowed
	// or denied) when approvals are on — gives live per-tool worker
	// activity in the Events tab + drill-in (stage events alone are sparse
	// during a long stage).
	EventToolDecision EventType = "tool.decision"

	// Stalls
	EventStallDetected EventType = "stall.detected"
	EventStallCleared  EventType = "stall.cleared"

	// Worker JSONL passthrough (for live drill-in)
	EventWorker EventType = "worker"

	// Documenter — structured output forwarded by the document stage via
	// the hive_submit_documentation MCP tool. Data keys: run_id (string),
	// stage (string), summary (string), files_changed ([]string at publish;
	// []any for subscribers after a JSON round-trip), changelog_entry (string).
	EventDocumentationSubmitted EventType = "documentation.submitted"

	// Async roadmap.decompose lifecycle (propose half). Data always carries
	// "decompose_id"; proposed also carries "project_slug", "phase", and the
	// RoadmapDecomposeResult under "result"; progress carries "phase_label";
	// failed carries "error".
	EventDecomposeProgress EventType = "decompose.progress"
	EventDecomposeProposed EventType = "decompose.proposed"
	EventDecomposeFailed   EventType = "decompose.failed"

	// Async project.graduate lifecycle. Data always carries "graduate_id"
	// and "project_slug". progress also carries "phase_label"; verdict
	// carries the *graduate.GraduationVerdict under "verdict" (fires even on
	// a blocking verdict, before the terminal failed event); done carries
	// "pr_url" and "dry_run"; failed carries "error".
	EventGraduateProgress EventType = "graduate.progress"
	EventGraduateVerdict  EventType = "graduate.verdict"
	EventGraduateDone     EventType = "graduate.done"
	EventGraduateFailed   EventType = "graduate.failed"

	// EventWorkerOrphanKilled is published once per orphan worker killed
	// by the boot-path recoverOrphanedWorkers sweep. After a clean
	// shutdown the sweep is a no-op; after a crash this fires once per
	// surviving claude subprocess. Data fields: run_id (string), pid
	// (int), was_alive (bool — false on ESRCH/already-dead), timestamp
	// (int64 unix-seconds).
	EventWorkerOrphanKilled EventType = "worker.orphan_killed"

	// Control
	EventResync EventType = "resync" // subscriber should re-fetch full state

	// Daemon lifecycle
	EventDaemonStopping  EventType = "daemon.stopping"  // subscribers should treat socket close as expected
	EventDaemonHeartbeat EventType = "daemon.heartbeat" // periodic keep-alive; data is {"ts": <unix-seconds>}
)

// EventMessage is the streamed event envelope sent over the
// event-subscription channel. Subscribers read these as JSONL frames.
//
// Data shape by Type:
//
//	task.created / task.updated     — {"task_id": string}
//	project.created                 — {"project_id": string, "slug": string, "name": string, "repo_path": string}
//	project.updated                 — {"project_id": string, "slug"?: string, "name"?: string, "repo_path"?: string, "deleted"?: bool}
//	run.started / run.updated /
//	  run.ended                     — {"run_id": string, "status": string, "task_id": string}
//	stage.started / stage.ended     — {"run_id": string, "stage_id": string, "name": string, "iter": int, "verdict": string?}
//	approval.requested              — {"approval_id": string, "run_id": string, "tool_name": string, "tool_input": object}
//	approval.resolved               — {"approval_id": string, "decision": "approve"|"deny", "reason": string}
//	stall.detected / stall.cleared  — {"run_id": string, "stage_id": string, "layer": "L1"|"L2"|"L3"}
//	worker                          — {"run_id": string, "stage_id": string, "raw": object (worker JSONL passthrough)}
//	resync                          — {} (subscriber should re-fetch full state)
//	daemon.stopping                 — {} (clean shutdown; socket close to follow)
//	daemon.heartbeat                — {"ts": <unix-seconds>}
type EventMessage struct {
	Type EventType      `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}
