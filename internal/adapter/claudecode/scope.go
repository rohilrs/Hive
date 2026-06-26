package claudecode

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ScopeRequest struct {
	StageDir string
	RealHome string
	Skills   []string

	// RestrictPermissions (Phase 4.5) writes a worker settings.json with
	// an EMPTY permissions.allow (preserving the source deny list +
	// other keys) instead of symlinking the user's settings.json. With
	// approvals on, this forces Claude Code to route every non-denied
	// tool through the --permission-prompt-tool gate rather than
	// auto-allowing from the user's personal allow-list.
	RestrictPermissions bool
}

type ScopeInfo struct {
	StageHome string
}

func MaterializeScope(req ScopeRequest) (*ScopeInfo, error) {
	if req.StageDir == "" {
		return nil, fmt.Errorf("stage_dir required")
	}
	if req.RealHome == "" {
		return nil, fmt.Errorf("real_home required")
	}

	stageHome := filepath.Join(req.StageDir, "home")
	dotClaude := filepath.Join(stageHome, ".claude")

	// Create the dir if it doesn't exist yet, but do NOT RemoveAll — that
	// would nuke claude-managed subdirs like .claude/projects/ which hold
	// session JSONL files that --resume depends on. We only clean the
	// entries we own (the symlinks / restricted-settings file) before
	// recreating them so the subsequent Symlink calls don't hit EEXIST.
	if err := os.MkdirAll(dotClaude, 0700); err != nil {
		return nil, fmt.Errorf("mkdir .claude: %w", err)
	}

	for _, f := range []string{".credentials.json", "settings.json", "CLAUDE.md"} {
		dst := filepath.Join(dotClaude, f)
		// Remove whatever we put here on a prior call (symlink or restricted
		// settings file) so we can recreate cleanly. Ignore ENOENT.
		_ = os.Remove(dst)

		src := filepath.Join(req.RealHome, ".claude", f)
		// Phase 4.5: when restricting permissions, materialize a modified
		// settings.json (empty allow-list) instead of symlinking the
		// user's, so the permission-prompt-tool gate is actually consulted.
		if f == "settings.json" && req.RestrictPermissions {
			if err := writeRestrictedSettings(src, dst); err != nil {
				return nil, fmt.Errorf("restricted settings: %w", err)
			}
			continue
		}
		if _, err := os.Stat(src); err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("stat %s: %w", src, err)
			}
			continue
		}
		if err := os.Symlink(src, dst); err != nil {
			return nil, fmt.Errorf("symlink %s: %w", f, err)
		}
	}

	// For the skills dir we own all contents — RemoveAll is safe here.
	skillsDst := filepath.Join(dotClaude, "skills")
	_ = os.RemoveAll(skillsDst)
	if err := os.MkdirAll(skillsDst, 0700); err != nil {
		return nil, fmt.Errorf("mkdir skills: %w", err)
	}
	for _, name := range req.Skills {
		src := resolveSkillSource(req.RealHome, name)
		if src == "" {
			// A skill is optional context, not a hard dependency — skip a
			// missing one with a warning rather than aborting the whole run.
			log.Printf("scope: skill %q not found under ~/.claude/skills or the plugin cache — skipping", name)
			continue
		}
		if err := os.Symlink(src, filepath.Join(skillsDst, skillDirName(name))); err != nil {
			return nil, fmt.Errorf("symlink skill %s: %w", name, err)
		}
	}
	return &ScopeInfo{StageHome: stageHome}, nil
}

// skillDirName is the on-disk directory name for a (possibly plugin-namespaced)
// skill — the bare name after any "plugin:" prefix, so the worker can invoke it
// by the same bare name the pipeline requested.
func skillDirName(name string) string {
	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// resolveSkillSource finds the on-disk directory backing a skill by name.
// User-authored skills in ~/.claude/skills take precedence; otherwise we search
// the Claude Code plugin cache
// (~/.claude/plugins/cache/<repo>/<plugin>/<version>/skills/<name>), where
// plugin-provided skills like the superpowers set actually live — they are NOT
// copied into ~/.claude/skills, which is why scoping previously skipped them and
// pipeline workers ran without brainstorming/writing-plans/etc. When a plugin
// ships multiple cached versions, the highest version wins. Returns "" if no
// match is found.
func resolveSkillSource(realHome, name string) string {
	bare := skillDirName(name)
	if user := filepath.Join(realHome, ".claude", "skills", bare); isDir(user) {
		return user
	}
	matches, _ := filepath.Glob(filepath.Join(realHome, ".claude", "plugins", "cache", "*", "*", "*", "skills", bare))
	best, bestVer := "", ""
	for _, m := range matches {
		if !isDir(m) {
			continue
		}
		// .../<version>/skills/<bare> → the version dir is two levels above <bare>.
		ver := filepath.Base(filepath.Dir(filepath.Dir(m)))
		if best == "" || compareVersions(ver, bestVer) > 0 {
			best, bestVer = m, ver
		}
	}
	return best
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// compareVersions compares dotted version strings numerically per segment
// ("6.0.10" > "6.0.2", which a lexical compare gets wrong). Non-numeric or
// missing segments compare as 0. Returns -1, 0, or 1.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// writeRestrictedSettings reads the source settings.json (if any),
// forces permissions.allow to empty (preserving permissions.deny and all
// other keys like enabledPlugins), and writes the result to dst. With an
// empty allow-list + Claude Code's default permission mode, every
// non-denied tool is routed to the --permission-prompt-tool gate. The
// deny list is kept so safety rules (e.g. .env reads) still hard-block
// regardless of the gate's decision.
func writeRestrictedSettings(src, dst string) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(src); err == nil {
		if uerr := json.Unmarshal(raw, &doc); uerr != nil {
			return fmt.Errorf("parse source settings.json: %w", uerr)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read source settings.json: %w", err)
	}
	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	perms["allow"] = []string{}
	doc["permissions"] = perms
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, out, 0600)
}
