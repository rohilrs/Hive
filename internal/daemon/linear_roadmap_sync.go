package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohilrs/Hive/internal/roadmap"
	"github.com/rohilrs/Hive/internal/store"
)

// mirrorState is the Linear-mirror identity map persisted under
// project.sources["linear"]["mirror"]: the roadmap document id + a phase-number
// → milestone-id map. Lets re-syncs update in place instead of duplicating.
type mirrorState struct {
	DocumentID string            `json:"document_id"`
	Milestones map[string]string `json:"milestones"`
}

// loadMirrorState reads the mirror map from a project's linear binding. Returns
// a zero-value (empty map) state when absent or malformed.
func loadMirrorState(proj *store.Project) mirrorState {
	ms := mirrorState{Milestones: map[string]string{}}
	lin, ok := proj.Sources["linear"].(map[string]any)
	if !ok {
		return ms
	}
	raw, ok := lin["mirror"]
	if !ok {
		return ms
	}
	b, _ := json.Marshal(raw)
	_ = json.Unmarshal(b, &ms)
	if ms.Milestones == nil {
		ms.Milestones = map[string]string{}
	}
	return ms
}

// saveMirrorState writes the mirror map into the project's linear binding and
// persists via UpdateProjectSources. Copies the maps before mutating so the
// caller's proj.Sources isn't aliased mid-flight, then updates proj.Sources to
// the persisted value so the in-memory project stays coherent.
func (d *Daemon) saveMirrorState(ctx context.Context, proj *store.Project, ms mirrorState) error {
	sources := make(map[string]any, len(proj.Sources)+1)
	for k, v := range proj.Sources {
		sources[k] = v
	}
	lin := map[string]any{}
	if existing, ok := sources["linear"].(map[string]any); ok {
		for k, v := range existing {
			lin[k] = v
		}
	}
	lin["mirror"] = map[string]any{"document_id": ms.DocumentID, "milestones": ms.Milestones}
	sources["linear"] = lin
	if err := d.store.UpdateProjectSources(ctx, proj.ID, sources); err != nil {
		return err
	}
	proj.Sources = sources
	return nil
}

// milestoneForTask returns the milestone id for a task's roadmap phase, or "".
func milestoneForTask(proj *store.Project, meta map[string]any) string {
	// meta may be nil; reading a nil map returns the zero value (no panic).
	phase, _ := meta["roadmap_phase"].(string)
	if phase == "" {
		return ""
	}
	return loadMirrorState(proj).Milestones[phase]
}

// phaseMilestoneName / phaseMilestoneDescription derive the Linear milestone
// name + description from a parsed roadmap phase.
func phaseMilestoneName(p roadmap.Phase) string {
	return fmt.Sprintf("Phase %s — %s", p.Number, p.Title)
}

func phaseMilestoneDescription(p roadmap.Phase) string {
	summary := ""
	for _, line := range strings.Split(p.Body, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			summary = s
			break
		}
	}
	if len(p.SpecPaths) > 0 {
		if summary != "" {
			summary += "\n\n"
		}
		summary += "Spec: " + p.SpecPaths[0]
	}
	return summary
}

// projectSlugOrUUID returns the bound Linear project id/slug to create entities
// in (the same project the issue write-back targets).
func projectSlugOrUUID(proj *store.Project) string {
	if _, projectID, ok := linearWriteTarget(proj); ok {
		return projectID
	}
	return ""
}

