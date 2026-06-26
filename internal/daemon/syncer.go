package daemon

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rohilrs/Hive/internal/sources"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// RegisterSource makes a source available to the syncer (composition root).
func (d *Daemon) RegisterSource(s sources.Source) { d.sources[s.Name()] = s }

// pipelineForLabels maps a hive:<pipeline> label to a registered pipeline,
// falling back to "build".
func (d *Daemon) pipelineForLabels(labels []string) string {
	for _, l := range labels {
		if p, ok := strings.CutPrefix(l, "hive:"); ok && d.HasPipeline(p) {
			return p
		}
	}
	return "build"
}

// SyncReport summarizes one Sync call, keyed by source name. When a source is
// bound to multiple projects its counts are source-wide aggregates and Error
// holds the last failing project's error (per-project detail is in the daemon
// log). For a solo dev with one project per source this is exact.
type SyncReport struct {
	PerSource map[string]*SourceResult `json:"per_source"`
}
type SourceResult struct {
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Closed   int    `json:"closed"`
	Error    string `json:"error,omitempty"` // last failing project for this source
}

// Sync ingests all bound sources for all active projects. sourceFilter != ""
// limits to that one source. A per-source fetch error is recorded and skipped,
// never aborting other sources/projects.
func (d *Daemon) Sync(ctx context.Context, sourceFilter string) *SyncReport {
	rep := &SyncReport{PerSource: map[string]*SourceResult{}}
	res := func(name string) *SourceResult {
		if rep.PerSource[name] == nil {
			rep.PerSource[name] = &SourceResult{}
		}
		return rep.PerSource[name]
	}
	projects, err := d.store.ListProjects(ctx, "active")
	if err != nil {
		log.Printf("sync: list projects: %v", err)
		return rep
	}
	for _, p := range projects {
		for name, src := range d.sources {
			if sourceFilter != "" && name != sourceFilter {
				continue
			}
			binding, bound := p.Sources[name]
			if !bound {
				continue
			}
			// Ensure the source appears in the report once it's bound +
			// attempted, so a synced-but-unchanged source reads "+0/~0/-0"
			// rather than being mistaken for "no bound sources".
			r := res(name)
			bindingJSON, _ := json.Marshal(binding)
			items, ferr := src.Fetch(ctx, p.Slug, bindingJSON)
			if ferr != nil {
				r.Error = ferr.Error()
				log.Printf("sync: %s/%s fetch: %v", p.Slug, name, ferr)
				continue
			}
			// Symmetric cross-source dedup: whichever of github/linear syncs
			// first stays canonical; the other side filters items the first
			// already owns. Order no longer matters — see dedupAgainstGitHub
			// and dedupAgainstLinear for direction details. Other sources
			// pass through.
			switch name {
			case "linear":
				items = d.dedupAgainstGitHub(ctx, p, items)
			case "github":
				items = d.dedupAgainstLinear(ctx, p, items)
			}
			existing, eerr := d.store.ListTasksBySource(ctx, p.ID, name)
			if eerr != nil {
				r.Error = eerr.Error()
				continue
			}
			for _, op := range sources.Reconcile(existing, items) {
				d.applyOp(ctx, p, name, op, r)
			}
		}
	}
	d.recordSyncState(rep)
	return rep
}

// syncState records the last sync time + result per source (manual or polled).
type syncState struct {
	mu         sync.Mutex
	lastSync   map[string]time.Time
	lastResult map[string]*SourceResult
}

// SourceStatusEntry is one source's recorded state for sources.status.
type SourceStatusEntry struct {
	LastSyncUnix int64  `json:"last_sync_unix"`
	Inserted     int    `json:"inserted"`
	Updated      int    `json:"updated"`
	Closed       int    `json:"closed"`
	Error        string `json:"error,omitempty"`
}

// SourceStatus returns a snapshot of per-source last-sync state.
func (d *Daemon) SourceStatus() map[string]SourceStatusEntry {
	d.syncStatus.mu.Lock()
	defer d.syncStatus.mu.Unlock()
	out := map[string]SourceStatusEntry{}
	for name, ts := range d.syncStatus.lastSync {
		e := SourceStatusEntry{LastSyncUnix: ts.Unix()}
		if r := d.syncStatus.lastResult[name]; r != nil {
			e.Inserted, e.Updated, e.Closed, e.Error = r.Inserted, r.Updated, r.Closed, r.Error
		}
		out[name] = e
	}
	return out
}

func (d *Daemon) recordSyncState(rep *SyncReport) {
	d.syncStatus.mu.Lock()
	defer d.syncStatus.mu.Unlock()
	now := time.Now()
	for name, r := range rep.PerSource {
		d.syncStatus.lastSync[name] = now
		d.syncStatus.lastResult[name] = r
	}
}

