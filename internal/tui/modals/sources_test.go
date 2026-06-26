package modals

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typeStringSources types runes into the modal's input one keystroke at
// a time. The input state machine only forwards to textinput in the
// config sub-states; in the list state the runes are interpreted as
// navigation keys (j/k) by updateList.
func typeStringSources(t *testing.T, m *SourcesModal, s string) {
	t.Helper()
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestSourcesModalInitialStateIsLoading(t *testing.T) {
	m := NewSourcesModal("hive")
	if m.state != "loading" {
		t.Errorf("state=%q want loading", m.state)
	}
	view := m.View(80, 24)
	if !strings.Contains(view, "loading") {
		t.Errorf("view should contain 'loading'; got:\n%s", view)
	}
	if !strings.Contains(view, "hive") {
		t.Errorf("view should contain the project slug in the title; got:\n%s", view)
	}
}

func TestSourcesModalListPopulatedFromRPCResult(t *testing.T) {
	m := NewSourcesModal("hive")
	// Wire shape per daemon: response is the project's Sources map
	// directly, keyed by source name. Github is bound; linear+inbox
	// are absent (=unbound).
	data := map[string]any{
		"github": map[string]any{"repo": "rohilrs/Hive"},
	}
	m2, _ := m.Update(RPCResultMsg{Kind: "sources_list", Data: data})
	mm := m2.(*SourcesModal)
	if mm.state != "list" {
		t.Fatalf("state=%q want list", mm.state)
	}
	if len(mm.sources) != 3 {
		t.Fatalf("sources count=%d want 3 (github+linear+inbox always rendered)", len(mm.sources))
	}
	// Order is fixed: github, linear, inbox.
	if mm.sources[0].Kind != "github" || !mm.sources[0].Bound {
		t.Errorf("row 0: kind=%q bound=%v want github,true", mm.sources[0].Kind, mm.sources[0].Bound)
	}
	if mm.sources[1].Kind != "linear" || mm.sources[1].Bound {
		t.Errorf("row 1: kind=%q bound=%v want linear,false", mm.sources[1].Kind, mm.sources[1].Bound)
	}
	if mm.sources[2].Kind != "inbox" || mm.sources[2].Bound {
		t.Errorf("row 2: kind=%q bound=%v want inbox,false", mm.sources[2].Kind, mm.sources[2].Bound)
	}
}

func TestSourcesModalCursorNavigation(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	if m.cursor != 0 {
		t.Fatalf("initial cursor=%d want 0", m.cursor)
	}
	// k/down moves cursor down.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.cursor != 1 {
		t.Errorf("after j, cursor=%d want 1", m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 2 {
		t.Errorf("after down, cursor=%d want 2", m.cursor)
	}
	// At bottom — further down is clamped.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 2 {
		t.Errorf("cursor should clamp at 2 (last row); got %d", m.cursor)
	}
	// k/up moves cursor up.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if m.cursor != 1 {
		t.Errorf("after k, cursor=%d want 1", m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("after up, cursor=%d want 0", m.cursor)
	}
	// At top — further up is clamped.
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("cursor should clamp at 0; got %d", m.cursor)
	}
}

func TestSourcesModalEnterOnBoundUnbinds(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{
		"github": map[string]any{"repo": "rohilrs/Hive"},
	}})
	// Cursor starts at 0 = github (which is bound). Enter should emit
	// a sources_unbind SubmitRequest.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on bound row should emit cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg %T want SubmitRequest", cmd())
	}
	if req.Kind != "sources_unbind" {
		t.Errorf("Kind=%q want sources_unbind", req.Kind)
	}
	if req.Params["source"] != "github" {
		t.Errorf("source=%v want github", req.Params["source"])
	}
	if req.Params["slug"] != "hive" {
		t.Errorf("slug=%v want hive", req.Params["slug"])
	}
	if !m.submitting {
		t.Errorf("submitting should flip to true on enter")
	}
}

