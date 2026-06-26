// Package modals holds the TUI's modal-overlay forms (new project,
// new task, confirm abandon).
package modals

import tea "github.com/charmbracelet/bubbletea"

// Modal is the contract each modal implements. Root model dispatches
// to the active modal first when one is set.
type Modal interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Modal, tea.Cmd)
	View(width, height int) string
	Title() string
}

// CloseMsg signals the modal should be torn down. Returned by modals
// on cancel (esc) + on successful submit.
type CloseMsg struct{}

// SubmitRequest is emitted by a modal's submit. Root model routes it
// to the corresponding RPC via the Client.
type SubmitRequest struct {
	Kind   string // "new_project" / "new_task" / "abandon_run"
	Params map[string]any
}

// RPCResultMsg is forwarded from the root model after an RPC call
// completes. The originating modal Update consumes it to either
// close (success) or show an error (failure).
type RPCResultMsg struct {
	Kind string
	Err  error
	Data map[string]any
}
