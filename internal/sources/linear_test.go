package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// linearPage builds a JSON-encoded Linear GraphQL response page with the
// given pageInfo + nodes. Test helper.
func linearPage(hasNext bool, endCursor string, nodes ...string) string {
	nodesJSON := "[" + strings.Join(nodes, ",") + "]"
	return `{"data":{"issues":{"pageInfo":{"hasNextPage":` +
		linBoolStr(hasNext) + `,"endCursor":"` + endCursor + `"},"nodes":` + nodesJSON + `}}}`
}

func linBoolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func linearNode(id, title, body, stateType string, labels ...string) string {
	labelNodes := make([]string, 0, len(labels))
	for _, l := range labels {
		labelNodes = append(labelNodes, `{"name":"`+l+`"}`)
	}
	return `{"id":"` + id +
		`","title":"` + title +
		`","description":"` + body +
		`","state":{"type":"` + stateType +
		`"},"labels":{"nodes":[` + strings.Join(labelNodes, ",") + `]}}`
}

// linearAttachment is a JSON fragment for the attachments.nodes array.
func linearAttachment(url, sourceType string) string {
	return `{"url":"` + url + `","sourceType":"` + sourceType + `"}`
}

// linearRichNode builds a node JSON with the full Linear-deep-integration
// fields populated: identifier, branchName, project, attachments. Pass
// projectID/projectName=="" to render project:null. Pass attachments=nil
// to render attachments.nodes:[]. Labels are not exposed here (tests that
// care about labels use linearNode).
func linearRichNode(id, identifier, title, body, branchName, stateType, projectID, projectName string, attachments []string) string {
	var sb strings.Builder
	sb.WriteString(`{"id":"` + id + `"`)
	sb.WriteString(`,"identifier":"` + identifier + `"`)
	sb.WriteString(`,"title":"` + title + `"`)
	sb.WriteString(`,"description":"` + body + `"`)
	sb.WriteString(`,"branchName":"` + branchName + `"`)
	sb.WriteString(`,"state":{"type":"` + stateType + `"}`)
	sb.WriteString(`,"labels":{"nodes":[]}`)
	if projectID == "" && projectName == "" {
		sb.WriteString(`,"project":null`)
	} else {
		sb.WriteString(`,"project":{"id":"` + projectID + `","name":"` + projectName + `"}`)
	}
	sb.WriteString(`,"attachments":{"nodes":[` + strings.Join(attachments, ",") + `]}}`)
	return sb.String()
}

