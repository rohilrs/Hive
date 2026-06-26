package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	anth "github.com/anthropics/anthropic-sdk-go"
)

func fakeServer(t *testing.T, resp string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(resp))
	}))
}

const approveJSON = `{
  "id": "msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",
  "content":[{"type":"text","text":"{\"verdict\":\"APPROVE\",\"confidence\":92}"}],
  "stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}
}`

func TestClassifyVerdictApprove(t *testing.T) {
	srv := fakeServer(t, approveJSON)
	defer srv.Close()
	c := NewSDK(SDKConfig{APIKey: "test", BaseURL: srv.URL, Model: "claude-haiku-4-5"})
	v, err := c.ClassifyVerdict(context.Background(), "looks good")
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "APPROVE" {
		t.Errorf("verdict=%s", v.Verdict)
	}
	if v.Confidence < 90 {
		t.Errorf("confidence=%d", v.Confidence)
	}
}

const unclearJSON = `{
  "id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-4-5",
  "content":[{"type":"text","text":"unparseable"}],
  "stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":5}
}`

func TestClassifyVerdictUnparseableFallsBackToChangesRequested(t *testing.T) {
	srv := fakeServer(t, unclearJSON)
	defer srv.Close()
	c := NewSDK(SDKConfig{APIKey: "test", BaseURL: srv.URL, Model: "claude-haiku-4-5"})
	v, err := c.ClassifyVerdict(context.Background(), "ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "CHANGES_REQUESTED" {
		t.Errorf("verdict=%s", v.Verdict)
	}
}

const runTurnToolUseJSON = `{
  "id":"msg_3","type":"message","role":"assistant","model":"claude-sonnet-4-5",
  "content":[
    {"type":"text","text":"let me check"},
    {"type":"tool_use","id":"tu_1","name":"hive_status","input":{}}
  ],
  "stop_reason":"tool_use","usage":{"input_tokens":120,"output_tokens":30}
}`

func TestRunTurnToolUse(t *testing.T) {
	srv := fakeServer(t, runTurnToolUseJSON)
	defer srv.Close()
	c := NewSDK(SDKConfig{APIKey: "test", BaseURL: srv.URL})
	out, err := c.RunTurn(context.Background(), TurnInput{
		Model:  "claude-sonnet-4-5",
		System: "you are a chat agent",
		Messages: []anth.MessageParam{
			anth.NewUserMessage(anth.NewTextBlock("what is the status?")),
		},
		Tools: []ToolDef{
			{Name: "hive_status", Description: "Get daemon status", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "let me check") {
		t.Errorf("text=%q", out.Text)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("toolcalls=%d, want 1", len(out.ToolCalls))
	}
	if out.ToolCalls[0].ID != "tu_1" {
		t.Errorf("toolcall id=%q", out.ToolCalls[0].ID)
	}
	if out.ToolCalls[0].Name != "hive_status" {
		t.Errorf("toolcall name=%q", out.ToolCalls[0].Name)
	}
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason=%q", out.StopReason)
	}
	if out.TokensIn != 120 || out.TokensOut != 30 {
		t.Errorf("tokens in=%d out=%d", out.TokensIn, out.TokensOut)
	}
	if len(out.Assistant.Content) == 0 {
		t.Error("expected non-nil/non-empty assistant message param")
	}
}

const runTurnEndTurnJSON = `{
  "id":"msg_4","type":"message","role":"assistant","model":"claude-sonnet-4-5",
  "content":[{"type":"text","text":"all good"}],
  "stop_reason":"end_turn","usage":{"input_tokens":80,"output_tokens":10}
}`

func TestRunTurnEndTurn(t *testing.T) {
	srv := fakeServer(t, runTurnEndTurnJSON)
	defer srv.Close()
	c := NewSDK(SDKConfig{APIKey: "test", BaseURL: srv.URL})
	out, err := c.RunTurn(context.Background(), TurnInput{
		Model: "claude-sonnet-4-5",
		Messages: []anth.MessageParam{
			anth.NewUserMessage(anth.NewTextBlock("hi")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "all good" {
		t.Errorf("text=%q", out.Text)
	}
	if len(out.ToolCalls) != 0 {
		t.Errorf("toolcalls=%d, want 0", len(out.ToolCalls))
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason=%q", out.StopReason)
	}
}

// TestRunTurnSerializesRequiredFields guards against the tool-schema
// conversion bug where a JSON-shaped InputSchema (whose "required" key is a
// []any, not a []string) silently dropped the required fields on the wire,
// making every field look optional to the model.
func TestRunTurnSerializesRequiredFields(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(runTurnEndTurnJSON))
	}))
	defer srv.Close()
	s := NewSDK(SDKConfig{APIKey: "test", BaseURL: srv.URL})
	_, err := s.RunTurn(context.Background(), TurnInput{
		Model: "claude-haiku-4-5", MaxTokens: 64,
		Messages: []anth.MessageParam{anth.NewUserMessage(anth.NewTextBlock("hi"))},
		Tools: []ToolDef{{
			Name: "hive_get_task", Description: "get a task",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []any{"id"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotBody), `"required":["id"]`) {
		t.Errorf("required not serialized; body=%s", string(gotBody))
	}
}
