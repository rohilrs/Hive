// Package tui implements Hive's Bubbletea TUI client.
package tui

import (
	"sort"
	"time"

	"github.com/rohilrs/Hive/internal/tui/tabs"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// Snapshot is the in-memory view of daemon state the TUI renders from.
// Updated by ApplySnapshot as the event stream advances; reset +
// rehydrated via initial-state RPCs on subscribe / resync.
type Snapshot struct {
	Projects map[string]*ProjectView
	Tasks    map[string]*TaskView
	Runs     map[string]*RunView
	Stages   map[int64]*StageView

	recentEvents    []tabs.TimedEvent
	MaxRecentEvents int

	LastHeartbeat int64 // unix seconds; 0 = never received

	// Phase 4.6: pending approvals awaiting an operator decision (ask
	// mode). Added on approval.requested, removed on approval.resolved.
	pendingApprovals map[string]*PendingApprovalView

	// SequenceStatus caches the latest sequence.status view per project,
	// keyed by project ID. Populated on initial fetch (for sequenced
	// projects) and refreshed on sequence.* events (Phase 4 sequenced TUI).
	SequenceStatus map[string]*rpc.SeqStatusView
}

// PendingApprovalView is a tool-use request blocked on operator approval.
type PendingApprovalView struct {
	ID, RunID, Stage, ToolName, Arg string
}

// ProjectView mirrors rpc.ProjectView with TUI-friendly fields.
type ProjectView struct {
	ID, Slug, Name, RepoPath, Status string
	CreatedAt                        int64
	// DispatchMode mirrors the daemon's per-project dispatch mode
	// ("manual"|"auto_all"|"sequenced"). Drives whether the TUI fetches +
	// caches a sequence.status view for the project (Phase 4 sequenced TUI).
	DispatchMode string
	// Feature-branch integration fields (Phase B). Set when the daemon
	// starts sending these via project.created / project.updated events.
	FeatureBranch string
	TargetBranch  string
	AutoIntegrate bool
	MergeMethod   string
	AutoFixCI     bool
	// CanSequence mirrors rpc.ProjectView.CanSequence: whether the project's
	// roadmap/spec gate currently passes, so the edit modal can offer the
	// "sequenced" dispatch mode.
	CanSequence bool
}

// The sequence.status wire structs live in pkg/rpc (rpc.SeqStatusView etc.)
// so the tabs + modals packages can consume them without importing tui.

// TaskView captures a task's state for TUI rendering.
type TaskView struct {
	ID, ProjectID, Title, Priority, Status string
	// Order is the roadmap order label ("P.I", e.g. "1.2") derived from the
	// task's roadmap_phase + roadmap_phase_index metadata. "" for non-roadmap
	// tasks. Surfaced as a "[1.2]" prefix in the Projects task list.
	Order string
	// IntegrationState tracks where the task is in the feature-branch
	// integration lifecycle: "" | "integrating" | "pr_open" | "ci" |
	// "merged" | "blocked". Set by EventTaskIntegrating / EventPROpened /
	// EventTaskMerged, or inferred from task status transitions.
	IntegrationState string
	PRURL            string
	PRNumber         int
}

// taskOrder formats a roadmap phase + intra-phase index into a compact "P.I"
// order label (e.g. "1.2"). Returns "" when either part is missing, so
// non-roadmap tasks render no prefix. Accepts the raw metadata/event values
// (which arrive as `any`-typed strings).
func taskOrder(phase, index any) string {
	p, _ := phase.(string)
	i, _ := index.(string)
	if p == "" || i == "" {
		return ""
	}
	return p + "." + i
}

// RunView captures a run's state for TUI rendering.
type RunView struct {
	ID, TaskID, Pipeline, Status, Summary string
	StartedAt, EndedAt                    int64
	DocumentationSkipped                  bool
	DocumentationSkipReason               string
	// ParentRunID is set on child fix runs (finish-branch auto-fix);
	// empty for root runs. Threaded into ActiveRunSummary so the Active
	// tab can render children indented under their parent (Phase 4.3.1
	// #4).
	ParentRunID string
}

// StageView captures a stage's state.
type StageView struct {
	ID                  int64
	RunID, Name, Model  string
	Iter                int
	StartedAt, EndedAt  int64
	Verdict             string
	TokensIn, TokensOut int
}

// NewSnapshot creates an empty snapshot.
func NewSnapshot() *Snapshot {
	return &Snapshot{
		Projects:         map[string]*ProjectView{},
		Tasks:            map[string]*TaskView{},
		Runs:             map[string]*RunView{},
		Stages:           map[int64]*StageView{},
		MaxRecentEvents:  200,
		pendingApprovals: map[string]*PendingApprovalView{},
		SequenceStatus:   map[string]*rpc.SeqStatusView{},
	}
}

// ApplySnapshot mutates s based on the event.
func ApplySnapshot(s *Snapshot, ev rpc.EventMessage) {
	// Record in ring buffer first so drill-in + Events tab can show the
	// firehose. Stamp arrival time (payloads carry no timestamp). Heartbeats are
	// EXCLUDED: at the 5s default cadence they fill the bounded 200-event ring in
	// ~17min and evict real run/stage/integration events, so the per-run/-task
	// events drill-in renders empty on a long run. The heartbeat is still handled
	// below (LastHeartbeat) for the daemon-liveness indicator.
	if s.MaxRecentEvents > 0 && ev.Type != rpc.EventDaemonHeartbeat {
		s.recentEvents = append(s.recentEvents, tabs.TimedEvent{
			At: time.Now(), Type: ev.Type, Data: ev.Data,
		})
		if len(s.recentEvents) > s.MaxRecentEvents {
			s.recentEvents = s.recentEvents[len(s.recentEvents)-s.MaxRecentEvents:]
		}
	}

	switch ev.Type {
	case rpc.EventRunStarted:
		runID, _ := ev.Data["run_id"].(string)
		if runID == "" {
			return
		}
		r := s.Runs[runID]
		if r == nil {
			r = &RunView{ID: runID}
			s.Runs[runID] = r
		}
		r.Status = "running"
		r.StartedAt = time.Now().Unix()
		if t, ok := ev.Data["task_id"].(string); ok {
			r.TaskID = t
		}
		if p, ok := ev.Data["pipeline"].(string); ok {
			r.Pipeline = p
		}
		// Phase 4.3.1 #4: child fix runs carry parent_run_id so the
		// Active tab can render them indented under their parent.
		if pr, ok := ev.Data["parent_run_id"].(string); ok {
			r.ParentRunID = pr
		}
		// Phase 3.7: auto-populate Task + Project from enriched event
		// so the Projects/Active tabs can render without a separate
		// task.list fetch (which excludes dispatched tasks).
		if title, ok := ev.Data["task_title"].(string); ok && r.TaskID != "" {
			if s.Tasks[r.TaskID] == nil {
				s.Tasks[r.TaskID] = &TaskView{ID: r.TaskID}
			}
			s.Tasks[r.TaskID].Title = title
		}
		// Phase 3.7.5: mark the task running so it leaves the Projects
		// "Queued" section (its run now represents it in the run rows).
		if r.TaskID != "" && s.Tasks[r.TaskID] != nil {
			s.Tasks[r.TaskID].Status = "running"
		}
		if pid, ok := ev.Data["project_id"].(string); ok && r.TaskID != "" {
			if s.Tasks[r.TaskID] == nil {
				s.Tasks[r.TaskID] = &TaskView{ID: r.TaskID}
			}
			s.Tasks[r.TaskID].ProjectID = pid
			if s.Projects[pid] == nil {
				slug, _ := ev.Data["project_slug"].(string)
				s.Projects[pid] = &ProjectView{ID: pid, Slug: slug, Name: slug}
			}
		}

	case rpc.EventRunEnded:
		runID, _ := ev.Data["run_id"].(string)
		if runID == "" {
			return
		}
		r := s.Runs[runID]
		if r == nil {
			r = &RunView{ID: runID}
			s.Runs[runID] = r
		}
		if st, ok := ev.Data["status"].(string); ok {
			r.Status = st
		}
		if sum, ok := ev.Data["summary"].(string); ok {
			r.Summary = sum
		}
		if skipped, ok := ev.Data["documentation_skipped"].(bool); ok && skipped {
			r.DocumentationSkipped = true
			r.DocumentationSkipReason, _ = ev.Data["documentation_skip_reason"].(string)
		}
		r.EndedAt = time.Now().Unix()
		// Reflect the run's terminal status on its task so the Projects
		// tab moves it out of "In progress": done -> done (shown as a
		// done run row), anything else (abandoned / needs_attention /
		// error) -> task goes needs_attention so it surfaces in the
		// re-runnable "Needs attention" lane.
		if r.TaskID != "" && s.Tasks[r.TaskID] != nil {
			if r.Status == "done" {
				s.Tasks[r.TaskID].Status = "done"
			} else {
				s.Tasks[r.TaskID].Status = "needs_attention"
			}
		}

	case rpc.EventRunUpdated:
		// Currently only carries documenter re-run results (Phase 4.4
		// follow-up `hive document`). Flip the chip on/off in place.
		runID, _ := ev.Data["run_id"].(string)
		if runID == "" {
			return
		}
		if r := s.Runs[runID]; r != nil {
			if skipped, ok := ev.Data["documentation_skipped"].(bool); ok {
				r.DocumentationSkipped = skipped
				r.DocumentationSkipReason, _ = ev.Data["documentation_skip_reason"].(string)
			}
		}

	case rpc.EventStageStarted:
		sid := asInt64(ev.Data["stage_id"])
		if sid == 0 {
			return
		}
		st := s.Stages[sid]
		if st == nil {
			st = &StageView{ID: sid}
			s.Stages[sid] = st
		}
		if rid, ok := ev.Data["run_id"].(string); ok {
			st.RunID = rid
		}
		if n, ok := ev.Data["name"].(string); ok {
			st.Name = n
		}
		if m, ok := ev.Data["model"].(string); ok {
			st.Model = m
		}
		st.Iter = int(asInt64(ev.Data["iter"]))
		st.StartedAt = time.Now().Unix()

	case rpc.EventStageEnded:
		sid := asInt64(ev.Data["stage_id"])
		if sid == 0 {
			return
		}
		st := s.Stages[sid]
		if st == nil {
			return
		}
		if v, ok := ev.Data["verdict"].(string); ok {
			st.Verdict = v
		}
		st.TokensIn = int(asInt64(ev.Data["tokens_in"]))
		st.TokensOut = int(asInt64(ev.Data["tokens_out"]))
		st.EndedAt = time.Now().Unix()

	case rpc.EventDaemonHeartbeat:
		s.LastHeartbeat = asInt64(ev.Data["ts"])

	case rpc.EventTaskCreated:
		// Phase 3.7.1c: hydrate Tasks (and Projects when needed) so
		// freshly-added tasks appear in the Projects tab queued list
		// without waiting for a re-fetch.
		taskID, _ := ev.Data["task_id"].(string)
		if taskID == "" {
			return
		}
		t := s.Tasks[taskID]
		if t == nil {
			t = &TaskView{ID: taskID}
			s.Tasks[taskID] = t
		}
		if title, ok := ev.Data["title"].(string); ok {
			t.Title = title
		}
		if st, ok := ev.Data["status"].(string); ok {
			t.Status = st
		}
		if ord := taskOrder(ev.Data["roadmap_phase"], ev.Data["roadmap_phase_index"]); ord != "" {
			t.Order = ord
		}
		if pid, ok := ev.Data["project_id"].(string); ok {
			t.ProjectID = pid
			if s.Projects[pid] == nil {
				slug, _ := ev.Data["project_slug"].(string)
				s.Projects[pid] = &ProjectView{ID: pid, Slug: slug, Name: slug}
			}
		}

	case rpc.EventTaskUpdated:
		// Phase 3.7.4: task edited or deleted. deleted=true removes it
		// from the snapshot; otherwise update the title in place.
		taskID, _ := ev.Data["task_id"].(string)
		if taskID == "" {
			return
		}
		if del, _ := ev.Data["deleted"].(bool); del {
			delete(s.Tasks, taskID)
			return
		}
		if t := s.Tasks[taskID]; t != nil {
			if title, ok := ev.Data["title"].(string); ok {
				t.Title = title
			}
			// A merge stamps roadmap phase on a previously-unordered task;
			// refresh the order label live when the event carries it.
			if ord := taskOrder(ev.Data["roadmap_phase"], ev.Data["roadmap_phase_index"]); ord != "" {
				t.Order = ord
			}
			if st, ok := ev.Data["status"].(string); ok {
				t.Status = st
				// Blocked inference: a task that transitions to needs_attention
				// while an integration is in flight surfaces as "blocked" so
				// the TUI can indicate a stuck finish-branch/PR chain (Task 7
				// merge-failure path).
				if st == "needs_attention" {
					switch t.IntegrationState {
					case "integrating", "pr_open", "ci":
						t.IntegrationState = "blocked"
					}
				}
			}
		}

	case rpc.EventTaskIntegrating:
		id, _ := ev.Data["task_id"].(string)
		if tv := s.Tasks[id]; tv != nil {
			tv.IntegrationState = "integrating"
		}

	case rpc.EventPROpened:
		id, _ := ev.Data["task_id"].(string)
		if tv := s.Tasks[id]; tv != nil {
			tv.IntegrationState = "pr_open"
			if u, ok := ev.Data["pr_url"].(string); ok {
				tv.PRURL = u
			}
			if n, ok := ev.Data["pr_number"].(float64); ok {
				tv.PRNumber = int(n)
			}
		}

	case rpc.EventTaskMerged:
		id, _ := ev.Data["task_id"].(string)
		if tv := s.Tasks[id]; tv != nil {
			tv.IntegrationState = "merged"
		}

	case rpc.EventProjectCreated:
		// Phase 8.C.2: hydrate Projects so a newly-added project appears
		// in the Projects-tab sidebar without waiting for a re-fetch.
		pid, _ := ev.Data["project_id"].(string)
		if pid == "" {
			return
		}
		if s.Projects[pid] == nil {
			s.Projects[pid] = &ProjectView{ID: pid}
		}
		p := s.Projects[pid]
		if slug, ok := ev.Data["slug"].(string); ok {
			p.Slug = slug
		}
		if name, ok := ev.Data["name"].(string); ok {
			p.Name = name
		}
		if rp, ok := ev.Data["repo_path"].(string); ok {
			p.RepoPath = rp
		}
		// Phase B: feature-branch integration fields (defensive; daemon
		// starts sending these in Tasks 11/13; reading-if-present is harmless).
		if fb, ok := ev.Data["feature_branch"].(string); ok {
			p.FeatureBranch = fb
		}
		if tb, ok := ev.Data["target_branch"].(string); ok {
			p.TargetBranch = tb
		}
		if ai, ok := ev.Data["task_auto_integrate"].(bool); ok {
			p.AutoIntegrate = ai
		}

	case rpc.EventProjectUpdated:
		// Phase 8.C.2: project edited or deleted. deleted=true removes
		// the project AND cascades to its tasks + runs (the daemon
		// cascades server-side; the snapshot mirrors that locally so the
		// Projects-tab right pane + Active tab don't reference a gone
		// project). Otherwise update name / repo_path in place.
		pid, _ := ev.Data["project_id"].(string)
		if pid == "" {
			return
		}
		if del, _ := ev.Data["deleted"].(bool); del {
			// Capture run IDs to delete BEFORE clearing tasks — runs link
			// to projects via Tasks[r.TaskID].ProjectID, so deleting tasks
			// first would break the join.
			runIDsToDelete := make([]string, 0)
			for rid, r := range s.Runs {
				if t, ok := s.Tasks[r.TaskID]; ok && t.ProjectID == pid {
					runIDsToDelete = append(runIDsToDelete, rid)
				}
			}
			for _, rid := range runIDsToDelete {
				delete(s.Runs, rid)
			}
			for tid, t := range s.Tasks {
				if t.ProjectID == pid {
					delete(s.Tasks, tid)
				}
			}
			delete(s.Projects, pid)
			return
		}
		if p := s.Projects[pid]; p != nil {
			if name, ok := ev.Data["name"].(string); ok && name != "" {
				p.Name = name
			}
			if rp, ok := ev.Data["repo_path"].(string); ok {
				p.RepoPath = rp
			}
			// Phase B: feature-branch integration fields (defensive; harmless
			// before Tasks 11/13 start sending them).
			if fb, ok := ev.Data["feature_branch"].(string); ok {
				p.FeatureBranch = fb
			}
			if tb, ok := ev.Data["target_branch"].(string); ok {
				p.TargetBranch = tb
			}
			if ai, ok := ev.Data["task_auto_integrate"].(bool); ok {
				p.AutoIntegrate = ai
			}
			// CanSequence is recomputed daemon-side (roadmap/spec gate) and
			// pushed here after a roadmap save so the edit modal's greyed
			// "sequenced" option un-greys live, without a TUI restart.
			if cs, ok := ev.Data["can_sequence"].(bool); ok {
				p.CanSequence = cs
			}
		}

	case rpc.EventApprovalRequested:
		s.ApplyPendingApproval(ev.Data)

	case rpc.EventApprovalResolved:
		if id, _ := ev.Data["approval_id"].(string); id != "" {
			delete(s.pendingApprovals, id)
		}
	}
}

// ApplyPendingApproval records a pending approval from an
// approval.requested event OR a daemon.status pending_approvals_list
// entry (both carry approval_id/run_id/stage/tool_name/tool_input).
func (s *Snapshot) ApplyPendingApproval(data map[string]any) {
	id, _ := data["approval_id"].(string)
	if id == "" {
		return
	}
	pa := &PendingApprovalView{ID: id}
	pa.RunID, _ = data["run_id"].(string)
	pa.Stage, _ = data["stage"].(string)
	pa.ToolName, _ = data["tool_name"].(string)
	if ti, ok := data["tool_input"].(map[string]any); ok {
		pa.Arg = canonicalArgFromInput(pa.ToolName, ti)
	}
	s.pendingApprovals[id] = pa
}

// canonicalArgFromInput extracts the display arg (Bash command, file
// path) from a tool input map for the Approvals tab.
func canonicalArgFromInput(toolName string, in map[string]any) string {
	switch toolName {
	case "Bash":
		if c, ok := in["command"].(string); ok {
			return c
		}
	case "Edit", "Write", "Read", "MultiEdit":
		if f, ok := in["file_path"].(string); ok {
			return f
		}
	}
	return ""
}

// PendingApprovals returns pending approvals sorted by ID (stable render),
// with a display tier computed from the tool.
func (s *Snapshot) PendingApprovals() []tabs.PendingApproval {
	out := make([]tabs.PendingApproval, 0, len(s.pendingApprovals))
	for _, pa := range s.pendingApprovals {
		title := pa.RunID
		if r := s.Runs[pa.RunID]; r != nil {
			if t := s.Tasks[r.TaskID]; t != nil && t.Title != "" {
				title = t.Title
			}
		}
		out = append(out, tabs.PendingApproval{
			ApprovalID: pa.ID, RunID: pa.RunID, TaskTitle: title, Stage: pa.Stage,
			ToolName: pa.ToolName, Arg: pa.Arg, Tier: tierFor(pa.ToolName),
		})
	}
	// Group by task (title), then by approval id within a task — stable
	// render that clusters a task's pending tools together.
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskTitle != out[j].TaskTitle {
			return out[i].TaskTitle < out[j].TaskTitle
		}
		return out[i].ApprovalID < out[j].ApprovalID
	})
	return out
}

