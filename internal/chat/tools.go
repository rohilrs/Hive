// Package chat implements the daemon-hosted conversational agent loop for
// Hive (Phase 6). It is provider-neutral and offline-testable: it may import
// internal/anthropic (for ToolDef/TurnInput shapes) but MUST NOT import
// internal/daemon. The daemon supplies tool handlers and the cost function at
// the composition root; the loop itself is daemon-agnostic.
package chat

import (
	"context"
	"encoding/json"

	"github.com/rohilrs/Hive/internal/anthropic"
)

// ToolResult is the outcome of executing one tool handler.
type ToolResult struct {
	Content string // text fed back to the model (usually JSON)
	IsError bool
}

// Tool is a registered tool: its provider-neutral definition, a Mutating flag
// (used in later sub-phases to gate confirm-before-execute), and the handler
// the agent invokes when the model requests it.
type Tool struct {
	Def      anthropic.ToolDef
	Mutating bool
	Handler  func(ctx context.Context, input json.RawMessage) ToolResult
}

// Registry is a name-keyed set of tools. It is daemon-agnostic; the daemon
// constructs and populates it at the composition root.
type Registry struct{ tools map[string]Tool }

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds (or replaces) a tool, keyed by its definition name.
func (r *Registry) Register(t Tool) { r.tools[t.Def.Name] = t }

// Get looks up a tool by name.
func (r *Registry) Get(name string) (Tool, bool) { t, ok := r.tools[name]; return t, ok }

// Defs returns the tool definitions to advertise to the model.
func (r *Registry) Defs() []anthropic.ToolDef {
	defs := make([]anthropic.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Def)
	}
	return defs
}
