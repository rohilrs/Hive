package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newLinearStub starts an httptest.Server that records the raw JSON request body
// into *gotBody and always responds with respJSON regardless of the query.
func newLinearStub(t *testing.T, gotBody *string, respJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respJSON))
	}))
}

// newTestWriter returns a *LinearWriter pointed at srv with a dummy API key,
// using the test server's HTTP client.
func newTestWriter(t *testing.T, srv *httptest.Server) *LinearWriter {
	t.Helper()
	return &LinearWriter{Endpoint: srv.URL, apiKeyOverride: "k", HTTPClient: srv.Client()}
}

// mockLinear serves canned GraphQL responses keyed by a substring of the query.
func mockLinear(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for substr, resp := range routes {
			if strings.Contains(body.Query, substr) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(resp))
				return
			}
		}
		t.Errorf("unexpected query: %s", body.Query)
		http.Error(w, "no route", 500)
	}))
}

func TestResolveTeam(t *testing.T) {
	srv := mockLinear(t, map[string]string{
		"teams(filter": `{"data":{"teams":{"nodes":[{"id":"team-uuid","states":{"nodes":[
			{"id":"s1","name":"Todo","type":"unstarted"},
			{"id":"s2","name":"In Progress","type":"started"},
			{"id":"s3","name":"In Review","type":"started"},
			{"id":"s4","name":"Done","type":"completed"}]}}]}}}`,
	})
	defer srv.Close()
	w := &LinearWriter{Endpoint: srv.URL, apiKeyOverride: "k", HTTPClient: srv.Client()}
	id, states, err := w.resolveTeam(context.Background(), "CONV")
	if err != nil {
		t.Fatal(err)
	}
	if id != "team-uuid" {
		t.Errorf("team id = %q, want team-uuid", id)
	}
	if len(states) != 4 || states[1].Name != "In Progress" || states[1].Type != "started" {
		t.Errorf("states = %+v", states)
	}
	// Second call must hit the cache (server would error on an unexpected re-query;
	// here it would still answer, so assert the cache map is populated instead).
	if _, ok := w.teamCache["CONV"]; !ok {
		t.Error("resolveTeam did not cache the team")
	}
}

func TestCreateIssue(t *testing.T) {
	srv := mockLinear(t, map[string]string{
		"teams(filter":    `{"data":{"teams":{"nodes":[{"id":"team-uuid","states":{"nodes":[]}}]}}}`,
		"projects(filter": `{"data":{"projects":{"nodes":[{"id":"proj-uuid","slugId":"57925d22","name":"Conv"}]}}}`,
		"issueCreate":     `{"data":{"issueCreate":{"success":true,"issue":{"id":"iss-uuid","identifier":"CONV-1","url":"https://linear.app/x/issue/CONV-1"}}}}`,
	})
	defer srv.Close()
	w := &LinearWriter{Endpoint: srv.URL, apiKeyOverride: "k", HTTPClient: srv.Client()}
	id, ident, url, err := w.CreateIssue(context.Background(), "CONV", "57925d22", "Title", "Body", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "iss-uuid" || ident != "CONV-1" || url == "" {
		t.Errorf("got (%q,%q,%q)", id, ident, url)
	}
}

func TestResolveStateID(t *testing.T) {
	states := []workflowState{
		{ID: "s1", Name: "Todo", Type: "unstarted"},
		{ID: "s2", Name: "In Progress", Type: "started"},
		{ID: "s3", Name: "In Review", Type: "started"},
		{ID: "s4", Name: "Done", Type: "completed"},
	}
	cases := map[string]string{
		"todo": "s1", "in_progress": "s2", "in_review": "s3", "done": "s4",
		"blocked": "s1", // no Blocked state -> falls back to todo
	}
	for logical, want := range cases {
		if got := resolveStateID(states, logical); got != want {
			t.Errorf("resolveStateID(%q) = %q, want %q", logical, got, want)
		}
	}
	// type fallback: no "In Review" name -> first started
	noReview := []workflowState{{ID: "a", Name: "Doing", Type: "started"}, {ID: "b", Name: "Shipped", Type: "completed"}}
	if got := resolveStateID(noReview, "in_review"); got != "a" {
		t.Errorf("type fallback in_review = %q, want a", got)
	}
	// total miss -> ""
	if got := resolveStateID(nil, "done"); got != "" {
		t.Errorf("empty states = %q, want empty", got)
	}
}

func TestSetIssueState(t *testing.T) {
	var captured string
	srv := mockLinear(t, map[string]string{
		"teams(filter": `{"data":{"teams":{"nodes":[{"id":"team-uuid","states":{"nodes":[
			{"id":"s2","name":"In Progress","type":"started"}]}}]}}}`,
		"issueUpdate": `{"data":{"issueUpdate":{"success":true}}}`,
	})
	defer srv.Close()
	_ = captured
	w := &LinearWriter{Endpoint: srv.URL, apiKeyOverride: "k", HTTPClient: srv.Client()}
	if err := w.SetIssueState(context.Background(), "CONV", "iss-uuid", "in_progress"); err != nil {
		t.Fatal(err)
	}
	// Unknown logical with no matching state -> skip (no error, no-op).
	if err := w.SetIssueState(context.Background(), "CONV", "iss-uuid", "canceled"); err != nil {
		t.Fatalf("missing-state push should be a logged no-op, got %v", err)
	}
}

func TestUpdateIssueContentSendsTitleAndDescription(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		capturedBody = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
	}))
	defer srv.Close()
	w := &LinearWriter{Endpoint: srv.URL, apiKeyOverride: "k", HTTPClient: srv.Client()}
	if err := w.UpdateIssueContent(context.Background(), "HBA", "iss-1", "New Title", "New body"); err != nil {
		t.Fatalf("UpdateIssueContent: %v", err)
	}
	got := string(capturedBody)
	for _, want := range []string{"issueUpdate", "iss-1", "New Title", "New body"} {
		if !strings.Contains(got, want) {
			t.Errorf("request body missing %q; body = %s", want, got)
		}
	}
}

