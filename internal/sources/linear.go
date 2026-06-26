package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultLinearEndpoint = "https://api.linear.app/graphql"

// LinearSource fetches issues for the bound teams from the Linear GraphQL API
// using $after-cursor pagination (mirroring the GitHub source). No upper bound:
// fetches every issue for the bound teams, paginating 100 at a time.
//
// Binding: {"teams":["AUTH","BILLING"]}. The API key is read from the env var
// named by APIKeyEnv (config Sources.Linear.APIKeyEnv, default LINEAR_API_KEY).
type LinearSource struct {
	APIKeyEnv  string
	Endpoint   string       // "" -> defaultLinearEndpoint
	HTTPClient *http.Client // nil -> a 30s-timeout client
}

func (s *LinearSource) Name() string { return "linear" }

type linBinding struct {
	Teams []string `json:"teams"`
	// Projects optionally narrows the issues fetched for the bound teams to
	// those belonging to these Linear project IDs. Empty/nil preserves the
	// pre-T2 behavior (all issues in the bound teams). When non-empty, the
	// GraphQL query adds a `project: {id: {in: $projects}}` clause to the
	// existing team filter — Linear allows combining team + project filters
	// in one query.
	Projects []string `json:"projects,omitempty"`
	// WriteBack enables Hive->Linear mirroring (Phase 1). Create target =
	// (WBTeam||Teams[0], WBProject||Projects[0]). WBTeam/WBProject are required
	// only when Teams/Projects has more than one entry (ambiguous target).
	WriteBack bool   `json:"write_back,omitempty"`
	WBTeam    string `json:"wb_team,omitempty"`
	WBProject string `json:"wb_project,omitempty"`
}

// linearIssue mirrors the GraphQL issues.nodes shape. identifier, branchName,
// project, and attachments are all nullable in Linear's schema; pointer/empty
// checks below handle missing values gracefully.
type linearIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"` // e.g. "HBA-42"; nullable
	Title       string `json:"title"`
	Description string `json:"description"`
	BranchName  string `json:"branchName"` // Linear's canonical per-issue branch; nullable
	State       struct {
		Type string `json:"type"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	// Project is nullable when the issue isn't assigned to a Linear project.
	Project *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	// Attachments includes Linear-side links (GitHub, Figma, Notion, ...).
	// We consume the FIRST one whose URL parses as a github.com issue/PR
	// (ParseGitHubIssueURL is the gate). sourceType is captured for future
	// use but not consulted — Linear's GH integrations use varied values
	// ("github" lowercase, "pull-request", etc.).
	Attachments struct {
		Nodes []struct {
			URL        string `json:"url"`
			SourceType string `json:"sourceType"`
		} `json:"nodes"`
	} `json:"attachments"`
}

// linearGraphQLResponse decodes one page of the issues query.
type linearGraphQLResponse struct {
	Data struct {
		Issues struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []linearIssue `json:"nodes"`
		} `json:"issues"`
	} `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Path       []any          `json:"path,omitempty"`
		Extensions map[string]any `json:"extensions,omitempty"`
	} `json:"errors"`
}

// linearIssuesQuery is the per-page GraphQL query. $teams is non-null
// (we reject empty binding earlier). $after is null on the first page.
// Page size 100 matches the GraphQL convention; pagination makes per-page
// size much less load-bearing than the old first:250.
const linearIssuesQuery = `query($teams:[String!]!,$after:String){issues(first:100,after:$after,filter:{team:{key:{in:$teams}}}){pageInfo{hasNextPage endCursor} nodes{id identifier title description branchName state{type} labels{nodes{name}} project{id name} attachments{nodes{url sourceType}}}}}`

// linearIssuesQueryWithProjectFilter is the T2 variant used when the
// binding has a non-empty Projects list. Adds `$projects: [ID!]!` to the
// query signature and a `project: {id: {in: $projects}}` clause to the
// filter object. Kept as a separate constant (rather than always sending
// $projects with a null value) so the GraphQL stays unambiguous and we
// don't depend on Linear's null-handling for the in-operator across
// schema versions.
const linearIssuesQueryWithProjectFilter = `query($teams:[String!]!,$projects:[ID!]!,$after:String){issues(first:100,after:$after,filter:{team:{key:{in:$teams}},project:{id:{in:$projects}}}){pageInfo{hasNextPage endCursor} nodes{id identifier title description branchName state{type} labels{nodes{name}} project{id name} attachments{nodes{url sourceType}}}}}`


