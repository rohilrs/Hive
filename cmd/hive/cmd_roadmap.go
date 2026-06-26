package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/daemon"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// roadmapCmd is the cobra parent under `hive roadmap` for roadmap-doc
// subcommands. Phase 8.B ships `decompose`; future sub-phases (status,
// lint, re-run, etc.) hang off the same parent.
//
// Architecture: each subcommand is a thin CLI wrapper over a daemon RPC.
// The decompose subcommand specifically:
//  1. Calls roadmap.decompose to get proposed subtasks (Sonnet via CC
//     subscription per Phase 8.B T4 composition-root wiring; no API spend
//     when [chat] provider="claude-code").
//  2. Pre-checks existing tasks for the same (phase, roadmap_path) so we
//     don't double-decompose a phase silently.
//  3. Renders the proposal + prompts y/n (skipped with --yes).
//  4. Applies the full approved set server-side via roadmap.decompose_apply:
//     inserts new tasks, merges overlapping work into existing tasks/Linear
//     issues, and stamps roadmap_phase/index/total metadata on each entry.
var roadmapCmd = newRoadmapCmd()

// newRoadmapCmd builds the parent + decompose subcommand. Constructor
// shape (vs package-level var with init() side effects) so cmd_roadmap_test.go
// can build a fresh cobra tree per test and assert behavior in isolation —
// matches the newPlanCmd pattern from Phase 8.A T7.
func newRoadmapCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "roadmap",
		Short: "Manage project roadmaps (decompose phases into tasks)",
	}
	parent.AddCommand(newRoadmapDecomposeCmd())
	parent.AddCommand(newRoadmapSyncLinearCmd())
	return parent
}

