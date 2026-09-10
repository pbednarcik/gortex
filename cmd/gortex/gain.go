// gain.go — forward-looking USD summary. Answers "what is gortex saving
// me per call, in dollars, at common model rates?" using the most
// recent bench tokens output as the projection source. Optionally
// folds in cumulative savings history (`gortex savings`) so existing
// users see both the typical-call projection and their own track
// record side by side.
//
// gain vs savings:
//   - savings   — rear-looking: what HAPPENED across past MCP calls
//     (your actual cumulative store + JSONL event log)
//   - gain      — forward-looking: what gortex SAVES on a typical
//     call (benchmark-derived, works on fresh installs
//     with no history)
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/savings"
)

var (
	gainBenchResult     string
	gainResponsesPerDay int
	gainModel           string
	gainSince           time.Duration
	gainJSON            bool
	gainNoHistory       bool
	gainCacheDir        string
)

var gainCmd = &cobra.Command{
	Use:   "gain",
	Short: "Show typical per-call USD savings (from latest bench) + optional history",
	Long: `gortex gain answers "what is gortex saving me, in dollars?" by
projecting the latest benchmark's median token savings against each
priced model. Pairs the benchmark projection with your cumulative
savings history when one exists.

Default behavior:
  1. Find the most recent gortex bench tokens output (auto-discovery
     under bench/results/, then a transparent re-run when none).
  2. Render a USD-per-model card scaled to --responses-per-day.
  3. Append a short "Your history" section from the savings ledger
     (~/.gortex/sidecar.sqlite) when --since's window has any tracked calls.

Flags:
  --bench-result PATH     specific bench tokens JSON to use (skip discovery)
  --responses-per-day N   scale for projection (default 1000)
  --model NAME            headline a single model in the output
  --since DURATION        history window (e.g. 24h, 7d; default 7d)
  --json                  emit machine-readable JSON
  --no-history            skip the cumulative-history section
  --cache-dir DIR         override the ledger directory (its sidecar.sqlite)`,
	RunE: runGain,
}

func init() {
	gainCmd.Flags().StringVar(&gainBenchResult, "bench-result", "", "specific bench tokens JSON to use; skips auto-discovery")
	gainCmd.Flags().IntVar(&gainResponsesPerDay, "responses-per-day", 1000, "responses/day used to scale the USD-per-model card")
	gainCmd.Flags().StringVar(&gainModel, "model", "", "headline a specific model in the output")
	gainCmd.Flags().DurationVar(&gainSince, "since", 7*24*time.Hour, "history window for the cumulative-savings section (e.g. 24h, 7d)")
	gainCmd.Flags().BoolVar(&gainJSON, "json", false, "emit machine-readable JSON")
	gainCmd.Flags().BoolVar(&gainNoHistory, "no-history", false, "skip the cumulative-history section")
	gainCmd.Flags().StringVar(&gainCacheDir, "cache-dir", "", "override the ledger directory (its sidecar.sqlite holds the savings ledger)")
	rootCmd.AddCommand(gainCmd)
}

func runGain(cmd *cobra.Command, _ []string) error {
	// 1. Find or generate bench tokens metrics. Auto-discovery first;
	//    fall through to running `gortex bench tokens` to a temp file
	//    so a fresh install still produces a meaningful number.
	benchPath := gainBenchResult
	autoFound := false
	if benchPath == "" {
		if found, ok := findLatestBenchTokens(); ok {
			benchPath = found
			autoFound = true
		}
	}
	var tmpBench string
	if benchPath == "" {
		f, err := os.CreateTemp("", "gortex-gain-*.json")
		if err != nil {
			return err
		}
		tmpBench = f.Name()
		_ = f.Close()
		defer func() { _ = os.Remove(tmpBench) }()
		if err := runBenchTokensInto(tmpBench); err != nil {
			return fmt.Errorf("running bench tokens: %w", err)
		}
		benchPath = tmpBench
	}
	metrics, err := loadTokensMetrics(benchPath)
	if err != nil {
		return fmt.Errorf("load bench tokens %s: %w", benchPath, err)
	}
	benchAge := benchSourceAge(benchPath, autoFound)

	// 2. Optionally fold in cumulative-history snapshot.
	var history *gainHistory
	if !gainNoHistory {
		h, herr := loadHistory(gainCacheDir, gainSince)
		if herr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "[gortex gain] history unavailable: %v\n", herr)
		} else {
			history = h
		}
	}

	if gainJSON {
		out := map[string]any{
			"generated":        time.Now().UTC().Format(time.RFC3339),
			"bench_source":     benchPath,
			"bench_age_string": benchAge,
			"projection":       buildUSDCardJSON(metrics, gainResponsesPerDay),
		}
		if history != nil {
			out["history"] = history.toJSON()
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		_, _ = cmd.OutOrStdout().Write(raw)
		fmt.Fprintln(cmd.OutOrStdout())
		return nil
	}

	// Markdown output. Writes go through cmd.OutOrStdout() (cobra
	// convention); errors on print to stdout are not actionable so
	// we explicitly discard the return values.
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "Gortex Token Gain")
	fmt.Fprintln(w, "=================")
	fmt.Fprintf(w, "Bench source:   %s\n", benchPath)
	if benchAge != "" {
		fmt.Fprintf(w, "Bench age:      %s\n", benchAge)
	}
	if gainModel != "" {
		fmt.Fprintf(w, "Headline model: %s\n", gainModel)
	}
	medianCL := medianSavedTokens(metrics, false)
	medianOpus := medianSavedTokens(metrics, true)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Median tokens saved / response: **%d** (cl100k_base)", medianCL)
	if medianOpus > 0 {
		fmt.Fprintf(w, ", **%d** (Opus 4.7)", medianOpus)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Projected at %d responses/day:\n\n", gainResponsesPerDay)
	renderGainProjection(w, metrics, gainResponsesPerDay, gainModel)

	if history != nil && history.Calls > 0 {
		fmt.Fprintln(w)
		renderGainHistory(w, history, gainModel)
	}
	return nil
}

