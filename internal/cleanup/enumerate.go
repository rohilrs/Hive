package cleanup

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnumerateDirs discovers per-run artifact dirs under hiveDir: scratch dirs
// (hiveDir/run-*) and worktrees (hiveDir/worktrees/run-*), keyed by run ID.
func EnumerateDirs(hiveDir string) ([]DirInfo, error) {
	byRun := map[string]*DirInfo{}
	get := func(name string) *DirInfo {
		di := byRun[name]
		if di == nil {
			di = &DirInfo{RunID: name}
			byRun[name] = di
		}
		return di
	}
	later := func(a, b time.Time) time.Time {
		if b.After(a) {
			return b
		}
		return a
	}

	ents, err := os.ReadDir(hiveDir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "run-") {
			di := get(e.Name())
			di.Scratch = filepath.Join(hiveDir, e.Name())
			if info, err := e.Info(); err == nil {
				di.Mtime = later(di.Mtime, info.ModTime())
			}
		}
	}
	wtRoot := filepath.Join(hiveDir, "worktrees")
	if wents, err := os.ReadDir(wtRoot); err == nil {
		for _, e := range wents {
			if e.IsDir() && strings.HasPrefix(e.Name(), "run-") {
				di := get(e.Name())
				di.Worktree = filepath.Join(wtRoot, e.Name())
				if info, err := e.Info(); err == nil {
					di.Mtime = later(di.Mtime, info.ModTime())
				}
			}
		}
	}

	out := make([]DirInfo, 0, len(byRun))
	for _, di := range byRun {
		out = append(out, *di)
	}
	return out, nil
}
