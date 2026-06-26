package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GitHubSource lists issues for a repo via the gh CLI (already authenticated)
// using `gh api graphql` with endCursor-based pagination. No upper bound:
// fetches every issue in any repo, paginating 100 at a time. Client-side
// label filter preserves the AND semantic of the previous `--label X --label Y`
// invocation pattern.
//
// Binding: {"repo":"org/name","labels":["hive-ready"]} (labels optional).
type GitHubSource struct {
	// Runner runs the gh CLI; nil defaults to exec'ing "gh". Injected in tests.
	Runner func(ctx context.Context, args ...string) ([]byte, error)
}

func (s *GitHubSource) Name() string { return "github" }

type ghBinding struct {
	Repo   string   `json:"repo"`
	Labels []string `json:"labels"`
}

// ghIssue mirrors the GraphQL repository.issues.nodes shape. The Labels
// field is a connection wrapper (nested .Nodes) — different from the
// flat-array shape returned by `gh issue list`.
type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

type ghGraphQLResponse struct {
	Data struct {
		Repository *struct {
			Issues struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []ghIssue `json:"nodes"`
			} `json:"issues"`
		} `json:"repository"`
	} `json:"data"`
	Errors []map[string]any `json:"errors,omitempty"`
}

// ghIssuesQuery is the per-page GraphQL query. 100 issues per page is the
// GitHub API maximum for the issues(first:N) field. 20 labels per issue
// covers any realistic case; an issue with >20 labels would have label
// pagination of its own, which we don't follow.
const ghIssuesQuery = `query($owner:String!, $name:String!, $cursor:String) {
  repository(owner:$owner, name:$name) {
    issues(first:100, after:$cursor, orderBy:{field:UPDATED_AT, direction:DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title body state
        labels(first:20) { nodes { name } }
      }
    }
  }
}`

func (s *GitHubSource) Fetch(ctx context.Context, _ string, binding json.RawMessage) ([]SourceItem, error) {
	var b ghBinding
	if err := json.Unmarshal(binding, &b); err != nil {
		return nil, fmt.Errorf("github binding: %w", err)
	}
	if b.Repo == "" {
		return nil, fmt.Errorf("github source: binding missing \"repo\"")
	}
	owner, name, ok := strings.Cut(b.Repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("github source: repo %q must be \"owner/name\"", b.Repo)
	}

	var allIssues []ghIssue
	cursor := ""
	for {
		raw, err := s.run(ctx, "api", "graphql",
			"-F", "owner="+owner,
			"-F", "name="+name,
			"-F", "cursor="+cursor,
			"-f", "query="+ghIssuesQuery,
		)
		if err != nil {
			return nil, fmt.Errorf("gh api graphql (cursor=%q): %w", cursor, err)
		}
		var resp ghGraphQLResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("parse graphql response (cursor=%q): %w", cursor, err)
		}
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("graphql errors: %v", resp.Errors)
		}
		if resp.Data.Repository == nil {
			return nil, fmt.Errorf("repository %s/%s not found or inaccessible", owner, name)
		}

		issues := resp.Data.Repository.Issues
		allIssues = append(allIssues, issues.Nodes...)
		if !issues.PageInfo.HasNextPage {
			break
		}
		// Defensive: server says hasNextPage but didn't advance the cursor.
		// Either an empty endCursor or one matching the cursor we just sent
		// would loop forever; treat as a malformed response.
		if issues.PageInfo.EndCursor == "" || issues.PageInfo.EndCursor == cursor {
			return nil, fmt.Errorf("graphql pagination broken: hasNextPage=true with empty/unchanged endCursor (was %q)", cursor)
		}
		cursor = issues.PageInfo.EndCursor
	}

	items := make([]SourceItem, 0, len(allIssues))
	for _, iss := range allIssues {
		issLabelNames := make([]string, 0, len(iss.Labels.Nodes))
		for _, l := range iss.Labels.Nodes {
			issLabelNames = append(issLabelNames, l.Name)
		}
		// AND semantic: every binding.Labels entry must appear in the
		// issue's label set. Matches the previous --label X --label Y
		// behavior. If an operator removes a gating label from an issue,
		// it drops out here → reconcile sees it as "absent" → closes
		// the corresponding task. Desired behavior; do NOT "fix" it.
		if !hasAllLabels(issLabelNames, b.Labels) {
			continue
		}
		state := "open"
		if strings.EqualFold(iss.State, "CLOSED") {
			state = "closed"
		}
		items = append(items, SourceItem{
			SourceID: strconv.Itoa(iss.Number),
			Title:    iss.Title,
			Body:     iss.Body,
			Labels:   issLabelNames,
			State:    state,
		})
	}
	return items, nil
}

// hasAllLabels returns true iff every label in want appears in have
// (case-insensitive, matching GitHub's web semantics). Empty want
// returns true (no filter = all pass).
func hasAllLabels(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	haveSet := make(map[string]bool, len(have))
	for _, l := range have {
		haveSet[strings.ToLower(l)] = true
	}
	for _, l := range want {
		if !haveSet[strings.ToLower(l)] {
			return false
		}
	}
	return true
}

func (s *GitHubSource) run(ctx context.Context, args ...string) ([]byte, error) {
	if s.Runner != nil {
		return s.Runner(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// gh writes its actual error message to stderr ("HTTP 404: Not Found
		// (repo doesn't exist)", "GraphQL: ...", etc). Without surfacing it
		// the caller only sees "exit status 1" — useless for diagnosis.
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil, err
	}
	return out, nil
}
