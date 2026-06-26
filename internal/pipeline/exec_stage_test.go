package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunShellStageSuccess(t *testing.T) {
	out, ok, err := RunShellStage(context.Background(), "echo hello", t.TempDir(), 5*time.Second, 1024)
	if err != nil {
		t.Fatalf("runShellStage: %v", err)
	}
	if !ok {
		t.Errorf("ok=false; want true")
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output %q missing 'hello'", out)
	}
}

func TestRunShellStageFailure(t *testing.T) {
	out, ok, err := RunShellStage(context.Background(), "echo failmsg; exit 7", t.TempDir(), 5*time.Second, 1024)
	if err != nil {
		t.Fatalf("runShellStage: %v (output=%q)", err, out)
	}
	if ok {
		t.Errorf("ok=true; want false (exit 7)")
	}
	if !strings.Contains(out, "failmsg") {
		t.Errorf("output %q missing 'failmsg'", out)
	}
}

func TestRunShellStageTruncatesLongOutput(t *testing.T) {
	out, _, err := RunShellStage(context.Background(), "yes | head -c 5000", t.TempDir(), 5*time.Second, 100)
	if err != nil {
		t.Fatalf("runShellStage: %v", err)
	}
	if len(out) > 300 {
		t.Errorf("len(out)=%d want <= 300 (100 + truncation marker)", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("output missing truncation marker: %q", out)
	}
}

func TestRunShellStageRespectsTimeout(t *testing.T) {
	start := time.Now()
	_, ok, _ := RunShellStage(context.Background(), "sleep 10", t.TempDir(), 200*time.Millisecond, 1024)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("took %s; timeout didn't fire", elapsed)
	}
	if ok {
		t.Errorf("ok=true; want false (killed)")
	}
}

func TestRunShellStageRespectsCwd(t *testing.T) {
	dir := t.TempDir()
	out, _, err := RunShellStage(context.Background(), "pwd", dir, 5*time.Second, 1024)
	if err != nil {
		t.Fatalf("runShellStage: %v", err)
	}
	if !strings.Contains(out, dir) && !strings.Contains(out, lastPathComponent(dir)) {
		t.Errorf("pwd=%q want cwd=%s", out, dir)
	}
}

func lastPathComponent(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func TestRunShellStageKeepsTail(t *testing.T) {
	got, ok, err := RunShellStage(context.Background(),
		`for i in $(seq 1 1000); do echo "line $i"; done; echo "FAILURE_AT_TAIL"; exit 1`,
		t.TempDir(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(got, "FAILURE_AT_TAIL") {
		t.Errorf("tail (the failure) must survive truncation; got:\n%s", got)
	}
	if !strings.Contains(got, "truncated head") {
		t.Errorf("expected a head-truncation marker; got:\n%s", got)
	}
}