func TestSourcesModalEnterOnUnboundInboxBinds(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	// Navigate cursor to inbox (index 2).
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on unbound inbox should emit cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg %T want SubmitRequest", cmd())
	}
	if req.Kind != "sources_bind" {
		t.Errorf("Kind=%q want sources_bind", req.Kind)
	}
	if req.Params["source"] != "inbox" {
		t.Errorf("source=%v want inbox", req.Params["source"])
	}
	binding, _ := req.Params["binding"].(map[string]any)
	if binding == nil || len(binding) != 0 {
		t.Errorf("binding=%v want empty map (inbox has no config)", binding)
	}
}

func TestSourcesModalEnterOnUnboundGithubTransitionsToConfig(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	// Cursor 0 = github, unbound.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.state != "configGithub" {
		t.Errorf("state=%q want configGithub", m.state)
	}
	if !m.input.Focused() {
		t.Errorf("config-state input should be focused")
	}
	// Enter should NOT have emitted a SubmitRequest — only transitioned.
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, ok := msg.(SubmitRequest); ok {
				t.Errorf("enter on unbound github must NOT emit SubmitRequest (config state needed first)")
			}
		}
	}
}

func TestSourcesModalConfigSubmitFiresBind(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	// Enter on github (cursor 0, unbound) → configGithub.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.state != "configGithub" {
		t.Fatalf("setup: state=%q want configGithub", m.state)
	}
	// Type "owner/repo" into the input.
	typeStringSources(t, m, "owner/repo")
	if m.input.Value() != "owner/repo" {
		t.Fatalf("setup: input=%q want owner/repo", m.input.Value())
	}
	// ctrl+s submits.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("ctrl+s in configGithub should emit cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg %T want SubmitRequest", cmd())
	}
	if req.Kind != "sources_bind" {
		t.Errorf("Kind=%q want sources_bind", req.Kind)
	}
	if req.Params["source"] != "github" {
		t.Errorf("source=%v want github", req.Params["source"])
	}
	binding, _ := req.Params["binding"].(map[string]any)
	if binding == nil {
		t.Fatalf("binding missing")
	}
	if binding["repo"] != "owner/repo" {
		t.Errorf("binding.repo=%v want owner/repo", binding["repo"])
	}
	if !m.submitting {
		t.Errorf("submitting should flip to true on submit")
	}
}

func TestSourcesModalConfigEscReturnsToList(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configGithub
	if m.state != "configGithub" {
		t.Fatalf("setup: state=%q want configGithub", m.state)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.state != "list" {
		t.Errorf("after esc, state=%q want list", m.state)
	}
}

func TestSourcesModalRpcErrorDisplayedInline(t *testing.T) {
	m := NewSourcesModal("hive")
	// Simulate a sources.list RPC error on initial load.
	m2, _ := m.Update(RPCResultMsg{Kind: "sources_list", Err: errors.New("project not found")})
	mm := m2.(*SourcesModal)
	if !strings.Contains(mm.errMsg, "project not found") {
		t.Errorf("errMsg=%q want to contain 'project not found'", mm.errMsg)
	}
	// State should NOT have transitioned to "list" on error — operator
	// stays in loading + sees the error so esc can dismiss.
	if mm.state == "list" {
		t.Errorf("state=%q; error should NOT transition to list (loading preserved)", mm.state)
	}
	// View should render the error.
	view := mm.View(80, 24)
	if !strings.Contains(view, "project not found") {
		t.Errorf("view should render the error inline; got:\n%s", view)
	}
}

// TestSourcesModalLinearBindingWrapsTeams documents the team_key-as-list
// quirk: the daemon's Linear binding expects `teams: []string`, NOT a
// single `team_key`. The modal collects one string and wraps it. The Linear
// bind is a two-step flow: configLinear (team, required) → configLinearProject
// (project filter, optional). Leaving the project step blank binds team-only
// with NO projects key.
func TestSourcesModalLinearBindingWrapsTeams(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	// Navigate cursor to linear (index 1).
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinear
	if m.state != "configLinear" {
		t.Fatalf("setup: state=%q want configLinear", m.state)
	}
	typeStringSources(t, m, "HBA")
	// First enter advances to the optional project step (does NOT submit).
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.state != "configLinearProject" {
		t.Fatalf("after team enter: state=%q want configLinearProject", m.state)
	}
	if cmd != nil {
		if _, ok := cmd().(SubmitRequest); ok {
			t.Fatal("team-key enter must advance to the project step, not submit")
		}
	}
	// Blank project filter → advance to the write-back toggle (does NOT submit).
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.state != "configLinearWriteBack" {
		t.Fatalf("after blank project enter: state=%q want configLinearWriteBack", m.state)
	}
	if cmd != nil {
		if _, ok := cmd().(SubmitRequest); ok {
			t.Fatal("blank project enter must advance to the write-back step, not submit")
		}
	}
	// Leave write-back OFF (default) → enter submits team-only.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the write-back step should submit")
	}
	req := cmd().(SubmitRequest)
	binding := req.Params["binding"].(map[string]any)
	teams, ok := binding["teams"].([]string)
	if !ok {
		t.Fatalf("binding.teams type=%T want []string", binding["teams"])
	}
	if len(teams) != 1 || teams[0] != "HBA" {
		t.Errorf("teams=%v want [HBA]", teams)
	}
	if _, present := binding["projects"]; present {
		t.Errorf("blank project step must omit the projects key; got %v", binding["projects"])
	}
	if _, present := binding["write_back"]; present {
		t.Errorf("write-back OFF must omit the write_back key; got %v", binding["write_back"])
	}
}