// --- bench auto-discovery -------------------------------------------

// findLatestBenchTokens scans bench/results/ for the most recent
// tokens metrics file. Two layouts are supported:
//   - bench/results/run-<ts>/tokens.json  (the `bench all` shape)
//   - bench/results/tokens-<ts>.json      (legacy / one-off shape)
//
// Returns the newest match by modification time; empty + false when
// no match exists.
func findLatestBenchTokens() (string, bool) {
	roots := []string{"bench/results"}
	type cand struct {
		path string
		mod  time.Time
	}
	var best cand
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			name := info.Name()
			if name != "tokens.json" && !strings.HasPrefix(name, "tokens-") {
				return nil
			}
			if !strings.HasSuffix(name, ".json") {
				return nil
			}
			if info.ModTime().After(best.mod) {
				best = cand{path: path, mod: info.ModTime()}
			}
			return nil
		})
	}
	if best.path == "" {
		return "", false
	}
	return best.path, true
}

// runBenchTokensInto invokes `gortex bench tokens --out-dir <tmp>
// --format markdown` so we get the JSON sidecar (the bench command
// always writes JSON when --out-dir is set). Suppresses the
// scorecard markdown that would otherwise hit stdout.
func runBenchTokensInto(jsonPath string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(jsonPath)
	subproc := exec.Command(self, "bench", "tokens",
		"--out-dir", dir,
		"--format", "json",
	)
	// Discard subprocess stdout (the JSON envelope) — we read the
	// per-row metrics from the artifact file instead, which is the
	// shape loadTokensMetrics expects.
	subproc.Stdout = nil
	subproc.Stderr = os.Stderr
	if err := subproc.Run(); err != nil {
		return err
	}
	// `bench tokens --out-dir DIR` writes tokens.json there; move it
	// to the caller's path so the rest of gain reads from a stable
	// location.
	produced := filepath.Join(dir, "tokens.json")
	if produced == jsonPath {
		return nil
	}
	return os.Rename(produced, jsonPath)
}

// benchSourceAge returns a short human description of when a bench
// artifact was produced relative to now. Empty when we can't stat.
// `autoFound` distinguishes auto-discovered (relevant context) vs
// user-provided (we don't editorialize about the user's choice).
func benchSourceAge(path string, autoFound bool) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	age := time.Since(info.ModTime())
	if !autoFound && age < time.Minute {
		return "(just generated)"
	}
	switch {
	case age < time.Minute:
		return "(seconds ago)"
	case age < time.Hour:
		return fmt.Sprintf("(%d min ago)", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("(%d hours ago)", int(age.Hours()))
	default:
		return fmt.Sprintf("(%d days ago)", int(age.Hours()/24))
	}
}

// --- USD projection rendering ---------------------------------------

// renderGainProjection prints the per-model day/month table. When a
// single model is headlined, we still print all rows so the reader
// can compare; the headline gets a leading marker.
func renderGainProjection(w interface{ Write([]byte) (int, error) }, rows []tokensMetric, responsesPerDay int, headline string) {
	medianCL := medianSavedTokens(rows, false)
	medianOpus := medianSavedTokens(rows, true)
	fmt.Fprintln(w, "| Model            | $/M input | $/day  | $/month |")
	fmt.Fprintln(w, "|------------------|----------:|-------:|--------:|")
	for _, p := range savings.Pricing() {
		median := medianCL
		if medianOpus > 0 && strings.Contains(strings.ToLower(p.Model), "opus") {
			median = medianOpus
		}
		perResponse := float64(median) / 1_000_000.0 * p.USDPerMInput
		perDay := perResponse * float64(responsesPerDay)
		perMonth := perDay * 30.0
		marker := " "
		if headline != "" && strings.EqualFold(p.Model, headline) {
			marker = "*"
		}
		fmt.Fprintf(w, "|%s%-16s | $%-8.2f | $%-5.2f | $%-7.2f |\n",
			marker, p.Model, p.USDPerMInput, perDay, perMonth)
	}
	if headline != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "*headlined: %s\n", headline)
	}
}

