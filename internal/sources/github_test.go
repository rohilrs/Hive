package sources

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// graphQLPage builds a JSON-encoded GraphQL response page given nodes
// + pagination info. Test helper.
func graphQLPage(hasNext bool, endCursor string, nodes ...string) []byte {
	nodesJSON := "[" + strings.Join(nodes, ",") + "]"
	return []byte(`{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":` +
		boolStr(hasNext) + `,"endCursor":"` + endCursor + `"},"nodes":` + nodesJSON + `}}}}`)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func ghNode(number int, title, state string, labels ...string) string {
	labelNodes := make([]string, 0, len(labels))
	for _, l := range labels {
		labelNodes = append(labelNodes, `{"name":"`+l+`"}`)
	}
	return `{"number":` + intStr(number) +
		`,"title":"` + title +
		`","body":"","state":"` + state +
		`","labels":{"nodes":[` + strings.Join(labelNodes, ",") + `]}}`
}

func intStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func TestGitHubFetchSinglePage(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return graphQLPage(false, "",
			ghNode(1, "first", "OPEN", "hive-ready"),
			ghNode(2, "second", "OPEN", "hive-ready"),
			ghNode(3, "third", "CLOSED", "hive-ready"),
		), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items[0].SourceID != "1" || items[0].Title != "first" || items[0].State != "open" {
		t.Errorf("items[0]=%+v", items[0])
	}
	if items[2].State != "closed" {
		t.Errorf("items[2].State=%q, want closed", items[2].State)
	}
}

func TestGitHubFetchMultiPagePagination(t *testing.T) {
	var calls int
	var cursorsSeen []string
	gh := &GitHubSource{Runner: func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		// Find the cursor= arg.
		for _, a := range args {
			if strings.HasPrefix(a, "cursor=") {
				cursorsSeen = append(cursorsSeen, strings.TrimPrefix(a, "cursor="))
				break
			}
		}
		if calls == 1 {
			return graphQLPage(true, "cur1",
				ghNode(1, "p1-a", "OPEN"),
				ghNode(2, "p1-b", "OPEN"),
				ghNode(3, "p1-c", "OPEN"),
			), nil
		}
		return graphQLPage(false, "",
			ghNode(4, "p2-a", "OPEN"),
			ghNode(5, "p2-b", "OPEN"),
		), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls=%d, want 2", calls)
	}
	if len(cursorsSeen) != 2 || cursorsSeen[0] != "" || cursorsSeen[1] != "cur1" {
		t.Errorf("cursorsSeen=%v, want [\"\", \"cur1\"]", cursorsSeen)
	}
	if len(items) != 5 {
		t.Fatalf("got %d items, want 5", len(items))
	}
	if items[0].SourceID != "1" || items[4].SourceID != "5" {
		t.Errorf("items order wrong: first=%s last=%s", items[0].SourceID, items[4].SourceID)
	}
}

func TestGitHubFetchEmptyRepo(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return graphQLPage(false, ""), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
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

func TestGitHubFetchLabelFilterAndSemantic(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return graphQLPage(false, "",
			ghNode(1, "A both", "OPEN", "hive-ready", "p0"),
			ghNode(2, "B partial", "OPEN", "hive-ready"),
			ghNode(3, "C neither", "OPEN", "other"),
		), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r","labels":["hive-ready","p0"]}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (only A passes AND filter)", len(items))
	}
	if items[0].SourceID != "1" {
		t.Errorf("items[0].SourceID=%s, want 1", items[0].SourceID)
	}
}

func TestGitHubFetchLabelFilterEmptyPasses(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return graphQLPage(false, "",
			ghNode(1, "A", "OPEN", "hive-ready", "p0"),
			ghNode(2, "B", "OPEN", "hive-ready"),
			ghNode(3, "C", "OPEN", "other"),
		), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("got %d items, want 3 (no label filter)", len(items))
	}
}

func TestGitHubFetchLabelFilterCaseInsensitive(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return graphQLPage(false, "",
			ghNode(1, "A", "OPEN", "hive-ready"),
		), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r","labels":["Hive-Ready"]}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("got %d items, want 1 (case-insensitive match)", len(items))
	}
}

