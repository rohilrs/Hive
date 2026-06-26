package daemon

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
)

// deriveLinearState maps a task's current Hive state to the desired logical
// Linear workflow state. Order matters: needs_attention is checked before the
// PR/running states so a failed task that already opened a PR shows Blocked,
// not In Review. See the spec's Status Derivation table. (Phase 1)
func deriveLinearState(t *store.Task) string {
	switch {
	case t.Status == "needs_attention":
		return "blocked"
	case t.GateState == sequence.GateSatisfied:
		return "done"
	case t.Status == "abandoned":
		return "canceled"
	case t.GateState == sequence.GatePROpen || t.GateState == sequence.GateAwaitingMerge:
		return "in_review"
	case t.Status == "running":
		return "in_progress"
	case t.Status == "done":
		return "done"
	default: // pending
		return "todo"
	}
}

// linearIssueWriter is the daemon's view of sources.LinearWriter (so tests can
// substitute a fake). Implemented by *sources.LinearWriter. Both methods take
// the team key (resolved to the team UUID internally), so the daemon never
// handles Linear team UUIDs. (Phase 1)
type linearIssueWriter interface {
	CreateIssue(ctx context.Context, teamKey, projectIDOrSlug, title, body, projectMilestoneID string) (id, identifier, url string, err error)
	SetIssueState(ctx context.Context, teamKey, issueID, logical string) error
	ArchiveIssue(ctx context.Context, issueID string) error
	UpdateIssueContent(ctx context.Context, teamKey, issueID, title, body string) error
	CreateDocument(ctx context.Context, projectIDOrSlug, title, content string) (string, error)
	UpdateDocument(ctx context.Context, docID, title, content string) error
	CreateProjectMilestone(ctx context.Context, projectIDOrSlug, name, description string, sortOrder float64) (string, error)
	UpdateProjectMilestone(ctx context.Context, msID, name, description string, sortOrder float64) error
	ArchiveProjectMilestone(ctx context.Context, msID string) error
	SetIssueMilestone(ctx context.Context, issueID, milestoneID string) error
	// FetchIssueMeta reads a single issue's human identifier ("HBA-42") and
	// canonical branchName by its UUID — used by the roadmap-apply path to
	// enrich a freshly-pulled task with the metadata the syncer otherwise stamps.
	FetchIssueMeta(ctx context.Context, issueID string) (identifier, branchName string, err error)
}

// linearWriteTarget reads a project's linear binding and returns the write-back
// create target (team key, project id/slug) when write_back is enabled. ok=false
// when the project has no linear binding or write_back is off. Resolves the
// single-team/single-project default and the WBTeam/WBProject overrides. (Phase 1)
func linearWriteTarget(proj *store.Project) (teamKey, projectID string, ok bool) {
	raw, bound := proj.Sources["linear"]
	if !bound {
		return "", "", false
	}
	var lb struct {
		Teams     []string `json:"teams"`
		Projects  []string `json:"projects"`
		WriteBack bool     `json:"write_back"`
		WBTeam    string   `json:"wb_team"`
		WBProject string   `json:"wb_project"`
	}
	bj, _ := json.Marshal(raw)
	if err := json.Unmarshal(bj, &lb); err != nil || !lb.WriteBack {
		return "", "", false
	}
	teamKey = lb.WBTeam
	if teamKey == "" && len(lb.Teams) == 1 {
		teamKey = lb.Teams[0]
	}
	projectID = lb.WBProject
	if projectID == "" && len(lb.Projects) == 1 {
		projectID = lb.Projects[0]
	}
	if teamKey == "" || projectID == "" {
		return "", "", false // ambiguous; bind-time validation should have caught it
	}
	return teamKey, projectID, true
}