// TestSourcesModalLinearProjectFilter pins the new project-filter step: a
// comma-separated list at the project step lands as binding.projects ([]string,
// trimmed, empties dropped), alongside the team.
func TestSourcesModalLinearProjectFilter(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinear
	typeStringSources(t, m, "HBA")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinearProject
	typeStringSources(t, m, "Bug Bash, proj-2 ,")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinearWriteBack (no submit)
	if m.state != "configLinearWriteBack" {
		t.Fatalf("after project enter: state=%q want configLinearWriteBack", m.state)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // write-back OFF → submit
	if cmd == nil {
		t.Fatal("enter on the write-back step should submit")
	}
	req := cmd().(SubmitRequest)
	binding := req.Params["binding"].(map[string]any)
	if teams := binding["teams"].([]string); len(teams) != 1 || teams[0] != "HBA" {
		t.Errorf("teams=%v want [HBA]", teams)
	}
	projects, ok := binding["projects"].([]string)
	if !ok {
		t.Fatalf("binding.projects type=%T want []string", binding["projects"])
	}
	if len(projects) != 2 || projects[0] != "Bug Bash" || projects[1] != "proj-2" {
		t.Errorf("projects=%v want [Bug Bash proj-2] (trimmed, empties dropped)", projects)
	}
}

// TestSourcesModalLinearWriteBackEnabled covers the new write-back step: with
// exactly one project filter, toggling write-back on (space) and submitting
// lands binding.write_back=true alongside teams+projects — parity with the CLI
// `hive sources bind <slug> linear --write-back --team X --project Y`.
func TestSourcesModalLinearWriteBackEnabled(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})  // → linear row
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinear
	typeStringSources(t, m, "HBA")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinearProject
	typeStringSources(t, m, "474abc")        // exactly one project
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinearWriteBack
	if m.state != "configLinearWriteBack" {
		t.Fatalf("state=%q want configLinearWriteBack", m.state)
	}
	// Toggle write-back ON via space.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if !m.writeBack {
		t.Fatal("space should toggle write-back on")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter with write-back on + one project should submit")
	}
	req := cmd().(SubmitRequest)
	binding := req.Params["binding"].(map[string]any)
	if wb, _ := binding["write_back"].(bool); !wb {
		t.Errorf("binding.write_back=%v want true", binding["write_back"])
	}
	projects, _ := binding["projects"].([]string)
	if len(projects) != 1 || projects[0] != "474abc" {
		t.Errorf("projects=%v want [474abc]", projects)
	}
	if !m.submitting {
		t.Error("submitting should flip true on write-back submit")
	}
}

