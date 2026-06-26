package sources

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrLinearKeyUnset is returned by LinearWriter methods when the API key env
// var is empty. Callers treat it as "Linear write-back disabled" and skip.
var ErrLinearKeyUnset = fmt.Errorf("linear write-back: API key env unset")

// workflowState is one Linear team workflow state.
type workflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // backlog|unstarted|started|completed|canceled|triage
}

// LinearWriter performs Hive->Linear mutations (issue create + state update).
// It reads the API key from APIKeyEnv at call time (so the key can appear after
// daemon start), mirroring LinearSource. Team id + workflow states are cached
// per team key. Concurrency-safe.
type LinearWriter struct {
	APIKeyEnv  string
	Endpoint   string       // "" -> defaultLinearEndpoint
	HTTPClient *http.Client // nil -> 30s client

	apiKeyOverride string // tests only; "" -> read APIKeyEnv
	mu             sync.Mutex
	teamCache      map[string]cachedTeam
}

type cachedTeam struct {
	id     string
	states []workflowState
}

func (w *LinearWriter) key() string {
	if w.apiKeyOverride != "" {
		return w.apiKeyOverride
	}
	env := w.APIKeyEnv
	if env == "" {
		env = "LINEAR_API_KEY"
	}
	return os.Getenv(env)
}

func (w *LinearWriter) endpoint() string {
	if w.Endpoint == "" {
		return defaultLinearEndpoint
	}
	return w.Endpoint
}

