// Package tabs holds the per-tab sub-models for Hive's TUI. The root
// app composes one of each tab via the TabModel interface; root's
// Update routes messages to the active tab.
package tabs

import (
	"encoding/json"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// TimedEvent is a streamed event plus its TUI-arrival time. The Events
// tab renders the timestamp; the snapshot stamps At on receipt (event
// payloads themselves carry no timestamp).
type TimedEvent struct {
	At   time.Time
	Type rpc.EventType
	Data map[string]any
}

// TabModel is the contract each tab implements. Sub-models hold their
// own state; the root model holds a slice of TabModel and dispatches
// to the active one.
//
// Name returns a short display name for the tab bar.
// KeyHelp returns a one-line summary of tab-specific keybindings for
// the status line (Phase 3.7.1 polish so users discover N/n/d/r/etc).
type TabModel interface {
	Name() string
	Init() tea.Cmd
	Update(msg tea.Msg) (TabModel, tea.Cmd)
	View() string
	KeyHelp() string
}

// DrillInRequest is emitted by tabs when the user selects a run for
// drill-in. Root model receives this (via tea.Cmd return from the
// tab's Update) and switches view.
type DrillInRequest struct{ RunID string }

// TabOpenModalRequest is emitted by a tab when the user presses a key
// that should open a modal (e.g., n/N on Projects for new task/project;
// d on Active for abandon-confirm). Root model constructs the matching
// modal via constructModal.
type TabOpenModalRequest struct {
	Kind         string
	InitialState map[string]any
}

// TabRunNowRequest is emitted by tabs when the user wants to dispatch
// a pending task (Enter on a queued task in Projects). Root model
// fires the run.now RPC.
type TabRunNowRequest struct{ TaskID string }

// ProjectSummary is the minimal info Projects tab renders per row.
// Lives in the tabs package so per-tab files import only what they
// need; the parent tui package converts from its own internal Snapshot
// to these summary types.
//
// RepoPath is needed by the 8.C.2 modal flows (Edit Project pre-fill,
// Roadmap Viewer file path) so the Projects tab can hand it to root
// alongside the slug without re-fetching project.list.
type ProjectSummary struct {
	ID, Slug, Name, RepoPath string
	// Status is the lifecycle status (active/paused/archived); seeds the edit
	// modal's status selector so a save round-trips it.
	Status string
	// DispatchMode is the resolved per-project dispatch mode
	// (manual/auto_all/sequenced); drives the sequenced row badge + seeds
	// the edit modal's selector (Phase 4).
	DispatchMode string
	// Feature-branch integration fields (Phase B). FeatureBranch/TargetBranch
	// are set when the project has branch integration configured;
	// AutoIntegrate enables the auto-merge badge in the Projects tab header.
	FeatureBranch, TargetBranch string
	AutoIntegrate               bool
	MergeMethod                 string
	AutoFixCI                   bool
	// CanSequence reports whether the roadmap/spec gate passes — seeds the
	// edit modal's ability to select the "sequenced" dispatch mode.
	CanSequence bool
}

// RunSummary is one row in the per-project section.
type RunSummary struct {
	ID, TaskID, TaskTitle, Status, Summary, Pipeline string
	EndedAt                                          int64 // Unix seconds; 0 if unknown
}

// TaskSummary is one row in the per-project pending section.
// Order is a compact roadmap order label ("P.I", e.g. "1.2") for
// roadmap-decomposed tasks; "" for tasks with no phase linkage.
type TaskSummary struct {
	ID, Title, Status, Order string
	// IntegrationState tracks where the task sits in the feature-branch
	// integration lifecycle: "" | "integrating" | "pr_open" | "ci" |
	// "merged" | "blocked". Drives the chip in the Projects task row.
	IntegrationState string
	// PRNumber is the GitHub PR number when IntegrationState is "pr_open".
	PRNumber int
}

// PendingApproval is one row in the Approvals inbox (Phase 4.6).
type PendingApproval struct {
	ApprovalID, RunID, TaskTitle, Stage, ToolName, Arg, Tier string
}

// TabApprovalResolveRequest is emitted by the Approvals tab to resolve a
// pending approval (root forwards it to the daemon via approval.resolve).
type TabApprovalResolveRequest struct {
	ApprovalID string
	Decision   string // "approve" | "deny"
	Remember   bool
	ToolName   string
	ArgMatcher string
}

// ActiveRunSummary is one row in the Active tab list.
type ActiveRunSummary struct {
	ID        string
	Project   string // Phase 3.7: project slug
	TaskTitle string
	Pipeline  string
	Status    string
	Stage     string
	Iter      int
	CostUSD   float64
	// ParentRunID is set on child fix runs (Phase 4.3.1 #4). When
	// non-empty, the row renders indented under its parent — see
	// groupRunsTreeOrder + the active tab View().
	ParentRunID string
}

// ChatFrameMsg is one streaming frame the daemon emitted on a chat.send RPC.
// Dispatched by Client.StreamChat goroutine into the Bubbletea program; the
// root forwards it directly to the chat tab regardless of active-tab focus.
type ChatFrameMsg struct {
	Frame chat.Frame
}

// ChatStreamStartedMsg signals the chat.send stream goroutine successfully
// connected (the first frame, typically the "session" frame, may already
// have been delivered separately as a ChatFrameMsg).
type ChatStreamStartedMsg struct{}

// ChatStreamEndedMsg signals the chat.send stream goroutine exited. Err is
// nil on clean turn_done; non-nil on transport or RPC error.
type ChatStreamEndedMsg struct{ Err error }

// TabChatSendRequest is the chat tab → root request to open a new chat.send
// stream for the given user message. SessionID empty means "new session".
type TabChatSendRequest struct {
	Message   string
	SessionID string
}

// TabChatConfirmRequest is the chat tab → root request to send a chat.confirm
// RPC resolving a pending tool_proposed by id.
//
// Reason is set on `c` (cancel) and overrides the default deny content
// the model sees. EditedInput is set on `e` (edit args via modal); when
// non-nil the tool runs with the edited args and the result content
// carries a [user edited args before running] prefix.
type TabChatConfirmRequest struct {
	SessionID   string
	ToolCallID  string
	Approve     bool
	Reason      string
	EditedInput json.RawMessage
}

// TabPlannerOpenRequest is the Projects tab → root request to open a
// planner-mode chat session for the named project. The root handler:
//  1. Resets the chat tab (clears sessionID, frames, pending confirms)
//     so the planner session starts clean even if a previous session
//     was active.
//  2. Switches the active tab to Chat.
//  3. Fires Client.StreamPlannerChat(slug) which opens a chat.send
//     stream with kind="plan" + project_slug; the daemon's planner
//     pipeline picks up the kind/slug and seeds the planner system
//     prompt. The first frame back is the "session" frame carrying
//     the fresh sessionID.
//
// Wired to the P keybind on the Projects tab — the TUI mirror of
// `hive plan <slug>` on the CLI.
type TabPlannerOpenRequest struct {
	ProjectSlug string
}

// TabEditProjectRequest is the Projects tab → root request to open the
// Edit Project modal pre-filled with the selected project's current
// values. The root handler constructs modals.NewEditProjectModal; submit
// flows through handleModalSubmit → Client.EditProject (project.edit RPC).
//
// Slug is the immutable identifier; Name + RepoPath are the editable
// fields seeded into the modal's text inputs.
// TabSequenceRequest is the Projects tab → root request to open the Sequence
// modal for a sequenced project (q keybind). The root seeds the modal from the
// cached sequence.status for ProjectID.
type TabSequenceRequest struct {
	Slug      string
	ProjectID string
}

// TabRepoConfigRequest is the Projects tab → root request to open the read-only
// Repo Config viewer for the selected project's repo. The root resolves the
// ~/.hive/repos/<RepoKey(RepoPath)>/config.toml path (config.RepoKey) and hands
// it to the modal, which reads + displays it. RepoPath is required to resolve
// the key (empty → the modal shows a "no repo path" notice).
type TabRepoConfigRequest struct {
	Slug     string
	RepoPath string
}

type TabEditProjectRequest struct {
	Slug     string
	Name     string
	RepoPath string
	// Status is the project's current lifecycle status (active/paused/archived),
	// seeding the edit modal's status selector so a save round-trips it (and lets
	// the operator archive/unarchive in-modal).
	Status string
	// DispatchMode is the project's current resolved mode (manual/auto_all/
	// sequenced) used to seed the modal's selector. Target/Policy seed the
	// sequenced-only fields (empty when the project isn't sequenced).
	DispatchMode string
	Target       string
	Policy       string

	// Integration settings ([integration] block) seeded into the modal so
	// they round-trip on save. FeatureBranch is the per-project feature
	// branch; MergeMethod is merge/squash/rebase; TaskAutoIntegrate /
	// AutoFixCI are the two integration toggles.
	FeatureBranch     string
	MergeMethod       string
	TaskAutoIntegrate bool
	AutoFixCI         bool

	// CanSequence reports whether the project's roadmap/spec gate currently
	// passes — controls whether the modal lets the operator select the
	// "sequenced" dispatch mode (greyed + skipped when false).
	CanSequence bool
}

// TabDeleteProjectRequest is the Projects tab → root request to open the
// Delete Project confirm modal. The modal requires the operator to type
// the slug exactly to enable y (8.C.2 destructive-action convention) and
// shows TaskCount + RunCount so the cascading scope is visible BEFORE
// the destructive action. Counts are computed by the tab from the
// snapshot so the modal doesn't need its own daemon round-trip.
type TabDeleteProjectRequest struct {
	Slug      string
	TaskCount int
	RunCount  int
}

// TabSourcesRequest is the Projects tab → root request to open the
// Sources modal for the selected project. The root opens the modal AND
// kicks off the initial sources.list fetch — the modal starts in a
// "loading" state, then transitions to "list" once the RPC response
// arrives as an RPCResultMsg. Bind/unbind submissions flow through
// handleModalSubmit → Client.SourcesBind / SourcesUnbind.
type TabSourcesRequest struct {
	ProjectSlug string
}

// TabRoadmapViewerRequest is the Projects tab → root request to open the
// Roadmap Viewer modal for the selected project. The modal reads + parses
// `<RepoPath>/docs/superpowers/roadmaps/<ProjectSlug>.md` synchronously on
// construction (errors render inline); D on a phase triggers the
// roadmap.decompose RPC + DecomposeConfirmModal transition handled by root.
//
// RepoPath is passed alongside ProjectSlug because the viewer needs to
// resolve the on-disk roadmap path without a fresh project.list lookup.
// 8.C.2 T1's ProjectSummary.RepoPath made this round-trip-free.
type TabRoadmapViewerRequest struct {
	ProjectSlug string
	RepoPath    string
}

// TabFeatureBranchHealthRequest opens the feature-branch health modal for a
// project (Projects tab 'H' key). Only emitted for projects with a feature branch.
type TabFeatureBranchHealthRequest struct {
	Slug     string
	RepoPath string
	Feature  string
	Target   string
}

// TabResolveTaskRequest is emitted by the Projects tab when the operator
// presses C on a needs_attention task. Root fires the resolve.now RPC
// (manual conflict-resolver trigger) for that task. Fire-and-forget — the
// resolver runs asynchronously on the daemon; results surface via the
// normal run.* event stream.
type TabResolveTaskRequest struct {
	TaskID string
}

// TabMergeRetryRequest is emitted by the Projects tab when the operator presses
// M on a task parked at merge_failed — re-attempt/recover via merge.retry.
type TabMergeRetryRequest struct {
	TaskID string
}

// TabDoctorRequest is the Active tab → root request to open the Doctor
// modal (TUI mirror of `hive doctor`). It carries no params — doctor is a
// daemon-global audit, not scoped to a project — so the root constructs the
// modal in its "running" state and fires doctor.Run off the UI thread (the
// checks do blocking RPC + local file/db/worktree reads). Wired to the D
// keybind on the Active tab.
type TabDoctorRequest struct{}

// TabCleanRequest is the Active tab → root request to open the Clean modal
// (TUI mirror of `hive clean`). Like Doctor it carries no params — cleanup is
// a daemon-global GC, not project-scoped — so the root opens the modal in its
// "preview" state and fires a DRY-RUN cleanup.run to populate what would be
// reclaimed. Wired to the x keybind on the Active tab.
type TabCleanRequest struct{}

// OpenChatSessionPickerMsg is the global Ctrl-K → root request to open the
// session picker modal.
type OpenChatSessionPickerMsg struct{}

// ResumeChatSessionMsg is the picker → root request to switch the active tab
// to chat and seed it with the chosen sessionID.
type ResumeChatSessionMsg struct{ SessionID string }

// OpenChatRenameMsg is the chat tab → root request to open the rename modal
// for the given session. CurrentName is the current display name (used as
// the textinput's initial value).
type OpenChatRenameMsg struct {
	SessionID   string
	CurrentName string
}

// ChatToolResultRow describes one tool_result frame from the chat
// session, used by OpenChatToolResultPickerMsg. The modal (in package
// modals) defines its own equivalent type; the root converts.
type ChatToolResultRow struct {
	Tool    string
	Result  string
	IsError bool
}

// OpenChatToolResultPickerMsg is the chat tab → root request to open
// the ChatToolResultPicker modal with the session's tool_result frames
// most-recent-first. The root converts to modals.ChatToolResultRow
// when constructing the modal.
type OpenChatToolResultPickerMsg struct {
	Rows []ChatToolResultRow
}

// OpenChatEditArgsMsg is the chat tab → root request to open the
// ChatEditArgsModal pre-filled with the named tool's current args.
// Mirrors OpenChatRenameMsg's pattern. The root constructs the modal
// using these fields and dispatches it.
type OpenChatEditArgsMsg struct {
	ToolCallID string
	ToolName   string
	Args       json.RawMessage
}

// ChatHistoryLoadedMsg is dispatched by root after a session picker resume
// (or any other "switch to existing session" path) so ChatTab can rebuild
// its visible history from the loaded messages.
//
// Session metadata (Name/Provider/TotalCostUSD) is included so the chat
// tab's metadata bar reflects the resumed session — otherwise the bar
// shows stale data from whatever session last emitted session_info /
// turn_done frames (state-bleed bug found 2026-06-01).
type ChatHistoryLoadedMsg struct {
	SessionID    string
	Name         string
	Provider     string
	TotalCostUSD float64
	Messages     []ChatHistoryMessage
}

// ChatHistoryMessage is the abstract form ChatTab consumes — root converts
// the wire-shape ChatMessageRow into this so tabs/ doesn't need to import
// the parent tui package for ChatMessageRow's type.
type ChatHistoryMessage struct {
	Role    string // "user" | "assistant" | "tool" | "error"
	Content string
	// ToolName lets tool_result frames render with a label when available.
	// Empty for non-tool messages.
	ToolName string
}
