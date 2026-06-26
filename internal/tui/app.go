package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/doctor"
	"github.com/rohilrs/Hive/internal/tui/modals"
	"github.com/rohilrs/Hive/internal/tui/style"
	"github.com/rohilrs/Hive/internal/tui/tabs"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// rootChromeBase is the minimum vertical budget the root reserves:
// tab bar (1) + blank separator (1) + global status (1) + per-tab
// key-help (1). The legend adds 2 rows when height >= 24, and the
// daemon-down banner adds 2 rows when shown. rootChromeRowsFor
// computes the total so tabs + modals sized to
// height - rootChromeRowsFor(...) don't clip the root chrome.
const rootChromeBase = 4

// rootChromeRowsFor returns the chrome budget given the current
// terminal height + whether the daemon-down banner is visible. Tabs
// that fill available height (notably Chat) MUST account for both,
// since the banner pushes content down by 2 rows when it appears.
func rootChromeRowsFor(height int, daemonDown bool) int {
	rows := rootChromeBase
	if height >= 24 {
		rows += 2 // status legend + its blank
	}
	if daemonDown {
		rows += 2 // ⚠ DAEMON DOWN banner + trailing newline
	}
	return rows
}

// refreshTabSizes forwards an adjusted WindowSizeMsg to every tab + the
// open modal so their layouts re-budget against current chrome. Called
// on real WindowSizeMsg AND whenever m.daemonDown flips (the banner
// adds 2 rows of chrome so tabs that fill height — Chat — need to
// re-flow).
func (m *Model) refreshTabSizes() {
	if m.width == 0 || m.height == 0 {
		return
	}
	adjusted := tea.WindowSizeMsg{
		Width:  m.width,
		Height: m.height - rootChromeRowsFor(m.height, m.daemonDown),
	}
	if adjusted.Height < 1 {
		adjusted.Height = 1
	}
	for i, t := range m.tabs {
		t, _ = t.Update(adjusted)
		m.tabs[i] = t
	}
	if m.modal != nil {
		upd, _ := m.modal.Update(adjusted)
		m.modal = upd
	}
}

// Model is the root TUI model. Holds the snapshot + per-tab sub-models
// + global UI state (current tab, drill-in run, daemon-down flag,
// window size).
type Model struct {
	Client *Client

	snapshot      *Snapshot
	tabs          []tabs.TabModel
	currentTab    int
	drillInRunID  string // empty = not in drill-in
	drillScroll   int    // drill-in events scroll offset (rows back from newest; 0 = newest)
	daemonDown    bool
	width, height int

	// heartbeatTimeoutSeconds: how stale LastHeartbeat must be before
	// we mark daemon-down. Default 15s (3× the 5s heartbeat cadence).
	heartbeatTimeoutSeconds int64

	// Phase 3.7: optional modal-overlay. When non-nil, root routes all
	// key events to the modal (modal handles esc-to-cancel + enter-to-
	// submit) and renders the modal instead of the active tab content.
	modal modals.Modal

	// pendingDecompose maps a decompose_id (from RoadmapDecomposeStart) to the
	// modal-construction context captured when the operator pressed D, so the
	// later decompose.proposed event can build the DecomposeConfirmModal with
	// full context. Cleared on proposed/failed (and on a daemon reconnect,
	// which drops the daemon-side in-flight job).
	pendingDecompose map[string]map[string]any

	// pendingGraduate maps a graduate_id WE started (from ProjectGraduateStart)
	// to its project slug, so the graduate.* event handlers only forward events
	// for OUR graduation to the open GraduateModal — a concurrent CLI/other-TUI
	// graduate doesn't hijack this modal — AND so a later G for that project can
	// detect the in-flight run and re-attach (re-open the modal in its running
	// state) instead of starting a fresh confirm. Entries are cleared on
	// graduate.done / graduate.failed (and on a daemon reconnect, which drops the
	// daemon-side in-flight job).
	pendingGraduate map[string]string
}

// NewModel constructs the root. tabModels are the per-tab models in
// display order (Projects, Active, ...). The snapshot is shared by
// reference — tabs read via getters on Snapshot.
func NewModel(client *Client, snapshot *Snapshot, tabModels []tabs.TabModel) *Model {
	return &Model{
		Client:                  client,
		snapshot:                snapshot,
		tabs:                    tabModels,
		heartbeatTimeoutSeconds: 15,
		pendingDecompose:        map[string]map[string]any{},
		pendingGraduate:         map[string]string{},
	}
}

// Snapshot returns the read-only snapshot pointer. Tabs use this to
// render. Concurrent reads are safe — only the root model mutates via
// ApplySnapshot (single-goroutine Bubbletea Update loop).
func (m *Model) Snapshot() *Snapshot { return m.snapshot }

