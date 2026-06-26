package chat

import (
	"fmt"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/codeintel"
)

// PlannerSystemPrompt returns the system prompt for planner-mode sessions.
// The slug + cwd are interpolated so the model knows which project it's
// planning for and where to look for existing docs.
func PlannerSystemPrompt(projectSlug, cwd string) string {
	return fmt.Sprintf(`You are the Hive Roadmap Planner. You help the operator converge on a phased project roadmap through Socratic Q&A.

PROJECT: %s
CWD:     %s

YOUR WORKFLOW:
1. On session start, call hive_list_specs(project_slug="%s") and report what you found. Ask whether to continue from any of them or start fresh.
2. If continuing from a spec, call hive_read_doc to load it. It returns ~16KB per call; if the response has truncated=true, the doc is larger — call hive_read_doc AGAIN with offset=next_offset and repeat until truncated=false, so you read the ENTIRE doc before summarizing. (Just page with offset; don't fiddle with length.) NEVER summarize, plan, or draft phases from a partially-read spec. Then summarize what it covers; ask what gaps remain.
3. If starting fresh, ask clarifying questions ONE AT A TIME — vision, target users, hard constraints, existing assets, success criteria, technical preferences. Don't move on until the prior answer is clear.
4. When you have enough signal for a phase boundary, propose the phase split to the operator. Wait for explicit approval before writing anything.
5. On approval, draft the roadmap markdown and call hive_save_roadmap. The confirm gate will show the operator the diff/content before writing.
6. For each phase that lacks a linked spec, offer to draft one. On acceptance, call hive_save_spec with a fresh date+slug.

RULES:
- Ask one question at a time when in Q&A mode. Brevity wins.
- DO NOT create Hive tasks. Task creation is a separate command the operator runs after the roadmap converges.
- EXISTING WORK: open tasks + un-pulled Linear issues for this project are listed at the START of this session under "EXISTING WORK". For each phase you write in the roadmap, add a blockquote line per existing item that belongs to that phase: "> Existing: <ref> (HBA-xx) — <title>". This is advisory — the decompose step uses it to MERGE existing work instead of duplicating it. Do not invent refs; only cite items from the provided list.
- GROUND IN THE CODEBASE: before proposing a phase or task, use hive_search_code to find the relevant code on the target branch, hive_read_doc to read it, and hive_query_capsule to see a symbol's CALLERS (blast radius) and CALLEES. Do NOT propose work that's already implemented — verify first. Scope and sequence phases by what the code shows (a change with many callers is bigger than it looks). If hive_query_capsule returns available:false, rely on hive_search_code + hive_read_doc.
- Roadmap doc structure: a top-level "# %s roadmap" heading, then per-phase H2 headings of the EXACT form "## Phase <N> — <Title>" where <N> is 1, 1a, 1b, 2, ... (e.g. "## Phase 1 — Foundation", "## Phase 2a — Capture expansion"). Use the literal word "Phase" and an em-dash separator. Any other H2 (e.g. "## Progress snapshot") is treated as non-phase prose. Each phase section should link its spec by relative path: "Spec: [docs/superpowers/specs/...](docs/superpowers/specs/...)". Embed a one-line phase summary + a short ordered task hint list.
- Spec doc structure: follow the existing pattern in docs/superpowers/specs/ — problem, design decisions, architecture, risks, success criteria.
- If the operator pushes back on a design decision, update the roadmap (call hive_save_roadmap again with the revised content). Roadmaps are living docs.

Begin by greeting the operator and running step 1.`,
		projectSlug, cwd, projectSlug, projectSlug)
}

