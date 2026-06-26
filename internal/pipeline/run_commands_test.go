package pipeline

import (
	"testing"
	"time"
)

func TestRunEffCommands(t *testing.T) {
	// nil Commands -> the pipeline Cfg defaults pass through.
	r := &Run{}
	if cmd, to := r.EffTest("go test ./...", 5*time.Minute); cmd != "go test ./..." || to != 5*time.Minute {
		t.Errorf("nil Commands EffTest = (%q,%v), want default", cmd, to)
	}
	if cmd, _ := r.EffValidate("go build ./...", 0); cmd != "go build ./..." {
		t.Errorf("nil Commands EffValidate = %q, want default", cmd)
	}
	// Set Commands -> override wins (incl. timeout).
	r.Commands = &RunCommands{Test: "npm test", TestTimeout: 2 * time.Minute, Validate: "npm run build"}
	if cmd, to := r.EffTest("go test ./...", 5*time.Minute); cmd != "npm test" || to != 2*time.Minute {
		t.Errorf("override EffTest = (%q,%v), want (npm test, 2m)", cmd, to)
	}
	if cmd, _ := r.EffValidate("go build ./...", 0); cmd != "npm run build" {
		t.Errorf("override EffValidate = %q, want npm run build", cmd)
	}
	// Explicit empty override -> empty (skip the stage), NOT the default.
	r.Commands = &RunCommands{Test: ""}
	if cmd, _ := r.EffTest("go test ./...", 5*time.Minute); cmd != "" {
		t.Errorf("explicit-empty EffTest = %q, want \"\" (skip)", cmd)
	}
}
