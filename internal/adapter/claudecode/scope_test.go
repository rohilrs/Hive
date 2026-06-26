package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func makeFakeUserClaude(t *testing.T) string {
	t.Helper()
	realHome := t.TempDir()
	realClaude := filepath.Join(realHome, ".claude")
	_ = os.MkdirAll(realClaude, 0755)
	_ = os.WriteFile(filepath.Join(realClaude, ".credentials.json"), []byte(`{"oauth":"fake"}`), 0600)
	_ = os.WriteFile(filepath.Join(realClaude, "settings.json"), []byte(`{}`), 0644)
	_ = os.WriteFile(filepath.Join(realClaude, "CLAUDE.md"), []byte("# user"), 0644)

	skillsDir := filepath.Join(realClaude, "skills")
	_ = os.MkdirAll(skillsDir, 0755)
	for _, name := range []string{"review-code", "test-engineer", "should-not-appear"} {
		s := filepath.Join(skillsDir, name)
		_ = os.MkdirAll(s, 0755)
		_ = os.WriteFile(filepath.Join(s, "SKILL.md"), []byte("# "+name), 0644)
	}
	return realHome
}

func TestMaterializeScopeSymlinksRequestedSkills(t *testing.T) {
	realHome := makeFakeUserClaude(t)
	stageDir := t.TempDir()

	info, err := MaterializeScope(ScopeRequest{
		StageDir: stageDir,
		RealHome: realHome,
		Skills:   []string{"review-code", "test-engineer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.StageHome != filepath.Join(stageDir, "home") {
		t.Errorf("StageHome=%s", info.StageHome)
	}

	creds := filepath.Join(info.StageHome, ".claude", ".credentials.json")
	if target, err := os.Readlink(creds); err != nil {
		t.Errorf("creds not a symlink: %v", err)
	} else if target != filepath.Join(realHome, ".claude", ".credentials.json") {
		t.Errorf("creds target=%s", target)
	}

	for _, name := range []string{"review-code", "test-engineer"} {
		if _, err := os.Lstat(filepath.Join(info.StageHome, ".claude", "skills", name)); err != nil {
			t.Errorf("skill %s missing: %v", name, err)
		}
	}
	forbidden := filepath.Join(info.StageHome, ".claude", "skills", "should-not-appear")
	if _, err := os.Lstat(forbidden); !os.IsNotExist(err) {
		t.Errorf("forbidden skill present at %s", forbidden)
	}
}

func TestMaterializeScopeSkipsUnknownSkill(t *testing.T) {
	// A missing skill is optional context, not a hard dependency — it is
	// skipped with a warning so one absent skill can't abort a whole run.
	realHome := makeFakeUserClaude(t)
	stageDir := t.TempDir()
	info, err := MaterializeScope(ScopeRequest{
		StageDir: stageDir, RealHome: realHome,
		Skills: []string{"review-code", "nonexistent-skill"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The present skill is still symlinked; the missing one simply absent.
	if _, err := os.Stat(filepath.Join(info.StageHome, ".claude", "skills", "review-code")); err != nil {
		t.Errorf("present skill review-code should be symlinked: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(info.StageHome, ".claude", "skills", "nonexistent-skill")); !os.IsNotExist(err) {
		t.Errorf("missing skill should not be symlinked, got err=%v", err)
	}
}

// addPluginSkill writes a fake plugin-cache skill at
// <home>/.claude/plugins/cache/<repo>/<plugin>/<version>/skills/<name>.
func addPluginSkill(t *testing.T, home, repo, plugin, version, name string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "plugins", "cache", repo, plugin, version, "skills", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+name+" "+version), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeScopeResolvesPluginCacheSkills(t *testing.T) {
	realHome := makeFakeUserClaude(t)
	// Superpowers skills live ONLY in the plugin cache, across two versions.
	addPluginSkill(t, realHome, "claude-plugins-official", "superpowers", "6.0.0", "writing-plans")
	addPluginSkill(t, realHome, "claude-plugins-official", "superpowers", "6.0.2", "writing-plans")
	stageDir := t.TempDir()

	info, err := MaterializeScope(ScopeRequest{
		StageDir: stageDir, RealHome: realHome,
		Skills: []string{"writing-plans"},
	})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(info.StageHome, ".claude", "skills", "writing-plans")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("plugin-cache skill not symlinked: %v", err)
	}
	// Highest version must win (6.0.2 > 6.0.0, and 6.0.2 vs 6.0.10-style lexical traps).
	if filepath.Base(filepath.Dir(filepath.Dir(target))) != "6.0.2" {
		t.Errorf("expected the 6.0.2 skill, got %s", target)
	}
}

func TestMaterializeScopeUserSkillBeatsPluginCache(t *testing.T) {
	realHome := makeFakeUserClaude(t) // review-code exists under ~/.claude/skills
	addPluginSkill(t, realHome, "claude-plugins-official", "superpowers", "9.9.9", "review-code")
	stageDir := t.TempDir()

	info, err := MaterializeScope(ScopeRequest{
		StageDir: stageDir, RealHome: realHome, Skills: []string{"review-code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(info.StageHome, ".claude", "skills", "review-code"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(realHome, ".claude", "skills", "review-code") {
		t.Errorf("user skill should win over plugin cache, got %s", target)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"6.0.2", "6.0.10", -1},
		{"6.0.10", "6.0.2", 1},
		{"6.0.0", "6.0.0", 0},
		{"6.1.0", "6.0.9", 1},
		{"5.1.0", "6.0.2", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMaterializeScopeIdempotent(t *testing.T) {
	realHome := makeFakeUserClaude(t)
	stageDir := t.TempDir()
	req := ScopeRequest{StageDir: stageDir, RealHome: realHome, Skills: []string{"review-code"}}
	if _, err := MaterializeScope(req); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeScope(req); err != nil {
		t.Fatal("second materialize failed:", err)
	}
}

func TestMaterializeScopePreservesClaudeProjects(t *testing.T) {
	realHome := t.TempDir()
	realClaude := filepath.Join(realHome, ".claude")
	if err := os.MkdirAll(realClaude, 0700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".credentials.json", "settings.json", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(realClaude, f), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stage := t.TempDir()

	// First materialize — sets up the symlinks.
	if _, err := MaterializeScope(ScopeRequest{StageDir: stage, RealHome: realHome}); err != nil {
		t.Fatalf("first materialize: %v", err)
	}

	// Simulate claude writing a session JSONL into .claude/projects/...
	projectsDir := filepath.Join(stage, "home", ".claude", "projects", "hashed-cwd")
	if err := os.MkdirAll(projectsDir, 0700); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(projectsDir, "abc-session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"role":"user","content":"hi"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Second materialize — must NOT delete the session JSONL.
	if _, err := MaterializeScope(ScopeRequest{StageDir: stage, RealHome: realHome}); err != nil {
		t.Fatalf("second materialize: %v", err)
	}

	if _, err := os.Stat(sessionFile); err != nil {
		t.Errorf("second materialize destroyed claude's session file: %v", err)
	}
	// Symlinks still point at the right targets.
	target, err := os.Readlink(filepath.Join(stage, "home", ".claude", ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(realHome, ".claude", ".credentials.json") {
		t.Errorf("symlink target wrong after re-materialize: %s", target)
	}
}

func TestMaterializeScopeRestrictsPermissions(t *testing.T) {
	realHome := t.TempDir()
	realClaude := filepath.Join(realHome, ".claude")
	_ = os.MkdirAll(realClaude, 0755)
	// User settings.json with a permissive allow-list + a deny rule.
	_ = os.WriteFile(filepath.Join(realClaude, "settings.json"),
		[]byte(`{"permissions":{"allow":["Bash(git *)","Write","Edit"],"deny":["Read(**/.env)"]},"enabledPlugins":{"x":true}}`), 0644)

	stageDir := t.TempDir()
	if _, err := MaterializeScope(ScopeRequest{StageDir: stageDir, RealHome: realHome, RestrictPermissions: true}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(stageDir, "home", ".claude", "settings.json")
	raw, err := os.ReadFile(out) // must be a real file, not a symlink to the permissive one
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Permissions.Allow) != 0 {
		t.Errorf("allow should be emptied; got %v", doc.Permissions.Allow)
	}
	if len(doc.Permissions.Deny) != 1 {
		t.Errorf("deny should be preserved; got %v", doc.Permissions.Deny)
	}
	if !doc.EnabledPlugins["x"] {
		t.Errorf("enabledPlugins should be preserved; got %v", doc.EnabledPlugins)
	}
}
