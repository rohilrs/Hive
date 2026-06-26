package rpc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunViewJSONShape(t *testing.T) {
	v := RunView{
		ID:                   "run-3f2a",
		TaskID:               "task-42",
		ProjectID:            "billing",
		Pipeline:             "build",
		Status:               RunStatusRunning,
		StartedAt:            time.Unix(1700000000, 0).UTC(),
		TotalCostUSD:         0.31,
		DocumentationSkipped: false,
	}
	raw, _ := json.Marshal(v)
	s := string(raw)
	for _, key := range []string{`"id"`, `"task_id"`, `"pipeline"`, `"status"`, `"total_cost_usd"`} {
		if !strings.Contains(s, key) {
			t.Errorf("missing JSON key %s in %s", key, s)
		}
	}
}

func TestTaskViewStatusEnum(t *testing.T) {
	for _, st := range []TaskStatus{
		TaskStatusPending, TaskStatusRunning, TaskStatusDone,
		TaskStatusNeedsAttention, TaskStatusAbandoned,
	} {
		if st == "" {
			t.Errorf("empty TaskStatus constant")
		}
	}
}

func TestEventMessageTypes(t *testing.T) {
	for _, et := range []EventType{
		EventTaskCreated, EventTaskUpdated, EventRunStarted, EventRunUpdated,
		EventStageStarted, EventStageEnded, EventApprovalRequested,
		EventApprovalResolved, EventStallDetected, EventResync,
	} {
		if et == "" {
			t.Errorf("empty EventType constant")
		}
	}
}

func TestEventMessageRoundTrip(t *testing.T) {
	msg := EventMessage{
		Type: EventStageStarted,
		Data: map[string]any{"run_id": "r-1", "stage": "implement"},
	}
	raw, _ := json.Marshal(msg)
	var out EventMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != EventStageStarted {
		t.Errorf("type=%s", out.Type)
	}
}