// NewPlannerRegistry returns a Registry pre-populated with the 4 planner
// tools, optionally composed on top of an existing read-tool registry so
// the planner inherits hive_list_tasks/hive_get_task/etc. for situational
// awareness. Pass base=nil to skip inheritance. featureBranch is the
// integration-loop feature branch (already checked out by plan_setup); when
// non-empty the write tools commit each saved doc to it, empty disables commits.
func NewPlannerRegistry(cwd string, base *Registry, featureBranch string, grounder *codeintel.Grounder) *Registry {
	reg := NewRegistry()
	if base != nil {
		for _, def := range base.Defs() {
			t, _ := base.Get(def.Name)
			reg.Register(t)
		}
	}

	read := NewPlannerReadTools(cwd)
	write := NewPlannerWriteTools(cwd, featureBranch)

	reg.Register(Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_list_specs",
			Description: "List existing design specs in docs/superpowers/specs/ matching the project slug. Read-only.",
			InputSchema: jsonObject(map[string]any{
				"project_slug": jsonStringField("the project's slug, used to filter matches"),
			}, []string{"project_slug"}),
		},
		Mutating: false,
		Handler:  read.ListSpecs,
	})
	reg.Register(Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_read_doc",
			Description: "Read a markdown doc relative to the project cwd, in ~16 KB chunks (sized to display inline). The response includes total_bytes and next_offset; when truncated=true there is more to read — call again with offset=next_offset and keep going until truncated=false so you read the WHOLE doc before summarizing. Read-only.",
			InputSchema: jsonObject(map[string]any{
				"path":   jsonStringField("relative path to the doc, e.g. docs/superpowers/specs/2026-...md"),
				"offset": jsonIntField("byte offset to start reading from (default 0). Pass next_offset from a prior truncated read to continue."),
				"length": jsonIntField("optional max bytes to return this call (default ~16KB; capped at ~16KB so the result always displays). Usually omit and just page with offset."),
			}, []string{"path"}),
		},
		Mutating: false,
		Handler:  read.ReadDoc,
	})
	reg.Register(Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_save_roadmap",
			Description: "Write or overwrite docs/superpowers/roadmaps/<project_slug>.md. The operator will be asked to confirm before the file is written.",
			InputSchema: jsonObject(map[string]any{
				"project_slug": jsonStringField("the project's slug"),
				"content":      jsonStringField("the full roadmap markdown content"),
			}, []string{"project_slug", "content"}),
		},
		Mutating: true,
		Handler:  write.SaveRoadmap,
	})
	reg.Register(Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_save_spec",
			Description: "Write a new spec at docs/superpowers/specs/<date>-<slug>.md. Refuses to overwrite an existing file — pick a fresh slug or date if it exists. The operator will be asked to confirm before the file is written.",
			InputSchema: jsonObject(map[string]any{
				"slug":    jsonStringField("a kebab-case slug for the spec, e.g. 'phase-1-auth'"),
				"date":    jsonStringField("YYYY-MM-DD"),
				"content": jsonStringField("the full spec markdown content"),
			}, []string{"slug", "date", "content"}),
		},
		Mutating: true,
		Handler:  write.SaveSpec,
	})

	code := NewPlannerCodeTools(grounder)
	reg.Register(Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_search_code",
			Description: "Search the project's CODEBASE (its target branch) for a pattern (regex) via git grep. Returns file:line + snippet hits. Use this to check what's already implemented before proposing work. Read-only.",
			InputSchema: jsonObject(map[string]any{
				"query": jsonStringField("regex to search for, e.g. a function or type name"),
				"globs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "optional pathspecs to restrict the search, e.g. [\"*.ts\"]"},
			}, []string{"query"}),
		},
		Mutating: false,
		Handler:  code.SearchCode,
	})
	reg.Register(Tool{
		Def: anthropic.ToolDef{
			Name:        "hive_query_capsule",
			Description: "Get a code capsule for a symbol from the project's target branch: TARGET, CALLERS (blast radius), CALLEES, and BODY. Use to understand impact/scope before sizing or sequencing a phase. Returns available:false when the code-graph is unavailable — fall back to hive_search_code + hive_read_doc. Read-only.",
			InputSchema: jsonObject(map[string]any{
				"file":   jsonStringField("repo-relative file path the symbol lives in, e.g. src/foo.ts"),
				"symbol": jsonStringField("symbol/function name to center the capsule on; omit for the file-level capsule"),
			}, []string{"file"}),
		},
		Mutating: false,
		Handler:  code.QueryCapsule,
	})

	return reg
}

// jsonObject is a small helper to build a JSON-Schema object for ToolDef
// InputSchema without dragging in a heavy schema lib. props is a map of
// fieldname -> field schema (returned by jsonStringField etc); required
// is the list of required field names. Returns map[string]any (not any) so
// it slots directly into anthropic.ToolDef.InputSchema.
func jsonObject(props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// jsonStringField returns a JSON-Schema fragment for a string-typed field.
func jsonStringField(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// jsonIntField returns a JSON-Schema fragment for an integer-typed field.
func jsonIntField(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
