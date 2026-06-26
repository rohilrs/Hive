package store

import "embed"

// MaxSchemaVersion is the version of the latest registered migration.
// Callers (e.g. internal/doctor) use it to verify the binary expects
// the same schema the running DB has applied.
//
// Computed from migrations() at init so the value never drifts when
// a new migration lands.
var MaxSchemaVersion = func() int {
	m := migrations()
	if len(m) == 0 {
		return 0
	}
	return m[len(m)-1].version
}()

//go:embed schema/*.sql
var schemaFS embed.FS

func migrations() []migration {
	return []migration{
		{version: 1, path: "schema/0001_initial.sql"},
		{version: 2, path: "schema/0002_iter_feedback.sql"},
		{version: 3, path: "schema/0003_run_predictions.sql"},
		{version: 4, path: "schema/0004_predictor_metrics.sql"},
		{version: 5, path: "schema/0005_predictor_accuracy.sql"},
		{version: 6, path: "schema/0006_run_config_snapshot.sql"},
		{version: 7, path: "schema/0007_stages.sql"},
		{version: 8, path: "schema/0008_tool_calls.sql"},
		{version: 9, path: "schema/0009_stalls.sql"},
		{version: 10, path: "schema/0010_task_pipeline.sql"},
		{version: 11, path: "schema/0011_approvals.sql"},
		{version: 12, path: "schema/0012_run_parent.sql"},
		{version: 13, path: "schema/0013_chat.sql"},
		{version: 14, path: "schema/0014_chat_provider_session.sql"},
		{version: 15, path: "schema/0015_chat_session_metadata.sql"},
		{version: 16, path: "schema/0016_run_worker_pid.sql"},
		{version: 17, path: "schema/0017_task_parent.sql"},
		{version: 18, path: "schema/0018_chat_sessions_kind.sql"},
		{version: 19, path: "schema/0019_chat_sessions_project_slug.sql"},
		{version: 20, path: "schema/0020_run_branch_pr.sql"},
		{version: 21, path: "schema/0021_sequence_dispatchers.sql"},
		{version: 22, path: "schema/0022_task_linear_synced_state.sql"},
		{version: 23, path: "schema/0023_dispatcher_completed_phases.sql"},
		{version: 24, path: "schema/0024_task_last_failure_feedback.sql"},
	}
}

type migration struct {
	version int
	path    string
}