// Init kicks off initial state fetch + heartbeat ticker.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tea.Tick(1*time.Second, func(t time.Time) tea.Msg { return heartbeatTickMsg(t) }),
	}
	if m.Client != nil {
		cmds = append(cmds, func() tea.Msg { return m.Client.FetchInitialState() })
	}
	for _, t := range m.tabs {
		if c := t.Init(); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

// Update is the root dispatch.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Phase 3.7: when a modal is open, it has dispatch priority for
	// keys + RPC result forwarding. Special msg types (close, submit
	// request, RPC result) flow through this branch.
	if m.modal != nil {
		switch v := msg.(type) {
		case tea.KeyMsg:
			// Global escape hatches that always work, even if a modal
			// has buggy/incomplete key handling — guarantees the user
			// can never get trapped (3.7.5 fix).
			switch v.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "ctrl+x":
				// Force-close the modal regardless of its internal state.
				m.modal = nil
				return m, nil
			}
			// Otherwise forward to the modal — modals own their close
			// logic (esc → CloseMsg; multi-state modals step back).
			upd, cmd := m.modal.Update(v)
			m.modal = upd
			return m, cmd
		case modals.CloseMsg:
			m.modal = nil
			return m, nil
		case modals.SubmitRequest:
			return m, m.handleModalSubmit(v)
		case rpcResultMsg:
			// A successful abandon exits any drill-in overlay so we land
			// back on the tab the drill-in was opened from (Projects or
			// Active). Abandoning from the Active tab without drilling in
			// leaves drillInRunID empty, so this is a harmless no-op there.
			if v.Kind == "abandon_run" && v.Err == nil {
				m.drillInRunID = ""
			}
			// roadmap_decompose_open is now an ASYNC start: the
			// RoadmapViewerModal emits the SubmitRequest, handleModalSubmit
			// fires RoadmapDecomposeStart, and the *ack* (decompose_id +
			// modal context, under Data["_decomposing"]) arrives here. We
			// stash the context under the id in m.pendingDecompose (done on
			// this Update goroutine to avoid a race with the event path) and
			// forward the "decomposing…" signal to the still-open viewer. The
			// DecomposeConfirmModal is built later, when the decompose.proposed
			// event lands (see the eventMsg branch). On a start error we
			// forward it to the viewer so it renders inline and the operator
			// can re-press D.
			// project_graduate_open is an ASYNC start: the GraduateModal emits the
			// SubmitRequest, handleModalSubmit fires ProjectGraduateStart, and the
			// *ack* (graduate_id under Data["graduate_id"]) arrives here. We record
			// the id in m.pendingGraduate on THIS Update goroutine (no race with the
			// event path), then forward a "_graduate_started" signal to the modal so
			// it can render the start error (if any) or keep spinning. The verdict /
			// done / failed lifecycle is forwarded later by the graduate.* event
			// handlers, keyed by the id we just recorded.
			if v.Kind == "project_graduate_open" {
				if v.Err == nil {
					if id, _ := v.Data["graduate_id"].(string); id != "" {
						// Map id→slug (slug threaded from handleModalSubmit's start
						// case) so a later G for this project can re-attach.
						slug, _ := v.Data["project_slug"].(string)
						m.pendingGraduate[id] = slug
					}
				}
				// Guard: the user may have esc-closed the modal in the sub-ms
				// window before this ack landed.
				if m.modal != nil {
					upd, cmd := m.modal.Update(modals.RPCResultMsg{
						Kind: "project_graduate_open",
						Err:  v.Err,
						Data: map[string]any{"_graduate_started": true},
					})
					m.modal = upd
					return m, cmd
				}
				return m, nil
			}
			if v.Kind == "roadmap_decompose_open" {
				if v.Err == nil {
					if id, _ := v.Data["decompose_id"].(string); id != "" {
						ctxData := map[string]any{}
						for k, val := range v.Data {
							if strings.HasPrefix(k, "__") {
								ctxData[k] = val
							}
						}
						m.pendingDecompose[id] = ctxData
					}
				}
				upd, cmd := m.modal.Update(modals.RPCResultMsg{
					Kind: "roadmap_decompose_open", Err: v.Err, Data: v.Data,
				})
				m.modal = upd
				return m, cmd
			}
			upd, cmd := m.modal.Update(modals.RPCResultMsg{
				Kind: v.Kind, Err: v.Err, Data: v.Data,
			})
			m.modal = upd
			// A successful project create/edit may have changed dispatch_mode
			// (and enabled/disabled sequencing). Refresh initial state so the
			// snapshot's ProjectView.DispatchMode + SequenceStatus reflect it —
			// the row badge + S keybind gate on DispatchMode, which only
			// project.list carries (config-resolved, not event-driven).
			if (v.Kind == "new_project" || v.Kind == "edit_project") && v.Err == nil {
				return m, tea.Batch(cmd, func() tea.Msg { return m.Client.FetchInitialState() })
			}
			return m, cmd
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.refreshTabSizes()
		return m, nil

	case tea.MouseMsg:
		// Forward mouse events (notably wheel) to the active tab. The
		// outer-switch fall-through at the end of Update would otherwise
		// silently drop them, which is how scroll wheel was broken in
		// the chat tab despite the tab's handler being correct.
		//
		// Gated on no-modal: when the picker (or any modal) is open the
		// tab is covered, so wheel events should not scroll the
		// invisible-but-still-active chat history underneath. No modal
		// today consumes wheel; if one wants to (e.g. picker scrolling
		// a long session list), forward through m.modal.Update here.
		if m.modal != nil {
			return m, nil
		}
		if len(m.tabs) > 0 {
			active, cmd := m.tabs[m.currentTab].Update(msg)
			m.tabs[m.currentTab] = active
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+k" && m.modal == nil {
			m.modal = modals.NewChatSessionPicker()
			return m, m.modal.Init()
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			// `q` quits — EXCEPT on the chat tab, where it must type into the
			// input (text inputs need printable keys; same rationale as the
			// ctrl+N tab-jump that keeps digits flowing to the chat textinput).
			// ctrl+c stays the universal quit. Not returning here lets `q` fall
			// through to the active-tab delegation at the end of this switch.
			// (Dogfood fix)
			if m.currentTab != m.findChatTabIndex() {
				return m, tea.Quit
			}
		case "tab", "right":
			m.currentTab = (m.currentTab + 1) % len(m.tabs)
			return m, nil
		case "shift+tab", "left":
			m.currentTab = (m.currentTab - 1 + len(m.tabs)) % len(m.tabs)
			return m, nil
		case "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5",
			"ctrl+6", "ctrl+7", "ctrl+8", "ctrl+9",
			"alt+1", "alt+2", "alt+3", "alt+4", "alt+5",
			"alt+6", "alt+7", "alt+8", "alt+9":
			// Use ctrl+N or alt+N for tab jumping so bare digits flow through to
			// the active tab's textinput (e.g. the chat input). Both modifiers
			// are handled because terminal support for Ctrl+digit varies — some
			// emulators don't distinguish Ctrl+1 from bare 1.
			s := msg.String()
			digit := s[len(s)-1] // last byte is always the digit rune
			idx := int(digit - '1')
			if idx >= 0 && idx < len(m.tabs) {
				m.currentTab = idx
			}
			return m, nil
		case "esc":
			if m.drillInRunID != "" {
				m.drillInRunID = ""
				m.drillScroll = 0
				return m, nil
			}
		case "up", "k":
			// Scroll the drill-in events box back (older). Clamp to
			// max(0, events − visibleRows) so there's no scroll when they
			// fit (fixes the last-row-disappears bug).
			if m.drillInRunID != "" {
				n := len(filterEventsForRun(m.snapshot.recentEvents, m.drillInRunID))
				maxOff := n - drillEventRows(m.height)
				if maxOff < 0 {
					maxOff = 0
				}
				if m.drillScroll < maxOff {
					m.drillScroll++
				}
				return m, nil
			}
		case "down", "j":
			if m.drillInRunID != "" {
				if m.drillScroll > 0 {
					m.drillScroll--
				}
				return m, nil
			}
		case "a", "A", "d", "D":
			// Resolve the next pending approval for the drilled-in run
			// (a/A approve(+remember), d/D deny(+remember)). No-op when
			// none pending. Outside drill-in, fall through to the tab.
			if m.drillInRunID != "" {
				return m, m.resolveDrillApproval(msg.String())
			}
		case "x":
			// Abandon the drilled-in run (moved from `d`, now deny).
			if m.drillInRunID != "" {
				m.modal = constructModal("confirm_abandon", map[string]any{"run_id": m.drillInRunID})
				if m.modal == nil {
					return m, nil
				}
				return m, m.modal.Init()
			}
		}
		active, cmd := m.tabs[m.currentTab].Update(msg)
		m.tabs[m.currentTab] = active
		return m, cmd

	case eventMsg:
		ApplySnapshot(m.snapshot, msg.Event)
		active, cmd := m.tabs[m.currentTab].Update(msg)
		m.tabs[m.currentTab] = active
		// On sequence.* events, refetch the affected project's
		// sequence.status so the cache (and any open Sequence modal /
		// Projects tab) reflects the new gate / phase / pause state. The
		// fetch is a side-effecting RPC, so it MUST run inside a tea.Cmd —
		// never here in Update. Batch it with the tab-update cmd.
		switch msg.Event.Type {
		case rpc.EventSequenceCreated, rpc.EventSequenceUpdated, rpc.EventSequenceGateChanged:
			if pid, slug, ok := m.seqEventProject(msg.Event); ok && m.Client != nil {
				refetch := func() tea.Msg {
					v, err := m.Client.SequenceStatus(slug)
					if err != nil {
						return nil
					}
					return sequenceStatusMsg{ProjectID: pid, View: v}
				}
				return m, tea.Batch(cmd, refetch)
			}

		// Async roadmap.decompose lifecycle. These events arrive on the
		// persistent stream (daemon broadcast). We only act on events for an
		// id WE started (present in m.pendingDecompose) so a concurrent
		// CLI/other-TUI decompose doesn't hijack this viewer.
		case rpc.EventDecomposeProgress:
			id, _ := msg.Event.Data["decompose_id"].(string)
			if _, ok := m.pendingDecompose[id]; ok {
				label, _ := msg.Event.Data["phase_label"].(string)
				if upd, pcmd := m.forwardDecomposeProgress(id, label); upd != nil {
					m.modal = upd
					return m, pcmd
				}
			}

		case rpc.EventDecomposeProposed:
			id, _ := msg.Event.Data["decompose_id"].(string)
			if ctxData, ok := m.pendingDecompose[id]; ok {
				delete(m.pendingDecompose, id)
				// Merge the daemon result (a map[string]any over the socket)
				// with the stashed modal context, then build the confirm modal
				// exactly as the old sync path did.
				merged := mergeDecomposeResult(msg.Event.Data["result"], ctxData)
				if modal := buildDecomposeConfirmFromResult(merged); modal != nil {
					m.modal = modal
					return m, modal.Init()
				}
				// No proposals / unbuildable — surface inline on the viewer
				// rather than silently dropping the spinner.
				if m.modal != nil {
					upd, fcmd := m.modal.Update(modals.RPCResultMsg{
						Kind: "roadmap_decompose_open",
					})
					m.modal = upd
					return m, fcmd
				}
			}

		case rpc.EventDecomposeFailed:
			id, _ := msg.Event.Data["decompose_id"].(string)
			if _, ok := m.pendingDecompose[id]; ok {
				delete(m.pendingDecompose, id)
				emsg, _ := msg.Event.Data["error"].(string)
				if m.modal != nil {
					upd, fcmd := m.modal.Update(modals.RPCResultMsg{
						Kind: "roadmap_decompose_open", Err: fmt.Errorf("%s", emsg),
					})
					m.modal = upd
					return m, fcmd
				}
			}

		// Async project.graduate lifecycle. We only act on events for a
		// graduate_id WE started (present in m.pendingGraduate) so a concurrent
		// CLI/other-TUI graduation doesn't hijack this modal. Each event is
		// forwarded to the open GraduateModal as an RPCResultMsg under the shared
		// "project_graduate_open" Kind with a "_graduate_*" discriminator.
		case rpc.EventGraduateProgress:
			id, _ := msg.Event.Data["graduate_id"].(string)
			if _, ok := m.pendingGraduate[id]; ok && m.modal != nil {
				label, _ := msg.Event.Data["phase_label"].(string)
				upd, pcmd := m.modal.Update(modals.RPCResultMsg{
					Kind: "project_graduate_open",
					Data: map[string]any{"_graduate_progress": label},
				})
				m.modal = upd
				return m, pcmd
			}

		case rpc.EventGraduateVerdict:
			id, _ := msg.Event.Data["graduate_id"].(string)
			if _, ok := m.pendingGraduate[id]; ok && m.modal != nil {
				// The daemon emits the verdict as a JSON object under Data["verdict"];
				// re-marshal to a string the modal unmarshals into GraduationVerdict.
				// A blocking verdict fires verdict-then-failed, so we do NOT clear the
				// pending entry here — the failed handler owns terminal cleanup.
				vj, _ := json.Marshal(msg.Event.Data["verdict"])
				upd, vcmd := m.modal.Update(modals.RPCResultMsg{
					Kind: "project_graduate_open",
					Data: map[string]any{"_graduate_verdict": string(vj)},
				})
				m.modal = upd
				return m, vcmd
			}

		case rpc.EventGraduateDone:
			id, _ := msg.Event.Data["graduate_id"].(string)
			if _, ok := m.pendingGraduate[id]; ok {
				delete(m.pendingGraduate, id)
				if m.modal != nil {
					prURL, _ := msg.Event.Data["pr_url"].(string)
					dryRun, _ := msg.Event.Data["dry_run"].(bool)
					upd, dcmd := m.modal.Update(modals.RPCResultMsg{
						Kind: "project_graduate_open",
						Data: map[string]any{
							"_graduate_done":    true,
							"_graduate_pr_url":  prURL,
							"_graduate_dry_run": dryRun,
						},
					})
					m.modal = upd
					return m, dcmd
				}
			}

		case rpc.EventGraduateFailed:
			id, _ := msg.Event.Data["graduate_id"].(string)
			if _, ok := m.pendingGraduate[id]; ok {
				delete(m.pendingGraduate, id)
				if m.modal != nil {
					// A blocking verdict fired graduate.verdict before this — the modal
					// keeps it visible and renders the failure beneath it.
					emsg, _ := msg.Event.Data["error"].(string)
					upd, fcmd := m.modal.Update(modals.RPCResultMsg{
						Kind: "project_graduate_open",
						Data: map[string]any{"_graduate_failed": emsg},
					})
					m.modal = upd
					return m, fcmd
				}
			}
		}
		return m, cmd

	case initialStateMsg:
		applyInitialState(m.snapshot, msg)
		// If the edit-project modal is open, re-seed its CanSequence from the
		// freshly-fetched project.list so a roadmap created while it was open
		// un-greys the "sequenced" option without a reopen (fired on modal open).
		if em, ok := m.modal.(*modals.EditProjectModal); ok {
			for _, p := range m.snapshot.Projects {
				if p.Slug == em.Slug {
					em.SetCanSequence(p.CanSequence)
					break
				}
			}
		}
		return m, nil

	case heartbeatTickMsg:
		if m.snapshot.LastHeartbeat > 0 {
			elapsed := time.Now().Unix() - m.snapshot.LastHeartbeat
			next := elapsed > m.heartbeatTimeoutSeconds
			if next != m.daemonDown {
				m.daemonDown = next
				m.refreshTabSizes() // banner appearing/disappearing shifts chrome by 2 rows
			}
		}
		return m, tea.Tick(1*time.Second, func(t time.Time) tea.Msg { return heartbeatTickMsg(t) })

	case daemonDownMsg:
		if !m.daemonDown {
			m.daemonDown = true
			m.refreshTabSizes()
		}
		return m, nil

	case daemonReconnectedMsg:
		if m.daemonDown {
			m.daemonDown = false
			m.refreshTabSizes()
		}
		// A daemon restart drops in-memory async jobs, so a pending id's terminal
		// event never arrives and the modal spinner would hang forever. Clear the
		// pending maps and tell the open modal to drop the spinner + show a retry
		// hint. (The FIRST reconnect fires at startup before any async op; the
		// len>0 guards make that a no-op.)
		if len(m.pendingGraduate) > 0 {
			m.pendingGraduate = map[string]string{}
			if m.modal != nil {
				// Route via the _graduate_failed discriminator so the modal stops
				// the spinner + renders the interruption (a bare Err is ignored).
				upd, rcmd := m.modal.Update(modals.RPCResultMsg{
					Kind: "project_graduate_open",
					Data: map[string]any{"_graduate_failed": "graduate interrupted (daemon reconnected) — re-press G to retry"},
				})
				m.modal = upd
				if m.Client != nil {
					return m, tea.Batch(rcmd, func() tea.Msg { return m.Client.FetchInitialState() })
				}
				return m, rcmd
			}
		}
		if len(m.pendingDecompose) > 0 {
			m.pendingDecompose = map[string]map[string]any{}
			if m.modal != nil {
				upd, rcmd := m.modal.Update(modals.RPCResultMsg{
					Kind: "roadmap_decompose_open",
					Err:  fmt.Errorf("decompose interrupted (daemon reconnected) — re-press D to retry"),
				})
				m.modal = upd
				if m.Client != nil {
					return m, tea.Batch(rcmd, func() tea.Msg { return m.Client.FetchInitialState() })
				}
				return m, rcmd
			}
		}
		if m.Client != nil {
			return m, func() tea.Msg { return m.Client.FetchInitialState() }
		}
		return m, nil

	case tabs.DrillInRequest:
		m.drillInRunID = msg.RunID
		m.drillScroll = 0
		// Phase 3.7.1: fetch stages from the daemon so drill-in has
		// data even for runs dispatched before the TUI subscribed
		// (snapshot's Stages map only populates from live events).
		if m.Client != nil {
			rid := msg.RunID
			return m, func() tea.Msg { return m.Client.FetchRunStages(rid) }
		}
		return m, nil

	case runStagesMsg:
		for _, st := range msg.Stages {
			if m.snapshot.Stages[st.ID] == nil {
				m.snapshot.Stages[st.ID] = &StageView{ID: st.ID}
			}
			sv := m.snapshot.Stages[st.ID]
			sv.RunID = st.RunID
			sv.Name = st.Name
			sv.Iter = st.Iter
			sv.Model = st.Model
			sv.StartedAt = st.StartedAt
			sv.EndedAt = st.EndedAt
			sv.Verdict = st.Verdict
			sv.TokensIn = st.TokensIn
			sv.TokensOut = st.TokensOut
		}
		return m, nil

	case taskDecomposeProposedMsg:
		// Result of the synchronous task.decompose fired from the task_detail
		// modal's D key. On error, forward it to the still-open task_detail
		// modal (it drops its decomposing spinner and shows the error inline);
		// the user may have esc-closed in the interim, so guard on m.modal.
		// On success, swap the modal out for the confirm modal seeded with the
		// typed proposals + task_id. (This is why D's result can't ride the
		// generic rpcResultMsg branch — that branch only feeds the already-open
		// modal; here we must CONSTRUCT a new one.)
		if msg.Err != nil {
			if m.modal != nil {
				upd, cmd := m.modal.Update(modals.RPCResultMsg{Kind: "task_decompose_open", Err: msg.Err})
				m.modal = upd
				return m, cmd
			}
			return m, nil
		}
		if m.modal == nil {
			// User esc-closed the task_detail modal while the Sonnet turn ran.
			// Drop the result rather than popping a confirm over a bare tab.
			return m, nil
		}
		var subs []decompose.ProposedSubtask
		if msg.Result != nil {
			subs = msg.Result.Subtasks
		}
		m.modal = modals.NewTaskDecomposeConfirmModal(msg.TaskID, subs)
		return m, m.modal.Init()

	case sequenceStatusMsg:
		// Fold a refreshed sequence.status view into the snapshot cache,
		// then forward to the current tab + open modal so a Sequence modal
		// / Projects tab re-renders against the new state. Mirrors how
		// runStagesMsg / rpcResultMsg results are surfaced.
		if m.snapshot.SequenceStatus == nil {
			m.snapshot.SequenceStatus = map[string]*rpc.SeqStatusView{}
		}
		m.snapshot.SequenceStatus[msg.ProjectID] = msg.View
		// Live-refresh an open Sequence modal for this project (the modal is a
		// modals-package type, so push the new view via SetView rather than
		// forwarding the tui-package msg it can't decode).
		if sm, ok := m.modal.(*modals.SequenceModal); ok && sm.ProjectID() == msg.ProjectID {
			sm.SetView(msg.View)
		}
		if len(m.tabs) > 0 {
			active, cmd := m.tabs[m.currentTab].Update(msg)
			m.tabs[m.currentTab] = active
			return m, cmd
		}
		return m, nil

	case tabs.TabOpenModalRequest:
		if msg.Kind == "task_detail" {
			if id, _ := msg.InitialState["task_id"].(string); id != "" {
				if tv := m.snapshot.Tasks[id]; tv != nil {
					msg.InitialState["integration_state"] = tv.IntegrationState
					msg.InitialState["pr_url"] = tv.PRURL
					msg.InitialState["pr_number"] = tv.PRNumber
				}
			}
		}
		m.modal = constructModal(msg.Kind, msg.InitialState)
		if m.modal == nil {
			return m, nil
		}
		return m, m.modal.Init()

	case tabs.TabRunNowRequest:
		if m.Client == nil {
			return m, nil
		}
		taskID := msg.TaskID
		return m, func() tea.Msg {
			_, _ = m.Client.RunNow(taskID)
			// Result reflects via run.started event into snapshot.
			return nil
		}

	case tabs.TabResolveTaskRequest:
		// C on a needs_attention task → fire resolve.now. Fire-and-forget:
		// the resolver runs asynchronously on the daemon; results surface
		// via the normal run.* event stream into the snapshot.
		if m.Client == nil {
			return m, nil
		}
		taskID := msg.TaskID
		return m, func() tea.Msg {
			_, _ = m.Client.ResolveTask(taskID)
			return nil
		}

	case tabs.TabMergeRetryRequest:
		// M on a needs_attention task → fire merge.retry. Fire-and-forget:
		// the daemon reconciles (PR already merged) or re-arms the merge
		// queue; results surface via task.* events into the snapshot.
		if m.Client == nil {
			return m, nil
		}
		taskID := msg.TaskID
		client := m.Client
		return m, func() tea.Msg {
			_, _ = client.MergeRetry(taskID)
			return nil // result surfaces via task.* events + the next snapshot
		}

	case tabs.TabApprovalResolveRequest:
		if m.Client == nil {
			return m, nil
		}
		req := msg
		return m, func() tea.Msg {
			_, _ = m.Client.ResolveApproval(req.ApprovalID, req.Decision, req.Remember, req.ToolName, req.ArgMatcher)
			// The blocked worker unblocks; approval.resolved clears the
			// snapshot entry.
			return nil
		}

	case tabs.TabChatSendRequest:
		if m.Client == nil {
			return m, nil
		}
		// Open a new chat.send stream. Client.StreamChat runs in its own
		// goroutine and fans chatFrameMsg / chatStreamEndedMsg back to the
		// Bubbletea program; no tea.Cmd wrapper needed.
		m.Client.StreamChat(msg.Message, msg.SessionID)
		return m, nil

	case tabs.TabPlannerOpenRequest:
		// P on Projects tab → reset chat tab, switch to it, fire a
		// planner-mode chat.send (kind=plan + project_slug). The daemon
		// returns a fresh session ID in the first "session" frame; the
		// chat tab captures it from the frame stream.
		chatIdx := m.findChatTabIndex()
		if chatIdx < 0 || m.Client == nil {
			return m, nil
		}
		// Reset the chat tab so any prior session's state (history,
		// metadata, pending confirms) is gone. The incoming ChatFrameMsg
		// sequence will populate the new planner-mode session.
		if resetter, ok := m.tabs[chatIdx].(interface{ Reset() }); ok {
			resetter.Reset()
		}
		m.currentTab = chatIdx
		slug := msg.ProjectSlug
		return m, func() tea.Msg {
			m.Client.StreamPlannerChat(slug)
			return nil
		}

	case tabs.TabEditProjectRequest:
		// e on Projects tab → open the Edit Project modal pre-filled with the
		// project's name + repo_path + current dispatch mode (and, when the
		// project is sequenced, its target branch + advancement policy — the
		// tab reads those from the cached sequence.status). Submit flows
		// through handleModalSubmit → Client.EditProject (project.edit RPC),
		// then the root writes the per-project dispatch_mode TOML and runs the
		// sequence enable/disable (Phase 4).
		m.modal = modals.NewEditProjectModal(msg.Slug, msg.Name, msg.RepoPath, msg.Status, msg.DispatchMode, msg.Target, msg.Policy,
			msg.FeatureBranch, msg.MergeMethod, msg.TaskAutoIntegrate, msg.AutoFixCI, msg.CanSequence)
		// CanSequence is computed daemon-side from the roadmap/spec gate; the
		// cached snapshot value can be stale (e.g. a roadmap was created since the
		// last fetch). Refresh so the re-seed in the initialStateMsg handler
		// un-greys the "sequenced" option if the gate now passes.
		return m, tea.Batch(m.modal.Init(), func() tea.Msg { return m.Client.FetchInitialState() })

	case tabs.TabSequenceRequest:
		// q on a sequenced project → open the Sequence modal, seeded with the
		// cached sequence.status (refreshed live via sequenceStatusMsg).
		m.modal = modals.NewSequenceModal(msg.Slug, msg.ProjectID, m.snapshot.SequenceStatus[msg.ProjectID])
		return m, m.modal.Init()

	case tabs.TabDeleteProjectRequest:
		// d on Projects tab → open the Delete Project confirm modal. The
		// modal forces the operator to type the slug to enable y; submit
		// flows through handleModalSubmit → Client.DeleteProject which the
		// daemon cascades to all tasks + runs of the project.
		m.modal = modals.NewDeleteProjectConfirmModal(msg.Slug, msg.TaskCount, msg.RunCount)
		return m, m.modal.Init()

	case tabs.TabRoadmapViewerRequest:
		// R on Projects tab → open the Roadmap Viewer modal. The modal reads +
		// parses the on-disk roadmap synchronously in its constructor. On a
		// working-tree miss (shared repo checked out on another branch) it enters
		// a loading state and we fetch the roadmap branch-aware from the daemon.
		vm := modals.NewRoadmapViewerModal(msg.ProjectSlug, msg.RepoPath)
		m.modal = vm
		cmd := vm.Init()
		if vm.NeedsContent() {
			slug := msg.ProjectSlug
			return m, tea.Batch(cmd, func() tea.Msg {
				content, err := m.Client.RoadmapContent(slug)
				return roadmapContentLoadedMsg{slug: slug, content: content, err: err}
			})
		}
		return m, cmd

	case roadmapContentLoadedMsg:
		// Branch-aware roadmap.content fetch (fired above) resolved. Feed it to
		// the still-open viewer if it's for the same project.
		if vm, ok := m.modal.(*modals.RoadmapViewerModal); ok && vm.Slug() == msg.slug {
			if msg.err != nil {
				vm.SetError("roadmap not found: " + msg.err.Error())
			} else {
				vm.SetContent(msg.content)
			}
		}
		return m, nil

	case tabs.TabRepoConfigRequest:
		// f on Projects tab → open the read-only Repo Config viewer. Resolve the
		// ~/.hive/repos/<key>/config.toml path here (repoConfigPath wraps
		// config.RepoKey + hiveDir, both TUI-package-local) and hand it to the
		// modal, which reads + displays it synchronously. Empty repo_path → ""
		// path → the modal shows a "no repo path" notice.
		m.modal = modals.NewRepoConfigModal(msg.Slug, repoConfigPath(msg.RepoPath))
		return m, m.modal.Init()

	case tabs.TabFeatureBranchHealthRequest:
		// H on Projects tab → open the feature-branch health-check modal.
		// Init fires the async git calls; the modal shows a spinner until
		// they complete, then renders the health report.
		m.modal = modals.NewHealthCheckModal(msg.Slug, msg.RepoPath, msg.Feature, msg.Target)
		return m, m.modal.Init()

	case tabs.TabGraduateRequest:
		// G on Projects tab → open the Graduate modal in its confirm state
		// (mode selector defaulting to Dry-run + a draft toggle). Submit emits a
		// SubmitRequest{Kind:"project_graduate_open"} which handleModalSubmit
		// turns into an async ProjectGraduateStart; the returned graduate_id is
		// recorded in m.pendingGraduate and the graduate.* lifecycle is forwarded
		// to the modal. Only emitted for projects with a feature branch.
		//
		// If a graduate we started is still in flight for THIS project (its
		// id→slug is in pendingGraduate), re-attach: re-open the modal directly
		// in its running state so the still-arriving progress/verdict/done land
		// in it via the existing event forwarding — instead of starting a fresh
		// confirm that would kick off a second, concurrent graduation.
		for _, sl := range m.pendingGraduate {
			if sl == msg.Slug {
				m.modal = modals.NewGraduateModalReattached(msg.Slug, msg.Feature, msg.Target)
				return m, m.modal.Init()
			}
		}
		m.modal = modals.NewGraduateModal(msg.Slug, msg.Feature, msg.Target)
		return m, m.modal.Init()

	case tabs.TabSourcesRequest:
		// s on Projects tab → open the Sources modal in its loading state
		// AND fire the initial sources.list fetch. The modal transitions
		// to its "list" state once the RPC response arrives (forwarded
		// via the standard rpcResultMsg → modal.Update path).
		if m.Client == nil {
			return m, nil
		}
		m.modal = modals.NewSourcesModal(msg.ProjectSlug)
		slug := msg.ProjectSlug
		client := m.Client
		return m, tea.Batch(m.modal.Init(), func() tea.Msg {
			data, err := client.SourcesList(slug)
			return rpcResultMsg{Kind: "sources_list", Err: err, Data: data}
		})

	case tabs.TabDoctorRequest:
		// D on Active tab → open the Doctor modal in its "running" state AND
		// run the full internal/doctor audit OFF the UI thread. doctor.Run does
		// blocking RPC (daemon.status/health, sources.status) + local file/db/
		// worktree reads, so it MUST NOT run inline on the Bubbletea goroutine —
		// the tea.Cmd closure executes it on a runtime worker and delivers the
		// Report back as an rpcResultMsg. The Report is carried as a typed value
		// under Data["report"] (no flat-map round-trip), which the modal
		// type-asserts back in its Update.
		if m.Client == nil {
			return m, nil
		}
		m.modal = modals.NewDoctorModal()
		client := m.Client
		return m, tea.Batch(m.modal.Init(), runDoctorCmd(client))

	case tabs.TabCleanRequest:
		// x on Active tab → open the Clean modal in its "preview" state AND
		// fire a DRY-RUN cleanup.run so the operator sees what WOULD be
		// reclaimed before confirming. The real (destructive) sweep only fires
		// on confirm via the cleanup_run SubmitRequest route below.
		if m.Client == nil {
			return m, nil
		}
		m.modal = modals.NewCleanModal()
		client := m.Client
		return m, tea.Batch(m.modal.Init(), func() tea.Msg {
			data, err := client.Cleanup(true, nil, false) // dry run
			return rpcResultMsg{Kind: "cleanup_preview", Err: err, Data: data}
		})

	case tabs.TabChatConfirmRequest:
		// Fire-and-forget — the daemon's gate resolves and the streaming
		// frame loop continues; we don't need the chat.confirm response.
		if m.Client == nil {
			return m, nil
		}
		req := ChatConfirmReq{
			SessionID:   msg.SessionID,
			ToolCallID:  msg.ToolCallID,
			Approve:     msg.Approve,
			Reason:      msg.Reason,
			EditedInput: msg.EditedInput,
		}
		go func() {
			_ = m.Client.ChatConfirm(req)
		}()
		return m, nil

	case tabs.CostRefreshRequest:
		if m.Client == nil {
			return m, nil
		}
		return m, func() tea.Msg { return m.Client.FetchCostSummary() }

	case costSummaryMsg:
		// Forward to the Costs tab as a CostSummaryUpdate so it can
		// refresh its cached payload + reschedule the refresh ticker.
		for i, t := range m.tabs {
			if _, ok := t.(*tabs.Costs); ok {
				tNew, cmd := t.Update(tabs.CostSummaryUpdate{Summary: msg.Summary})
				m.tabs[i] = tNew
				return m, cmd
			}
		}
		return m, nil

	case tabs.ChatFrameMsg, tabs.ChatStreamStartedMsg, tabs.ChatStreamEndedMsg:
		// Always route chat-stream messages to the chat tab regardless of
		// active focus — the chat tab keeps consuming the stream even when
		// the user has switched away.
		chatIdx := m.findChatTabIndex()
		if chatIdx >= 0 {
			updated, cmd := m.tabs[chatIdx].Update(msg)
			m.tabs[chatIdx] = updated
			return m, cmd
		}
		return m, nil

	case tabs.ChatHistoryLoadedMsg:
		// Route loaded history to the chat tab so it rebuilds its frame list.
		chatIdx := m.findChatTabIndex()
		if chatIdx >= 0 {
			updated, cmd := m.tabs[chatIdx].Update(msg)
			m.tabs[chatIdx] = updated
			return m, cmd
		}
		return m, nil

	case modals.SessionPickerLoadMsg:
		// Root satisfies the picker's load request via a tea.Cmd so the
		// runtime dispatches it in a goroutine and delivers the result msg
		// automatically — no m.program.Send needed.
		if m.Client == nil {
			return m, func() tea.Msg {
				return modals.SessionPickerLoadResultMsg{Err: fmt.Errorf("no client")}
			}
		}
		return m, func() tea.Msg {
			rows, err := m.Client.ChatHistoryList(50)
			if err != nil {
				return modals.SessionPickerLoadResultMsg{Err: err}
			}
			converted := make([]modals.SessionRow, 0, len(rows))
			for _, r := range rows {
				converted = append(converted, modals.SessionRow{
					ID:           r.ID,
					Surface:      r.Surface,
					StartedAt:    r.StartedAt,
					EndedAt:      r.EndedAt,
					TotalCostUSD: r.TotalCostUSD,
					Name:         r.Name,
					Provider:     r.Provider,
				})
			}
			return modals.SessionPickerLoadResultMsg{Rows: converted}
		}

	case modals.SessionPickerLoadResultMsg:
		if picker, ok := m.modal.(*modals.ChatSessionPicker); ok {
			if msg.Err != nil {
				picker.SetError(msg.Err)
			} else {
				picker.SetRows(msg.Rows)
			}
		}
		return m, nil

	case modals.ChatSessionDeleteRequestMsg:
		// Picker `d` + `y` confirm. Call Client.ChatDelete; on success
		// remove the row in place + if the deleted session was the
		// chat tab's active one, reset the tab so the next message
		// starts a fresh session. Daemon-side handler evicts scope
		// cache + on-disk scratch dir already.
		sessionID := msg.SessionID
		return m, func() tea.Msg {
			if m.Client == nil {
				return modals.ChatSessionDeleteResultMsg{SessionID: sessionID, Err: fmt.Errorf("no client")}
			}
			err := m.Client.ChatDelete(sessionID)
			return modals.ChatSessionDeleteResultMsg{SessionID: sessionID, Err: err}
		}

	case modals.ChatSessionDeleteResultMsg:
		if picker, ok := m.modal.(*modals.ChatSessionPicker); ok {
			if msg.Err == nil {
				picker.ApplyDeletedRow(msg.SessionID)
			} else {
				picker.SetError(msg.Err)
			}
		}
		if msg.Err == nil {
			chatIdx := m.findChatTabIndex()
			if chatIdx >= 0 {
				if getter, ok := m.tabs[chatIdx].(interface{ SessionID() string }); ok {
					if getter.SessionID() == msg.SessionID {
						if resetter, ok := m.tabs[chatIdx].(interface{ Reset() }); ok {
							resetter.Reset()
						}
					}
				}
			}
		}
		return m, nil

	case modals.NewChatSessionMsg:
		// Sentinel "+ New session" entry or Ctrl-N from the picker.
		// Reset the chat tab to a fresh empty state, close the modal,
		// and switch focus to the chat tab. The new session ID is
		// assigned daemon-side on first message send (InsertChatSession
		// in chat.send), so there's nothing to fetch here.
		chatIdx := m.findChatTabIndex()
		if chatIdx >= 0 {
			if resetter, ok := m.tabs[chatIdx].(interface{ Reset() }); ok {
				resetter.Reset()
			}
			m.currentTab = chatIdx
		}
		m.modal = nil
		return m, nil

	case modals.SessionPickerResumeMsg:
		// Note: the picker emits SessionPickerResumeMsg only — NOT also CloseMsg.
		// Root handles modal cleanup here (m.modal = nil) so the resume + close
		// happen atomically. Other modals that DO emit CloseMsg get cleaned up
		// via the modal-priority CloseMsg case at the top of Update.
		chatIdx := m.findChatTabIndex()
		if chatIdx >= 0 {
			if setter, ok := m.tabs[chatIdx].(interface{ SetSessionID(string) }); ok {
				setter.SetSessionID(msg.SessionID)
			}
			m.currentTab = chatIdx
		}
		m.modal = nil
		// Fetch the picked session's messages so the chat tab shows the prior
		// conversation, not just continues with empty history. Session
		// metadata (Name/Provider/TotalCostUSD) comes from the picker's row
		// — it already loaded these via chat.history_list — so we forward
		// them into ChatHistoryLoadedMsg without an extra RPC.
		if m.Client != nil {
			sessionID := msg.SessionID
			name := msg.Name
			provider := msg.Provider
			totalCost := msg.TotalCostUSD
			return m, func() tea.Msg {
				rows, err := m.Client.ChatHistoryGet(sessionID)
				if err != nil {
					// Surface the metadata even on history-fetch failure so
					// the bar reflects the picked session instead of stale data.
					return tabs.ChatHistoryLoadedMsg{
						SessionID:    sessionID,
						Name:         name,
						Provider:     provider,
						TotalCostUSD: totalCost,
					}
				}
				converted := make([]tabs.ChatHistoryMessage, 0, len(rows))
				for _, r := range rows {
					converted = append(converted, tabs.ChatHistoryMessage{
						Role:    r.Role,
						Content: r.Content,
						// ToolName is not stored in the persisted message row —
						// tool_result frames render without a leading tool name on
						// resume. Acceptable for now.
					})
				}
				return tabs.ChatHistoryLoadedMsg{
					SessionID:    sessionID,
					Name:         name,
					Provider:     provider,
					TotalCostUSD: totalCost,
					Messages:     converted,
				}
			}
		}
		return m, nil

	case tabs.OpenChatRenameMsg:
		m.modal = modals.NewChatRenameModal(msg.SessionID, msg.CurrentName)
		return m, m.modal.Init()

	case tabs.OpenChatEditArgsMsg:
		m.modal = modals.NewChatEditArgsModal(msg.ToolName, msg.ToolCallID, msg.Args, m.width, m.height)
		return m, m.modal.Init()

	case tabs.OpenChatToolResultPickerMsg:
		// Convert tabs.ChatToolResultRow → modals.ChatToolResultRow.
		// The two types are decoupled to keep the "tabs doesn't import
		// modals" architectural boundary intact.
		modalRows := make([]modals.ChatToolResultRow, len(msg.Rows))
		for i, r := range msg.Rows {
			modalRows[i] = modals.ChatToolResultRow{Tool: r.Tool, Result: r.Result, IsError: r.IsError}
		}
		m.modal = modals.NewChatToolResultPicker(modalRows, m.width, m.height-rootChromeRowsFor(m.height, m.daemonDown))
		return m, m.modal.Init()

	case modals.ChatEditArgsSubmitMsg:
		// Map modal submit to a chat.confirm RPC with Approve=true +
		// EditedInput set. The modal already emitted CloseMsg via
		// tea.Batch so m.modal will be cleared by the CloseMsg case.
		chatIdx := m.findChatTabIndex()
		sessionID := ""
		if chatIdx >= 0 {
			if getter, ok := m.tabs[chatIdx].(interface{ SessionID() string }); ok {
				sessionID = getter.SessionID()
			}
			if resolver, ok := m.tabs[chatIdx].(interface {
				ResolveByEdit(toolCallID string, editedArgs json.RawMessage)
			}); ok {
				resolver.ResolveByEdit(msg.ToolCallID, msg.EditedArgs)
			}
		}
		toolCallID := msg.ToolCallID
		editedArgs := msg.EditedArgs
		return m, func() tea.Msg {
			return tabs.TabChatConfirmRequest{
				SessionID:   sessionID,
				ToolCallID:  toolCallID,
				Approve:     true,
				EditedInput: editedArgs,
			}
		}

	case modals.ChatRenameSubmitMsg:
		sessionID := msg.SessionID
		name := msg.Name
		return m, func() tea.Msg {
			if err := m.Client.ChatSetName(sessionID, name); err != nil {
				return modals.ChatRenameErrorMsg{Err: err}
			}
			return chatRenameSuccessMsg{Name: name}
		}

	case chatRenameSuccessMsg:
		m.modal = nil
		chatIdx := m.findChatTabIndex()
		if chatIdx >= 0 {
			if setter, ok := m.tabs[chatIdx].(interface{ SetSessionName(string) }); ok {
				setter.SetSessionName(msg.Name)
			}
		}
		return m, nil

	case modals.ChatRenameErrorMsg:
		if m.modal != nil {
			updated, cmd := m.modal.Update(msg)
			m.modal = updated
			return m, cmd
		}
		return m, nil
	default:
		// A modal's own async/animation messages have no explicit case here
		// (e.g. the health modal's local-Cmd healthLoadedMsg + spinner.TickMsg,
		// whose types are unexported by the modals package). When a modal is
		// open, forward any otherwise-unhandled message to it so it can load
		// and animate — without this such a modal spins forever. Modals ignore
		// message types they don't recognize, so this is safe for all of them.
		if m.modal != nil {
			upd, cmd := m.modal.Update(msg)
			m.modal = upd
			return m, cmd
		}
	}
	return m, nil
}

