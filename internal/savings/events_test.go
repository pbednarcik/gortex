package savings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// appendLegacyEventLine writes one JSONL line the way the flat-file era
// did — test fixture for the legacy reader.
func appendLegacyEventLine(t *testing.T, path string, ev Event) {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

// TestEventsPathFor asserts the sibling-path derivation. EventsPathFor
// names a file on disk (never a '/'-separated graph key), so it builds
// with filepath.Join and returns the host's native separators — the
// fixtures are therefore spelled natively too.
func TestEventsPathFor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{filepath.FromSlash("/cache/savings.json"), filepath.FromSlash("/cache/savings.jsonl")},
		{filepath.FromSlash("/cache/whatever.foo"), filepath.FromSlash("/cache/whatever.jsonl")},
		{filepath.FromSlash("./bare"), "bare.jsonl"},
	}
	for _, c := range cases {
		got := EventsPathFor(c.in)
		if got != c.want {
			t.Errorf("EventsPathFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAddObservation_RecordsEvent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sidecar.sqlite"))
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	s.AddObservation(Observation{Repo: "/repo-a", Language: "go", Tool: "get_symbol_source", SessionID: "sess-1", Returned: 100, Saved: 900})
	s.AddObservation(Observation{Repo: "/repo-b", Language: "ts", Tool: "batch_symbols", Returned: 50, Saved: 50})

	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(evs), 2; got != want {
		t.Fatalf("EventsSince = %d, want %d", got, want)
	}
	if evs[0].Tool != "get_symbol_source" || evs[0].Saved != 900 || evs[0].Repo != "/repo-a" {
		t.Errorf("event 0 = %+v", evs[0])
	}
	if evs[0].SessionID != "sess-1" {
		t.Errorf("event 0 session = %q, want sess-1", evs[0].SessionID)
	}
	if evs[1].Tool != "batch_symbols" || evs[1].Returned != 50 || evs[1].Language != "ts" {
		t.Errorf("event 1 = %+v", evs[1])
	}
}

