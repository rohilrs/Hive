// Package rpc defines the wire protocol shared between the Hive daemon,
// TUI processes, and CLI invocations. Types only; no server or client
// implementation lives here.
package rpc

// Method name constants. Each is a JSON-RPC method identifier used by the
// daemon's Unix-socket RPC server.
const (
	// Task management
	MethodListTasks  = "task.list"
	MethodGetTask    = "task.get"
	MethodAddTask    = "task.add"
	MethodEditTask   = "task.edit"
	MethodDeleteTask = "task.delete"

	// Scheduling helpers
	MethodPredict        = "task.predict"
	MethodDecompose      = "task.decompose"
	MethodDecomposeApply = "task.decompose_apply"

	// Roadmap (Phase 8.B / existing-work reconciliation)
	MethodRoadmapContent        = "roadmap.content"
	MethodRoadmapDecompose      = "roadmap.decompose"
	MethodRoadmapDecomposeApply = "roadmap.decompose_apply"
	MethodRoadmapSyncLinear     = "roadmap.sync_linear"
	MethodRoadmapPlanSetup      = "roadmap.plan_setup"
	MethodRoadmapPlanPush       = "roadmap.plan_push"

	// Sequenced dispatcher (Phase 2a)
	MethodSequenceEnable  = "sequence.enable"
	MethodSequenceDisable = "sequence.disable"
	MethodSequenceStatus  = "sequence.status"

	// Sequenced dispatcher (Phase 2b)
	MethodSequencePause  = "sequence.pause"
	MethodSequenceResume = "sequence.resume"
	MethodSequenceSkip   = "sequence.skip"

	// Sequenced dispatcher (Phase 3)
	MethodSequenceAdvance  = "sequence.advance"
	MethodSequenceComplete = "sequence.complete"

	// Run lifecycle
	MethodRunNow        = "run.now"
	MethodActiveWorkers = "run.active"
	MethodGetRun        = "run.get"
	MethodResume        = "run.resume"
	MethodAbandon       = "run.abandon"
	MethodRunDocument   = "run.document" // re-run the documenter stage for a run
	MethodTaskFinish    = "task.finish"
	MethodResolveNow    = "resolve.now" // manual conflict-resolver trigger for a stuck task
	MethodMergeRetry    = "merge.retry" // recover a task parked at the terminal merge_failed gate
	MethodAttachRun     = "run.attach"  // TODO(phase-1c): define params/result; opens tmux pane attached to worker

	// Documenter
	MethodDocumentationSubmit = "documentation.submit" // documenter stage forwards structured output (hive_submit_documentation MCP tool)

	// Approvals
	MethodApprove          = "approval.approve"
	MethodDeny             = "approval.deny"
	MethodApprovalEvaluate = "approval.evaluate" // permission-prompt-tool -> engine
	MethodApprovalList     = "approval.list"     // recent audit rows
	MethodApprovalRuleAdd  = "approval.rule.add" // add an allow/deny rule
	MethodApprovalResolve  = "approval.resolve"  // TUI/CLI resolves a pending approval
	MethodApprovalPending  = "approval.pending"  // list in-flight pending approvals

	// Observability
	MethodCostSummary = "cost.summary"
	MethodStatus      = "daemon.status"
	MethodHealth      = "daemon.health"
	MethodSearch      = "search"
	MethodShowDiff    = "run.diff"

	// Health remediation (TUI health modal: rebase/merge feature onto target)
	MethodHealthRemediate = "health.remediate"

	// Run-artifact cleanup / GC
	MethodCleanupRun = "cleanup.run"

	// Event subscription (long-lived stream)
	MethodSubscribeEvents = "events.subscribe"

	// Project CRUD (Phase 3.5b/3.7 TUI; edit/delete/archive Phase 6.0)
	MethodListProjects   = "project.list"
	MethodAddProject     = "project.add"
	MethodEditProject    = "project.edit"
	MethodDeleteProject  = "project.delete"
	MethodArchiveProject = "project.archive"

	// Project graduation (async: returns a graduate_id, streams
	// graduate.progress/verdict/done/failed on the bus)
	MethodProjectGraduate       = "project.graduate"
	MethodProjectGraduateStatus = "project.graduate_status"
	MethodProjectRemediate      = "project.remediate"

	// Run drill-in (Phase 3.7.1)
	MethodRunStages = "run.stages"

	// Sources (Phase 5.0)
	MethodSourcesSync   = "sources.sync"
	MethodSourcesBind   = "sources.bind"
	MethodSourcesList   = "sources.list"
	MethodSourcesUnbind = "sources.unbind"
	MethodSourcesStatus = "sources.status"

	// Chat (Phase 6.1)
	MethodChatSend        = "chat.send"
	MethodChatTool        = "chat.tool"    // CC chat provider's MCP server forwards a read-tool call to the shared chat.Registry
	MethodChatConfirm     = "chat.confirm" // CLI/TUI resolves a pending confirm gate (approve/deny)
	MethodChatHistoryList = "chat.history_list"
	MethodChatHistoryGet  = "chat.history_get"
	MethodChatSetName     = "chat.set_name"
	MethodChatDelete      = "chat.delete"
)