// chatRenameSuccessMsg is root-internal: after the daemon accepts the
// rename, we need a single message that BOTH closes the modal AND pushes
// the new name to the chat tab. We split it apart in the case handler.
type chatRenameSuccessMsg struct{ Name string }

// seqEventProject resolves the project ID + slug carried by a sequence.*
// event's Data["project_id"]. ok is false when the event lacks a
// project_id or the project isn't in the snapshot (so the caller can skip
// the refetch). Pure lookup — no I/O — so it's safe to call from Update.
func (m *Model) seqEventProject(ev rpc.EventMessage) (projectID, slug string, ok bool) {
	projectID, _ = ev.Data["project_id"].(string)
	if projectID == "" {
		return "", "", false
	}
	p, exists := m.snapshot.Projects[projectID]
	if !exists || p == nil {
		return "", "", false
	}
	return projectID, p.Slug, true
}

// findChatTabIndex returns the position of the chat tab in m.tabs, or -1.
func (m *Model) findChatTabIndex() int {
	for i, t := range m.tabs {
		if t.Name() == "Chat" {
			return i
		}
	}
	return -1
}

// View renders the TUI.
func (m *Model) View() string {
	if m.width == 0 {
		return "initializing..."
	}

	// Drill-in overlay — but a modal (e.g. confirm-abandon opened with d
	// from the drill-in) takes render priority so its prompt is visible.
	// Otherwise the modal would receive keys while the drill-in stayed on
	// screen, making abandon-from-drill-in look like a no-op.
	if m.drillInRunID != "" && m.modal == nil {
		return renderDrillIn(m.snapshot, m.drillInRunID, m.width, m.height, m.drillScroll)
	}

	var b strings.Builder

	// Daemon-down banner pinned to the top so the global "nothing's
	// happening" signal is the FIRST row, not buried below tab content.
	if m.daemonDown {
		b.WriteString(style.DaemonDownBanner.Render("⚠ DAEMON DOWN — reconnecting…"))
		b.WriteString("\n")
	}

	var tabBits []string
	for i, t := range m.tabs {
		s := fmt.Sprintf(" %d. %s ", i+1, t.Name())
		if i == m.currentTab {
			tabBits = append(tabBits, style.TabActive.Render(s))
		} else {
			tabBits = append(tabBits, style.TabInactive.Render(s))
		}
	}
	b.WriteString(strings.Join(tabBits, " "))
	b.WriteString("\n\n")

	if m.modal != nil {
		b.WriteString(m.modal.View(m.width, m.height-rootChromeRowsFor(m.height, m.daemonDown)))
	} else {
		b.WriteString(m.tabs[m.currentTab].View())
	}

	// Render status on two lines so long per-tab help (Projects is
	// ~60 chars) doesn't get truncated on standard-width terminals.
	// Line 1: global keys + counts. Line 2: per-tab keys.
	b.WriteString("\n")
	counts := fmt.Sprintf("[%d projects | %d runs] ", len(m.snapshot.Projects), len(m.snapshot.Runs))
	global := style.Hint.Render(counts) +
		style.Key.Render("q") + style.Hint.Render(" quit · ") +
		style.Key.Render("tab") + style.Hint.Render(" switch · ") +
		style.Key.Render("esc") + style.Hint.Render(" back")
	b.WriteString(global)
	if len(m.tabs) > 0 {
		if tabHelp := m.tabs[m.currentTab].KeyHelp(); tabHelp != "" {
			// Truncate to the terminal width so a long per-tab key line
			// degrades to "…" instead of clipping mid-glyph at the right edge
			// (KeyHelp strings have a habit of outgrowing the window).
			if m.width > 0 {
				tabHelp = runewidth.Truncate(tabHelp, m.width, "…")
			}
			b.WriteString("\n")
			b.WriteString(style.DimText.Render(tabHelp))
		}
	}

	// Status legend — make the color coding self-documenting. Only render
	// when there's vertical room (small terminals get to skip it).
	if m.height >= 24 {
		legend := style.Running.Render("● running") + "  " +
			style.Done.Render("● done") + "  " +
			style.NeedsAttention.Render("● needs attention") + "  " +
			style.ErrorStyle.Render("● error")
		b.WriteString("\n")
		b.WriteString(legend)
	}

	return b.String()
}