// TestCreateIssueSendsTodoStateID verifies CreateIssue resolves the team's Todo
// state and passes it as stateId in issueCreate, so the new issue is born in
// Todo rather than Linear's default (e.g. Backlog). Regression for the live-smoke
// finding that issues were created in Backlog while synced_state claimed "todo".
func TestCreateIssueSendsTodoStateID(t *testing.T) {
	var gotStateID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "teams(filter"):
			_, _ = w.Write([]byte(`{"data":{"teams":{"nodes":[{"id":"team-uuid","states":{"nodes":[{"id":"todo-id","name":"Todo","type":"unstarted"},{"id":"bk","name":"Backlog","type":"backlog"}]}}]}}}`))
		case strings.Contains(body.Query, "issueCreate"):
			if v, ok := body.Variables["stateId"].(string); ok {
				gotStateID = v
			}
			_, _ = w.Write([]byte(`{"data":{"issueCreate":{"success":true,"issue":{"id":"iss","identifier":"HBA-1","url":"u"}}}}`))
		default:
			t.Errorf("unexpected query: %s", body.Query)
		}
	}))
	defer srv.Close()
	w := &LinearWriter{Endpoint: srv.URL, apiKeyOverride: "k", HTTPClient: srv.Client()}
	if _, _, _, err := w.CreateIssue(context.Background(), "HBA", "", "T", "B", ""); err != nil {
		t.Fatal(err)
	}
	if gotStateID != "todo-id" {
		t.Errorf("issueCreate stateId = %q, want todo-id (issue should be born in Todo, not Backlog)", gotStateID)
	}
}

func TestCreateDocumentSendsProjectTitleContent(t *testing.T) {
	// Pass a real UUID so resolveProjectIDs short-circuits (no network round-trip
	// for project resolution), meaning newLinearStub only sees the documentCreate
	// mutation and can capture it cleanly.
	const projectUUID = "00000000-0000-0000-0000-000000000001"
	var gotBody string
	srv := newLinearStub(t, &gotBody, `{"data":{"documentCreate":{"success":true,"document":{"id":"doc-1"}}}}`)
	defer srv.Close()
	w := newTestWriter(t, srv)
	id, err := w.CreateDocument(context.Background(), projectUUID, "Conv Roadmap", "# roadmap\nbody")
	if err != nil {
		t.Fatal(err)
	}
	if id != "doc-1" {
		t.Errorf("doc id=%q want doc-1", id)
	}
	for _, want := range []string{"documentCreate", "Conv Roadmap", "# roadmap"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request missing %q: %s", want, gotBody)
		}
	}
}

func TestUpdateDocumentSendsIdTitleContent(t *testing.T) {
	var gotBody string
	srv := newLinearStub(t, &gotBody, `{"data":{"documentUpdate":{"success":true}}}`)
	defer srv.Close()
	w := newTestWriter(t, srv)
	if err := w.UpdateDocument(context.Background(), "doc-1", "Updated Title", "new content"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"documentUpdate", "doc-1", "Updated Title", "new content"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request missing %q: %s", want, gotBody)
		}
	}
}

