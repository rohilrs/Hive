package rpc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMethodConstantsAreUnique(t *testing.T) {
	methods := []string{
		MethodListTasks, MethodGetTask, MethodAddTask, MethodEditTask,
		MethodPredict, MethodDecompose, MethodRunNow, MethodActiveWorkers,
		MethodGetRun, MethodResume, MethodAbandon, MethodAttachRun,
		MethodCostSummary, MethodStatus, MethodApprove, MethodDeny,
		MethodSearch, MethodShowDiff, MethodSubscribeEvents,
		MethodTaskFinish, MethodResolveNow,
	}
	seen := make(map[string]bool)
	for _, m := range methods {
		if seen[m] {
			t.Errorf("duplicate method constant: %s", m)
		}
		seen[m] = true
	}
}

type sampleParams struct {
	Project string `json:"project"`
}

func TestRequestEnvelopeRoundTrip(t *testing.T) {
	req := Request[sampleParams]{
		ID:     "req-1",
		Method: MethodListTasks,
		Params: sampleParams{Project: "auth-service"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var out Request[sampleParams]
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Method != MethodListTasks {
		t.Errorf("method=%s", out.Method)
	}
	if out.Params.Project != "auth-service" {
		t.Errorf("project=%s", out.Params.Project)
	}
}

func TestResponseEnvelopeError(t *testing.T) {
	resp := Response[sampleParams]{
		ID:    "req-1",
		Error: &RPCError{Code: ErrInvalidParams, Message: "bad input"},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	var out Response[sampleParams]
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != ErrInvalidParams {
		t.Errorf("error not preserved: %+v", out.Error)
	}
}

func TestResponseEnvelopeOmitsResultOnError(t *testing.T) {
	resp := Response[sampleParams]{
		ID:    "req-1",
		Error: &RPCError{Code: ErrInvalidParams, Message: "bad input"},
	}
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), `"result"`) {
		t.Errorf("error response should not contain 'result' key, got: %s", string(raw))
	}
}