// applyInitialState seeds the snapshot from the daemon.status + lists.
func applyInitialState(s *Snapshot, msg initialStateMsg) {
	for _, p := range msg.Projects {
		s.Projects[p.ID] = &ProjectView{
			ID: p.ID, Slug: p.Slug, Name: p.Name,
			RepoPath: p.RepoPath, Status: p.Status, CreatedAt: p.CreatedAt,
			DispatchMode:  p.DispatchMode,
			FeatureBranch: p.FeatureBranch,
			TargetBranch:  p.TargetBranch,
			AutoIntegrate: p.AutoIntegrate,
			MergeMethod:   p.MergeMethod,
			AutoFixCI:     p.AutoFixCI,
			CanSequence:   p.CanSequence,
		}
	}
	// Seed the per-project sequence.status cache fetched alongside the
	// project list. Guard nil so a non-sequenced fetch (empty map or nil)
	// still leaves the snapshot with a usable map.
	if msg.SequenceStatus != nil {
		if s.SequenceStatus == nil {
			s.SequenceStatus = map[string]*rpc.SeqStatusView{}
		}
		for pid, v := range msg.SequenceStatus {
			s.SequenceStatus[pid] = v
		}
	}
	for _, t := range msg.Tasks {
		s.Tasks[t.ID] = &TaskView{
			ID: t.ID, ProjectID: t.ProjectID, Title: t.Title,
			Priority: t.Priority, Status: string(t.Status),
			Order: taskOrder(t.Metadata["roadmap_phase"], t.Metadata["roadmap_phase_index"]),
		}
	}
	if msg.Status != nil {
		// Tasks with a currently-running run (authoritative now). Used so
		// the recent block doesn't override a live-running task's status,
		// without relying on the (possibly stale, on reconnect) snapshot.
		runningTaskIDs := map[string]bool{}
		if running, ok := msg.Status["running"].([]any); ok {
			for _, r := range running {
				if m, ok := r.(map[string]any); ok {
					if tid, ok := m["task_id"].(string); ok && tid != "" {
						runningTaskIDs[tid] = true
					}
				}
			}
		}
		if running, ok := msg.Status["running"].([]any); ok {
			for _, r := range running {
				m, _ := r.(map[string]any)
				runID, _ := m["id"].(string)
				if runID == "" {
					continue
				}
				rv := &RunView{ID: runID, Status: "running"}
				if t, ok := m["task_id"].(string); ok {
					rv.TaskID = t
				}
				if p, ok := m["pipeline"].(string); ok {
					rv.Pipeline = p
				}
				if ts, ok := m["started_at"].(float64); ok {
					rv.StartedAt = int64(ts)
				}
				s.Runs[runID] = rv
				// Phase 3.7: hydrate Task + Project from enriched payload
				// so the snapshot has everything it needs (task.list
				// doesn't return dispatched tasks).
				if rv.TaskID != "" {
					if s.Tasks[rv.TaskID] == nil {
						s.Tasks[rv.TaskID] = &TaskView{ID: rv.TaskID}
					}
					// The task has a running run — mark it running so it
					// doesn't ALSO render in the Projects "Queued" lane
					// (its status would otherwise stay "" → queued, showing
					// the task in both "in progress" and "queued").
					s.Tasks[rv.TaskID].Status = "running"
					if title, ok := m["task_title"].(string); ok {
						s.Tasks[rv.TaskID].Title = title
					}
					if pid, ok := m["project_id"].(string); ok {
						s.Tasks[rv.TaskID].ProjectID = pid
						if s.Projects[pid] == nil {
							slug, _ := m["project_slug"].(string)
							s.Projects[pid] = &ProjectView{ID: pid, Slug: slug, Name: slug}
						}
					}
				}
			}
		}
		if recent, ok := msg.Status["recent"].([]any); ok {
			for _, r := range recent {
				m, _ := r.(map[string]any)
				runID, _ := m["id"].(string)
				if runID == "" {
					continue
				}
				rv := s.Runs[runID]
				if rv == nil {
					rv = &RunView{ID: runID}
					s.Runs[runID] = rv
				}
				if st, ok := m["status"].(string); ok {
					rv.Status = st
				}
				if sum, ok := m["summary"].(string); ok {
					rv.Summary = sum
				}
				if ts, ok := m["ended_at"].(float64); ok {
					rv.EndedAt = int64(ts)
				}
				// The recent feed deliberately does NOT derive task status. It is
				// a bounded "last N runs" list that can contain SUPERSEDED runs
				// (e.g. a failed build later re-run to done + merged), so mirroring
				// an arbitrary run's status onto its task mislabels it — a
				// done+merged task showed as needs_attention off its earlier
				// failed build run. Task status comes only from the authoritative
				// sources: task.list (pending/needs_attention), recent_done (done,
				// guarded), the running feed, and live run events.
			}
		}
		// Consume the bounded, task-deduped recent_done feed. Each entry is
		// the latest done run per task (from store.ListRecentDoneTasks). We
		// hydrate these runs + tasks so the snapshot's done section is
		// bounded to N distinct tasks rather than N raw runs. The existing
		// "recent" feed is kept for needs_attention/running hydration.
		if recentDone, ok := msg.Status["recent_done"].([]any); ok {
			for _, r := range recentDone {
				m, _ := r.(map[string]any)
				runID, _ := m["id"].(string)
				if runID == "" {
					continue
				}
				rv := s.Runs[runID]
				if rv == nil {
					rv = &RunView{ID: runID}
					s.Runs[runID] = rv
				}
				if taskID, ok := m["task_id"].(string); ok && taskID != "" {
					rv.TaskID = taskID
				}
				if pl, ok := m["pipeline"].(string); ok {
					rv.Pipeline = pl
				}
				if st, ok := m["status"].(string); ok {
					rv.Status = st
				}
				if sum, ok := m["summary"].(string); ok {
					rv.Summary = sum
				}
				if ts, ok := m["ended_at"].(float64); ok {
					rv.EndedAt = int64(ts)
				}
				if rv.TaskID != "" && !runningTaskIDs[rv.TaskID] {
					if s.Tasks[rv.TaskID] == nil {
						s.Tasks[rv.TaskID] = &TaskView{ID: rv.TaskID}
					}
					t := s.Tasks[rv.TaskID]
					if title, ok := m["task_title"].(string); ok && title != "" {
						t.Title = title
					}
					if pid, ok := m["project_id"].(string); ok {
						t.ProjectID = pid
						if s.Projects[pid] == nil {
							slug, _ := m["project_slug"].(string)
							s.Projects[pid] = &ProjectView{ID: pid, Slug: slug, Name: slug}
						}
					}
					// Don't downgrade a task the daemon reported as
					// needs_attention (seeded above from task.list) just
					// because it has a done run — a stuck merge keeps the
					// task needs_attention while its build runs are done. The
					// authoritative task status must win over run-derived done.
					if t.Status != "needs_attention" {
						t.Status = "done"
					}
				}
			}
		}
		// Phase 4.6: hydrate pending approvals so a (re)subscribing TUI
		// shows ones whose approval.requested event fired before it
		// connected (otherwise the inbox + ⚠ indicators stay empty).
		if pend, ok := msg.Status["pending_approvals_list"].([]any); ok {
			for _, pa := range pend {
				m, _ := pa.(map[string]any)
				if m == nil {
					continue
				}
				s.ApplyPendingApproval(m)
			}
		}
	}
}

