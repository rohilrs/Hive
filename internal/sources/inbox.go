package sources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// InboxSource ingests local markdown from <Root>/<project-slug>/*.md.
// A deleted file is absent from Fetch -> the reconciler closes its task.
type InboxSource struct {
	Root string // e.g. ~/.hive/inbox
}

func (s *InboxSource) Name() string { return "inbox" }

func (s *InboxSource) Fetch(_ context.Context, projectSlug string, _ json.RawMessage) ([]SourceItem, error) {
	dir := filepath.Join(s.Root, projectSlug)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil // not bound / no files yet
	}
	if err != nil {
		return nil, err
	}
	var items []SourceItem
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		fm, body := splitFrontmatter(string(raw))
		title := fm["title"]
		if title == "" {
			title = firstHeading(body)
		}
		if title == "" {
			title = strings.TrimSuffix(e.Name(), ".md")
		}
		var labels []string
		if fm["labels"] != "" {
			for _, l := range strings.Split(fm["labels"], ",") {
				labels = append(labels, strings.TrimSpace(l))
			}
		}
		if fm["pipeline"] != "" {
			labels = append(labels, "hive:"+fm["pipeline"])
		}
		items = append(items, SourceItem{
			SourceID: e.Name(),
			Title:    title,
			Body:     strings.TrimSpace(body),
			Labels:   labels,
			State:    "open",
			Priority: fm["priority"],
		})
	}
	return items, nil
}

// splitFrontmatter returns the key:value map of a leading ---\n...\n--- block
// (if present) and the remaining body. Minimal: top-level "key: value" lines.
func splitFrontmatter(s string) (map[string]string, string) {
	fm := map[string]string{}
	if !strings.HasPrefix(s, "---\n") {
		return fm, s
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return fm, s
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return fm, rest[end+len("\n---\n"):]
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return ""
}
