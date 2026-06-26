package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/rohilrs/Hive/internal/roadmap"
	"github.com/rohilrs/Hive/internal/store"
)

// reconcileRoadmapStatus writes "✅ Done" into the roadmap markdown Status line
// of every derived-Complete phase whose Status isn't already a done-marker, for
// each sequenced project with a parseable roadmap. Best-effort, write-on-change;
// preserves existing done lines (provenance). Runs from reconcileOnce.
func (d *Daemon) reconcileRoadmapStatus(ctx context.Context) {
	projs, err := d.store.ListProjects(ctx, "") // "" = all projects
	if err != nil {
		log.Printf("roadmap reconcile: list projects: %v", err)
		return
	}
	for _, proj := range projs {
		if _, derr := d.store.GetSequenceDispatcher(ctx, proj.ID); derr != nil {
			if !errors.Is(derr, store.ErrNotFound) {
				log.Printf("roadmap reconcile: dispatcher %s: %v", proj.Slug, derr)
			}
			continue // not a sequenced project
		}
		rm, roadmapPath, lerr := d.loadProjectRoadmap(proj)
		if lerr != nil {
			continue // no/invalid roadmap — skip silently (normal for non-roadmap projects)
		}
		plan, perr := d.derivePlan(ctx, proj, rm)
		if perr != nil {
			log.Printf("roadmap reconcile: derive %s: %v", proj.Slug, perr)
			continue
		}
		// Read the roadmap branch-aware (working tree, else the feature/target
		// branch) so a shared repo checked out on another branch doesn't spam
		// read errors every tick.
		roadmapRel := "docs/superpowers/roadmaps/" + proj.Slug + ".md"
		body, ok := d.readProjectDoc(proj.Slug, *proj.RepoPath, roadmapRel)
		if !ok {
			continue
		}
		md := string(body)
		dirty := false
		for _, ph := range plan.Phases {
			if !ph.Complete || roadmap.PhaseStatusIsDone(md, ph.Number) {
				continue
			}
			if out, changed := roadmap.SetPhaseStatus(md, ph.Number, "✅ Done — marked complete via Hive"); changed {
				md = out
				dirty = true
			}
		}
		if dirty {
			// Only write back when the roadmap is in the WORKING TREE — we can't
			// commit to a non-checked-out feature branch from here, and writing to
			// the working tree would put the file on the wrong branch. Branch-only:
			// skip silently (the Status line is cosmetic; phase state lives in the DB).
			if _, statErr := os.Stat(roadmapPath); statErr != nil {
				continue
			}
			if werr := atomicWriteFile(roadmapPath, []byte(md), 0o644); werr != nil {
				log.Printf("roadmap reconcile: write %s: %v", roadmapPath, werr)
			}
		}
	}
}

// atomicWriteFile writes data to a temp file in the same directory then renames
// it over path, so a concurrent reader/writer never sees a torn or truncated
// file (and a crash mid-write can't leave the roadmap truncated). Same-dir
// rename is atomic on POSIX.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".roadmap-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