// handleModalSubmit routes the modal's SubmitRequest to the appropriate
// Client RPC. Returns a tea.Cmd producing an rpcResultMsg.
// resolveDrillApproval resolves the FIRST pending approval for the
// drilled-in run. key is a/A (approve / approve+remember) or d/D (deny /
// deny+remember). No-op (nil cmd) when nothing is pending.
func (m *Model) resolveDrillApproval(key string) tea.Cmd {
	if m.Client == nil {
		return nil
	}
	var pa *tabs.PendingApproval
	for _, p := range m.snapshot.PendingApprovals() {
		if p.RunID == m.drillInRunID {
			pp := p
			pa = &pp
			break
		}
	}
	if pa == nil {
		return nil
	}
	decision := "approve"
	if key == "d" || key == "D" {
		decision = "deny"
	}
	remember := key == "A" || key == "D"
	argMatcher := ""
	if remember && pa.ToolName == "Bash" {
		argMatcher = firstBashGlob(pa.Arg)
	}
	id, tool := pa.ApprovalID, pa.ToolName
	return func() tea.Msg {
		_, _ = m.Client.ResolveApproval(id, decision, remember, tool, argMatcher)
		return nil
	}
}

// firstBashGlob turns a Bash command into a remember-glob from its first
// token, e.g. "make all" -> "make *".
func firstBashGlob(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if i := strings.IndexByte(cmd, ' '); i > 0 {
		return cmd[:i] + " *"
	}
	if cmd == "" {
		return ""
	}
	return cmd + " *"
}