func TestGitHubFetchErrorOnFirstPagePropagates(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, errors.New("gh: not authenticated")
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
	if err == nil {
		t.Fatal("expected error from first-page failure")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil (no partial)", items)
	}
}

func TestGitHubFetchErrorOnSecondPagePropagates(t *testing.T) {
	var calls int
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return graphQLPage(true, "cur1",
				ghNode(1, "p1", "OPEN"),
				ghNode(2, "p1", "OPEN"),
			), nil
		}
		return nil, errors.New("gh: rate limited")
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
	if err == nil {
		t.Fatal("expected error from second-page failure")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil (no partial; reconcile would erroneously close unfetched issues)", items)
	}
}

func TestGitHubFetchInvalidRepoMissingSlash(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		t.Fatal("Runner should not be called when repo is malformed")
		return nil, nil
	}}
	if _, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"no-slash"}`)); err == nil {
		t.Fatal("expected error for repo without slash")
	}
}

func TestGitHubFetchRequiresRepo(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) { return nil, nil }}
	if _, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error when repo missing")
	}
}

func TestGitHubFetchInvalidJSONFromGh(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("[}"), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil", items)
	}
}

func TestGitHubFetchGraphQLLevelErrors(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"data":{"repository":null},"errors":[{"message":"Could not resolve to a Repository"}]}`), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
	if err == nil {
		t.Fatal("expected error from GraphQL errors array")
	}
	if items != nil {
		t.Errorf("items=%+v, want nil", items)
	}
}

func TestGitHubFetchRepoNotFound(t *testing.T) {
	// data.repository is null without an errors array — gh returns this
	// for a repo the user can't see (private, deleted, etc).
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"data":{"repository":null}}`), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
	if err == nil {
		t.Fatal("expected 'not found' error")
	}
	if !strings.Contains(err.Error(), "not found or inaccessible") {
		t.Errorf("error msg=%q, want 'not found or inaccessible'", err.Error())
	}
	if items != nil {
		t.Errorf("items=%+v, want nil", items)
	}
}

func TestGitHubFetchBrokenPagination(t *testing.T) {
	// hasNextPage=true but endCursor is empty (malformed response).
	// Must NOT loop forever; must return an error.
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return graphQLPage(true, "",
			ghNode(1, "first", "OPEN"),
		), nil
	}}
	items, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`))
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

func TestGitHubFetchSurfacesRunnerError(t *testing.T) {
	gh := &GitHubSource{Runner: func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, errors.New("gh: not authenticated")
	}}
	if _, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"o/r"}`)); err == nil {
		t.Fatal("want error surfaced from runner")
	}
}

func TestGitHubFetchArgvShape(t *testing.T) {
	var gotArgs []string
	gh := &GitHubSource{Runner: func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return graphQLPage(false, ""), nil
	}}
	_, err := gh.Fetch(context.Background(), "p", json.RawMessage(`{"repo":"org/r"}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"api", "graphql", "-F owner=org", "-F name=r", "-F cursor=", "-f query="} {
		if !strings.Contains(joined, want) {
			t.Errorf("gh args missing %q; got %q", want, joined)
		}
	}
}

func TestHasAllLabels(t *testing.T) {
	cases := []struct {
		name string
		have []string
		want []string
		ok   bool
	}{
		{"empty want passes", []string{"x"}, []string{}, true},
		{"empty want nil passes", []string{"x"}, nil, true},
		{"want subset of have", []string{"a", "b", "c"}, []string{"a", "b"}, true},
		{"want equals have", []string{"a", "b"}, []string{"a", "b"}, true},
		{"want has element not in have", []string{"a"}, []string{"a", "b"}, false},
		{"case difference matches", []string{"Hive-Ready"}, []string{"hive-ready"}, true},
		{"empty have nonempty want fails", []string{}, []string{"a"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasAllLabels(c.have, c.want)
			if got != c.ok {
				t.Errorf("hasAllLabels(%v, %v) = %v; want %v", c.have, c.want, got, c.ok)
			}
		})
	}
}
