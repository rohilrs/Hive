package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/decompose"
)

func sampleDecomposeResult() *decompose.Result {
	return &decompose.Result{
		Model:        "claude-sonnet-4-6",
		CostUSD:      0.041,
		InputTokens:  1800,
		OutputTokens: 720,
		Subtasks: []decompose.ProposedSubtask{
			{Title: "Migration 0017 + InsertSubtasks", Body: "Add the column + helper.", Priority: "P0", Pipeline: "build"},
			{Title: "internal/decompose package", Body: "Sonnet tool call + validation.", Priority: "P1", Pipeline: "build"},
		},
	}
}

func TestRenderProposalContainsExpectedLines(t *testing.T) {
	var buf bytes.Buffer
	renderDecomposeProposal(&buf, "task-1", "ship phase 7", "hive", sampleDecomposeResult())
	got := buf.String()
	if !strings.Contains(got, "task-1") {
		t.Error("missing task id")
	}
	if !strings.Contains(got, "ship phase 7") {
		t.Error("missing task title")
	}
	if !strings.Contains(got, "$0.041") {
		t.Error("missing cost")
	}
	if !strings.Contains(got, "claude-sonnet-4-6") {
		t.Error("missing model")
	}
	if !strings.Contains(got, "Migration 0017") {
		t.Errorf("missing subtask 1 title: %s", got)
	}
	if !strings.Contains(got, "P0") || !strings.Contains(got, "P1") {
		t.Error("missing priority labels")
	}
}

func TestPromptYesNoYesAcceptsLowercaseY(t *testing.T) {
	in := strings.NewReader("y\n")
	if got := promptYesNo(in); !got {
		t.Errorf("got %v, want true for 'y'", got)
	}
}

func TestPromptYesNoUppercaseYesAccepts(t *testing.T) {
	in := strings.NewReader("YES\n")
	if got := promptYesNo(in); !got {
		t.Errorf("got %v, want true for 'YES'", got)
	}
}

func TestPromptYesNoEmptyDefaultsNo(t *testing.T) {
	in := strings.NewReader("\n")
	if got := promptYesNo(in); got {
		t.Errorf("got %v, want false for empty input", got)
	}
}

func TestPromptYesNoNRejects(t *testing.T) {
	in := strings.NewReader("n\n")
	if got := promptYesNo(in); got {
		t.Errorf("got %v, want false for 'n'", got)
	}
}