// syncRoadmapToLinear is the idempotent reconcile: parse the project's roadmap,
// upsert the Linear document + per-phase milestones (keyed by phase number),
// archive orphaned milestones, backfill issue→milestone links, and persist the
// id map. Best-effort: per-call Linear failures are collected and logged; the id
// map is persisted with whatever succeeded so the next run resumes.
func (d *Daemon) syncRoadmapToLinear(ctx context.Context, proj *store.Project) error {
	if d.linearWriter == nil {
		return nil
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return fmt.Errorf("sync roadmap: project %q has no repo_path", proj.Slug)
	}
	roadmapPath := filepath.Join(*proj.RepoPath, "docs", "superpowers", "roadmaps", proj.Slug+".md")
	b, err := os.ReadFile(roadmapPath)
	if err != nil {
		return fmt.Errorf("sync roadmap: read %s: %w", roadmapPath, err)
	}
	rm, err := roadmap.Parse(b)
	if err != nil {
		return fmt.Errorf("sync roadmap: parse %s: %w", roadmapPath, err)
	}

	ms := loadMirrorState(proj)
	var firstErr error
	note := func(e error) {
		if e != nil {
			log.Printf("sync roadmap %s: %v", proj.Slug, e)
			if firstErr == nil {
				firstErr = e
			}
		}
	}
	projTarget := projectSlugOrUUID(proj)
	if projTarget == "" {
		return fmt.Errorf("sync roadmap: project %q has no resolvable Linear project target (not write-back bound)", proj.Slug)
	}
	title := proj.Name + " Roadmap"

	// 1. Document.
	if ms.DocumentID != "" {
		if e := d.linearWriter.UpdateDocument(ctx, ms.DocumentID, title, string(b)); e != nil {
			id, ce := d.linearWriter.CreateDocument(ctx, projTarget, title, string(b))
			if ce != nil {
				note(fmt.Errorf("document update+recreate: %v / %v", e, ce))
			} else {
				ms.DocumentID = id
			}
		}
	} else {
		if id, e := d.linearWriter.CreateDocument(ctx, projTarget, title, string(b)); e != nil {
			note(fmt.Errorf("document create: %v", e))
		} else {
			ms.DocumentID = id
		}
	}

	// 2. Milestones (upsert, keyed by phase number; sortOrder = roadmap order).
	current := map[string]bool{}
	for i, ph := range rm.Phases {
		current[ph.Number] = true
		name, desc, order := phaseMilestoneName(ph), phaseMilestoneDescription(ph), float64(i)
		if id := ms.Milestones[ph.Number]; id != "" {
			if e := d.linearWriter.UpdateProjectMilestone(ctx, id, name, desc, order); e != nil {
				note(fmt.Errorf("milestone update %s: %v", ph.Number, e))
			}
		} else {
			if id, e := d.linearWriter.CreateProjectMilestone(ctx, projTarget, name, desc, order); e != nil {
				note(fmt.Errorf("milestone create %s: %v", ph.Number, e))
			} else {
				ms.Milestones[ph.Number] = id
			}
		}
	}

	// 3. Archive orphans.
	for number, id := range ms.Milestones {
		if current[number] {
			continue
		}
		if e := d.linearWriter.ArchiveProjectMilestone(ctx, id); e != nil {
			note(fmt.Errorf("milestone archive %s: %v", number, e))
			continue
		}
		log.Printf("sync roadmap %s: phase %s removed; archived milestone %s", proj.Slug, number, id)
		delete(ms.Milestones, number)
	}

	// 4. Backfill issue→milestone links.
	tasks, terr := d.store.ListTasksByProject(ctx, proj.ID)
	if terr != nil {
		note(fmt.Errorf("list tasks: %v", terr))
	} else {
		for _, t := range tasks {
			if t.Source != "linear" || t.SourceID == "" {
				continue
			}
			phase, _ := t.Metadata["roadmap_phase"].(string)
			if phase == "" {
				continue
			}
			if id := ms.Milestones[phase]; id != "" {
				if e := d.linearWriter.SetIssueMilestone(ctx, t.SourceID, id); e != nil {
					note(fmt.Errorf("link issue %s: %v", t.SourceID, e))
				}
			}
		}
	}

	// 5. Persist the id map.
	if e := d.saveMirrorState(ctx, proj, ms); e != nil {
		note(fmt.Errorf("persist mirror map: %v", e))
	}
	return firstErr
}