// PendingApprovalRunIDs returns the set of run IDs that currently have a
// pending approval — used to flag those runs/tasks in other tabs.
func (s *Snapshot) PendingApprovalRunIDs() map[string]bool {
	out := make(map[string]bool, len(s.pendingApprovals))
	for _, pa := range s.pendingApprovals {
		if pa.RunID != "" {
			out[pa.RunID] = true
		}
	}
	return out
}

func tierFor(toolName string) string {
	switch toolName {
	case "Read", "Grep", "Glob":
		return "trivial"
	case "Edit", "Write", "MultiEdit":
		return "code"
	case "Bash":
		return "bash"
	}
	return "other"
}

// asInt64 normalizes numeric fields in event Data. JSON unmarshal yields
// float64 for numbers; TUI consumers want int64. Returns 0 on missing/
// unparseable.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// --- Snapshot accessors used by the tabs as their "snapshot reader" interface. ---

// RecentEvents returns the ring buffer of recently observed events as
// timed entries. Used by the Events tab (timestamps + filtering).
func (s *Snapshot) RecentEvents() []tabs.TimedEvent { return s.recentEvents }

// AllProjects returns project summaries for the Projects tab sidebar.
// Sorted by Name for stable rendering.
func (s *Snapshot) AllProjects() []tabs.ProjectSummary {
	out := make([]tabs.ProjectSummary, 0, len(s.Projects))
	for _, p := range s.Projects {
		out = append(out, tabs.ProjectSummary{
			ID: p.ID, Slug: p.Slug, Name: p.Name, RepoPath: p.RepoPath,
			Status:        p.Status,
			DispatchMode:  p.DispatchMode,
			FeatureBranch: p.FeatureBranch, TargetBranch: p.TargetBranch, AutoIntegrate: p.AutoIntegrate,
			MergeMethod: p.MergeMethod, AutoFixCI: p.AutoFixCI,
			CanSequence: p.CanSequence,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SequenceStatus returns the cached sequence.status view for a project, or nil
// if the project isn't sequenced / hasn't been fetched yet. Implements the
// Projects tab's ProjectsReader contract.
func (s *Snapshot) SequenceStatusFor(projectID string) *rpc.SeqStatusView {
	if s.SequenceStatus == nil {
		return nil
	}
	return s.SequenceStatus[projectID]
}

// RunsForProject returns the runs belonging to the given project ID.
// Resolves project via the run's task → task's ProjectID. Output is
// sorted by ID for stable rendering (3.7.1 fix: map iteration was
// reordering rows on each render).
func (s *Snapshot) RunsForProject(projectID string) []tabs.RunSummary {
	out := make([]tabs.RunSummary, 0)
	for _, r := range s.Runs {
		if t, ok := s.Tasks[r.TaskID]; ok && t.ProjectID == projectID {
			out = append(out, tabs.RunSummary{
				ID: r.ID, TaskID: r.TaskID, TaskTitle: t.Title,
				Status: r.Status, Summary: r.Summary, Pipeline: r.Pipeline,
				EndedAt: r.EndedAt,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// TasksForProject returns pending tasks for the given project. Sorted
// by ID for stable rendering.
func (s *Snapshot) TasksForProject(projectID string) []tabs.TaskSummary {
	out := make([]tabs.TaskSummary, 0)
	for _, t := range s.Tasks {
		if t.ProjectID == projectID {
			out = append(out, tabs.TaskSummary{
				ID: t.ID, Title: t.Title, Status: t.Status, Order: t.Order,
				IntegrationState: t.IntegrationState, PRNumber: t.PRNumber,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ActiveRuns returns rows for the Active tab — every run in running
// status, with the latest stage's name + iter as the "current stage."
func (s *Snapshot) ActiveRuns() []tabs.ActiveRunSummary {
	out := make([]tabs.ActiveRunSummary, 0)
	for _, r := range s.Runs {
		if r.Status != "running" {
			continue
		}
		var title, stageName, projectSlug string
		var iter int
		if t, ok := s.Tasks[r.TaskID]; ok {
			title = t.Title
			if p, ok := s.Projects[t.ProjectID]; ok {
				projectSlug = p.Slug
			}
		}
		var latest *StageView
		for _, st := range s.Stages {
			if st.RunID != r.ID {
				continue
			}
			if latest == nil || st.StartedAt > latest.StartedAt {
				latest = st
			}
		}
		if latest != nil {
			stageName = latest.Name
			iter = latest.Iter
		}
		out = append(out, tabs.ActiveRunSummary{
			ID: r.ID, Project: projectSlug, TaskTitle: title,
			Pipeline: r.Pipeline, Status: r.Status,
			Stage: stageName, Iter: iter,
			ParentRunID: r.ParentRunID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