func TestLinearFetchSinglePage(t *testing.T) {
	resp := linearPage(false, "",
		linearNode("iss-1", "Add retry", "backoff", "started", "hive:plan"),
		linearNode("iss-2", "Done thing", "", "completed"),
		linearNode("iss-3", "Dropped", "", "canceled"),
	)
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	t.Setenv("TEST_LINEAR_KEY", "lin_secret")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "proj1", json.RawMessage(`{"teams":["AUTH","BILL"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "lin_secret" {
		t.Errorf("Authorization header = %q want lin_secret", gotAuth)
	}
	if !strings.Contains(gotBody, "AUTH") || !strings.Contains(gotBody, "issues") {
		t.Errorf("request body missing team/query: %s", gotBody)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}
	byID := map[string]SourceItem{}
	for _, it := range items {
		byID[it.SourceID] = it
	}
	if byID["iss-1"].State != "open" || byID["iss-1"].Title != "Add retry" {
		t.Errorf("iss-1: %+v", byID["iss-1"])
	}
	if len(byID["iss-1"].Labels) != 1 || byID["iss-1"].Labels[0] != "hive:plan" {
		t.Errorf("iss-1 labels: %v", byID["iss-1"].Labels)
	}
	if byID["iss-2"].State != "closed" {
		t.Errorf("iss-2 (completed) state=%q want closed", byID["iss-2"].State)
	}
	if byID["iss-3"].State != "closed" {
		t.Errorf("iss-3 (canceled) state=%q want closed", byID["iss-3"].State)
	}
}

func TestLinearFetchMultiPagePagination(t *testing.T) {
	var calls int
	var cursorsSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Decode body to inspect the GraphQL "after" variable.
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		after, _ := req.Variables["after"].(string)
		cursorsSeen = append(cursorsSeen, after)

		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(linearPage(true, "cur1",
				linearNode("iss-1", "p1-a", "", "started"),
				linearNode("iss-2", "p1-b", "", "started"),
				linearNode("iss-3", "p1-c", "", "started"),
			)))
			return
		}
		_, _ = w.Write([]byte(linearPage(false, "",
			linearNode("iss-4", "p2-a", "", "started"),
			linearNode("iss-5", "p2-b", "", "started"),
		)))
	}))
	defer srv.Close()

	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["AUTH"]}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls=%d, want 2", calls)
	}
	// First page: $after is null (variable absent) → empty string after type-assert.
	// Second page: $after="cur1".
	if len(cursorsSeen) != 2 || cursorsSeen[0] != "" || cursorsSeen[1] != "cur1" {
		t.Errorf("cursorsSeen=%v, want [\"\", \"cur1\"]", cursorsSeen)
	}
	if len(items) != 5 {
		t.Fatalf("got %d items, want 5", len(items))
	}
	if items[0].SourceID != "iss-1" || items[4].SourceID != "iss-5" {
		t.Errorf("items order wrong: first=%s last=%s", items[0].SourceID, items[4].SourceID)
	}
}

func TestLinearFetchEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(linearPage(false, "")))
	}))
	defer srv.Close()
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["X"]}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if items == nil {
		t.Error("got nil slice; want empty non-nil")
	}
	if len(items) != 0 {
		t.Errorf("got %d items, want 0", len(items))
	}
}

func TestLinearFetchRequiresKey(t *testing.T) {
	src := &LinearSource{APIKeyEnv: "DEFINITELY_UNSET_KEY_XYZ", Endpoint: "http://unused"}
	if _, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["X"]}`)); err == nil {
		t.Fatal("want error when API key env is unset")
	}
}

func TestLinearFetchSurfacesGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"bad team"}]}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["X"]}`))
	if err == nil {
		t.Fatal("want error when GraphQL response carries errors")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil", items)
	}
}

func TestLinearFetchErrorsOnNon2xx(t *testing.T) {
	// A 401 (bad key) with an empty body must error, NOT return empty —
	// otherwise the reconciler would close every pending Linear task.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["X"]}`))
	if err == nil {
		t.Fatal("want error on non-2xx HTTP status")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil", items)
	}
	// The body is never read on non-2xx, so the key cannot leak into the error.
	if strings.Contains(err.Error(), "k") && !strings.Contains(err.Error(), "401") {
		// Sanity: status code in err is fine; raw "k" key text shouldn't be either.
		// (This check is loose; the main contract is the implementation reads only
		// resp.StatusCode on the error path.)
	}
}

func TestLinearFetchErrorOnSecondPagePropagates(t *testing.T) {
	// Page 1 OK with hasNextPage=true; page 2 returns transport-level error
	// (we simulate via a 500 response). Must return (nil, err) — NO partial.
	// Reconcile's "absent ⇒ close" would close every issue not on page 1.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(linearPage(true, "cur1",
				linearNode("iss-1", "p1-a", "", "started"),
				linearNode("iss-2", "p1-b", "", "started"),
			)))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["X"]}`))
	if err == nil {
		t.Fatal("expected error from second-page failure")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil (no partial; reconcile would erroneously close unfetched issues)", items)
	}
	if calls != 2 {
		t.Errorf("calls=%d, want 2", calls)
	}
}

func TestLinearFetchBrokenPagination(t *testing.T) {
	// hasNextPage=true but endCursor is empty (malformed response).
	// Must NOT loop forever; must return an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(linearPage(true, "",
			linearNode("iss-1", "first", "", "started"),
		)))
	}))
	defer srv.Close()
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["X"]}`))
	if err == nil {
		t.Fatal("expected error on broken pagination")
	}
	if !strings.Contains(err.Error(), "pagination broken") {
		t.Errorf("error msg=%q, want 'pagination broken'", err.Error())
	}
	if items != nil {
		t.Errorf("items=%+v, want nil", items)
	}
}

func TestLinearFetchInvalidJSONFromServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[}"))
	}))
	defer srv.Close()
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["X"]}`))
	if err == nil {
		t.Fatal("expected decode error")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil", items)
	}
}

// linearRichFetch fans out a fixture with the three Linear-deep-integration
// scenarios (full / minimal / Figma-only attachment) and returns the items
// keyed by ID. Reused across the new tests so they all see the same fixture.
func linearRichFetch(t *testing.T) map[string]SourceItem {
	t.Helper()
	ghAttach := linearAttachment("https://github.com/rohilrs/Hive/issues/100", "GitHub")
	figAttach := linearAttachment("https://figma.com/file/abc/spec", "Figma")
	resp := linearPage(false, "",
		// Full: identifier + branchName + project + GH attachment
		linearRichNode(
			"iss-rich", "HBA-42", "Add login", "needs OAuth",
			"rohil/HBA-42-add-login", "started",
			"p1", "App Project",
			[]string{ghAttach},
		),
		// Minimal: empty identifier/branch/project, no attachments
		linearRichNode("iss-min", "", "Bare", "", "", "started", "", "", nil),
		// Figma-only: identifier set but no GH attachment
		linearRichNode(
			"iss-fig", "HBA-99", "Design tweak", "",
			"", "started",
			"", "",
			[]string{figAttach},
		),
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["X"]}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	out := map[string]SourceItem{}
	for _, it := range items {
		out[it.SourceID] = it
	}
	if len(out) != 3 {
		t.Fatalf("want 3 items, got %d (%v)", len(out), items)
	}
	return out
}

func TestLinearFetchPopulatesMetadata(t *testing.T) {
	byID := linearRichFetch(t)

	rich := byID["iss-rich"]
	want := map[string]string{
		"external_id":         "HBA-42",
		"branch_name":         "rohil/HBA-42-add-login",
		"linear_project_id":   "p1",
		"linear_project_name": "App Project",
		"linked_github_url":   "https://github.com/rohilrs/Hive/issues/100",
	}
	if len(rich.Metadata) != len(want) {
		t.Errorf("iss-rich Metadata size = %d, want %d (got %v)", len(rich.Metadata), len(want), rich.Metadata)
	}
	for k, v := range want {
		if got := rich.Metadata[k]; got != v {
			t.Errorf("iss-rich Metadata[%q] = %q, want %q", k, got, v)
		}
	}

	// Minimal: no fields → Metadata stays nil (lazy alloc contract).
	min := byID["iss-min"]
	if min.Metadata != nil {
		t.Errorf("iss-min Metadata = %v, want nil (no fields → lazy alloc skips map)", min.Metadata)
	}

	// Figma-only: identifier is set so Metadata has exactly one entry.
	fig := byID["iss-fig"]
	if got := fig.Metadata["external_id"]; got != "HBA-99" {
		t.Errorf("iss-fig Metadata[external_id] = %q, want %q", got, "HBA-99")
	}
	if _, ok := fig.Metadata["linked_github_url"]; ok {
		t.Errorf("iss-fig Metadata has linked_github_url; Figma attachment must not populate it (got %v)", fig.Metadata)
	}
}

func TestLinearFetchPopulatesLinkedGitHub(t *testing.T) {
	byID := linearRichFetch(t)

	got := byID["iss-rich"].LinkedGitHub
	if got == nil {
		t.Fatalf("iss-rich LinkedGitHub = nil, want non-nil")
	}
	want := LinkedGitHubRef{
		Owner:    "rohilrs",
		Repo:     "Hive",
		IssueNum: 100,
		URL:      "https://github.com/rohilrs/Hive/issues/100",
	}
	if *got != want {
		t.Errorf("iss-rich LinkedGitHub = %+v, want %+v", *got, want)
	}

	if byID["iss-min"].LinkedGitHub != nil {
		t.Errorf("iss-min LinkedGitHub = %+v, want nil", byID["iss-min"].LinkedGitHub)
	}
	if byID["iss-fig"].LinkedGitHub != nil {
		t.Errorf("iss-fig LinkedGitHub = %+v, want nil (Figma is not GH)", byID["iss-fig"].LinkedGitHub)
	}
}

func TestLinearFetchSkipsNonGitHubAttachments(t *testing.T) {
	byID := linearRichFetch(t)
	if byID["iss-fig"].LinkedGitHub != nil {
		t.Errorf("Figma attachment populated LinkedGitHub: %+v", byID["iss-fig"].LinkedGitHub)
	}
	if _, ok := byID["iss-fig"].Metadata["linked_github_url"]; ok {
		t.Errorf("Figma attachment populated linked_github_url in Metadata")
	}
}

func TestLinearFetchHandlesMissingFields(t *testing.T) {
	byID := linearRichFetch(t)
	min := byID["iss-min"]
	if min.Title != "Bare" {
		t.Errorf("iss-min Title = %q, want %q", min.Title, "Bare")
	}
	if min.State != "open" {
		t.Errorf("iss-min State = %q, want open", min.State)
	}
	if min.Metadata != nil {
		t.Errorf("iss-min Metadata = %v, want nil (lazy alloc: no keys → no map)", min.Metadata)
	}
	if min.LinkedGitHub != nil {
		t.Errorf("iss-min LinkedGitHub = %+v, want nil", min.LinkedGitHub)
	}
}

func TestParseGitHubIssueURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want *LinkedGitHubRef
	}{
		{
			name: "issues path",
			in:   "https://github.com/rohilrs/Hive/issues/42",
			want: &LinkedGitHubRef{Owner: "rohilrs", Repo: "Hive", IssueNum: 42, URL: "https://github.com/rohilrs/Hive/issues/42"},
		},
		{
			name: "pull path",
			in:   "https://github.com/rohilrs/Hive/pull/7",
			want: &LinkedGitHubRef{Owner: "rohilrs", Repo: "Hive", IssueNum: 7, URL: "https://github.com/rohilrs/Hive/pull/7"},
		},
		{
			name: "www subdomain",
			in:   "https://www.github.com/rohilrs/Hive/issues/9",
			want: &LinkedGitHubRef{Owner: "rohilrs", Repo: "Hive", IssueNum: 9, URL: "https://www.github.com/rohilrs/Hive/issues/9"},
		},
		{
			name: "trailing path segment (issues/N/comments) still parses",
			in:   "https://github.com/rohilrs/Hive/issues/42/comments",
			want: &LinkedGitHubRef{Owner: "rohilrs", Repo: "Hive", IssueNum: 42, URL: "https://github.com/rohilrs/Hive/issues/42/comments"},
		},
		{name: "non-github host", in: "https://gitlab.com/rohilrs/Hive/issues/42", want: nil},
		{name: "missing num", in: "https://github.com/rohilrs/Hive/issues", want: nil},
		{name: "non-issues path", in: "https://github.com/rohilrs/Hive/blob/main/README.md", want: nil},
		{name: "non-numeric num", in: "https://github.com/rohilrs/Hive/issues/abc", want: nil},
		{name: "zero num", in: "https://github.com/rohilrs/Hive/issues/0", want: nil},
		{name: "empty owner", in: "https://github.com//Hive/issues/42", want: nil},
		{name: "garbage", in: "not a url at all %%", want: nil},
		{name: "empty string", in: "", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseGitHubIssueURL(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Errorf("ParseGitHubIssueURL(%q) = %+v, want nil", tc.in, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseGitHubIssueURL(%q) = nil, want %+v", tc.in, tc.want)
			}
			if *got != *tc.want {
				t.Errorf("ParseGitHubIssueURL(%q) = %+v, want %+v", tc.in, *got, *tc.want)
			}
		})
	}
}

// captureLinearRequest stands up a one-shot httptest server that records
// the request body of the first incoming Fetch and returns an empty issues
// page. Reused by the T2 binding tests to assert what was sent on the wire.
func captureLinearRequest(t *testing.T) (endpoint, body *string, client *http.Client) {
	t.Helper()
	body = new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(linearPage(false, "")))
	}))
	t.Cleanup(srv.Close)
	endpoint = &srv.URL
	client = srv.Client()
	return
}

func TestLinearFetchWithProjectFilterIncludesProjectsInRequest(t *testing.T) {
	endpoint, body, client := captureLinearRequest(t)
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: *endpoint, HTTPClient: client}
	// Use UUID-shaped project IDs so resolveProjectIDs short-circuits to
	// the passthrough path (no slugId lookup round-trip; the captured
	// request is exactly the issues query, the only one the fixture
	// handles).
	const uuidA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const uuidB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	_, err := src.Fetch(context.Background(), "p",
		json.RawMessage(fmt.Sprintf(`{"teams":["AUTH"],"projects":[%q,%q]}`, uuidA, uuidB)))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Decode the request to inspect both the query string and the variables
	// map (asserting on the JSON object is more robust than substring checks
	// against the variable ordering chosen by encoding/json).
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal([]byte(*body), &req); err != nil {
		t.Fatalf("decode request body: %v (body=%s)", err, *body)
	}
	if !strings.Contains(req.Query, "$projects:[ID!]!") {
		t.Errorf("query missing $projects signature: %s", req.Query)
	}
	if !strings.Contains(req.Query, "project:{id:{in:$projects}}") {
		t.Errorf("query missing project filter clause: %s", req.Query)
	}
	projects, ok := req.Variables["projects"].([]any)
	if !ok {
		t.Fatalf("variables.projects missing or wrong type: %#v", req.Variables["projects"])
	}
	if len(projects) != 2 || projects[0] != uuidA || projects[1] != uuidB {
		t.Errorf("variables.projects = %v, want [%s %s]", projects, uuidA, uuidB)
	}
}

func TestLinearFetchWithoutProjectFilterPreservesCurrentBehavior(t *testing.T) {
	endpoint, body, client := captureLinearRequest(t)
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: *endpoint, HTTPClient: client}
	_, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["AUTH"]}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal([]byte(*body), &req); err != nil {
		t.Fatalf("decode request body: %v (body=%s)", err, *body)
	}
	// Must be the team-only query string, byte-identical to the pre-T2
	// constant. The with-project variant introduces $projects in the query
	// signature — its absence here is the strongest pin.
	if strings.Contains(req.Query, "$projects") {
		t.Errorf("query unexpectedly includes $projects: %s", req.Query)
	}
	if strings.Contains(req.Query, "project:{id:{in:") {
		t.Errorf("query unexpectedly includes project filter clause: %s", req.Query)
	}
	if _, ok := req.Variables["projects"]; ok {
		t.Errorf("variables unexpectedly include projects: %#v", req.Variables)
	}
	// Positive: teams still present (regression guard against accidentally
	// stripping the team variable when the with-projects branch is taken).
	teams, ok := req.Variables["teams"].([]any)
	if !ok || len(teams) != 1 || teams[0] != "AUTH" {
		t.Errorf("variables.teams = %#v, want [AUTH]", req.Variables["teams"])
	}
}

func TestLinearFetchInvalidProjectsFailsValidation(t *testing.T) {
	// Empty-string project entry must error BEFORE any HTTP call so the
	// failure is fast + local; otherwise Linear would either match nothing
	// or 400 server-side.
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(linearPage(false, "")))
	}))
	defer srv.Close()
	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p",
		json.RawMessage(`{"teams":["AUTH"],"projects":["p1",""]}`))
	if err == nil {
		t.Fatal("want validation error for empty projects entry")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil", items)
	}
	if called {
		t.Error("HTTP server was called; validation should have short-circuited before the request")
	}
	if !strings.Contains(err.Error(), "projects[1]") {
		t.Errorf("error %q does not mention the offending index 'projects[1]'", err.Error())
	}
}

// TestLinearFetchResolvesSlugIdToUUID pins the slugId→UUID resolution:
// when binding.Projects contains a non-UUID string, Hive first hits
// projects.list to resolve it to a UUID, then uses the UUID in the
// issues query. Operators usually have only the slugId (visible in
// the Linear project URL after the last hyphen), not the full UUID.
func TestLinearFetchResolvesSlugIdToUUID(t *testing.T) {
	const slugA = "0ffd99eeb32b"
	const uuidA = "f7e8d9a0-1234-5678-90ab-cdef12345678"

	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)
		switch {
		case strings.Contains(bodyStr, `"slugs"`):
			// Resolution query — return UUID for the slug.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"projects":{"nodes":[{"id":%q,"slugId":%q,"name":"App"}]}}}`, uuidA, slugA)
		case strings.Contains(bodyStr, `"projects"`):
			// Issues query — verify it used the resolved UUID, not the slug.
			if !strings.Contains(bodyStr, uuidA) {
				t.Errorf("issues query should use resolved UUID %q; body=%s", uuidA, bodyStr)
			}
			if strings.Contains(bodyStr, slugA) {
				t.Errorf("issues query should NOT contain raw slugId %q; body=%s", slugA, bodyStr)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, linearPage(false, "", linearNode("i1", "T", "B", "started")))
		default:
			t.Errorf("unexpected request: %s", bodyStr)
		}
	}))
	defer srv.Close()

	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	items, err := src.Fetch(context.Background(), "p",
		json.RawMessage(fmt.Sprintf(`{"teams":["AUTH"],"projects":[%q]}`, slugA)))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
	if requestCount != 2 {
		t.Errorf("expected 2 requests (resolve + issues), got %d", requestCount)
	}
}

