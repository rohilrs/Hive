package chat

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/codeintel"
)

func TestPlannerCodeTools_SearchNilGrounderDegrades(t *testing.T) {
	tools := NewPlannerCodeTools(nil)
	res := tools.SearchCode(context.Background(), json.RawMessage(`{"query":"x"}`))
	if !res.IsError || !strings.Contains(res.Content, "unavailable") {
		t.Fatalf("nil grounder should degrade, got %+v", res)
	}
}

func TestPlannerCodeTools_SearchReturnsHits(t *testing.T) {
	repo := codeintelRepo(t)
	g := codeintel.NewGrounder(repo, "main", "", "scavenger", false, 0)
	tools := NewPlannerCodeTools(g)
	res := tools.SearchCode(context.Background(), json.RawMessage(`{"query":"func Alpha"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "foo.go") || !strings.Contains(res.Content, `"ref":"main"`) {
		t.Errorf("search result missing file/ref: %s", res.Content)
	}
}

func TestPlannerCodeTools_CapsuleDisabledIsAvailableFalse(t *testing.T) {
	repo := codeintelRepo(t)
	g := codeintel.NewGrounder(repo, "main", "", "scavenger", false, 0) // disabled
	tools := NewPlannerCodeTools(g)
	res := tools.QueryCapsule(context.Background(), json.RawMessage(`{"file":"foo.go"}`))
	if res.IsError {
		t.Fatalf("disabled capsule must not be an error result: %s", res.Content)
	}
	if !strings.Contains(res.Content, `"available":false`) {
		t.Errorf("want available:false, got %s", res.Content)
	}
}

func TestNewPlannerRegistry_RegistersCodeTools(t *testing.T) {
	reg := NewPlannerRegistry("/tmp/x", nil, "", nil)
	for _, name := range []string{"hive_search_code", "hive_query_capsule"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("registry missing %s", name)
		}
	}
}

func TestCapStr(t *testing.T) {
	if s, trunc := capStr("hello", 10); trunc || s != "hello" {
		t.Errorf("short string: got %q trunc=%v", s, trunc)
	}
	if s, trunc := capStr("hello world", 5); !trunc || s != "hello…" {
		t.Errorf("long string: got %q trunc=%v, want \"hello…\" true", s, trunc)
	}
}

func TestPlannerCodeTools_SearchEmptyQueryErrors(t *testing.T) {
	g := codeintel.NewGrounder(codeintelRepo(t), "main", "", "scavenger", false, 0)
	res := NewPlannerCodeTools(g).SearchCode(context.Background(), json.RawMessage(`{"query":""}`))
	if !res.IsError {
		t.Errorf("empty query should error: %s", res.Content)
	}
}

func TestPlannerCodeTools_CapsuleEmptyFileErrors(t *testing.T) {
	g := codeintel.NewGrounder(codeintelRepo(t), "main", "", "scavenger", false, 0)
	res := NewPlannerCodeTools(g).QueryCapsule(context.Background(), json.RawMessage(`{}`))
	if !res.IsError {
		t.Errorf("empty file should error: %s", res.Content)
	}
}

func codeintelRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t.t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package p\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}
