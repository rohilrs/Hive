// Package sources ingests external issues (GitHub, Linear) and local
// markdown (inbox) into normalized SourceItems for the daemon to reconcile
// into tasks. Provider-neutral: sources may import internal/store, but
// internal/store must never import internal/sources.
package sources

import (
	"context"
	"encoding/json"
)

// SourceItem is the normalized, provider-agnostic shape of one upstream item.
type SourceItem struct {
	SourceID string   // stable upstream id (GH issue number, Linear id, inbox filename)
	Title    string
	Body     string
	Labels   []string // upstream labels; used for pipeline mapping
	State    string   // "open" | "closed"
	Priority string   // "" = use default
	// Metadata is provider-specific kv. Persisted onto task.metadata when
	// the syncer inserts. Reserved cross-source keys (populated by the
	// Linear source today; other sources leave the map nil for now):
	//   "branch_name"          — preferred branch for runs (Linear's branchName)
	//   "external_id"          — provider's human identifier (Linear "HBA-42")
	//   "linear_project_id"    — Linear project this issue belongs to
	//   "linear_project_name"
	//   "linked_github_url"    — the GH issue URL when this item is linked
	// Allocated lazily — nil when no keys would be set.
	Metadata map[string]string
	// LinkedGitHub is non-nil when this item links to a GitHub issue
	// (Linear's attachments field includes a GH-typed attachment). The
	// syncer uses this for cross-source dedup against pre-existing GH
	// tasks for the same project.
	LinkedGitHub *LinkedGitHubRef
}

// Source fetches the current items for one project's binding to this source.
type Source interface {
	Name() string // "github" | "linear" | "inbox"
	// Fetch returns ALL current items (open + closed) for the binding so the
	// reconciler can detect closures. projectSlug is supplied for sources
	// that key off it (inbox dir); API sources may ignore it. binding is the
	// project's sources[Name()] JSON object.
	Fetch(ctx context.Context, projectSlug string, binding json.RawMessage) ([]SourceItem, error)
}

// LinkedGitHubRef captures a GitHub issue link Linear has attached to an
// issue. Parsed from URLs like https://github.com/owner/repo/issues/42
// (also accepts /pull/N paths).
type LinkedGitHubRef struct {
	Owner    string
	Repo     string
	IssueNum int
	URL      string
}
