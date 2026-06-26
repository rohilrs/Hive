package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/doctor"
)

func TestRenderJSONMatchesReportShape(t *testing.T) {
	rep := doctor.Report{
		Checks: []doctor.Check{
			{Name: "daemon.pidfile", Subsystem: "daemon", Status: doctor.StatusOK, Message: "alive"},
			{Name: "config.api_key", Subsystem: "config", Status: doctor.StatusSkip, Message: "skipped"},
		},
		Summary: doctor.Summary{OK: 1, Skipped: 1},
	}
	var buf bytes.Buffer
	if err := renderJSON(&buf, rep); err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	var got doctor.Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Checks) != 2 {
		t.Errorf("Checks=%d, want 2", len(got.Checks))
	}
	if got.Summary.OK != 1 || got.Summary.Skipped != 1 {
		t.Errorf("Summary=%+v, want OK=1 Skipped=1", got.Summary)
	}
}

func TestRenderHumanHidesOKChecksByDefault(t *testing.T) {
	rep := doctor.Report{
		Checks: []doctor.Check{
			{Name: "daemon.pidfile", Subsystem: "daemon", Status: doctor.StatusOK, Message: "alive"},
			{Name: "daemon.last_tick", Subsystem: "daemon", Status: doctor.StatusWarn, Message: "20s stale"},
		},
		Summary: doctor.Summary{OK: 1, Warnings: 1},
	}
	var buf bytes.Buffer
	renderHuman(&buf, rep, false) // verbose=false
	got := buf.String()
	if strings.Contains(got, "daemon.pidfile") {
		t.Errorf("non-verbose output unexpectedly contains OK check: %s", got)
	}
	if !strings.Contains(got, "daemon.last_tick") {
		t.Errorf("non-verbose output missing warn check: %s", got)
	}
}

func TestRenderHumanShowsOKChecksWhenVerbose(t *testing.T) {
	rep := doctor.Report{
		Checks: []doctor.Check{
			{Name: "daemon.pidfile", Subsystem: "daemon", Status: doctor.StatusOK, Message: "alive"},
		},
		Summary: doctor.Summary{OK: 1},
	}
	var buf bytes.Buffer
	renderHuman(&buf, rep, true) // verbose=true
	got := buf.String()
	if !strings.Contains(got, "daemon.pidfile") {
		t.Errorf("verbose output missing OK check: %s", got)
	}
}

func TestExitCodeFromReportNoErrorsNoStrict(t *testing.T) {
	rep := doctor.Report{Summary: doctor.Summary{OK: 5, Warnings: 2}}
	if code := exitCodeFromReport(rep, false); code != 0 {
		t.Errorf("2 warnings non-strict: code=%d, want 0", code)
	}
}

func TestExitCodeFromReportNoErrorsStrict(t *testing.T) {
	rep := doctor.Report{Summary: doctor.Summary{OK: 5, Warnings: 2}}
	if code := exitCodeFromReport(rep, true); code != 2 {
		t.Errorf("2 warnings strict: code=%d, want 2", code)
	}
}

func TestExitCodeFromReportErrors(t *testing.T) {
	rep := doctor.Report{Summary: doctor.Summary{OK: 1, Errors: 1}}
	if code := exitCodeFromReport(rep, false); code != 1 {
		t.Errorf("1 error: code=%d, want 1", code)
	}
}

func TestRenderHumanMultiLineHint(t *testing.T) {
	rep := doctor.Report{
		Checks: []doctor.Check{
			{Name: "worktrees.orphans", Subsystem: "worktrees", Status: doctor.StatusWarn,
				Message: "2 orphan dirs", Hint: "rm -rf path/a\nrm -rf path/b"},
		},
		Summary: doctor.Summary{Warnings: 1},
	}
	var buf bytes.Buffer
	renderHuman(&buf, rep, false)
	got := buf.String()
	if !strings.Contains(got, "rm -rf path/a") || !strings.Contains(got, "rm -rf path/b") {
		t.Errorf("multi-line hint not rendered properly: %s", got)
	}
}

func TestRenderJSONIncludesSkipStatus(t *testing.T) {
	rep := doctor.Report{
		Checks: []doctor.Check{
			{Name: "mcp.http_listener", Subsystem: "mcp", Status: doctor.StatusSkip, Message: "not enabled", Hint: ""},
		},
		Summary: doctor.Summary{Skipped: 1},
	}
	var buf bytes.Buffer
	if err := renderJSON(&buf, rep); err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	var got doctor.Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Checks[0].Status != doctor.StatusSkip {
		t.Errorf("skip status lost in JSON round-trip: %s", got.Checks[0].Status)
	}
}

func TestRenderHumanSubsystemHiddenWhenAllOK(t *testing.T) {
	rep := doctor.Report{
		Checks: []doctor.Check{
			{Name: "store.db_readable", Subsystem: "store", Status: doctor.StatusOK},
			{Name: "store.schema_version", Subsystem: "store", Status: doctor.StatusOK},
			{Name: "daemon.pidfile", Subsystem: "daemon", Status: doctor.StatusError, Message: "stale"},
		},
		Summary: doctor.Summary{OK: 2, Errors: 1},
	}
	var buf bytes.Buffer
	renderHuman(&buf, rep, false)
	got := buf.String()
	if strings.Contains(got, "store") {
		t.Errorf("non-verbose: all-OK subsystem 'store' should be hidden, got: %s", got)
	}
	if !strings.Contains(got, "daemon") {
		t.Errorf("non-verbose: subsystem 'daemon' with error should be shown")
	}
}

func TestRenderHumanVerboseShowsOKWithinWarnSubsystem(t *testing.T) {
	rep := doctor.Report{
		Checks: []doctor.Check{
			{Name: "daemon.pidfile", Subsystem: "daemon", Status: doctor.StatusOK, Message: "alive"},
			{Name: "daemon.last_tick", Subsystem: "daemon", Status: doctor.StatusWarn, Message: "20s"},
		},
		Summary: doctor.Summary{OK: 1, Warnings: 1},
	}
	var buf bytes.Buffer
	renderHuman(&buf, rep, true) // verbose
	got := buf.String()
	if !strings.Contains(got, "daemon.pidfile") || !strings.Contains(got, "daemon.last_tick") {
		t.Errorf("verbose should show both: %s", got)
	}
}