func (w *LinearWriter) client() *http.Client {
	if w.HTTPClient != nil {
		return w.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

const linearTeamStatesQuery = `query($key:String!){teams(filter:{key:{eq:$key}}){nodes{id states{nodes{id name type}}}}}`

// resolveTeam returns the team UUID + its workflow states for a team key,
// caching the result. Returns ErrLinearKeyUnset if no API key is configured.
func (w *LinearWriter) resolveTeam(ctx context.Context, teamKey string) (string, []workflowState, error) {
	key := w.key()
	if key == "" {
		return "", nil, ErrLinearKeyUnset
	}
	w.mu.Lock()
	if w.teamCache == nil {
		w.teamCache = map[string]cachedTeam{}
	}
	if ct, ok := w.teamCache[teamKey]; ok {
		w.mu.Unlock()
		return ct.id, ct.states, nil
	}
	w.mu.Unlock()

	var out struct {
		Data struct {
			Teams struct {
				Nodes []struct {
					ID     string `json:"id"`
					States struct {
						Nodes []workflowState `json:"nodes"`
					} `json:"states"`
				} `json:"nodes"`
			} `json:"teams"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearTeamStatesQuery,
		map[string]any{"key": teamKey}, &out); err != nil {
		return "", nil, fmt.Errorf("linear resolveTeam %q: %w", teamKey, err)
	}
	if len(out.Errors) > 0 {
		return "", nil, fmt.Errorf("linear resolveTeam %q: %s", teamKey, out.Errors[0].Message)
	}
	if len(out.Data.Teams.Nodes) == 0 {
		return "", nil, fmt.Errorf("linear resolveTeam: team key %q not found", teamKey)
	}
	n := out.Data.Teams.Nodes[0]
	ct := cachedTeam{id: n.ID, states: n.States.Nodes}
	w.mu.Lock()
	w.teamCache[teamKey] = ct
	w.mu.Unlock()
	return ct.id, ct.states, nil
}

const linearIssueCreateMutation = `mutation($teamId:String!,$projectId:String,$projectMilestoneId:String,$stateId:String,$title:String!,$description:String){issueCreate(input:{teamId:$teamId,projectId:$projectId,projectMilestoneId:$projectMilestoneId,stateId:$stateId,title:$title,description:$description}){success issue{id identifier url}}}`

// CreateIssue creates a Linear issue in (teamKey, projectIDOrSlug). The team key
// is resolved to its UUID via resolveTeam; projectIDOrSlug may be a UUID or a
// slugId (resolved via resolveProjectIDs). projectMilestoneID optionally attaches
// the new issue to a project milestone (pass "" to omit). Returns the new issue's
// id, human identifier, and url. ErrLinearKeyUnset if no key.
func (w *LinearWriter) CreateIssue(ctx context.Context, teamKey, projectIDOrSlug, title, body, projectMilestoneID string) (string, string, string, error) {
	key := w.key()
	if key == "" {
		return "", "", "", ErrLinearKeyUnset
	}
	teamID, states, err := w.resolveTeam(ctx, teamKey)
	if err != nil {
		return "", "", "", err
	}
	projectID := projectIDOrSlug
	if projectIDOrSlug != "" {
		ids, err := resolveProjectIDs(ctx, w.client(), w.endpoint(), key, []string{projectIDOrSlug})
		if err != nil {
			return "", "", "", err
		}
		// resolveProjectIDs returns exactly one id per input on success; guard
		// against ever sending the raw slug to Linear as a project UUID.
		if len(ids) != 1 {
			return "", "", "", fmt.Errorf("linear issueCreate: project %q did not resolve to a single id", projectIDOrSlug)
		}
		projectID = ids[0]
	}
	var out struct {
		Data struct {
			IssueCreate struct {
				Success bool `json:"success"`
				Issue   struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					URL        string `json:"url"`
				} `json:"issue"`
			} `json:"issueCreate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	vars := map[string]any{"teamId": teamID, "title": title, "description": body}
	if projectID != "" {
		vars["projectId"] = projectID
	}
	if projectMilestoneID != "" {
		vars["projectMilestoneId"] = projectMilestoneID
	}
	// Create the issue directly in the team's "Todo" state. Without an explicit
	// stateId Linear defaults new issues to the team's default state (often
	// "Backlog"), which would diverge from the synced_state="todo" the caller
	// records and skip the Todo step of the lifecycle. Omitted if the team has
	// no resolvable unstarted/Todo state (Linear then applies its default).
	if stateID := resolveStateID(states, "todo"); stateID != "" {
		vars["stateId"] = stateID
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearIssueCreateMutation, vars, &out); err != nil {
		return "", "", "", fmt.Errorf("linear issueCreate: %w", err)
	}
	if len(out.Errors) > 0 {
		return "", "", "", fmt.Errorf("linear issueCreate: %s", out.Errors[0].Message)
	}
	if !out.Data.IssueCreate.Success || out.Data.IssueCreate.Issue.ID == "" {
		return "", "", "", fmt.Errorf("linear issueCreate: not successful")
	}
	is := out.Data.IssueCreate.Issue
	return is.ID, is.Identifier, is.URL, nil
}

const linearIssueMetaQuery = `query($id:String!){issue(id:$id){identifier branchName}}`

// FetchIssueMeta reads a single Linear issue's human identifier (e.g. "HBA-42")
// and canonical branchName by its UUID. Used to enrich a roadmap-pulled task
// with the metadata the syncer's OpInsert would otherwise stamp. ErrLinearKeyUnset
// if no key.
func (w *LinearWriter) FetchIssueMeta(ctx context.Context, issueID string) (identifier, branchName string, err error) {
	key := w.key()
	if key == "" {
		return "", "", ErrLinearKeyUnset
	}
	var out struct {
		Data struct {
			Issue struct {
				Identifier string `json:"identifier"`
				BranchName string `json:"branchName"`
			} `json:"issue"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearIssueMetaQuery, map[string]any{"id": issueID}, &out); err != nil {
		return "", "", fmt.Errorf("linear issue meta %s: %w", issueID, err)
	}
	if len(out.Errors) > 0 {
		return "", "", fmt.Errorf("linear issue meta %s: %s", issueID, out.Errors[0].Message)
	}
	return out.Data.Issue.Identifier, out.Data.Issue.BranchName, nil
}

const linearIssueUpdateStateMutation = `mutation($id:String!,$stateId:String!){issueUpdate(id:$id,input:{stateId:$stateId}){success}}`

// statePref maps a logical state to (preferred names, required type, fallback
// logical). Name match (case-insensitive) wins; then first state of the type;
// then the fallback logical (blocked->todo); then "".
var statePref = map[string]struct {
	names    []string
	typ      string
	fallback string
}{
	"todo":        {[]string{"todo", "to do", "backlog"}, "unstarted", ""},
	"in_progress": {[]string{"in progress"}, "started", ""},
	"in_review":   {[]string{"in review", "review"}, "started", ""},
	"blocked":     {[]string{"blocked"}, "", "todo"},
	"done":        {[]string{"done", "merged", "completed"}, "completed", ""},
	"canceled":    {[]string{"canceled", "cancelled"}, "canceled", ""},
}

// resolveStateID maps a logical state label to the UUID of the matching workflow
// state in the given slice. Resolution order: case-insensitive name match, then
// first state of the preferred type, then recursive fallback (blocked->todo),
// then "". A "" return is treated as a logged no-op by SetIssueState.
func resolveStateID(states []workflowState, logical string) string {
	pref, ok := statePref[logical]
	if !ok {
		return ""
	}
	for _, want := range pref.names {
		for _, s := range states {
			if strings.EqualFold(s.Name, want) {
				return s.ID
			}
		}
	}
	if pref.typ != "" {
		for _, s := range states {
			if s.Type == pref.typ {
				return s.ID
			}
		}
	}
	if pref.fallback != "" {
		return resolveStateID(states, pref.fallback)
	}
	return ""
}

// SetIssueState moves issueID to the team's workflow state matching the logical
// label. A no-op (logged, nil return) when the team has no resolvable state, so
// a missing "Blocked"/"Canceled" state never errors the caller. Returns
// ErrLinearKeyUnset if no API key is configured.
func (w *LinearWriter) SetIssueState(ctx context.Context, teamKey, issueID, logical string) error {
	key := w.key()
	if key == "" {
		return ErrLinearKeyUnset
	}
	_, states, err := w.resolveTeam(ctx, teamKey)
	if err != nil {
		return err
	}
	stateID := resolveStateID(states, logical)
	if stateID == "" {
		log.Printf("linear write-back: team %s has no state for %q; skipping issue %s", teamKey, logical, issueID)
		return nil
	}
	var out struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearIssueUpdateStateMutation,
		map[string]any{"id": issueID, "stateId": stateID}, &out); err != nil {
		return fmt.Errorf("linear issueUpdate: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear issueUpdate: %s", out.Errors[0].Message)
	}
	return nil
}

const linearIssueUpdateContentMutation = `mutation($id:String!,$title:String!,$desc:String!){issueUpdate(id:$id,input:{title:$title,description:$desc}){success}}`

// UpdateIssueContent rewrites a Linear issue's title + description. Used by
// existing-work reconciliation when a decompose MERGE rewrites an item that
// lives in Linear: pushing the merged content up keeps Hive and Linear in
// agreement so the next sync is a no-op (the merge is durable). teamKey is
// accepted for signature symmetry with SetIssueState; issueUpdate needs no team
// resolution. Best-effort by contract: the caller logs failures and proceeds.
func (w *LinearWriter) UpdateIssueContent(ctx context.Context, teamKey, issueID, title, body string) error {
	key := w.key()
	if key == "" {
		return ErrLinearKeyUnset
	}
	var out struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearIssueUpdateContentMutation,
		map[string]any{"id": issueID, "title": title, "desc": body}, &out); err != nil {
		return fmt.Errorf("linear issueUpdate content: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear issueUpdate content: %s", out.Errors[0].Message)
	}
	if !out.Data.IssueUpdate.Success {
		return fmt.Errorf("linear issueUpdate content: success=false for issue %s", issueID)
	}
	return nil
}

const linearIssueArchiveMutation = `mutation($id:String!){issueArchive(id:$id){success}}`

// ArchiveIssue archives a Linear issue (issueArchive mutation). Archived
// issues drop out of the default `issues` query, so the syncer's reconcile
// pass won't re-import a Hive task that was just deleted locally — closing
// the delete→re-pull loop. The issueID is the Linear issue UUID (what Hive
// stores as tasks.source_id). No team/state resolution is needed, so this is
// simpler than SetIssueState. Best-effort by contract: the caller logs a
// failure and still deletes the local task.
func (w *LinearWriter) ArchiveIssue(ctx context.Context, issueID string) error {
	key := w.key()
	if key == "" {
		return ErrLinearKeyUnset
	}
	var out struct {
		Data struct {
			IssueArchive struct {
				Success bool `json:"success"`
			} `json:"issueArchive"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearIssueArchiveMutation,
		map[string]any{"id": issueID}, &out); err != nil {
		return fmt.Errorf("linear issueArchive: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear issueArchive: %s", out.Errors[0].Message)
	}
	if !out.Data.IssueArchive.Success {
		return fmt.Errorf("linear issueArchive: success=false for issue %s", issueID)
	}
	return nil
}

const linearIssueSetMilestoneMutation = `mutation($id:String!,$projectMilestoneId:String){issueUpdate(id:$id,input:{projectMilestoneId:$projectMilestoneId}){success}}`

// SetIssueMilestone assigns an issue to a project milestone. Pass an empty
// milestoneID to clear it. Best-effort.
func (w *LinearWriter) SetIssueMilestone(ctx context.Context, issueID, milestoneID string) error {
	key := w.key()
	if key == "" {
		return ErrLinearKeyUnset
	}
	var out struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	vars := map[string]any{"id": issueID, "projectMilestoneId": milestoneID}
	if milestoneID == "" {
		vars["projectMilestoneId"] = nil
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearIssueSetMilestoneMutation, vars, &out); err != nil {
		return fmt.Errorf("linear issueUpdate milestone: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear issueUpdate milestone: %s", out.Errors[0].Message)
	}
	if !out.Data.IssueUpdate.Success {
		return fmt.Errorf("linear issueUpdate milestone: success=false for %s", issueID)
	}
	return nil
}

// resolveProject maps a project UUID-or-slugId to its UUID via resolveProjectIDs.
// "" → "" (no project). Used by the document/milestone creators.
func (w *LinearWriter) resolveProject(ctx context.Context, projectIDOrSlug string) (string, error) {
	if projectIDOrSlug == "" {
		return "", nil
	}
	key := w.key()
	if key == "" {
		return "", ErrLinearKeyUnset
	}
	ids, err := resolveProjectIDs(ctx, w.client(), w.endpoint(), key, []string{projectIDOrSlug})
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("linear: project %q did not resolve to a single id", projectIDOrSlug)
	}
	return ids[0], nil
}

const linearDocumentCreateMutation = `mutation($projectId:String!,$title:String!,$content:String){documentCreate(input:{projectId:$projectId,title:$title,content:$content}){success document{id}}}`

// CreateDocument creates a Linear document in the given project (UUID or slugId,
// resolved via resolveProjectIDs). Returns the new document id. content is
// markdown. Best-effort by contract; the caller logs failures.
func (w *LinearWriter) CreateDocument(ctx context.Context, projectIDOrSlug, title, content string) (string, error) {
	key := w.key()
	if key == "" {
		return "", ErrLinearKeyUnset
	}
	projectID, err := w.resolveProject(ctx, projectIDOrSlug)
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			DocumentCreate struct {
				Success  bool `json:"success"`
				Document struct {
					ID string `json:"id"`
				} `json:"document"`
			} `json:"documentCreate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearDocumentCreateMutation,
		map[string]any{"projectId": projectID, "title": title, "content": content}, &out); err != nil {
		return "", fmt.Errorf("linear documentCreate: %w", err)
	}
	if len(out.Errors) > 0 {
		return "", fmt.Errorf("linear documentCreate: %s", out.Errors[0].Message)
	}
	if !out.Data.DocumentCreate.Success || out.Data.DocumentCreate.Document.ID == "" {
		return "", fmt.Errorf("linear documentCreate: not successful")
	}
	return out.Data.DocumentCreate.Document.ID, nil
}

const linearDocumentUpdateMutation = `mutation($id:String!,$title:String!,$content:String){documentUpdate(id:$id,input:{title:$title,content:$content}){success}}`

// UpdateDocument rewrites a Linear document's title + content (markdown).
func (w *LinearWriter) UpdateDocument(ctx context.Context, docID, title, content string) error {
	key := w.key()
	if key == "" {
		return ErrLinearKeyUnset
	}
	var out struct {
		Data struct {
			DocumentUpdate struct {
				Success bool `json:"success"`
			} `json:"documentUpdate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearDocumentUpdateMutation,
		map[string]any{"id": docID, "title": title, "content": content}, &out); err != nil {
		return fmt.Errorf("linear documentUpdate: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear documentUpdate: %s", out.Errors[0].Message)
	}
	if !out.Data.DocumentUpdate.Success {
		return fmt.Errorf("linear documentUpdate: success=false for %s", docID)
	}
	return nil
}

const linearMilestoneCreateMutation = `mutation($projectId:String!,$name:String!,$description:String,$sortOrder:Float){projectMilestoneCreate(input:{projectId:$projectId,name:$name,description:$description,sortOrder:$sortOrder}){success projectMilestone{id}}}`

// CreateProjectMilestone creates a milestone in the given project (UUID or
// slugId). sortOrder controls display order. Returns the new milestone id.
func (w *LinearWriter) CreateProjectMilestone(ctx context.Context, projectIDOrSlug, name, description string, sortOrder float64) (string, error) {
	key := w.key()
	if key == "" {
		return "", ErrLinearKeyUnset
	}
	projectID, err := w.resolveProject(ctx, projectIDOrSlug)
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			ProjectMilestoneCreate struct {
				Success          bool `json:"success"`
				ProjectMilestone struct {
					ID string `json:"id"`
				} `json:"projectMilestone"`
			} `json:"projectMilestoneCreate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearMilestoneCreateMutation,
		map[string]any{"projectId": projectID, "name": name, "description": description, "sortOrder": sortOrder}, &out); err != nil {
		return "", fmt.Errorf("linear projectMilestoneCreate: %w", err)
	}
	if len(out.Errors) > 0 {
		return "", fmt.Errorf("linear projectMilestoneCreate: %s", out.Errors[0].Message)
	}
	if !out.Data.ProjectMilestoneCreate.Success || out.Data.ProjectMilestoneCreate.ProjectMilestone.ID == "" {
		return "", fmt.Errorf("linear projectMilestoneCreate: not successful")
	}
	return out.Data.ProjectMilestoneCreate.ProjectMilestone.ID, nil
}

const linearMilestoneUpdateMutation = `mutation($id:String!,$name:String!,$description:String,$sortOrder:Float){projectMilestoneUpdate(id:$id,input:{name:$name,description:$description,sortOrder:$sortOrder}){success}}`

// UpdateProjectMilestone rewrites a milestone's name, description, and sortOrder.
func (w *LinearWriter) UpdateProjectMilestone(ctx context.Context, msID, name, description string, sortOrder float64) error {
	key := w.key()
	if key == "" {
		return ErrLinearKeyUnset
	}
	var out struct {
		Data struct {
			ProjectMilestoneUpdate struct {
				Success bool `json:"success"`
			} `json:"projectMilestoneUpdate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearMilestoneUpdateMutation,
		map[string]any{"id": msID, "name": name, "description": description, "sortOrder": sortOrder}, &out); err != nil {
		return fmt.Errorf("linear projectMilestoneUpdate: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear projectMilestoneUpdate: %s", out.Errors[0].Message)
	}
	if !out.Data.ProjectMilestoneUpdate.Success {
		return fmt.Errorf("linear projectMilestoneUpdate: success=false for %s", msID)
	}
	return nil
}

const linearMilestoneDeleteMutation = `mutation($id:String!){projectMilestoneDelete(id:$id){success}}`

// ArchiveProjectMilestone soft-deletes (archives, recoverable from trash) a
// milestone. Linear's delete mutation is a soft archive, matching ArchiveIssue.
func (w *LinearWriter) ArchiveProjectMilestone(ctx context.Context, msID string) error {
	key := w.key()
	if key == "" {
		return ErrLinearKeyUnset
	}
	var out struct {
		Data struct {
			ProjectMilestoneDelete struct {
				Success bool `json:"success"`
			} `json:"projectMilestoneDelete"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, w.client(), w.endpoint(), key, linearMilestoneDeleteMutation,
		map[string]any{"id": msID}, &out); err != nil {
		return fmt.Errorf("linear projectMilestoneDelete: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear projectMilestoneDelete: %s", out.Errors[0].Message)
	}
	if !out.Data.ProjectMilestoneDelete.Success {
		return fmt.Errorf("linear projectMilestoneDelete: success=false for %s", msID)
	}
	return nil
}
