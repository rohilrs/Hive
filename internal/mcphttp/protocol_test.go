package mcphttp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.JSONRPC != "2.0" || req.Method != "tools/list" {
		t.Errorf("decode mismatch: %+v", req)
	}
	if got := req.IntID(); got != 1 {
		t.Errorf("IntID=%d, want 1", got)
	}
}

func TestErrorCodes(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"method not found", ErrMethodNotFound},
		{"invalid params", ErrInvalidParams},
		{"internal error", ErrInternalError},
		{"server error", ErrServerCustomBase},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.code == 0 {
				t.Errorf("%s code is 0", c.name)
			}
		})
	}
}

func TestResponseAlwaysIncludesIDAndResult(t *testing.T) {
	// Even when ID is nil and Result is nil, both keys must appear
	// (with value null) per JSON-RPC 2.0 §5.
	resp := Response{JSONRPC: "2.0"}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, `"id":null`) {
		t.Errorf("Response missing id:null when ID nil: %s", got)
	}
	if !strings.Contains(got, `"result":null`) {
		t.Errorf("Response missing result:null when Result nil: %s", got)
	}
}

func TestErrorResponseAlwaysIncludesID(t *testing.T) {
	resp := ErrorResponse{JSONRPC: "2.0", Error: RPCError{Code: ErrParseError, Message: "parse error"}}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, `"id":null`) {
		t.Errorf("ErrorResponse missing id:null when ID nil: %s", got)
	}
}

func TestRequestIDOptionalForNotifications(t *testing.T) {
	// Request.ID keeps omitempty — JSON-RPC notifications legitimately
	// omit id.
	req := Request{JSONRPC: "2.0", Method: "tools/list"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"id":`) {
		t.Errorf("Request with no ID should omit the id field: %s", raw)
	}
}

func TestRequestIDRoundTripsAcrossTypes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // expected canonical form of ID after re-marshal
	}{
		{"number", `{"jsonrpc":"2.0","id":42,"method":"x"}`, `"id":42`},
		{"string", `{"jsonrpc":"2.0","id":"abc","method":"x"}`, `"id":"abc"`},
		{"null", `{"jsonrpc":"2.0","id":null,"method":"x"}`, `"id":null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req Request
			if err := json.Unmarshal([]byte(c.raw), &req); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), c.want) {
				t.Errorf("re-marshal lost id shape: got %s, want substring %s", raw, c.want)
			}
		})
	}
}
