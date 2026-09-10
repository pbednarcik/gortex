package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/savings"
)

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		24 * time.Hour:      "24h",
		7 * 24 * time.Hour:  "7d",
		30 * 24 * time.Hour: "30d",
		2 * time.Hour:       "2h",
		90 * time.Minute:    "1h30m0s",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestBenchSourceAge_Bucketing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fresh file, user-provided: explicit timestamp marker.
	if got := benchSourceAge(path, false); !strings.Contains(got, "just generated") {
		t.Errorf("fresh user-provided source = %q, want '(just generated)'", got)
	}
	// Fresh file, auto-discovered: "seconds ago".
	if got := benchSourceAge(path, true); !strings.Contains(got, "seconds ago") {
		t.Errorf("fresh auto-discovered source = %q, want '(seconds ago)'", got)
	}

	// Backdate file → "min ago".
	old := time.Now().Add(-15 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if got := benchSourceAge(path, true); !strings.Contains(got, "min ago") {
		t.Errorf("15-min-old source = %q, want '(N min ago)'", got)
	}

	// Backdate to days.
	old = time.Now().Add(-3 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if got := benchSourceAge(path, true); !strings.Contains(got, "days ago") {
		t.Errorf("3-day-old source = %q, want '(N days ago)'", got)
	}

	// Missing file → empty string (no spurious age).
	if got := benchSourceAge(filepath.Join(dir, "missing.json"), false); got != "" {
		t.Errorf("missing file age = %q, want empty", got)
	}
}

func TestFindLatestBenchTokens_FindsMostRecent(t *testing.T) {
	dir := t.TempDir()
	// Build a minimal bench/results layout mirroring `bench all`.
	run1 := filepath.Join(dir, "run-20260101-000000")
	run2 := filepath.Join(dir, "run-20260518-000000")
	for _, r := range []string{run1, run2} {
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r, "tokens.json"), []byte("[]"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Make run2 newer by explicit chtimes.
	now := time.Now()
	_ = os.Chtimes(filepath.Join(run1, "tokens.json"), now.Add(-72*time.Hour), now.Add(-72*time.Hour))
	_ = os.Chtimes(filepath.Join(run2, "tokens.json"), now.Add(-1*time.Hour), now.Add(-1*time.Hour))

	// chdir to the temp dir so the relative `bench/results` root the
	// function walks resolves under our fixture.
	tmpRoot := filepath.Join(dir, "workspace")
	resultsDir := filepath.Join(tmpRoot, "bench", "results")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{run1, run2} {
		dst := filepath.Join(resultsDir, filepath.Base(r))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(r, "tokens.json")
		dstFile := filepath.Join(dst, "tokens.json")
		data, _ := os.ReadFile(src)
		_ = os.WriteFile(dstFile, data, 0o644)
		st, _ := os.Stat(src)
		_ = os.Chtimes(dstFile, st.ModTime(), st.ModTime())
	}
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(tmpRoot); err != nil {
		t.Fatal(err)
	}

	got, ok := findLatestBenchTokens()
	if !ok {
		t.Fatal("expected to find a bench tokens artifact")
	}
	if !strings.Contains(got, filepath.Base(run2)) {
		t.Errorf("findLatestBenchTokens = %q, want a path under %s (newer)", got, filepath.Base(run2))
	}
}

func TestFindLatestBenchTokens_MissingDirReturnsFalse(t *testing.T) {
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got, ok := findLatestBenchTokens(); ok {
		t.Errorf("expected no result in empty dir, got %q", got)
	}
}

func TestLoadHistory_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	h, err := loadHistory(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if h.Calls != 0 || h.Saved != 0 {
		t.Errorf("empty store should produce zero totals, got %+v", h)
	}
}

func TestLoadHistory_SinceZeroUsesCumulative(t *testing.T) {
	// Populate a ledger with a known total, then call loadHistory with
	// since=0. The result must come from the cumulative snapshot, not
	// an event scan.
	dir := t.TempDir()
	store, err := savings.Open(persistence.DefaultSidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddObservation(savings.Observation{Repo: "/r", Language: "go", Tool: "get_symbol_source", Returned: 50, Saved: 500})
	h, err := loadHistory(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Calls != 1 || h.Saved != 500 {
		t.Errorf("since=0 should reflect cumulative ledger, got %+v", h)
	}
}

func TestLoadHistory_WindowFiltersEvents(t *testing.T) {
	dir := t.TempDir()
	store, err := savings.Open(persistence.DefaultSidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddObservation(savings.Observation{Repo: "/r", Language: "go", Tool: "read_file", Returned: 10, Saved: 90})

	h, err := loadHistory(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if h.Calls != 1 || h.Saved != 90 {
		t.Errorf("fresh event should fall inside a 24h window, got %+v", h)
	}
}

func TestGainHistory_JSON(t *testing.T) {
	h := &gainHistory{
		Path:     "/tmp/savings.json",
		Since:    24 * time.Hour,
		Calls:    42,
		Saved:    1000,
		Returned: 200,
		Costs:    map[string]float64{"claude-opus-4": 0.015},
	}
	out := h.toJSON()
	raw, _ := json.Marshal(out)
	for _, want := range []string{
		`"calls_counted":42`,
		`"tokens_saved":1000`,
		`"cost_avoided_usd":{`,
		`"claude-opus-4":0.015`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("history JSON missing %q\n%s", want, raw)
		}
	}
}

func TestRenderGainProjection_HeadlineMarker(t *testing.T) {
	var buf bytes.Buffer
	rows := []tokensMetric{
		{Case: "a", JSONTokens: 100, GCXTokens: 80},
	}
	renderGainProjection(&buf, rows, 1000, "claude-opus-4-8")
	out := buf.String()
	if !strings.Contains(out, "*claude-opus-4-8") {
		t.Errorf("headline model should be marked with *: %s", out)
	}
	if !strings.Contains(out, "*headlined: claude-opus-4-8") {
		t.Errorf("footer should name the headlined model: %s", out)
	}
}

func TestRenderGainProjection_NoHeadlineMarksNone(t *testing.T) {
	var buf bytes.Buffer
	rows := []tokensMetric{
		{Case: "a", JSONTokens: 100, GCXTokens: 80},
	}
	renderGainProjection(&buf, rows, 1000, "")
	out := buf.String()
	if strings.Contains(out, "*headlined") {
		t.Errorf("no headline → no footer; got: %s", out)
	}
	if strings.Contains(out, "*claude-opus-4") {
		t.Errorf("no headline → no row marker; got: %s", out)
	}
}

func TestGainCmd_Registered(t *testing.T) {
	subs := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		subs[c.Name()] = true
	}
	if !subs["gain"] {
		t.Errorf("rootCmd missing `gain`; have %v", subs)
	}
}