func TestCreateProjectMilestoneSendsFields(t *testing.T) {
	var gotBody string
	srv := newLinearStub(t, &gotBody, `{"data":{"projectMilestoneCreate":{"success":true,"projectMilestone":{"id":"ms-1"}}}}`)
	defer srv.Close()
	w := newTestWriter(t, srv)
	id, err := w.CreateProjectMilestone(context.Background(), "00000000-0000-0000-0000-000000000001", "Phase 2a — Capture", "desc", 5)
	if err != nil {
		t.Fatal(err)
	}
	if id != "ms-1" {
		t.Errorf("ms id=%q want ms-1", id)
	}
	for _, want := range []string{"projectMilestoneCreate", "Phase 2a", "desc"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request missing %q: %s", want, gotBody)
		}
	}
}

func TestUpdateProjectMilestoneSendsIdFields(t *testing.T) {
	var gotBody string
	srv := newLinearStub(t, &gotBody, `{"data":{"projectMilestoneUpdate":{"success":true}}}`)
	defer srv.Close()
	w := newTestWriter(t, srv)
	if err := w.UpdateProjectMilestone(context.Background(), "ms-1", "Phase 2a — Capture", "d2", 5); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"projectMilestoneUpdate", "ms-1", "d2"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request missing %q: %s", want, gotBody)
		}
	}
}

func TestArchiveProjectMilestoneSendsId(t *testing.T) {
	var gotBody string
	srv := newLinearStub(t, &gotBody, `{"data":{"projectMilestoneDelete":{"success":true}}}`)
	defer srv.Close()
	w := newTestWriter(t, srv)
	if err := w.ArchiveProjectMilestone(context.Background(), "ms-1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"projectMilestoneDelete", "ms-1"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request missing %q: %s", want, gotBody)
		}
	}
}

func TestSetIssueMilestoneSendsIds(t *testing.T) {
	var gotBody string
	srv := newLinearStub(t, &gotBody, `{"data":{"issueUpdate":{"success":true}}}`)
	defer srv.Close()
	w := newTestWriter(t, srv)
	if err := w.SetIssueMilestone(context.Background(), "iss-1", "ms-1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"issueUpdate", "iss-1", "ms-1", "projectMilestoneId"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request missing %q: %s", want, gotBody)
		}
	}
}

// TestCreateIssueIncludesMilestone verifies that when a non-empty
// projectMilestoneID is passed to CreateIssue, the issueCreate mutation body
// contains "projectMilestoneId". Mirrors TestCreateIssueSendsTodoStateID: pass
// "" for projectIDOrSlug to skip the project-resolution round-trip, then branch
// on query content to answer the team query and capture the issueCreate variables.
func TestCreateIssueIncludesMilestone(t *testing.T) {
	var gotMilestoneID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "teams(filter"):
			_, _ = w.Write([]byte(`{"data":{"teams":{"nodes":[{"id":"team-uuid","states":{"nodes":[]}}]}}}`))
		case strings.Contains(body.Query, "issueCreate"):
			if v, ok := body.Variables["projectMilestoneId"].(string); ok {
				gotMilestoneID = v
			}
			_, _ = w.Write([]byte(`{"data":{"issueCreate":{"success":true,"issue":{"id":"iss","identifier":"HBA-2","url":"u"}}}}`))
		default:
			t.Errorf("unexpected query: %s", body.Query)
		}
	}))
	defer srv.Close()
	w := &LinearWriter{Endpoint: srv.URL, apiKeyOverride: "k", HTTPClient: srv.Client()}
	if _, _, _, err := w.CreateIssue(context.Background(), "HBA", "", "T", "B", "ms-42"); err != nil {
		t.Fatal(err)
	}
	if gotMilestoneID != "ms-42" {
		t.Errorf("issueCreate projectMilestoneId = %q, want ms-42", gotMilestoneID)
	}
}

// TestCreateIssueMutationHasMilestoneVar is a constant-level guard ensuring the
// issueCreate mutation string declares and uses projectMilestoneId.
func TestCreateIssueMutationHasMilestoneVar(t *testing.T) {
	if !strings.Contains(linearIssueCreateMutation, "projectMilestoneId") {
		t.Error("issueCreate mutation must declare projectMilestoneId")
	}
}