// runDoctorCmd returns a tea.Cmd that executes the full internal/doctor audit
// off the UI thread (the runtime dispatches the closure on a worker goroutine)
// and delivers the Report back as an rpcResultMsg the Doctor modal consumes.
// The Report is carried as a typed value under Data["report"] rather than a
// flat map — the modal renders the structured Report field-for-field, so a
// typed hand-off is cleaner than serializing the whole thing through the map.
func runDoctorCmd(client *Client) tea.Cmd {
	return func() tea.Msg {
		rep := doctor.Run(context.Background(), hiveDir(), newTUIDoctorClient(client))
		return rpcResultMsg{Kind: "doctor_report", Data: map[string]any{"report": rep}}
	}
}

func (m *Model) handleModalSubmit(req modals.SubmitRequest) tea.Cmd {
	if m.Client == nil {
		return func() tea.Msg {
			return rpcResultMsg{Kind: req.Kind, Err: fmt.Errorf("no client")}
		}
	}
	return func() tea.Msg {
		var data map[string]any
		var err error
		switch req.Kind {
		case "new_project":
			data, err = m.Client.AddProject(
				getStrFromParams(req.Params, "slug"),
				getStrFromParams(req.Params, "name"),
				getStrFromParams(req.Params, "repo_path"),
				getStrFromParams(req.Params, "dispatch_mode"),
				getStrFromParams(req.Params, "target_branch"),
				getStrFromParams(req.Params, "feature_branch"),
				getStrFromParams(req.Params, "merge_method"),
				getBoolFromParams(req.Params, "task_auto_integrate"),
				getBoolFromParams(req.Params, "auto_fix_ci"),
			)
		case "edit_project":
			data, err = m.Client.EditProject(
				getStrFromParams(req.Params, "slug"),
				getStrFromParams(req.Params, "name"),
				getStrFromParams(req.Params, "repo_path"),
				getStrFromParams(req.Params, "status"),
				getStrFromParams(req.Params, "dispatch_mode"),
				getStrFromParams(req.Params, "target_branch"),
				getStrFromParams(req.Params, "policy"),
				getStrFromParams(req.Params, "feature_branch"),
				getStrFromParams(req.Params, "merge_method"),
				getBoolFromParams(req.Params, "task_auto_integrate"),
				getBoolFromParams(req.Params, "auto_fix_ci"),
			)
		case "delete_project":
			data, err = m.Client.DeleteProject(getStrFromParams(req.Params, "slug"))
		case "sequence_pause":
			data, err = m.Client.SequencePause(getStrFromParams(req.Params, "project_slug"))
		case "sequence_resume":
			data, err = m.Client.SequenceResume(getStrFromParams(req.Params, "project_slug"))
		case "sequence_advance":
			data, err = m.Client.SequenceAdvance(getStrFromParams(req.Params, "project_slug"))
		case "sequence_skip":
			data, err = m.Client.SequenceSkip(getStrFromParams(req.Params, "task_id"))
		case "sequence_complete":
			data, err = m.Client.SequenceComplete(
				getStrFromParams(req.Params, "project_slug"),
				getStrFromParams(req.Params, "phase"),
			)
		case "sources_sync":
			data, err = m.Client.SourcesSync(getStrFromParams(req.Params, "source"))
		case "roadmap_sync_linear":
			data, err = m.Client.RoadmapSyncLinear(getStrFromParams(req.Params, "project_slug"))
		case "new_task":
			data, err = m.Client.AddTask(
				getStrFromParams(req.Params, "project_slug"),
				getStrFromParams(req.Params, "title"),
				getStrFromParams(req.Params, "body"),
				getStrFromParams(req.Params, "pipeline"),
			)
		case "abandon_run":
			data, err = m.Client.AbandonRun(getStrFromParams(req.Params, "run_id"))
		case "task_get":
			data, err = m.Client.GetTask(getStrFromParams(req.Params, "task_id"))
		case "project_remediate":
			data, err = m.Client.ProjectRemediate(getStrFromParams(req.Params, "project_slug"))
		case "task_edit":
			data, err = m.Client.EditTask(
				getStrFromParams(req.Params, "task_id"),
				getStrFromParams(req.Params, "title"),
				getStrFromParams(req.Params, "body"),
			)
		case "task_delete":
			data, err = m.Client.DeleteTask(getStrFromParams(req.Params, "task_id"))
		case "run_now":
			data, err = m.Client.RunNow(getStrFromParams(req.Params, "task_id"))
		case "task_decompose_open":
			// Fire task.decompose OFF the UI thread (this closure already runs
			// on a runtime worker). It's a Sonnet tool turn (120s deadline), so
			// carry the proposals back as a TYPED taskDecomposeProposedMsg
			// (in-memory, like runDoctorCmd's doctor.Report) rather than
			// serializing through Data. The Update handler for that msg builds
			// the confirm modal on success, or forwards the error to the still-
			// open task_detail modal so it drops its spinner. NOTE: we return a
			// distinct message type here (not the generic rpcResultMsg) so the
			// success path can construct a NEW modal — the generic branch only
			// forwards results into the already-open modal.
			taskID := getStrFromParams(req.Params, "task_id")
			res, derr := m.Client.TaskDecompose(taskID, 0)
			return taskDecomposeProposedMsg{TaskID: taskID, Result: res, Err: derr}
		case "task_decompose_apply":
			// Apply via task.decompose_apply — inserts the confirmed children
			// of task_id. Param shape matches the CLI (parent_task_id +
			// subtasks); Client.TaskDecomposeApply builds that envelope.
			subtasks, _ := req.Params["subtasks"].([]decompose.ProposedSubtask)
			data, err = m.Client.TaskDecomposeApply(
				getStrFromParams(req.Params, "task_id"), subtasks)
		case "sources_list_refresh":
			// Re-fetch the project's sources after a bind/unbind so the
			// modal's list view reflects the new state. The modal emits
			// this Kind on its own (via RPCResultMsg handling) so the
			// refresh is owned by the modal lifecycle, not the root.
			data, err = m.Client.SourcesList(getStrFromParams(req.Params, "slug"))
			// The modal listens for "sources_list" — re-tag the result
			// so the same code path consumes both the initial load and
			// the post-action refresh.
			return rpcResultMsg{Kind: "sources_list", Err: err, Data: data}
		case "sources_bind":
			data, err = m.Client.SourcesBind(
				getStrFromParams(req.Params, "slug"),
				getStrFromParams(req.Params, "source"),
				getMapFromParams(req.Params, "binding"),
			)
		case "sources_unbind":
			data, err = m.Client.SourcesUnbind(
				getStrFromParams(req.Params, "slug"),
				getStrFromParams(req.Params, "source"),
			)
		case "cleanup_run":
			// The REAL reclaim (the Clean modal's confirm). The modal already
			// showed a dry-run preview; this run actually removes the
			// artifacts. Re-tagged to "cleanup_done" so the modal's done-state
			// handler consumes it.
			data, err = m.Client.Cleanup(false, nil, false)
			return rpcResultMsg{Kind: "cleanup_done", Err: err, Data: data}
		case "doctor_run":
			// Re-run of the doctor audit (the Doctor modal's `r` key). Runs the
			// same off-thread audit as the initial TabDoctorRequest open. The
			// outer func() already runs on a runtime worker, so calling
			// doctor.Run synchronously here is safe (not on the UI goroutine).
			rep := doctor.Run(context.Background(), hiveDir(), newTUIDoctorClient(m.Client))
			return rpcResultMsg{Kind: "doctor_report", Data: map[string]any{"report": rep}}
		case "roadmap_decompose_open":
			// START the async roadmap.decompose for the cursored phase. The
			// start RPC returns a decompose_id immediately (no model-turn read
			// deadline); the proposal arrives later as a decompose.proposed
			// event. We forward the decompose_id + the modal-construction
			// context (__project_slug / __phase_title / __roadmap_path /
			// __spec_paths) through Data so the rpcResultMsg branch (which runs
			// on the Update goroutine — no data race) can stash it into
			// m.pendingDecompose keyed by the id, and signal the viewer to show
			// a "decomposing…" spinner. 0 lets the daemon apply its default
			// max_subtasks. The confirm modal is built when the proposed event
			// lands, NOT here.
			slug := getStrFromParams(req.Params, "project_slug")
			phase := getStrFromParams(req.Params, "phase")
			id, serr := m.Client.RoadmapDecomposeStart(slug, phase, 0)
			if serr != nil {
				err = serr
				break
			}
			data = map[string]any{
				"_decomposing":   true,
				"decompose_id":   id,
				"phase":          phase,
				"__project_slug": slug,
				"__phase_title":  getStrFromParams(req.Params, "phase_title"),
				"__roadmap_path": getStrFromParams(req.Params, "roadmap_path"),
			}
			if sp, ok := req.Params["spec_paths"].([]string); ok {
				data["__spec_paths"] = sp
			}
		case "roadmap_insert_tasks":
			// Apply via roadmap.decompose_apply in one daemon call so
			// server-side merge/pull logic (rewrite hive task, pull Linear
			// issue) runs correctly. The old per-task task.add loop could
			// not handle MergeFrom.
			slug := getStrFromParams(req.Params, "project_slug")
			phase := getStrFromParams(req.Params, "phase")
			phaseTitle := getStrFromParams(req.Params, "phase_title")
			roadmapPath := getStrFromParams(req.Params, "roadmap_path")
			specPath := getStrFromParams(req.Params, "spec_path")
			subtasks, _ := req.Params["subtasks"].([]decompose.ProposedSubtask)
			subs := make([]map[string]any, 0, len(subtasks))
			for _, st := range subtasks {
				subs = append(subs, map[string]any{
					"title":          st.Title,
					"body":           st.Body,
					"priority":       st.Priority,
					"pipeline":       st.Pipeline,
					"merge_from":     st.MergeFrom,
					"depends_on":     st.DependsOn,
					"relevant_files": st.RelevantFiles,
				})
			}
			data, err = m.Client.RoadmapDecomposeApply(map[string]any{
				"project_slug": slug,
				"roadmap_path": roadmapPath,
				"phase":        phase,
				"phase_title":  phaseTitle,
				"spec_path":    specPath,
				"subtasks":     subs,
			})
			if err == nil && data != nil {
				ins, _ := data["inserted"].(float64)
				mrg, _ := data["merged"].(float64)
				pll, _ := data["pulled"].(float64)
				data["_status"] = fmt.Sprintf("inserted %d, merged %d, pulled %d",
					int(ins), int(mrg), int(pll))
			}
		case "project_graduate_open":
			// START the async project.graduate. The start RPC returns a graduate_id
			// immediately (no model-turn read deadline); progress/verdict/done/failed
			// arrive later as graduate.* events. We forward the graduate_id through
			// Data so the rpcResultMsg branch (running on the Update goroutine — no
			// data race) records it in m.pendingGraduate. dry_run defaults true at the
			// modal (safe); force/draft are the explicit selections.
			slug := getStrFromParams(req.Params, "slug")
			force := getBoolFromParams(req.Params, "force")
			draft := getBoolFromParams(req.Params, "draft")
			dryRun := getBoolFromParams(req.Params, "dry_run")
			id, serr := m.Client.ProjectGraduateStart(slug, force, draft, dryRun)
			if serr != nil {
				err = serr
				break
			}
			// Thread the slug through so the rpcResultMsg ack branch can record
			// the id→slug mapping (a later G uses it to re-attach an in-flight run).
			data = map[string]any{"graduate_id": id, "project_slug": slug}
		case "graduate_status_fetch":
			// On-open fetch of the GraduateModal's last persisted run. Marshal the
			// record (empty string when none/err so the modal shows "none yet") and
			// re-tag the result under "graduate_status" — the generic rpcResultMsg
			// branch forwards it straight to the modal.
			slug := getStrFromParams(req.Params, "slug")
			rec, gerr := m.Client.GraduateStatus(slug)
			js := ""
			if gerr == nil && rec != nil {
				if rb, mErr := json.Marshal(rec); mErr == nil {
					js = string(rb)
				}
			}
			return rpcResultMsg{Kind: "graduate_status", Data: map[string]any{"_graduate_status": js}}
		case "remediate_health":
			data, err = m.Client.RemediateHealth(
				getStrFromParams(req.Params, "project_slug"),
				getStrFromParams(req.Params, "action"),
			)
		default:
			err = fmt.Errorf("unknown submit kind: %s", req.Kind)
		}
		// NOTE: the new/edit project dispatch-mode transition is now owned
		// entirely by the daemon — Client.AddProject/EditProject carry
		// dispatch_mode (+ target/policy) and handleAddProject/handleEditProject
		// run applyDispatchMode (enable-gate, dispatcher row, teardown, [scheduler]
		// write) inside the same RPC. The old TUI-side post-RPC block that called
		// SequenceEnable/SequenceDisable + setProjectDispatchMode was removed to
		// avoid double-applying the transition (a redundant second enable-gate /
		// dispatcher upsert, and an auto_all→manual→auto_all config churn).
		return rpcResultMsg{Kind: req.Kind, Err: err, Data: data}
	}
}

