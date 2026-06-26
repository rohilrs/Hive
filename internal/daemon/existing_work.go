package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/rohilrs/Hive/internal/store"
)

// maxExistingItems bounds how many existing items we feed into a prompt, so a
// large project can't blow the context window. Truncation is logged, never
// silent.
const maxExistingItems = 60

// metaString reads a string value from an opaque metadata map, returning "" when
// the key is absent or not a string.
func metaString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// ExistingItem is one piece of pre-existing open work for a project: either an
// open Hive task or an un-pulled open Linear issue. Ref is the stable token the
// decompose model echoes back in merge_from ("hive:<taskID>" / "linear:<uuid>").
type ExistingItem struct {
	Ref        string
	Kind       string // "hive_task" | "linear_issue"
	TaskID     string // set for hive_task
	SourceID   string // Linear issue UUID — set for linear_issue and mirrored hive_tasks
	ExternalID string // "HBA-32" when known
	Title      string
	Body       string
}

// gatherExistingWork returns the open Hive tasks plus un-pulled open Linear
// issues for a project. targetPhase "" (planner) keeps all candidates; a
// non-empty targetPhase (decompose) excludes tasks already stamped with a
// DIFFERENT roadmap_phase. Linear fetch errors are soft: we log and return just
// the Hive tasks. Capped at maxExistingItems (logged if truncated).
func (d *Daemon) gatherExistingWork(ctx context.Context, proj *store.Project, targetPhase string) ([]ExistingItem, error) {
	var items []ExistingItem

	tasks, err := d.store.ListPendingTasksByProject(ctx, proj.ID)
	if err != nil {
		return nil, fmt.Errorf("gather existing work: list tasks: %w", err)
	}
	for _, t := range tasks {
		if targetPhase != "" {
			if ph, _ := t.Metadata["roadmap_phase"].(string); ph != "" && ph != targetPhase {
				continue // belongs to another phase — not a candidate
			}
		}
		ext, _ := t.Metadata["external_id"].(string)
		items = append(items, ExistingItem{
			Ref: "hive:" + t.ID, Kind: "hive_task", TaskID: t.ID,
			SourceID: t.SourceID, ExternalID: ext, Title: t.Title, Body: t.Body,
		})
	}

	// Un-pulled open Linear issues. Soft-fail on any Linear error.
	if raw, bound := proj.Sources["linear"]; bound {
		if src, ok := d.sources["linear"]; ok {
			binding, _ := json.Marshal(raw)
			fetched, ferr := src.Fetch(ctx, proj.Slug, binding)
			if ferr != nil {
				log.Printf("gather existing work: linear fetch %s: %v (using hive tasks only)", proj.Slug, ferr)
			} else {
				existing, eerr := d.store.ListTasksBySource(ctx, proj.ID, "linear")
				if eerr != nil {
					log.Printf("gather existing work: list linear tasks %s: %v (skipping linear items)", proj.Slug, eerr)
				} else {
					pulled := map[string]bool{}
					for _, t := range existing {
						pulled[t.SourceID] = true
					}
					for _, it := range fetched {
						if it.State != "open" || pulled[it.SourceID] {
							continue
						}
						items = append(items, ExistingItem{
							Ref: "linear:" + it.SourceID, Kind: "linear_issue", SourceID: it.SourceID,
							ExternalID: it.Metadata["external_id"], Title: it.Title, Body: it.Body,
						})
					}
				}
			}
		}
	}

	if len(items) > maxExistingItems {
		log.Printf("gather existing work: %s has %d items; truncating to %d for the prompt", proj.Slug, len(items), maxExistingItems)
		items = items[:maxExistingItems]
	}
	return items, nil
}

// formatExistingWorkBlock renders items as one bullet per line for embedding in
// a decompose/planner prompt. Bodies are excerpted to keep the block compact.
func formatExistingWorkBlock(items []ExistingItem) string {
	var b strings.Builder
	for _, it := range items {
		ext := ""
		if it.ExternalID != "" {
			ext = " (" + it.ExternalID + ")"
		}
		excerpt := strings.ReplaceAll(it.Body, "\n", " ")
		const maxBodyBytes = 160
		if len(excerpt) > maxBodyBytes {
			cut := maxBodyBytes
			for cut > 0 && !utf8.ValidString(excerpt[:cut]) {
				cut--
			}
			excerpt = excerpt[:cut] + "…"
		}
		if excerpt != "" {
			fmt.Fprintf(&b, "- [%s]%s %s — %s\n", it.Ref, ext, it.Title, excerpt)
		} else {
			fmt.Fprintf(&b, "- [%s]%s %s\n", it.Ref, ext, it.Title)
		}
	}
	return b.String()
}