// dueSources returns source names whose interval has elapsed since their last
// sync (or that have never synced). interval <= 0 disables a source.
func dueSources(now time.Time, lastSync map[string]time.Time, intervals map[string]time.Duration) []string {
	var due []string
	for name, iv := range intervals {
		if iv <= 0 {
			continue
		}
		last, ok := lastSync[name]
		if !ok || now.Sub(last) >= iv {
			due = append(due, name)
		}
	}
	return due
}

func (d *Daemon) sourceIntervals() map[string]time.Duration {
	m := map[string]time.Duration{}
	s := d.cfg.Cfg.Sources
	if _, ok := d.sources["inbox"]; ok {
		m["inbox"] = time.Duration(s.Inbox.SyncIntervalMinutes) * time.Minute
	}
	if _, ok := d.sources["github"]; ok {
		m["github"] = time.Duration(s.GitHub.SyncIntervalMinutes) * time.Minute
	}
	if _, ok := d.sources["linear"]; ok {
		m["linear"] = time.Duration(s.Linear.SyncIntervalMinutes) * time.Minute
	}
	return m
}

// syncLoop polls bound sources on their configured intervals until ctx is done.
// Intervals are snapshotted at start (config + source registration are fixed at
// daemon construction; restart to change them — no hot-reload).
func (d *Daemon) syncLoop(ctx context.Context) {
	intervals := d.sourceIntervals()
	allZero := true
	for _, iv := range intervals {
		if iv > 0 {
			allZero = false
		}
	}
	if allZero {
		return
	}
	const base = 30 * time.Second
	t := time.NewTicker(base)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.syncStatus.mu.Lock()
			ls := make(map[string]time.Time, len(d.syncStatus.lastSync))
			for k, v := range d.syncStatus.lastSync {
				ls[k] = v
			}
			d.syncStatus.mu.Unlock()
			for _, name := range dueSources(time.Now(), ls, intervals) {
				d.Sync(ctx, name)
				// Stamp the poll clock on every ATTEMPT, not just when a
				// binding produced a result — otherwise a registered-but-
				// unbound source (github/linear are registered by default)
				// would never record a sync and re-fire every tick.
				d.markSynced(name)
			}
		}
	}
}

// markSynced advances a source's poll clock after a sync attempt.
func (d *Daemon) markSynced(name string) {
	d.syncStatus.mu.Lock()
	defer d.syncStatus.mu.Unlock()
	d.syncStatus.lastSync[name] = time.Now()
}

// dedupAgainstGitHub drops Linear items whose LinkedGitHub points at a
// GitHub issue that is already imported as a Hive task (source="github")
// for the same project.
//
// Cross-repo safety: the gh source writes a BARE issue number as
// tasks.source_id (e.g. "42"), not "owner/repo#42" — so the issue number
// alone isn't a globally unique key. We constrain matches to the project's
// own gh binding's repo: only when the Linear attachment's owner/repo
// matches the bound repo does the IssueNum collision count as a dup.
// Items pointing at any OTHER repo are different work; they pass through.
//
// Fails open: any error loading the project binding or existing source-ids
// returns items unfiltered (better to risk a duplicate task than to drop
// every Linear-ingested item because of a transient store error).
func (d *Daemon) dedupAgainstGitHub(ctx context.Context, p *store.Project, items []sources.SourceItem) []sources.SourceItem {
	ghRepo := projectGitHubRepo(p)
	if ghRepo == "" {
		return items // no gh binding on this project → nothing to dedup against
	}
	existing, err := d.store.ListTaskSourceIDsForProject(ctx, p.ID, "github")
	if err != nil {
		log.Printf("sync: dedup load existing github tasks: %v", err)
		return items
	}
	out := items[:0]
	for _, it := range items {
		if it.LinkedGitHub == nil {
			out = append(out, it)
			continue
		}
		linkedRepo := it.LinkedGitHub.Owner + "/" + it.LinkedGitHub.Repo
		if linkedRepo != ghRepo {
			// Linear attachment points at a DIFFERENT repo than the bound
			// gh source — different work, not a dup.
			out = append(out, it)
			continue
		}
		key := strconv.Itoa(it.LinkedGitHub.IssueNum)
		if existing[key] {
			// Already imported via gh source — skip.
			continue
		}
		out = append(out, it)
	}
	return out
}