func getStrFromParams(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func getBoolFromParams(m map[string]any, k string) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return false
}

// getMapFromParams pulls a map[string]any field out of a modal's
// SubmitRequest.Params. Returns an empty map (not nil) when the field is
// absent or mistyped so callers can pass the result straight into a
// client wrapper that JSON-marshals it.
func getMapFromParams(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// getStrMapFromParams pulls a map[string]string field out of a modal's
// SubmitRequest.Params (the typed-string form used by the roadmap metadata
// flow). Returns an empty map on absent/mistyped so the caller can JSON-
// marshal it unconditionally.
func getStrMapFromParams(m map[string]any, k string) map[string]string {
	if v, ok := m[k].(map[string]string); ok {
		return v
	}
	return map[string]string{}
}

// copyStrMap defensively copies a map[string]string. The roadmap_insert_tasks
// batch loop hands each task.add call its own copy so a downstream wrapper
// can't mutate the next iteration's metadata.
func copyStrMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// buildDecomposeConfirmFromResult constructs a DecomposeConfirmModal from
// the raw roadmap.decompose RPC response. The wire shape after json.Unmarshal
// into map[string]any is:
//
//   - phase_number string
//   - phase_title  string
//   - roadmap_path string
//   - spec_paths   []any (each entry a string)
//   - subtasks     []any (each entry a map[string]any with title/body/priority/pipeline)
//
// The handleModalSubmit branch above stashes the viewer-side context under
// `__project_slug` / `__phase_title` / `__roadmap_path` / `__spec_paths`
// keys so we don't rely on the RPC response carrying everything (the
// daemon DOES set phase_title + roadmap_path + spec_paths but we keep the
// viewer-side copies as fallback when the response is partial).
//
// Returns nil when the response has no subtasks — caller treats that as
// an error and leaves the viewer up.
// mergeDecomposeResult flattens the daemon RoadmapDecomposeResult (delivered as
// a map[string]any under the decompose.proposed event's "result" after the
// socket JSON round-trip) and overlays the stashed __ modal context, yielding
// the single map buildDecomposeConfirmFromResult reads. The result map supplies
// phase_number / roadmap_path / spec_paths / subtasks; ctxData supplies
// __project_slug (required) and the __* fallbacks.
func mergeDecomposeResult(result any, ctxData map[string]any) map[string]any {
	out := map[string]any{}
	if rm, ok := result.(map[string]any); ok {
		for k, v := range rm {
			out[k] = v
		}
	}
	for k, v := range ctxData {
		out[k] = v
	}
	return out
}

// forwardDecomposeProgress pushes a decompose.progress label into the open
// RoadmapViewerModal's spinner footer. It returns the updated modal + cmd, or
// (nil, nil) when no modal is open (caller no-ops). The viewer reads the label
// from Data["_decomposing_label"].
func (m *Model) forwardDecomposeProgress(id, label string) (modals.Modal, tea.Cmd) {
	if m.modal == nil {
		return nil, nil
	}
	return m.modal.Update(modals.RPCResultMsg{
		Kind: "roadmap_decompose_open",
		Data: map[string]any{"decompose_id": id, "_decomposing_label": label},
	})
}

func buildDecomposeConfirmFromResult(data map[string]any) modals.Modal {
	if data == nil {
		return nil
	}
	projectSlug, _ := data["__project_slug"].(string)
	if projectSlug == "" {
		// No viewer-side context — try to fall through to the daemon's
		// fields, but the daemon doesn't return project_slug in the
		// response (it knows it from the request). Fail closed.
		return nil
	}
	phaseNumber, _ := data["phase_number"].(string)
	if phaseNumber == "" {
		return nil
	}
	phaseTitle, _ := data["phase_title"].(string)
	if phaseTitle == "" {
		phaseTitle, _ = data["__phase_title"].(string)
	}
	roadmapPath, _ := data["roadmap_path"].(string)
	if roadmapPath == "" {
		roadmapPath, _ = data["__roadmap_path"].(string)
	}

	var specPaths []string
	if raw, ok := data["spec_paths"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				specPaths = append(specPaths, s)
			}
		}
	}
	if len(specPaths) == 0 {
		if fallback, ok := data["__spec_paths"].([]string); ok {
			specPaths = fallback
		}
	}

	subtasks := parseProposedSubtasks(data["subtasks"])
	if len(subtasks) == 0 {
		return nil
	}
	return modals.NewDecomposeConfirmModal(projectSlug, phaseNumber, phaseTitle, roadmapPath, specPaths, subtasks)
}