// newRoadmapDecomposeCmd is the `hive roadmap decompose <project>` child.
func newRoadmapDecomposeCmd() *cobra.Command {
	var phase string
	var yes bool
	var maxN int
	cmd := &cobra.Command{
		Use:   "decompose <project>",
		Short: "Decompose a roadmap phase into Hive tasks",
		Long: `Reads docs/superpowers/roadmaps/<project>.md (written by hive plan),
extracts the named phase and any linked specs, calls Sonnet via the
claude-code subscription (no API spend when [chat] provider="claude-code")
to propose a list of tasks, and inserts the approved ones with metadata
linking back to the roadmap.

Refuses to re-decompose a phase that already has matching tasks — clean
those up first or pick a different phase.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRoadmapDecompose(args[0], phase, yes, maxN, defaultRoadmapDeps())
		},
	}
	cmd.Flags().StringVar(&phase, "phase", "", "phase number to decompose (e.g. 1, 1a, 2)")
	_ = cmd.MarkFlagRequired("phase")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve all proposed tasks without prompting")
	cmd.Flags().IntVar(&maxN, "max-subtasks", 0, "cap on proposed subtasks (0 = server default)")
	return cmd
}

// newRoadmapSyncLinearCmd is the `hive roadmap sync-linear <project>` child:
// mirror the project's roadmap into Linear (document + per-phase milestones +
// issue links). Idempotent; safe to re-run.
func newRoadmapSyncLinearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync-linear <project>",
		Short: "Mirror the project's roadmap into Linear (document + per-phase milestones + issue links)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The sync makes many sequential Linear round-trips (resolve project +
			// 1 document + N milestones + issue backfill), which can exceed the 10s
			// default. Use a generous timeout like decompose's Sonnet path.
			raw, err := rpcCallRawWithTimeout(rpc.MethodRoadmapSyncLinear, map[string]any{"project_slug": args[0]}, 120*time.Second)
			if err != nil {
				return err
			}
			var res daemon.RoadmapSyncLinearResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Synced to Linear: document=%d, milestones=%d.\n", res.Document, res.Milestones)
			for _, e := range res.Errors {
				fmt.Fprintf(os.Stderr, "  ! %s\n", e)
			}
			return nil
		},
	}
}

// roadmapDeps is the dependency-injection seam for the decompose command.
// Three side-effecting hooks: (1) fetch proposals from the daemon's
// roadmap.decompose RPC, (2) count existing roadmap-linked tasks for the
// pre-decompose guard, (3) apply the full approved proposal set via the
// roadmap.decompose_apply RPC (handles insert/merge/pull server-side).
// defaultRoadmapDeps wires the real RPC implementations; tests inject
// in-memory fakes.
//
// confirm is the y/n prompt source. Defaults to bufio over os.Stdin;
// tests override to canned responses (though our test suite uses --yes
// so confirm is mostly there to keep the prompt path testable in the
// future without recompiling the CLI).
type roadmapDeps struct {
	fetchProposals func(slug, phase string, maxSubtasks int) (*daemon.RoadmapDecomposeResult, error)
	listExisting   func(slug, phase, roadmapPath string) (count int, err error)
	applyDecompose func(params map[string]any) (*daemon.RoadmapDecomposeApplyResult, error)
	confirm        func(io.Reader) bool
	stdin          io.Reader
	stdout         io.Writer
	stderr         io.Writer
}

func defaultRoadmapDeps() *roadmapDeps {
	return &roadmapDeps{
		fetchProposals: fetchRoadmapProposals,
		listExisting:   listRoadmapPhaseTasks,
		applyDecompose: func(params map[string]any) (*daemon.RoadmapDecomposeApplyResult, error) {
			raw, err := rpcCallRaw(rpc.MethodRoadmapDecomposeApply, params)
			if err != nil {
				return nil, err
			}
			var out daemon.RoadmapDecomposeApplyResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, err
			}
			return &out, nil
		},
		confirm: promptYesNo,
		stdin:   os.Stdin,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
	}
}

// runRoadmapDecompose is the pure-ish orchestration entry point. All I/O
// is routed through deps so tests can pin behavior without dialing the
// daemon. Returns an error only for setup / hard failures; per-task
// insert errors are surfaced on stderr but do NOT abort the batch (a
// failing insert mid-list shouldn't strand the rest of the proposals).
func runRoadmapDecompose(slug, phase string, yes bool, maxSubtasks int, deps *roadmapDeps) error {
	if deps.stdout == nil {
		deps.stdout = os.Stdout
	}
	if deps.stderr == nil {
		deps.stderr = os.Stderr
	}
	if deps.stdin == nil {
		deps.stdin = os.Stdin
	}
	if deps.confirm == nil {
		deps.confirm = promptYesNo
	}

	// 1. Fetch proposals from the daemon.
	res, err := deps.fetchProposals(slug, phase, maxSubtasks)
	if err != nil {
		return fmt.Errorf("fetch proposals: %w", err)
	}
	if res == nil || len(res.Subtasks) == 0 {
		return errors.New("no subtasks proposed; check the roadmap phase body + linked spec(s)")
	}

	// 2. Pre-check for already-decomposed phase.
	existing, err := deps.listExisting(slug, res.PhaseNumber, res.RoadmapPath)
	if err != nil {
		return fmt.Errorf("check existing tasks: %w", err)
	}
	if existing > 0 {
		return fmt.Errorf("phase %s already decomposed (%d existing task(s) match roadmap_phase=%s, roadmap_path=%s). "+
			"Clean those up first (hive task delete ...) or pick a different phase",
			res.PhaseNumber, existing, res.PhaseNumber, res.RoadmapPath)
	}

	// 3. Render the proposal.
	fmt.Fprintf(deps.stdout, "Phase %s: %s\n", res.PhaseNumber, res.PhaseTitle)
	fmt.Fprintf(deps.stdout, "Roadmap: %s\n", res.RoadmapPath)
	if len(res.SpecPaths) > 0 {
		fmt.Fprintf(deps.stdout, "Specs: %s\n", strings.Join(res.SpecPaths, ", "))
	}
	fmt.Fprintf(deps.stdout, "\nProposed %d sub-task(s):\n\n", len(res.Subtasks))
	for i, st := range res.Subtasks {
		mergeNote := ""
		if st.MergeFrom != "" {
			mergeNote = "  ← merges " + st.MergeFrom
		}
		fmt.Fprintf(deps.stdout, "  %d. [%s / %s] %s%s\n", i+1, defaultPriority(st.Priority), defaultPipeline(st.Pipeline), st.Title, mergeNote)
		body := previewBody(st.Body, 240)
		for _, line := range strings.Split(body, "\n") {
			fmt.Fprintf(deps.stdout, "     %s\n", line)
		}
		fmt.Fprintln(deps.stdout)
	}

	// 4. Confirm.
	if !yes {
		fmt.Fprintf(deps.stdout, "Insert %d sub-task(s) into project %s? [y/N] ", len(res.Subtasks), slug)
		if !deps.confirm(deps.stdin) {
			fmt.Fprintln(deps.stdout, "Cancelled.")
			return nil
		}
	}

	// 5. Apply the full proposal set via a single roadmap.decompose_apply RPC.
	// The daemon owns insertion, merge, and Linear write-back so merge_from
	// semantics stay server-side (the old per-task task.add loop couldn't do
	// merges at all).
	subtaskMaps := make([]map[string]any, 0, len(res.Subtasks))
	for _, st := range res.Subtasks {
		subtaskMaps = append(subtaskMaps, map[string]any{
			"title":          st.Title,
			"body":           st.Body,
			"priority":       defaultPriority(st.Priority),
			"pipeline":       defaultPipeline(st.Pipeline),
			"merge_from":     st.MergeFrom,
			"depends_on":     st.DependsOn,
			"relevant_files": st.RelevantFiles,
		})
	}
	applyRes, err := deps.applyDecompose(map[string]any{
		"project_slug": slug,
		"roadmap_path": res.RoadmapPath,
		"phase":        res.PhaseNumber,
		"phase_title":  res.PhaseTitle,
		"spec_path":    firstSpec(res.SpecPaths),
		"subtasks":     subtaskMaps,
	})
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	fmt.Fprintf(deps.stdout, "Phase %s: inserted %d, merged %d, pulled %d.\n",
		res.PhaseNumber, applyRes.Inserted, applyRes.Merged, applyRes.Pulled)
	for _, e := range applyRes.Errors {
		fmt.Fprintf(deps.stderr, "  ! %s\n", e)
	}
	return nil
}

// firstSpec returns the first element of paths, or "" if the slice is empty.
// Multi-spec phases stash the primary spec path; downstream tooling can
// re-derive others from the roadmap_path if needed.
func firstSpec(paths []string) string {
	if len(paths) > 0 {
		return paths[0]
	}
	return ""
}

func defaultPipeline(p string) string {
	if p == "" {
		return "build"
	}
	return p
}

// defaultPriority lifts an empty proposed priority to P3, the same fall-
// back the daemon's handleAddTask uses on the wire side. Keeping it local
// (rather than the daemon's defaultStr) avoids a daemon import outside
// the daemon-typed surfaces this file already touches.
func defaultPriority(p string) string {
	if p == "" {
		return "P3"
	}
	return p
}

// ---- daemon RPC plumbing ----

// fetchRoadmapProposals starts an async roadmap.decompose and blocks until
// the daemon publishes its terminal event (decompose.proposed or
// decompose.failed). It subscribes to the event stream BEFORE starting the
// job so a fast decompose's event cannot be missed between start and
// subscribe. Progress lines are printed to stderr while it waits.
//
// Backstop: if no terminal event for this decompose_id arrives within 15
// minutes, the subscriber connection is closed and an error is returned.
// The 15-minute timer is reset on every event received (progress included),
// so a long-but-actively-progressing decompose is not killed prematurely.
// A daemon disconnect (stream EOF) surfaces immediately as a clear error.
//
// The backstop is implemented with a goroutine + time.NewTimer (not a read
// deadline), so the main event-read loop is never interrupted mid-frame.
// The goroutine cannot leak: on timeout it closes the conn, which causes
// ReadBytes in the main goroutine to return an error and exit; when the main
// goroutine returns first (terminal event or error), its deferred
// close(resultCh) signals the backstop goroutine to exit cleanly.
func fetchRoadmapProposals(slug, phase string, maxSubtasks int) (*daemon.RoadmapDecomposeResult, error) {
	// 1. Subscribe FIRST so we cannot miss events from a fast decompose.
	conn, err := net.DialTimeout("unix", daemonSocketPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w (is `hive daemon` running?)", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(fmt.Sprintf(
		`{"id":"decompose-%d","method":"events.subscribe","params":{}}%s`,
		time.Now().UnixNano(), "\n"))); err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	rdr := bufio.NewReader(conn)
	if _, err := rdr.ReadBytes('\n'); err != nil { // consume ack line
		return nil, fmt.Errorf("read subscribe ack: %w", err)
	}

	// 2. Start the async job on a SEPARATE short-lived connection (the
	//    subscribe conn is now busy streaming events).
	params := map[string]any{"project_slug": slug, "phase": phase}
	if maxSubtasks > 0 {
		params["max_subtasks"] = maxSubtasks
	}
	startRaw, err := rpcCallRawWithTimeout(rpc.MethodRoadmapDecompose, params, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var ack struct {
		DecomposeID string `json:"decompose_id"`
	}
	if err := json.Unmarshal(startRaw, &ack); err != nil || ack.DecomposeID == "" {
		return nil, fmt.Errorf("start decompose: bad ack %s", startRaw)
	}

	// 3. Backstop: a 15-minute timer that closes the conn if no terminal
	//    event arrives. Implemented as a goroutine so ReadBytes is never
	//    interrupted mid-frame. The timer is reset (via a channel) whenever
	//    any event arrives. When the main goroutine terminates (success or
	//    error), it closes resultCh which makes the backstop goroutine exit
	//    cleanly — no leak.
	const backstop = 15 * time.Minute
	resetCh := make(chan struct{}, 8) // buffered so progress-heavy paths don't block
	resultCh := make(chan struct{})   // closed when the main goroutine is done

	go func() {
		timer := time.NewTimer(backstop)
		defer timer.Stop()
		for {
			select {
			case <-resultCh:
				// Main goroutine finished; exit the backstop goroutine.
				return
			case <-resetCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(backstop)
			case <-timer.C:
				// Close the conn to unblock the ReadBytes in the main goroutine.
				conn.Close()
				return
			}
		}
	}()
	defer close(resultCh) // signal the backstop goroutine when we return

	// 4. Read events until the terminal event for our decompose_id.
	for {
		line, rerr := rdr.ReadBytes('\n')
		if rerr != nil {
			// Both backstop-induced close and a real disconnect surface here;
			// the error message covers both cases.
			return nil, fmt.Errorf("decompose interrupted (daemon disconnected): %w", rerr)
		}

		// Notify the backstop goroutine that an event arrived.
		select {
		case resetCh <- struct{}{}:
		default:
		}

		var ev rpc.EventMessage
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if id, _ := ev.Data["decompose_id"].(string); id != ack.DecomposeID {
			continue
		}
		switch ev.Type {
		case rpc.EventDecomposeProgress:
			label, _ := ev.Data["phase_label"].(string)
			fmt.Fprintf(os.Stderr, "  decomposing: %s…\n", label)
		case rpc.EventDecomposeFailed:
			msg, _ := ev.Data["error"].(string)
			return nil, fmt.Errorf("decompose failed: %s", msg)
		case rpc.EventDecomposeProposed:
			b, _ := json.Marshal(ev.Data["result"])
			var res daemon.RoadmapDecomposeResult
			if err := json.Unmarshal(b, &res); err != nil {
				return nil, fmt.Errorf("decode result: %w", err)
			}
			return &res, nil
		}
	}
}

// listRoadmapPhaseTasks counts tasks already linked to (phase, roadmap_path)
// by filtering the task.list response client-side. There's no server-side
// metadata filter yet, but task.list is fine for the volumes we expect
// (V1 — a few dozen open tasks per project).
//
// Note: task.list returns PENDING tasks only. A phase that was decomposed
// AND all its child tasks completed will pass this guard — that's the
// intended UX (operator wants to add fresh tasks for the same phase
// post-cleanup; the previous batch is no longer in flight).
func listRoadmapPhaseTasks(slug, phase, roadmapPath string) (int, error) {
	raw, err := rpcCallRaw(rpc.MethodListTasks, map[string]any{})
	if err != nil {
		return 0, err
	}
	var tasks []rpc.TaskView
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return 0, fmt.Errorf("decode task list: %w", err)
	}
	count := 0
	for _, t := range tasks {
		if matchMetaString(t.Metadata, "roadmap_phase", phase) &&
			matchMetaString(t.Metadata, "roadmap_path", roadmapPath) {
			// Also gate on project slug via project.list lookup. The
			// task.list response carries ProjectID, not slug; resolve once.
			if projectSlugForID(t.ProjectID) == slug {
				count++
			}
		}
	}
	return count, nil
}

// matchMetaString returns true iff metadata[key] is a string equal to want.
// The metadata column is map[string]any so values are interface-typed;
// the wire shape may also unmarshal as float64 for numerics. We only need
// string-equality here (roadmap_phase / roadmap_path / spec_path are all
// stored as strings on the insert side).
func matchMetaString(meta map[string]any, key, want string) bool {
	v, ok := meta[key]
	if !ok {
		return false
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	return s == want
}

// projectSlugForID is a thin wrapper over the existing lookupProjectSlug
// helper from cmd_decompose.go. Kept as a named function so the metadata
// filter reads cleanly.
func projectSlugForID(projectID string) string {
	return lookupProjectSlug(projectID)
}

// Note: the y/n prompt source is promptYesNo from cmd_decompose.go,
// wired via roadmapDeps.confirm. Re-uses the existing helper so both
// commands behave identically (lowercase "y"/"yes" → approved, else
// cancel).