// reconcileLoop is the daemon's single reconcile loop (Phase 2). Each tick:
// (1) detectMerges — the general PR-merge detector for ALL dispatch modes (feeds
// the sequenced gate-FSM); (2)+(3) the Linear write-back outbox + backfill for
// write-back projects. Ticks at the merge-poll cadence so sequenced phase
// advancement stays fast; the diff-gated outbox does not spam Linear. Runs even
// with no Linear writer (the inbox is a general dispatch concern).
func (d *Daemon) reconcileLoop(ctx context.Context) {
	iv := d.cfg.Cfg.Scheduler.ResolvedMergePollInterval()
	if iv <= 0 {
		iv = 30 * time.Second // never disable the unified loop (it hosts the outbox)
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reconcileOnce(ctx)
		case <-d.kickMerge:
			// Low-latency wake-up: a task just parked at awaiting_merge. Run only
			// the merge-detection/queue pass (not the full reconcile + Linear
			// outbox) so a merge starts well before the 30s poll would fire. The
			// ticker remains the backstop for anything a dropped kick missed.
			d.detectMerges(ctx)
		}
	}
}

// reconcileOnce runs one reconcile pass across all write-back projects:
// (1) BACKFILL pending Hive-originated (source_id=="") tasks -> create a Linear
// issue and mark them mirrored; (2) OUTBOX: for each mirrored task, push the
// derived logical state if it differs from linear_synced_state. Best-effort:
// per-task errors are logged and skipped, never aborting the pass. (Phase 1)
func (d *Daemon) reconcileOnce(ctx context.Context) {
	// INBOX: general merge detection (all dispatch modes) — runs regardless of
	// Linear write-back, since it drives the sequenced gate-FSM. (Phase 2)
	d.detectMerges(ctx)

	// ROADMAP STATUS: write-back "✅ Done" into roadmap markdown for any
	// derived-Complete phase whose Status line isn't already a done-marker.
	// Runs for all sequenced projects, independent of Linear write-back.
	d.reconcileRoadmapStatus(ctx)

	// OUTBOX + BACKFILL: Linear write-back, write-back projects only. (Phase 1)
	if d.linearWriter == nil {
		return
	}
	projs, err := d.store.ListProjects(ctx, "") // "" = all projects
	if err != nil {
		log.Printf("linear write-back: list projects: %v", err)
		return
	}
	for _, proj := range projs {
		teamKey, projectID, ok := linearWriteTarget(proj)
		if !ok {
			continue
		}
		tasks, err := d.store.ListTasksByProject(ctx, proj.ID)
		if err != nil {
			log.Printf("linear write-back: list tasks %s: %v", proj.Slug, err)
			continue
		}
		for _, task := range tasks {
			// BACKFILL: pending, Hive-originated, unmirrored.
			if task.SourceID == "" && task.Status == "pending" {
				issueID, identifier, _, cerr := d.linearWriter.CreateIssue(ctx, teamKey, projectID, task.Title, task.Body, "")
				if cerr != nil {
					log.Printf("linear write-back: backfill %s: %v", task.ID, cerr)
					continue
				}
				if err := d.store.SetTaskLinearMirror(ctx, task.ID, issueID, identifier); err != nil {
					log.Printf("linear write-back: persist mirror %s: %v", task.ID, err)
					continue
				}
				task.Source = "linear"
				task.SourceID = issueID
				// A pending task derives "todo"; set synced_state to "todo" so the
				// immediate OUTBOX pass below is a no-op (no redundant push).
				task.LinearSyncedState = "todo"
				_ = d.store.UpdateTaskLinearSyncedState(ctx, task.ID, "todo")
			}
			if task.Source != "linear" || task.SourceID == "" {
				continue
			}
			// OUTBOX: push the derived logical state on diff.
			desired := deriveLinearState(task)
			if desired == task.LinearSyncedState {
				continue
			}
			if err := d.linearWriter.SetIssueState(ctx, teamKey, task.SourceID, desired); err != nil {
				log.Printf("linear write-back: set state %s->%s: %v", task.ID, desired, err)
				continue
			}
			if err := d.store.UpdateTaskLinearSyncedState(ctx, task.ID, desired); err != nil {
				log.Printf("linear write-back: persist synced_state %s: %v", task.ID, err)
			}
		}
	}
}