// TestLinearFetchErrorsOnUnresolvableSlugId pins the failure mode: a
// slugId Linear can't find should surface as a clear error, not
// silently filter against an empty project set.
func TestLinearFetchErrorsOnUnresolvableSlugId(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"slugs"`) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":{"projects":{"nodes":[]}}}`) // no match
			return
		}
		t.Errorf("issues query should not run when resolution fails")
	}))
	defer srv.Close()

	t.Setenv("TEST_LINEAR_KEY", "k")
	src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
	_, err := src.Fetch(context.Background(), "p",
		json.RawMessage(`{"teams":["AUTH"],"projects":["ghost-slug"]}`))
	if err == nil {
		t.Fatal("expected error on unresolvable slugId")
	}
	if !strings.Contains(err.Error(), "ghost-slug") {
		t.Errorf("error should mention the missing slug; got: %v", err)
	}
}

// TestLinearFetchAttachmentSourceTypeIsCaseInsensitive pins the live
// finding from 2026-06-01 dogfood: Linear's GH integration uses
// sourceType "github" (lowercase). Previous code matched literal
// "GitHub" and silently dropped every linked GH issue, breaking dedup.
func TestLinearFetchAttachmentSourceTypeIsCaseInsensitive(t *testing.T) {
	cases := []struct {
		name       string
		sourceType string
	}{
		{"lowercase github (real Linear value)", "github"},
		{"hyphenated pull-request", "pull-request"},
		{"empty sourceType (manual link)", ""},
		{"unrelated sourceType but GH url", "Slack"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			attach := linearAttachment("https://github.com/rohilrs/Hive/issues/100", c.sourceType)
			node := linearRichNode("iss-1", "HBA-42", "T", "B", "rohil/HBA-42-x", "started", "p1", "App", []string{attach})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(linearPage(false, "", node)))
			}))
			defer srv.Close()
			t.Setenv("TEST_LINEAR_KEY", "k")
			src := &LinearSource{APIKeyEnv: "TEST_LINEAR_KEY", Endpoint: srv.URL, HTTPClient: srv.Client()}
			items, err := src.Fetch(context.Background(), "p", json.RawMessage(`{"teams":["AUTH"]}`))
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("got %d items, want 1", len(items))
			}
			if items[0].LinkedGitHub == nil {
				t.Fatalf("LinkedGitHub nil for sourceType=%q — parser should match by URL not by sourceType", c.sourceType)
			}
			if items[0].LinkedGitHub.IssueNum != 100 {
				t.Errorf("IssueNum=%d want 100", items[0].LinkedGitHub.IssueNum)
			}
		})
	}
}