// dedupAgainstLinear is the reverse direction of dedupAgainstGitHub: it
// drops github items whose issue number is referenced by an existing
// Linear task's metadata.linked_github_url (i.e. Linear already owns
// this work item with its richer metadata — branch_name, identifier,
// project_id). Together they make cross-source dedup symmetric so
// whichever source syncs first stays canonical; the other side just
// doesn't double-import.
//
// Cross-repo safety: same constraint as dedupAgainstGitHub. We only
// dedup when the Linear metadata's linked URL points at the project's
// own gh binding's repo — Linear tasks linking to a DIFFERENT repo are
// different work and pass through.
//
// Fails open: store/parse errors return items unfiltered. Better to
// risk a duplicate than to drop legitimate gh items because of a
// transient store error or a malformed linked_github_url.
func (d *Daemon) dedupAgainstLinear(ctx context.Context, p *store.Project, items []sources.SourceItem) []sources.SourceItem {
	ghRepo := projectGitHubRepo(p)
	if ghRepo == "" {
		return items // no gh binding on this project → nothing to dedup
	}
	linearTasks, err := d.store.ListTasksBySource(ctx, p.ID, "linear")
	if err != nil {
		log.Printf("sync: dedup load existing linear tasks: %v", err)
		return items
	}
	// Build a set of {bare-issue-num : bool} keyed off Linear tasks whose
	// linked_github_url maps to THIS project's gh repo. The gh source
	// writes a bare number (e.g. "42") as source_id, so the matching key
	// is the same shape.
	refs := map[string]bool{}
	for _, t := range linearTasks {
		if t.Metadata == nil {
			continue
		}
		urlStr, _ := t.Metadata["linked_github_url"].(string)
		if urlStr == "" {
			continue
		}
		ref := sources.ParseGitHubIssueURL(urlStr)
		if ref == nil {
			continue
		}
		if ref.Owner+"/"+ref.Repo != ghRepo {
			continue
		}
		refs[strconv.Itoa(ref.IssueNum)] = true
	}
	if len(refs) == 0 {
		return items
	}
	out := items[:0]
	for _, it := range items {
		if refs[it.SourceID] {
			continue // Linear already owns this work item — skip.
		}
		out = append(out, it)
	}
	return out
}

// projectGitHubRepo reads the "owner/repo" string from a project's
// sources["github"]["repo"] binding. Returns "" when there's no github
// binding, the binding isn't an object, or "repo" is missing/non-string.
//
// Mirrors the ghBinding shape in internal/sources/github.go without
// importing it (avoids a cycle and keeps the daemon source-agnostic).
func projectGitHubRepo(p *store.Project) string {
	if p == nil || p.Sources == nil {
		return ""
	}
	raw, ok := p.Sources["github"]
	if !ok {
		return ""
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	repo, ok := obj["repo"].(string)
	if !ok {
		return ""
	}
	return repo
}

// applyMetadataToTask copies SourceItem.Metadata into a fresh task's
// Metadata map. No-op (leaves t.Metadata nil) when the source supplied no
// metadata; InsertTask defaults nil to an empty map for the JSON column.
func applyMetadataToTask(t *store.Task, kv map[string]string) {
	if len(kv) == 0 {
		return
	}
	t.Metadata = make(map[string]any, len(kv))
	for k, v := range kv {
		t.Metadata[k] = v
	}
}

func (d *Daemon) applyOp(ctx context.Context, p *store.Project, source string, op sources.Op, r *SourceResult) {
	switch op.Kind {
	case sources.OpInsert:
		t := &store.Task{
			ID: newID("task"), ProjectID: p.ID, Source: source, SourceID: op.Item.SourceID,
			Title: op.Item.Title, Body: op.Item.Body, Priority: op.Item.Priority,
			Status: "pending", Pipeline: d.pipelineForLabels(op.Item.Labels),
		}
		applyMetadataToTask(t, op.Item.Metadata)
		if err := d.store.InsertTask(ctx, t); err != nil {
			log.Printf("sync: insert: %v", err)
			return
		}
		r.Inserted++
		d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskCreated, Data: map[string]any{
			"task_id": t.ID, "project_id": p.ID, "title": t.Title, "pipeline": t.Pipeline, "source": source,
		}})
	case sources.OpUpdate:
		if err := d.store.UpdateTaskContent(ctx, op.TaskID, op.Item.Title, op.Item.Body); err != nil {
			log.Printf("sync: update: %v", err)
			return
		}
		// Merge any new SourceItem.Metadata onto the existing task. Merge
		// (not replace) preserves keys we wrote on insert but that the
		// upstream provider didn't re-send this round (e.g. a Linear sync
		// that drops branchName intermittently mustn't blank it).
		if len(op.Item.Metadata) > 0 {
			if err := d.store.MergeTaskMetadata(ctx, op.TaskID, op.Item.Metadata); err != nil {
				log.Printf("sync: merge metadata: %v", err)
				// Soft-fail: title/body update already landed.
			}
		}
		r.Updated++
		d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskUpdated, Data: map[string]any{"task_id": op.TaskID}})
	case sources.OpClose:
		if err := d.store.MarkTaskSourceClosed(ctx, op.TaskID); err != nil {
			log.Printf("sync: close: %v", err)
			return
		}
		r.Closed++
		d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskUpdated, Data: map[string]any{"task_id": op.TaskID, "status": "source_closed"}})
	}
}