func TestLoadEvents_FiltersSince(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.jsonl")

	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	for i, ts := range []time.Time{
		now.Add(-48 * time.Hour),
		now.Add(-24 * time.Hour),
		now.Add(-1 * time.Hour),
	} {
		appendLegacyEventLine(t, path, Event{TS: ts, Tool: "t", Saved: int64(i+1) * 10})
	}

	// since = now - 25h should drop the 48h-old one.
	evs, err := LoadEvents(path, now.Add(-25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Errorf("LoadEvents(since=-25h) = %d, want 2", len(evs))
	}

	// since = zero returns all.
	evs, err = LoadEvents(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Errorf("LoadEvents(since=0) = %d, want 3", len(evs))
	}
}

func TestLoadEvents_SkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.jsonl")

	// One good line, one garbage line, one good line — simulates a
	// crashed mid-write previous process.
	body := `{"ts":"2026-05-18T10:00:00Z","tool":"a","saved":10,"returned":1}` + "\n" +
		`{not json at all}` + "\n" +
		`{"ts":"2026-05-18T11:00:00Z","tool":"b","saved":20,"returned":2}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	evs, err := LoadEvents(path, time.Time{})
	if err != nil {
		t.Fatalf("LoadEvents should tolerate malformed lines, got: %v", err)
	}
	if len(evs) != 2 {
		t.Errorf("LoadEvents = %d, want 2 (malformed line skipped)", len(evs))
	}
}

func TestLoadEvents_MissingFile(t *testing.T) {
	evs, err := LoadEvents("/nonexistent/savings.jsonl", time.Time{})
	if err != nil {
		t.Errorf("missing event log should not error, got: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("missing event log = %d events, want 0", len(evs))
	}
}

func TestAggregateByTool_SortedDescending(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{TS: now, Tool: "smart_context", Saved: 100, Returned: 10},
		{TS: now, Tool: "get_symbol_source", Saved: 500, Returned: 50},
		{TS: now, Tool: "get_symbol_source", Saved: 200, Returned: 20},
		{TS: now, Tool: "", Saved: 5, Returned: 5},
	}
	total, per := AggregateByTool(events)
	if total.CallsCounted != 4 {
		t.Errorf("total calls = %d, want 4", total.CallsCounted)
	}
	if total.TokensSaved != 805 {
		t.Errorf("total saved = %d, want 805", total.TokensSaved)
	}
	if len(per) != 3 {
		t.Fatalf("per-tool rows = %d, want 3", len(per))
	}
	// Sorted by tokens-saved descending: get_symbol_source(700) → smart_context(100) → (unknown)(5)
	if per[0].Tool != "get_symbol_source" || per[0].TokensSaved != 700 {
		t.Errorf("row 0 = %+v, want get_symbol_source=700", per[0])
	}
	if per[1].Tool != "smart_context" {
		t.Errorf("row 1 = %+v, want smart_context", per[1])
	}
	if per[2].Tool != "(unknown)" {
		t.Errorf("row 2 = %+v, want (unknown) for empty tool", per[2])
	}
}

func TestFilterDay_LocalAndUTC(t *testing.T) {
	// 2026-05-18 03:00 UTC == 2026-05-17 20:00 PDT.
	pdt, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("PDT zoneinfo unavailable")
	}
	earlyUTC := time.Date(2026, 5, 18, 3, 0, 0, 0, time.UTC)
	lateUTC := time.Date(2026, 5, 18, 23, 0, 0, 0, time.UTC) // 16:00 PDT same day
	events := []Event{
		{TS: earlyUTC, Tool: "a", Saved: 1, Returned: 0},
		{TS: lateUTC, Tool: "b", Saved: 1, Returned: 0},
	}

	// In UTC, both fall on 2026-05-18 → both selected.
	got := FilterDay(events, lateUTC, time.UTC)
	if len(got) != 2 {
		t.Errorf("UTC day filter = %d events, want 2", len(got))
	}

	// In PDT, earlyUTC is 2026-05-17 → only lateUTC matches when we
	// pass "today" as 2026-05-18 PDT.
	dayPDT := time.Date(2026, 5, 18, 12, 0, 0, 0, pdt)
	got = FilterDay(events, dayPDT, pdt)
	if len(got) != 1 || got[0].Tool != "b" {
		t.Errorf("PDT day filter = %+v, want [b]", got)
	}
}

func TestBuildDashboard_BucketsByWindow(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	// 3 today events, 2 within 7d but not today, 1 outside 7d.
	mk := func(ts time.Time, tool string, saved int64) Event {
		return Event{TS: ts, Tool: tool, Saved: saved, Returned: 10}
	}
	events := []Event{
		mk(now.Add(-30*24*time.Hour), "old_tool", 9999), // outside 7d
		mk(now.Add(-5*24*time.Hour), "smart_context", 300),
		mk(now.Add(-3*24*time.Hour), "smart_context", 200),
		mk(now.Add(-3*time.Hour), "get_symbol_source", 150),
		mk(now.Add(-2*time.Hour), "get_symbol_source", 100),
		mk(now.Add(-30*time.Minute), "batch_symbols", 50),
	}

	all := Totals{TokensSaved: 9999 + 300 + 200 + 50 + 150 + 100, TokensReturned: 60, CallsCounted: 6}

	_, allPerTool := AggregateByTool(events)
	buckets := BuildDashboard(events, all, allPerTool, now, time.UTC)
	if len(buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(buckets))
	}

	today := buckets[0]
	if today.Label != "Today" {
		t.Errorf("buckets[0].Label = %q, want Today", today.Label)
	}
	if today.Totals.CallsCounted != 3 {
		t.Errorf("Today calls = %d, want 3", today.Totals.CallsCounted)
	}
	if today.Totals.TokensSaved != 300 {
		t.Errorf("Today saved = %d, want 300", today.Totals.TokensSaved)
	}

	week := buckets[1]
	if week.Label != "Last 7 days" {
		t.Errorf("buckets[1].Label = %q, want Last 7 days", week.Label)
	}
	if week.Totals.CallsCounted != 5 {
		t.Errorf("7d calls = %d, want 5 (today + 2 within 7d)", week.Totals.CallsCounted)
	}
	if week.Totals.TokensSaved != 800 {
		t.Errorf("7d saved = %d, want 800", week.Totals.TokensSaved)
	}

	all2 := buckets[2]
	if all2.Label != "All time" {
		t.Errorf("buckets[2].Label = %q, want All time", all2.Label)
	}
	if all2.Totals != all {
		t.Errorf("All time totals = %+v, want %+v (from store, not events)", all2.Totals, all)
	}
	// All time per-tool covers the full history, so the 30-day-old
	// event is included.
	gotTools := map[string]bool{}
	for _, tt := range all2.PerTool {
		gotTools[tt.Tool] = true
	}
	if !gotTools["old_tool"] {
		t.Errorf("All-time per-tool missing the >7d event (got %v)", gotTools)
	}
}

func TestSavingsPercent(t *testing.T) {
	cases := []struct {
		name string
		in   Totals
		want float64
	}{
		{"empty", Totals{}, 0},
		{"only saved", Totals{TokensSaved: 100}, 100},
		{"only returned", Totals{TokensReturned: 100}, 0},
		{"half-half", Totals{TokensSaved: 50, TokensReturned: 50}, 50},
		{"three quarters", Totals{TokensSaved: 75, TokensReturned: 25}, 75},
	}
	for _, c := range cases {
		got := SavingsPercent(c.in)
		if got != c.want {
			t.Errorf("%s: SavingsPercent(%+v) = %.2f, want %.2f", c.name, c.in, got, c.want)
		}
	}
}

func TestBarString_Width(t *testing.T) {
	cases := []struct {
		pct      float64
		cells    int
		wantFill int
	}{
		{0, 16, 0},
		{100, 16, 16},
		{50, 16, 8},
		{75, 16, 12},
		{-5, 16, 0},
		{150, 16, 16},
	}
	for _, c := range cases {
		bar := BarString(c.pct, c.cells)
		// Each cell is one rune. Count runes (not bytes — █ and ░ are multi-byte).
		fill := strings.Count(bar, "█")
		empty := strings.Count(bar, "░")
		if fill+empty != c.cells {
			t.Errorf("pct=%.1f cells=%d: total cells = %d, want %d (bar=%q)", c.pct, c.cells, fill+empty, c.cells, bar)
		}
		if fill != c.wantFill {
			t.Errorf("pct=%.1f cells=%d: filled = %d, want %d (bar=%q)", c.pct, c.cells, fill, c.wantFill, bar)
		}
	}
}

func TestBarString_DefaultsCellsTo16(t *testing.T) {
	bar := BarString(50, 0)
	if got := strings.Count(bar, "█") + strings.Count(bar, "░"); got != 16 {
		t.Errorf("BarString(50, 0) cells = %d, want default 16", got)
	}
}

func TestEvents_ConcurrentWritersAllRecorded(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sidecar.sqlite"))
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}

	const writers = 8
	const per = 50
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range per {
				s.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "tool", Returned: 1, Saved: 10})
			}
		})
	}
	wg.Wait()

	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(evs), writers*per; got != want {
		t.Errorf("EventsSince = %d, want %d (concurrent observation lost)", got, want)
	}
}

func TestEventHistory_InMemoryStoreStaysInMemory(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	s.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "tool", Returned: 1, Saved: 10})
	if s.sc != nil {
		t.Fatal("empty-path store must not open a sidecar")
	}
	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Errorf("in-memory event history = %d, want 1", len(evs))
	}
}
