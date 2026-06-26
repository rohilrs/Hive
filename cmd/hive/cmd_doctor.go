package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/rohilrs/Hive/internal/doctor"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// doctorCmd is registered in main.go alongside the other subcommands.
var doctorCmd = newDoctorCmd()

func newDoctorCmd() *cobra.Command {
	var jsonOut, verbose, strict bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Audit daemon + state + config across all subsystems",
		Long: `hive doctor runs read-only health and drift checks across
the daemon process, store, worktrees, sources, MCP server, config
files, and approvals queue. Exits 0 on success, 1 on any errors,
2 on warnings when --strict is set.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			hiveDir := hiveDirForDoctor()
			client := newDoctorRPCClient()
			rep := doctor.Run(context.Background(), hiveDir, client)

			if jsonOut {
				if err := renderJSON(os.Stdout, rep); err != nil {
					return err
				}
			} else {
				renderHuman(os.Stdout, rep, verbose)
			}
			os.Exit(exitCodeFromReport(rep, strict))
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON report instead of human output")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show OK checks (default hides them)")
	cmd.Flags().BoolVar(&strict, "strict", false, "exit code 2 if any warnings (default: only errors exit non-zero)")
	return cmd
}

// hiveDirForDoctor mirrors how other CLI commands locate ~/.hive.
// There's no HIVE_DIR env var today; the daemon's socket+state always
// lives at ~/.hive (see cmd_daemon.go).
func hiveDirForDoctor() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hive")
}

// doctorRPCClient wraps the existing UDS-dial helpers (rpcCall) so the
// doctor package gets a narrow Status/Health interface without an
// internal/daemon import.
type doctorRPCClient struct{}

func newDoctorRPCClient() doctor.RPCClient {
	return &doctorRPCClient{}
}

// Status pings daemon.status. A successful response (even an empty
// result map) means the socket is reachable; any error means it isn't.
func (c *doctorRPCClient) Status(ctx context.Context) error {
	_, err := rpcCall(rpc.MethodStatus, map[string]any{})
	return err
}

// Health calls daemon.health and unmarshals into a doctor.HealthSnapshot.
// Both sides share the same JSON tags so the field-for-field unmarshal
// is direct.
func (c *doctorRPCClient) Health(ctx context.Context) (doctor.HealthSnapshot, error) {
	raw, err := rpcCallRaw(rpc.MethodHealth, map[string]any{})
	if err != nil {
		return doctor.HealthSnapshot{}, err
	}
	var snap doctor.HealthSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return doctor.HealthSnapshot{}, fmt.Errorf("unmarshal health: %w", err)
	}
	return snap, nil
}

// SourcesStatus calls sources.status and unmarshals into a map keyed by
// source name. Same JSON-tag mirroring as Health.
func (c *doctorRPCClient) SourcesStatus(ctx context.Context) (map[string]doctor.SourceStatusEntry, error) {
	raw, err := rpcCallRaw(rpc.MethodSourcesStatus, map[string]any{})
	if err != nil {
		return nil, err
	}
	var out map[string]doctor.SourceStatusEntry
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse sources_status: %w", err)
	}
	return out, nil
}

func renderJSON(w io.Writer, rep doctor.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func renderHuman(w io.Writer, rep doctor.Report, verbose bool) {
	fmt.Fprintln(w, "hive doctor — daemon + state audit")
	fmt.Fprintln(w)
	// Group by subsystem; preserve insertion order across subsystems.
	groups := make(map[string][]doctor.Check)
	var order []string
	for _, c := range rep.Checks {
		if _, seen := groups[c.Subsystem]; !seen {
			order = append(order, c.Subsystem)
		}
		groups[c.Subsystem] = append(groups[c.Subsystem], c)
	}
	for _, sub := range order {
		// Skip subsystem section entirely if every check is OK and !verbose.
		hasNonOK := false
		for _, c := range groups[sub] {
			if c.Status != doctor.StatusOK {
				hasNonOK = true
				break
			}
		}
		if !hasNonOK && !verbose {
			continue
		}
		fmt.Fprintln(w, sub)
		// Stable within subsystem.
		sorted := append([]doctor.Check(nil), groups[sub]...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, c := range sorted {
			if c.Status == doctor.StatusOK && !verbose {
				continue
			}
			fmt.Fprintf(w, "  %s %-32s %s\n", glyph(c.Status), c.Name, c.Message)
			if c.Hint != "" {
				// TrimRight defends against a trailing newline producing
				// an empty final line in the rendered output.
				for _, line := range strings.Split(strings.TrimRight(c.Hint, "\n"), "\n") {
					fmt.Fprintf(w, "    — %s\n", line)
				}
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "summary: %d ok · %d warnings · %d errors · %d skipped\n",
		rep.Summary.OK, rep.Summary.Warnings, rep.Summary.Errors, rep.Summary.Skipped)
}

// glyph chooses between Unicode glyphs (TTY output) and ASCII labels
// (piped output / non-UTF terminals). The TTY check is memoized so the
// per-check render path stays cheap.
func glyph(s doctor.Status) string {
	if useASCII() {
		return asciiLabel(s)
	}
	return unicodeGlyph(s)
}

var useASCIICached *bool

// useASCII returns true when stdout is NOT a TTY, or when NO_COLOR is
// set, or when TERM=dumb. Memoized for the lifetime of the process so
// renderHuman doesn't re-syscall per check.
func useASCII() bool {
	if useASCIICached != nil {
		return *useASCIICached
	}
	notTTY := !term.IsTerminal(int(os.Stdout.Fd()))
	noColor := os.Getenv("NO_COLOR") != ""
	dumb := os.Getenv("TERM") == "dumb"
	v := notTTY || noColor || dumb
	useASCIICached = &v
	return v
}

func unicodeGlyph(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return "✓"
	case doctor.StatusWarn:
		return "⚠"
	case doctor.StatusError:
		return "✗"
	case doctor.StatusSkip:
		return "·"
	}
	return "?"
}

// asciiLabel pads to a uniform width (6 chars incl. brackets) so the
// "%-32s" check-name column stays aligned even with mixed statuses.
func asciiLabel(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return "[OK]  "
	case doctor.StatusWarn:
		return "[WARN]"
	case doctor.StatusError:
		return "[ERR] "
	case doctor.StatusSkip:
		return "[--]  "
	}
	return "[?]   "
}

func exitCodeFromReport(rep doctor.Report, strict bool) int {
	if rep.Summary.Errors > 0 {
		return 1
	}
	if strict && rep.Summary.Warnings > 0 {
		return 2
	}
	return 0
}
