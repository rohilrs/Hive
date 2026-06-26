package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/store"
)

var (
	statsProject string
	statsSince   string
	statsFormat  string
	statsStrict  bool
)

var predictStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Aggregate predictor metrics (latency, counts, error rate)",
	Long: `Reads the predictor_metrics table populated by the daemon
on each dispatch and prints latency percentiles + candidate-count
means + truncation/error rates.

Use --project to scope to one project; --since to limit the window
(e.g. --since 24h, --since 7d).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, err := defaultHiveDBPath()
		if err != nil {
			return err
		}
		s, err := store.Open(cmd.Context(), dbPath)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer s.Close()

		var sinceT time.Time
		if statsSince != "" {
			d, err := parseSinceDuration(statsSince)
			if err != nil {
				return fmt.Errorf("--since: %w", err)
			}
			sinceT = time.Now().Add(-d)
		}
		hadRows, err := runPredictStats(cmd.Context(), cmd.OutOrStdout(), s, statsProject, sinceT, statsFormat)
		if err != nil {
			return err
		}
		// --strict: exit 2 when no rows matched the filter, so scripted
		// alerts can fire on "predictor has run zero times in the window".
		if statsStrict && !hadRows {
			fmt.Fprintln(os.Stderr, "no predictor_metrics rows matched the filter")
			os.Exit(2)
		}
		return nil
	},
}

func init() {
	predictStatsCmd.Flags().StringVar(&statsProject, "project", "", "filter to one project slug (optional)")
	predictStatsCmd.Flags().StringVar(&statsSince, "since", "", "filter to last N (e.g. 24h, 7d)")
	predictStatsCmd.Flags().StringVar(&statsFormat, "format", "human", "output format: human|json")
	predictStatsCmd.Flags().BoolVar(&statsStrict, "strict", false, "exit code 2 if no rows match (useful for scripted alerts)")
}

// defaultHiveDBPath returns the daemon's DB path: $HOME/.hive/db.sqlite.
// Mirrors the os.UserHomeDir() pattern used in cmd_daemon.go.
func defaultHiveDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".hive", "db.sqlite"), nil
}

// parseSinceDuration extends time.ParseDuration with a "Nd" (days)
// suffix. "7d" → 168h. The Go stdlib only supports ns/us/ms/s/m/h.
// Anything not ending in "d" is passed straight through.
func parseSinceDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid days value: %s", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// stats is the computed aggregate, used for both human + JSON formats.
type stats struct {
	Count             int     `json:"count"`
	HaikuLatencyP50MS int64   `json:"haiku_latency_p50_ms"`
	HaikuLatencyP95MS int64   `json:"haiku_latency_p95_ms"`
	FetchLatencyP50MS int64   `json:"fetch_latency_p50_ms"`
	FetchLatencyP95MS int64   `json:"fetch_latency_p95_ms"`
	MeanCandidates    float64 `json:"mean_candidates"`
	MeanInline        float64 `json:"mean_inline"`
	MeanOverflow      float64 `json:"mean_overflow"`
	TruncationRate    float64 `json:"truncation_rate"`
	ErrorRate         float64 `json:"error_rate"`

	// Phase 2c.1 additions
	PrecisionP50     float64 `json:"precision_p50"`
	PrecisionP95     float64 `json:"precision_p95"`
	RecallP50        float64 `json:"recall_p50"`
	RecallP95        float64 `json:"recall_p95"`
	AccuracyCoverage float64 `json:"accuracy_coverage"` // fraction of metric rows that have a *computed* (non-skipped) accuracy row
}

// runPredictStats returns (hadRows, err) — hadRows is true iff at least one
// predictor_metrics row matched the filter. Used by --strict to set exit code.
func runPredictStats(ctx context.Context, out io.Writer, s *store.Store, project string, since time.Time, format string) (bool, error) {
	rows, err := s.ListPredictorMetrics(ctx, store.ListPredictorMetricsFilter{
		ProjectID: project,
		Since:     since,
	})
	if err != nil {
		return false, err
	}
	accs, err := s.ListPredictorAccuracy(ctx, store.ListPredictorAccuracyFilter{Since: since})
	if err != nil {
		return false, err
	}
	st := computeStats(rows, accs)
	switch format {
	case "json":
		return st.Count > 0, writeStatsJSON(out, st)
	default:
		writeStatsHuman(out, st)
		return st.Count > 0, nil
	}
}

func computeStats(rows []*store.PredictorMetric, accs []*store.PredictorAccuracy) stats {
	var st stats
	st.Count = len(rows)
	if st.Count == 0 {
		return st
	}
	haikuMS := make([]int64, len(rows))
	fetchMS := make([]int64, len(rows))
	var sumC, sumI, sumO int
	var truncated, errored int
	for i, r := range rows {
		haikuMS[i] = r.HaikuLatencyMS
		fetchMS[i] = r.FetchLatencyMS
		sumC += r.CandidateCount
		sumI += r.InlineCount
		sumO += r.OverflowCount
		if r.Truncated {
			truncated++
		}
		if r.Error != "" {
			errored++
		}
	}
	st.HaikuLatencyP50MS = percentileInt64(haikuMS, 0.50)
	st.HaikuLatencyP95MS = percentileInt64(haikuMS, 0.95)
	st.FetchLatencyP50MS = percentileInt64(fetchMS, 0.50)
	st.FetchLatencyP95MS = percentileInt64(fetchMS, 0.95)
	st.MeanCandidates = float64(sumC) / float64(st.Count)
	st.MeanInline = float64(sumI) / float64(st.Count)
	st.MeanOverflow = float64(sumO) / float64(st.Count)
	st.TruncationRate = float64(truncated) / float64(st.Count)
	st.ErrorRate = float64(errored) / float64(st.Count)

	// Phase 2c.1: accuracy aggregates. Coverage = fraction of metric
	// rows that have a non-skipped accuracy row. Percentiles compute
	// over computed-only rows.
	metricRunIDs := make(map[string]bool, len(rows))
	for _, r := range rows {
		metricRunIDs[r.RunID] = true
	}
	var precVals, recVals []float64
	for _, a := range accs {
		if !metricRunIDs[a.RunID] || a.SkippedReason != "" {
			continue
		}
		if a.Precision != nil {
			precVals = append(precVals, *a.Precision)
		}
		if a.Recall != nil {
			recVals = append(recVals, *a.Recall)
		}
	}
	if len(precVals) > 0 {
		st.PrecisionP50 = percentileFloat64(precVals, 0.50)
		st.PrecisionP95 = percentileFloat64(precVals, 0.95)
	}
	if len(recVals) > 0 {
		st.RecallP50 = percentileFloat64(recVals, 0.50)
		st.RecallP95 = percentileFloat64(recVals, 0.95)
	}
	st.AccuracyCoverage = float64(len(precVals)) / float64(st.Count)
	return st
}

// percentileFloat64 is the float analog of percentileInt64 (nearest-rank).
func percentileFloat64(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)
	rank := int(float64(len(sorted))*p + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// percentileInt64 uses the nearest-rank method (NIST-recommended for small
// samples). For p=0.50 with 3 elements [100,200,300]: rank=int(0.5*3+0.5)=2,
// returns sorted[1]=200. For p=0.95: rank=int(0.95*3+0.5)=3, returns
// sorted[2]=300.
func percentileInt64(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]int64, len(xs))
	copy(sorted, xs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(float64(len(sorted))*p + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func writeStatsHuman(out io.Writer, s stats) {
	if s.Count == 0 {
		fmt.Fprintln(out, "no predictor metrics in selected window")
		return
	}
	fmt.Fprintf(out, "count: %d\n\n", s.Count)
	fmt.Fprintln(out, "haiku latency:")
	fmt.Fprintf(out, "  p50: %dms\n", s.HaikuLatencyP50MS)
	fmt.Fprintf(out, "  p95: %dms\n", s.HaikuLatencyP95MS)
	fmt.Fprintln(out, "fetch latency:")
	fmt.Fprintf(out, "  p50: %dms\n", s.FetchLatencyP50MS)
	fmt.Fprintf(out, "  p95: %dms\n", s.FetchLatencyP95MS)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "mean candidates: %.2f\n", s.MeanCandidates)
	fmt.Fprintf(out, "mean inline:     %.2f\n", s.MeanInline)
	fmt.Fprintf(out, "mean overflow:   %.2f\n", s.MeanOverflow)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "truncation rate: %.1f%%\n", s.TruncationRate*100)
	fmt.Fprintf(out, "error rate: %.1f%%\n", s.ErrorRate*100)

	// Phase 2c.1: accuracy
	fmt.Fprintln(out)
	fmt.Fprintf(out, "accuracy coverage: %.1f%%\n", s.AccuracyCoverage*100)
	if s.AccuracyCoverage > 0 {
		fmt.Fprintf(out, "precision p50: %.3f\n", s.PrecisionP50)
		fmt.Fprintf(out, "precision p95: %.3f\n", s.PrecisionP95)
		fmt.Fprintf(out, "recall p50: %.3f\n", s.RecallP50)
		fmt.Fprintf(out, "recall p95: %.3f\n", s.RecallP95)
	}
}

func writeStatsJSON(out io.Writer, s stats) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}