// doGraphQL POSTs a GraphQL query+variables to the Linear endpoint with the
// API key and decodes the JSON response body into out. Shared by the read
// (Fetch/resolveProjectIDs) and write (LinearWriter) paths. Returns an error
// on transport failure or HTTP status >= 300; GraphQL-level `errors` are left
// for the caller to inspect on `out` (each query has its own errors shape).
func doGraphQL(ctx context.Context, client *http.Client, endpoint, apiKey, query string, vars map[string]any, out any) error {
	reqBody, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("linear: unexpected HTTP status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *LinearSource) Fetch(ctx context.Context, _ string, binding json.RawMessage) ([]SourceItem, error) {
	keyEnv := s.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "LINEAR_API_KEY"
	}
	key := os.Getenv(keyEnv)
	if key == "" {
		return nil, fmt.Errorf("linear source: %s is not set", keyEnv)
	}
	var b linBinding
	if err := json.Unmarshal(binding, &b); err != nil {
		return nil, fmt.Errorf("linear binding: %w", err)
	}
	if len(b.Teams) == 0 {
		return nil, fmt.Errorf("linear source: binding has no teams")
	}
	// Sanity: if a Projects list is provided, every entry must be non-empty.
	// An empty string in the GraphQL `in:` list would either match nothing
	// or get rejected server-side after the round-trip; surface it locally.
	for i, p := range b.Projects {
		if p == "" {
			return nil, fmt.Errorf("linear source: binding projects[%d] is empty", i)
		}
	}

	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = defaultLinearEndpoint
	}
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	// Pick the query + variables shape based on whether the binding
	// constrains by Linear project. Capturing the choice once outside the
	// pagination loop keeps the per-page body identical (only $after
	// changes), which matches the existing single-page test expectations.
	query := linearIssuesQuery
	withProjects := len(b.Projects) > 0
	resolvedProjects := b.Projects
	if withProjects {
		query = linearIssuesQueryWithProjectFilter
		// Linear's ProjectFilter.id requires full UUIDs. Operators usually
		// have only the short slugId visible in the project URL — resolve
		// any non-UUID entries to their UUIDs via one extra round-trip
		// before the issues query.
		var rerr error
		resolvedProjects, rerr = resolveProjectIDs(ctx, client, endpoint, key, b.Projects)
		if rerr != nil {
			return nil, rerr
		}
	}

	var allNodes []linearIssue
	cursor := ""
	for {
		vars := map[string]any{"teams": b.Teams}
		if withProjects {
			vars["projects"] = resolvedProjects
		}
		if cursor != "" {
			vars["after"] = cursor
		}
		var out linearGraphQLResponse
		if err := doGraphQL(ctx, client, endpoint, key, query, vars, &out); err != nil {
			return nil, fmt.Errorf("linear request (cursor=%q): %w", cursor, err)
		}
		if len(out.Errors) > 0 {
			// Linear's payload includes path + extensions with the actual
			// validation detail (which argument, what was expected).
			// Without surfacing them the caller only sees the generic
			// "Argument Validation Error" — useless for diagnosis.
			var msgs []string
			for _, e := range out.Errors {
				m := e.Message
				if len(e.Path) > 0 {
					m += fmt.Sprintf(" path=%v", e.Path)
				}
				if len(e.Extensions) > 0 {
					if b, jerr := json.Marshal(e.Extensions); jerr == nil {
						m += " ext=" + string(b)
					}
				}
				msgs = append(msgs, m)
			}
			return nil, fmt.Errorf("linear graphql (cursor=%q): %s", cursor, strings.Join(msgs, "; "))
		}

		allNodes = append(allNodes, out.Data.Issues.Nodes...)
		if !out.Data.Issues.PageInfo.HasNextPage {
			break
		}
		// Defensive: server says hasNextPage but didn't advance the cursor.
		// Either an empty endCursor or one matching the cursor we just sent
		// would loop forever; treat as a malformed response.
		if out.Data.Issues.PageInfo.EndCursor == "" || out.Data.Issues.PageInfo.EndCursor == cursor {
			return nil, fmt.Errorf("linear pagination broken: hasNextPage=true with empty/unchanged endCursor (was %q)", cursor)
		}
		cursor = out.Data.Issues.PageInfo.EndCursor
	}

	items := make([]SourceItem, 0, len(allNodes))
	for _, n := range allNodes {
		state := "open"
		if n.State.Type == "completed" || n.State.Type == "canceled" {
			state = "closed"
		}
		var labels []string
		for _, l := range n.Labels.Nodes {
			labels = append(labels, l.Name)
		}

		// Collect provider-specific metadata + first GH-typed attachment.
		// Allocate the map lazily so issues with no extra fields keep a
		// nil Metadata (cleaner round-trip; matches reserved-keys docs).
		var meta map[string]string
		setMeta := func(k, v string) {
			if v == "" {
				return
			}
			if meta == nil {
				meta = make(map[string]string)
			}
			meta[k] = v
		}
		setMeta("external_id", n.Identifier)
		setMeta("branch_name", n.BranchName)
		if n.Project != nil {
			setMeta("linear_project_id", n.Project.ID)
			setMeta("linear_project_name", n.Project.Name)
		}
		var linked *LinkedGitHubRef
		for _, a := range n.Attachments.Nodes {
			// Linear's GH integration uses sourceType "github" (lowercase),
			// the marketplace integration uses "pull-request", and a manual
			// link may have a different value entirely — verified live
			// 2026-06-01. ParseGitHubIssueURL is the authoritative gate
			// (it rejects non-github hosts), so we accept anything whose
			// URL parses as a github.com issue/PR. Empty sourceType still
			// passes since the URL is the source of truth.
			ref := ParseGitHubIssueURL(a.URL)
			if ref == nil {
				// Non-GH attachment (Figma, Slack, Notion, etc.) or a
				// malformed URL. Try the next attachment.
				continue
			}
			linked = ref
			setMeta("linked_github_url", a.URL)
			break
		}

		items = append(items, SourceItem{
			SourceID:     n.ID,
			Title:        n.Title,
			Body:         n.Description,
			Labels:       labels,
			State:        state,
			Metadata:     meta,
			LinkedGitHub: linked,
		})
	}
	return items, nil
}