// TestSourcesModalWriteBackGuardRequiresOneProject pins the client-side guard:
// write-back demands exactly one project (the daemon would otherwise reject the
// ambiguous write target). With a blank project filter, toggling write-back on
// and submitting must NOT emit a bind — it sets an inline error instead.
func TestSourcesModalWriteBackGuardRequiresOneProject(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})  // → linear row
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinear
	typeStringSources(t, m, "HBA")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → configLinearProject
	// Leave project filter BLANK → zero projects.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})                     // → configLinearWriteBack
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}}) // toggle write-back on via mnemonic
	if !m.writeBack {
		t.Fatal("w should toggle write-back on")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		if _, ok := cmd().(SubmitRequest); ok {
			t.Fatal("write-back with 0 projects must NOT submit (ambiguous write target)")
		}
	}
	if m.submitting {
		t.Error("submitting must stay false when the guard blocks")
	}
	if !strings.Contains(m.errMsg, "exactly one project") {
		t.Errorf("errMsg=%q want it to mention the one-project requirement", m.errMsg)
	}
}

// TestSourcesModalSyncNowEmitsAndSummarizes covers the `y` sync-now action:
// it emits a global sources_sync (no project/source scope) and renders the
// summed per-source report inline on success.
func TestSourcesModalSyncNowEmitsAndSummarizes(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("y should emit a sources_sync request")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok || req.Kind != "sources_sync" {
		t.Fatalf("got %T/%v want SubmitRequest/sources_sync", cmd(), req.Kind)
	}
	if !m.submitting {
		t.Error("submitting should flip true while the sync runs")
	}
	// Daemon returns a SyncReport; the modal sums it into a notice line.
	m.Update(RPCResultMsg{Kind: "sources_sync", Data: map[string]any{
		"per_source": map[string]any{
			"github": map[string]any{"inserted": float64(2), "updated": float64(1), "closed": float64(0)},
			"linear": map[string]any{"inserted": float64(0), "updated": float64(3), "closed": float64(1), "error": "boom"},
		},
	}})
	if m.submitting {
		t.Error("submitting should reset on result")
	}
	if !strings.Contains(m.noticeMsg, "2 new") || !strings.Contains(m.noticeMsg, "4 updated") || !strings.Contains(m.noticeMsg, "1 closed") {
		t.Errorf("noticeMsg=%q want summed counts 2 new / 4 updated / 1 closed", m.noticeMsg)
	}
	if !strings.Contains(m.noticeMsg, "errored") {
		t.Errorf("noticeMsg=%q want it to flag the errored source", m.noticeMsg)
	}
}

func TestSummarizeSyncReportEmpty(t *testing.T) {
	if got := summarizeSyncReport(map[string]any{}); !strings.Contains(got, "no bound sources") {
		t.Errorf("empty report summary=%q want 'no bound sources'", got)
	}
}

// TestSourcesModalRefreshOnBindSuccess documents the post-action refresh:
// a successful sources_bind RPC result should trigger a sources_list_refresh
// SubmitRequest so the rows re-render with the new bound status.
func TestSourcesModalRefreshOnBindSuccess(t *testing.T) {
	m := NewSourcesModal("hive")
	m.Update(RPCResultMsg{Kind: "sources_list", Data: map[string]any{}})
	// Bind inbox so we have a fast path that doesn't enter the config state.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // emits SubmitRequest{Kind:sources_bind}
	if !m.submitting {
		t.Fatalf("setup: submitting should be true after enter")
	}
	// Daemon "succeeds".
	_, cmd := m.Update(RPCResultMsg{Kind: "sources_bind"})
	if m.submitting {
		t.Errorf("submitting should reset to false on RPC result")
	}
	if cmd == nil {
		t.Fatal("success result should emit a refresh cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg %T want SubmitRequest (refresh)", cmd())
	}
	if req.Kind != "sources_list_refresh" {
		t.Errorf("Kind=%q want sources_list_refresh", req.Kind)
	}
	if req.Params["slug"] != "hive" {
		t.Errorf("slug=%v want hive", req.Params["slug"])
	}
}
