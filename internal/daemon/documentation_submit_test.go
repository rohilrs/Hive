package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestHandleDocumentationSubmitEmitsEvent(t *testing.T) {
	hiveDir := t.TempDir()
	cfg, err := config.Load(config.LoadOptions{ConfigDir: hiveDir, SkipEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.TUI.HeartbeatSeconds = 0

	d, err := New(Config{HiveDir: hiveDir, Cfg: cfg, Adapter: minimalAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewRPCServer(d)

	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	params := json.RawMessage(`{"run_id":"run-x","stage":"document","summary":"Added Multiply/Divide docs","files_changed":["CHANGELOG.md","mathutil/mathutil.go"],"changelog_entry":"## Unreleased\n- add Multiply/Divide"}`)
	out, rpcErr := srv.handleDocumentationSubmit(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("handleDocumentationSubmit returned error: %+v", rpcErr)
	}

	var res struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !res.OK {
		t.Errorf("result ok=false, want true")
	}

	select {
	case ev := <-ch:
		if ev.Type != rpc.EventDocumentationSubmitted {
			t.Fatalf("got event type=%v want %v", ev.Type, rpc.EventDocumentationSubmitted)
		}
		if got, _ := ev.Data["run_id"].(string); got != "run-x" {
			t.Errorf("event run_id=%q want run-x", got)
		}
		if got, _ := ev.Data["summary"].(string); got != "Added Multiply/Divide docs" {
			t.Errorf("event summary=%q want 'Added Multiply/Divide docs'", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no documentation.submitted event published within 2s")
	}
}