// linearUUIDRegex matches Linear's canonical project UUID shape
// (8-4-4-4-12 hex, case-insensitive). Used to detect entries in
// binding.Projects that are already UUIDs vs short slugIds visible in
// project URLs (e.g. "0ffd99eeb32b") which Linear's ProjectFilter.id
// rejects.
var linearUUIDRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// linearProjectLookupQuery resolves a list of slugIds to their UUIDs.
// Filters by slugId.in to grab them in one round-trip; works regardless
// of which Linear team owns the projects.
const linearProjectLookupQuery = `query($slugs:[String!]!){projects(filter:{slugId:{in:$slugs}}){nodes{id slugId name}}}`

// resolveProjectIDs accepts a mixed list of UUIDs + Linear slugIds and
// returns a list of UUIDs. UUIDs pass through unchanged; slugIds get
// looked up via the projects API. Returns an error if any slugId fails
// to resolve (no matching Linear project) — better to surface the
// misconfiguration than to silently filter against an unintended
// project set.
func resolveProjectIDs(ctx context.Context, client *http.Client, endpoint, apiKey string, input []string) ([]string, error) {
	if len(input) == 0 {
		return input, nil
	}
	// Partition: UUIDs pass through as-is; non-UUID strings are treated as slugIds.
	out := make([]string, 0, len(input))
	var slugs []string
	for _, p := range input {
		if linearUUIDRegex.MatchString(p) {
			out = append(out, p)
		} else {
			slugs = append(slugs, p)
		}
	}
	if len(slugs) == 0 {
		return out, nil
	}

	// One round-trip for all slugIds across all teams.
	var lookup struct {
		Data struct {
			Projects struct {
				Nodes []struct {
					ID     string `json:"id"`
					SlugID string `json:"slugId"`
					Name   string `json:"name"`
				} `json:"nodes"`
			} `json:"projects"`
		} `json:"data"`
		Errors []struct {
			Message    string         `json:"message"`
			Extensions map[string]any `json:"extensions,omitempty"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, client, endpoint, apiKey, linearProjectLookupQuery,
		map[string]any{"slugs": slugs}, &lookup); err != nil {
		return nil, fmt.Errorf("linear resolve slugIds: %w", err)
	}
	if len(lookup.Errors) > 0 {
		return nil, fmt.Errorf("linear resolve slugIds: %s", lookup.Errors[0].Message)
	}

	// Build slugId → UUID map.
	bySlug := make(map[string]string, len(lookup.Data.Projects.Nodes))
	for _, n := range lookup.Data.Projects.Nodes {
		bySlug[n.SlugID] = n.ID
	}

	// Append resolved UUIDs in original slug order; error on any miss.
	for _, slug := range slugs {
		id, ok := bySlug[slug]
		if !ok {
			return nil, fmt.Errorf("linear: project slugId %q not found (check the Linear URL for the slug after the last hyphen)", slug)
		}
		out = append(out, id)
	}
	return out, nil
}

// ParseGitHubIssueURL extracts owner/repo/num from a github.com issue or
// pull URL. Returns nil on parse failure or non-github hosts so callers
// can fall through (we treat malformed URLs as "no link" rather than a
// hard error — see the loop above for the rationale).
//
// Exported so the daemon's reverse-direction dedup (gh sync skips items
// already referenced by an existing Linear task's metadata.linked_github_url)
// can reuse this parser without duplicating the URL grammar.
func ParseGitHubIssueURL(raw string) *LinkedGitHubRef {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	if u.Host != "github.com" && u.Host != "www.github.com" {
		return nil
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Expect: [owner, repo, "issues"|"pull", num, ...maybe more]
	if len(parts) < 4 {
		return nil
	}
	if parts[2] != "issues" && parts[2] != "pull" {
		return nil
	}
	if parts[0] == "" || parts[1] == "" {
		return nil
	}
	num, err := strconv.Atoi(parts[3])
	if err != nil || num <= 0 {
		return nil
	}
	return &LinkedGitHubRef{Owner: parts[0], Repo: parts[1], IssueNum: num, URL: raw}
}