// parseProposedSubtasks decodes the JSON-unmarshaled []any → []ProposedSubtask.
// Tolerant: missing fields fall back to zero values (the daemon's response
// always includes title/body, and may omit priority/pipeline when the model
// didn't propose them — defaults are applied on the insert side).
func parseProposedSubtasks(raw any) []decompose.ProposedSubtask {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]decompose.ProposedSubtask, 0, len(arr))
	for _, v := range arr {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		st := decompose.ProposedSubtask{}
		st.Title, _ = m["title"].(string)
		st.Body, _ = m["body"].(string)
		st.Priority, _ = m["priority"].(string)
		st.Pipeline, _ = m["pipeline"].(string)
		st.MergeFrom, _ = m["merge_from"].(string)
		// depends_on/relevant_files survive the daemon→TUI decode so the apply
		// payload can carry them (JSON numbers decode to float64, arrays to []any).
		if rawDeps, ok := m["depends_on"].([]any); ok {
			for _, d := range rawDeps {
				if f, ok := d.(float64); ok {
					st.DependsOn = append(st.DependsOn, int(f))
				}
			}
		}
		if rawFiles, ok := m["relevant_files"].([]any); ok {
			for _, f := range rawFiles {
				if s, ok := f.(string); ok {
					st.RelevantFiles = append(st.RelevantFiles, s)
				}
			}
		}
		out = append(out, st)
	}
	return out
}

// constructModal instantiates the right modal type for the given kind.
// Per-modal constructors live in internal/tui/modals.
func constructModal(kind string, init map[string]any) modals.Modal {
	switch kind {
	case "new_project":
		return modals.NewNewProject()
	case "new_task":
		slug := getStrFromParams(init, "project_slug")
		return modals.NewNewTask(slug)
	case "confirm_abandon":
		runID := getStrFromParams(init, "run_id")
		return modals.NewConfirmAbandon(runID)
	case "task_detail":
		taskID := getStrFromParams(init, "task_id")
		title := getStrFromParams(init, "title")
		var runs []modals.RunRef
		if rows, ok := init["runs"].([]map[string]any); ok {
			for _, row := range rows {
				runs = append(runs, modals.RunRef{
					ID:      getStrFromParams(row, "id"),
					Status:  getStrFromParams(row, "status"),
					Summary: getStrFromParams(row, "summary"),
				})
			}
		}
		integ := getStrFromParams(init, "integration_state")
		prURL := getStrFromParams(init, "pr_url")
		prNum := 0
		if n, ok := init["pr_number"].(int); ok {
			prNum = n
		} else if f, ok := init["pr_number"].(float64); ok {
			prNum = int(f)
		}
		return modals.NewTaskDetail(taskID, title, runs, integ, prURL, prNum)
	}
	return nil
}