// --- history side ---------------------------------------------------

// gainHistory is the snapshot of the cumulative savings store
// constrained to the --since window. Zero-population when no calls
// fell inside the window.
type gainHistory struct {
	Path     string
	Since    time.Duration
	Calls    int64
	Saved    int64
	Returned int64
	Costs    map[string]float64
}

func (h *gainHistory) toJSON() map[string]any {
	return map[string]any{
		"path":             h.Path,
		"since_seconds":    int64(h.Since.Seconds()),
		"calls_counted":    h.Calls,
		"tokens_saved":     h.Saved,
		"tokens_returned":  h.Returned,
		"cost_avoided_usd": h.Costs,
	}
}

// loadHistory loads the cumulative savings ledger, restricted to the
// since-window via the event history. Returns an error only on hard
// I/O failures; an empty ledger produces a populated gainHistory with
// Calls=0 so the caller can decide whether to render.
func loadHistory(cacheDir string, since time.Duration) (*gainHistory, error) {
	path := savings.DefaultDBPath()
	if cacheDir != "" {
		path = persistence.DefaultSidecarPath(cacheDir)
	}
	store, err := savings.Open(path)
	if err != nil {
		return nil, err
	}
	// One-shot read: release the sidecar handle before returning. Leaving it
	// open pins the sqlite file for the rest of the process, which on Windows
	// makes the containing directory undeletable.
	defer func() { _ = store.Close() }()
	// Same rule as `gortex savings`: the legacy import only runs against
	// the default location — a --cache-dir read must not rename files.
	if cacheDir == "" {
		_ = store.ImportLegacy(savings.DefaultPath())
	}
	snap, serr := store.Snapshot()
	if serr != nil {
		fmt.Fprintf(os.Stderr, "[gortex gain] savings totals read failed: %v\n", serr)
	}

	if since <= 0 {
		// --since 0 → entire-history view; just use the cumulative
		// totals. No event scan needed.
		return &gainHistory{
			Path:     path,
			Since:    since,
			Calls:    snap.Totals.CallsCounted,
			Saved:    snap.Totals.TokensSaved,
			Returned: snap.Totals.TokensReturned,
			Costs:    savings.CostAvoidedAll(snap.Totals.TokensSaved),
		}, nil
	}

	cutoff := time.Now().UTC().Add(-since)
	events, err := store.EventsSince(cutoff)
	if err != nil {
		return nil, err
	}
	totals, _ := savings.AggregateByTool(events)
	return &gainHistory{
		Path:     path,
		Since:    since,
		Calls:    totals.CallsCounted,
		Saved:    totals.TokensSaved,
		Returned: totals.TokensReturned,
		Costs:    savings.CostAvoidedAll(totals.TokensSaved),
	}, nil
}

// renderGainHistory prints the "your history" section: how many calls
// in the window, tokens saved, cost avoided at each priced model.
func renderGainHistory(w interface{ Write([]byte) (int, error) }, h *gainHistory, headline string) {
	fmt.Fprintf(w, "Your history (last %s):\n", humanDuration(h.Since))
	fmt.Fprintf(w, "  Calls counted:  %d\n", h.Calls)
	fmt.Fprintf(w, "  Tokens saved:   %s\n", humanInt(h.Saved))
	if h.Returned > 0 {
		ratio := float64(h.Saved+h.Returned) / float64(h.Returned)
		fmt.Fprintf(w, "  Efficiency:     %.1fx vs naive full-file reads\n", ratio)
	}
	if len(h.Costs) > 0 {
		featured, model := pickHeadlineCost(h.Costs, headline)
		fmt.Fprintf(w, "  Cost avoided:   %s (%s)\n", formatUSD(featured), model)
		// Show all rows under the headline so the reader can compare.
		names := make([]string, 0, len(h.Costs))
		for n := range h.Costs {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintln(w, "  Per model:")
		for _, n := range names {
			marker := " "
			if strings.EqualFold(n, model) {
				marker = "*"
			}
			fmt.Fprintf(w, "    %s%-20s %s\n", marker, n, formatUSD(h.Costs[n]))
		}
	}
}

// humanDuration renders a Duration in a way the user reads naturally
// for the common windows: 24h → "24h", 7*24h → "7d", 30*24h → "30d".
// Falls back to the default Go format for unusual values.
func humanDuration(d time.Duration) string {
	switch {
	case d == 24*time.Hour:
		return "24h"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return d.String()
	}
}
