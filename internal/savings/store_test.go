package savings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testLedgerPath returns a fresh sidecar DB path. Each test gets its own
// file so the process-shared sidecar handle cache can't leak state
// between tests.
func testLedgerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sidecar.sqlite")
}

// mustSnapshot unwraps Snapshot for tests that expect a healthy ledger.
func mustSnapshot(t *testing.T, s *Store) File {
	t.Helper()
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snap
}

// closeOnCleanup releases the sidecar handle when the test ends, so the
// process-wide handle cache doesn't accumulate open DBs (and TempDir
// cleanup works on platforms that refuse to delete open files).
func closeOnCleanup(t *testing.T, s *Store) *Store {
	t.Helper()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAddObservation_PerLanguageBucket(t *testing.T) {
	path := testLedgerPath(t)

	s, err := Open(path)
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatal(err)
	}

	s.AddObservation(Observation{Repo: "/repo-a", Language: "go", Tool: "get_symbol_source", Returned: 100, Saved: 200})
	s.AddObservation(Observation{Repo: "/repo-a", Language: "go", Tool: "get_symbol_source", Returned: 50, Saved: 80})
	s.AddObservation(Observation{Repo: "/repo-b", Language: "typescript", Tool: "batch_symbols", Returned: 30, Saved: 70})
	// Empty language is allowed (e.g. record() called with a nil node);
	// it should accumulate in the totals but not in any per-language bucket.
	s.AddObservation(Observation{Repo: "/repo-c", Tool: "smart_context", Returned: 10, Saved: 20})

	reopened, err := Open(path)
	if err == nil {
		closeOnCleanup(t, reopened)
	}
	if err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, reopened)

	if got, want := snap.Totals.CallsCounted, int64(4); got != want {
		t.Errorf("CallsCounted = %d, want %d", got, want)
	}
	if len(snap.PerLanguage) != 2 {
		t.Errorf("PerLanguage size = %d, want 2 (empty-language observation must not create a bucket)", len(snap.PerLanguage))
	}
	if g := snap.PerLanguage["go"]; g == nil || g.CallsCounted != 2 || g.TokensSaved != 280 {
		t.Errorf("go bucket = %+v, want calls=2 saved=280 (200+80)", g)
	}
	if ts := snap.PerLanguage["typescript"]; ts == nil || ts.CallsCounted != 1 || ts.TokensSaved != 70 {
		t.Errorf("typescript bucket = %+v, want calls=1 saved=70", ts)
	}
	if len(snap.PerRepo) != 3 {
		t.Errorf("PerRepo size = %d, want 3", len(snap.PerRepo))
	}
}

// The headline property of the sidecar-backed ledger: an observation is
// durable the moment it is recorded. No flush, no ticker, no graceful
// shutdown required — the failure mode that left the flat-file ledger
// permanently empty under SIGKILLing MCP clients.
func TestAddObservation_DurableImmediately(t *testing.T) {
	path := testLedgerPath(t)

	s, err := Open(path)
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatal(err)
	}
	s.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "read_file", Returned: 10, Saved: 90})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ledger DB must exist immediately after the first observation: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 1 || snap.Totals.TokensSaved != 90 {
		t.Errorf("snapshot = %+v, want calls=1 saved=90 with no flush", snap.Totals)
	}
	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Tool != "read_file" {
		t.Errorf("events = %+v, want one read_file event", evs)
	}
}

func TestConcurrentWriters_SameLedger(t *testing.T) {
	path := testLedgerPath(t)

	const perStore = 200
	stores := make([]*Store, 4)
	for i := range stores {
		s, err := Open(path)
		if err == nil {
			closeOnCleanup(t, s)
		}
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
	}

	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func(s *Store, repo string) {
			defer wg.Done()
			for j := 0; j < perStore; j++ {
				s.AddObservation(Observation{Repo: repo, Tool: "test", Returned: 1, Saved: 10})
			}
		}(s, "/repo-"+string(rune('a'+i)))
	}
	wg.Wait()

	snap := mustSnapshot(t, stores[0])
	wantCalls := int64(len(stores) * perStore)
	if got := snap.Totals.CallsCounted; got != wantCalls {
		t.Errorf("CallsCounted = %d, want %d (observation lost across writers)", got, wantCalls)
	}
	if got, want := snap.Totals.TokensSaved, wantCalls*10; got != want {
		t.Errorf("TokensSaved = %d, want %d", got, want)
	}
	evs, err := stores[0].EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(len(evs)); got != wantCalls {
		t.Errorf("events = %d, want %d", got, wantCalls)
	}
}

// A fresh ledger reports nothing — including a zero FirstSeen. The
// flat-file store seeded FirstSeen=now at Open, which made the dashboard
// print "tracking since <the moment you ran the CLI>" on a machine that
// had never recorded anything.
func TestOpen_FreshLedger_EmptySnapshot(t *testing.T) {
	s, err := Open(testLedgerPath(t))
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 0 {
		t.Errorf("new ledger has CallsCounted=%d, want 0", snap.Totals.CallsCounted)
	}
	if !snap.FirstSeen.IsZero() {
		t.Errorf("new ledger FirstSeen = %v, want zero time (nothing recorded yet)", snap.FirstSeen)
	}
	if snap.Version != schemaVersion {
		t.Errorf("new ledger version=%d, want %d", snap.Version, schemaVersion)
	}
}

func TestObservation_StampsFirstAndLastSeen(t *testing.T) {
	s, err := Open(testLedgerPath(t))
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	s.AddObservation(Observation{Tool: "test", Returned: 1, Saved: 1})
	after := time.Now().UTC().Add(time.Second)

	snap := mustSnapshot(t, s)
	if snap.FirstSeen.Before(before) || snap.FirstSeen.After(after) {
		t.Errorf("FirstSeen = %v, want within [%v, %v]", snap.FirstSeen, before, after)
	}
	if snap.LastUpdated.Before(before) || snap.LastUpdated.After(after) {
		t.Errorf("LastUpdated = %v, want within [%v, %v]", snap.LastUpdated, before, after)
	}
}

func TestAddObservation_ConcurrentSafe(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	const per = 250
	var wg sync.WaitGroup
	var expectedSaved atomic.Int64
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				s.AddObservation(Observation{Tool: "test", Returned: 10, Saved: 100})
				expectedSaved.Add(100)
			}
		}()
	}
	wg.Wait()

	snap := mustSnapshot(t, s)
	if got, want := snap.Totals.CallsCounted, int64(workers*per); got != want {
		t.Errorf("CallsCounted = %d, want %d", got, want)
	}
	if got, want := snap.Totals.TokensSaved, expectedSaved.Load(); got != want {
		t.Errorf("TokensSaved = %d, want %d", got, want)
	}
}

func TestOpen_EmptyPath_InMemoryOnly(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	s.AddObservation(Observation{Repo: "r", Tool: "test", Returned: 10, Saved: 100})
	if err := s.Flush(); err != nil {
		t.Errorf("Flush on in-memory store should no-op, got: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 1 {
		t.Errorf("in-memory store should track, got CallsCounted=%d", snap.Totals.CallsCounted)
	}
	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Errorf("in-memory store should keep events, got %d", len(evs))
	}
}

func TestReset_ClearsLedger(t *testing.T) {
	path := testLedgerPath(t)

	s, _ := Open(path)
	closeOnCleanup(t, s)
	s.AddObservation(Observation{Repo: "/r", Tool: "test", Returned: 50, Saved: 500})

	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 0 {
		t.Errorf("totals should be cleared after reset, got CallsCounted=%d", snap.Totals.CallsCounted)
	}
	if !snap.FirstSeen.IsZero() {
		t.Errorf("FirstSeen should be cleared after reset, got %v", snap.FirstSeen)
	}
	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Errorf("events should be cleared after reset, got %d", len(evs))
	}
}

// Snapshot keeps the JSON shape graph_stats and `gortex savings --json`
// expose — the surface contract of cumulative_savings.
func TestSnapshot_JSONShape(t *testing.T) {
	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	s.AddObservation(Observation{Repo: "/repo-a", Language: "go", Tool: "test", Returned: 10, Saved: 100})

	data, err := json.Marshal(mustSnapshot(t, s))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "first_seen", "last_updated", "totals", "per_repo", "per_language"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing key %q in snapshot JSON", key)
		}
	}
}

func TestEventsSince_Filters(t *testing.T) {
	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	s.AddObservation(Observation{Tool: "a", Returned: 1, Saved: 1})
	time.Sleep(5 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	s.AddObservation(Observation{Tool: "b", Returned: 1, Saved: 1})

	evs, err := s.EventsSince(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Tool != "b" {
		t.Errorf("EventsSince(cutoff) = %+v, want only [b]", evs)
	}
	all, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("EventsSince(zero) = %d, want 2", len(all))
	}
}

func TestImportLegacy_FullFlatFiles(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	jsonlPath := filepath.Join(legacyDir, "savings.jsonl")

	firstSeen := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	lastUpdated := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	legacy := File{
		Version:     schemaVersion,
		FirstSeen:   firstSeen,
		LastUpdated: lastUpdated,
		Totals:      Totals{TokensSaved: 1000, TokensReturned: 100, CallsCounted: 10},
		PerRepo:     map[string]*Totals{"repo-a": {TokensSaved: 1000, TokensReturned: 100, CallsCounted: 10}},
		PerLanguage: map[string]*Totals{"go": {TokensSaved: 1000, TokensReturned: 100, CallsCounted: 10}},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(Event{TS: lastUpdated, Repo: "repo-a", Language: "go", Tool: "get_symbol_source", Returned: 23, Saved: 77})
	if err := os.WriteFile(jsonlPath, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(testLedgerPath(t))
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}

	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 10 || snap.Totals.TokensSaved != 1000 {
		t.Errorf("imported totals = %+v, want calls=10 saved=1000", snap.Totals)
	}
	if r := snap.PerRepo["repo-a"]; r == nil || r.CallsCounted != 10 {
		t.Errorf("imported repo bucket = %+v", r)
	}
	if !snap.FirstSeen.Equal(firstSeen) {
		t.Errorf("FirstSeen = %v, want %v (carried from legacy file)", snap.FirstSeen, firstSeen)
	}
	evs, _ := s.EventsSince(time.Time{})
	if len(evs) != 1 || evs[0].Tool != "get_symbol_source" || evs[0].Saved != 77 {
		t.Errorf("imported events = %+v", evs)
	}

	// Legacy files renamed aside, originals gone.
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Errorf("legacy savings.json should be renamed after import, stat err=%v", err)
	}
	if _, err := os.Stat(jsonPath + ".bak"); err != nil {
		t.Errorf("expected savings.json.bak, stat err=%v", err)
	}
	if _, err := os.Stat(jsonlPath + ".bak"); err != nil {
		t.Errorf("expected savings.jsonl.bak, stat err=%v", err)
	}

	// Idempotent: a second import (e.g. another entry point racing the
	// first) must not double-count.
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("second ImportLegacy: %v", err)
	}
	if got := mustSnapshot(t, s).Totals.CallsCounted; got != 10 {
		t.Errorf("totals after second import = %d, want 10 (no double count)", got)
	}
}

// A jsonl without its cumulative file (the flat-file flush never ran
// before the process died — the common SIGKILL case) still imports:
// totals are rebuilt from the events.
func TestImportLegacy_EventsOnlyRebuildsTotals(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	jsonlPath := filepath.Join(legacyDir, "savings.jsonl")

	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	var lines []byte
	for i := 0; i < 3; i++ {
		line, _ := json.Marshal(Event{TS: ts.Add(time.Duration(i) * time.Minute), Repo: "r", Language: "go", Tool: "smart_context", Returned: 10, Saved: 30})
		lines = append(lines, line...)
		lines = append(lines, '\n')
	}
	if err := os.WriteFile(jsonlPath, lines, 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 3 || snap.Totals.TokensSaved != 90 {
		t.Errorf("rebuilt totals = %+v, want calls=3 saved=90", snap.Totals)
	}
	if r := snap.PerRepo["r"]; r == nil || r.CallsCounted != 3 {
		t.Errorf("rebuilt repo bucket = %+v", r)
	}
	if !snap.FirstSeen.Equal(ts) {
		t.Errorf("FirstSeen = %v, want first event ts %v", snap.FirstSeen, ts)
	}
}

// With nothing to import the mark is still set, so legacy files that
// appear later (e.g. restored from a backup) are not silently merged
// into a ledger that has moved on.
func TestImportLegacy_NothingToImportMarksDone(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("ImportLegacy on missing files: %v", err)
	}

	// A legacy file materializing afterwards is ignored.
	legacy := File{Version: schemaVersion, Totals: Totals{TokensSaved: 5000, CallsCounted: 50}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatal(err)
	}
	if got := mustSnapshot(t, s).Totals.CallsCounted; got != 0 {
		t.Errorf("late-appearing legacy file must not import, got calls=%d", got)
	}
}

// TestDefaultPath_HonorsXDGCacheHome verifies the legacy flat-file path is
// routed through the XDG resolver: an absolute $XDG_CACHE_HOME relocates
// it to <XDG_CACHE_HOME>/gortex/savings.json, so the legacy import looks
// where the flat-file era actually wrote.
func TestDefaultPath_HonorsXDGCacheHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	want := filepath.Join(xdg, "gortex", "savings.json")
	if got := DefaultPath(); got != want {
		t.Fatalf("DefaultPath() with XDG_CACHE_HOME = %s, want %s", got, want)
	}

	// The sibling event-log path follows the same root.
	wantEvents := filepath.Join(xdg, "gortex", "savings.jsonl")
	if got := DefaultEventsPath(); got != wantEvents {
		t.Fatalf("DefaultEventsPath() with XDG_CACHE_HOME = %s, want %s", got, wantEvents)
	}
}

// TestDefaultDBPath_HonorsXDGDataHome verifies the ledger DB follows the
// data-dir resolver — the same sidecar.sqlite the notes/memories
// managers share.
func TestDefaultDBPath_HonorsXDGDataHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	want := filepath.Join(xdg, "gortex", "sidecar.sqlite")
	if got := DefaultDBPath(); got != want {
		t.Fatalf("DefaultDBPath() with XDG_DATA_HOME = %s, want %s", got, want)
	}
}

// A legacy file with JSON null bucket values must import cleanly — a
// nil *Totals dereference here would crash-loop every server start.
func TestImportLegacy_NullBucketValues(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	body := `{"version":1,"totals":{"tokens_saved":10,"tokens_returned":1,"calls_counted":1},"per_repo":{"x":null},"per_language":{"y":null}}`
	if err := os.WriteFile(jsonPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("ImportLegacy with null buckets: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 1 {
		t.Errorf("totals = %+v, want calls=1", snap.Totals)
	}
	if len(snap.PerRepo) != 0 || len(snap.PerLanguage) != 0 {
		t.Errorf("null buckets must be dropped, got repo=%v lang=%v", snap.PerRepo, snap.PerLanguage)
	}
	if _, err := os.Stat(jsonPath + ".bak"); err != nil {
		t.Errorf("legacy file should be renamed after import: %v", err)
	}
}

// The flat-file cumulative was flush-batched while the event log
// appended eagerly; the import floors totals at what the events
// reconstruct so "Last 7 days" can never exceed "All time".
func TestImportLegacy_FlushLaggedTotalsFlooredByEvents(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	jsonlPath := filepath.Join(legacyDir, "savings.jsonl")

	lagged := File{
		Version: schemaVersion,
		Totals:  Totals{TokensSaved: 10, TokensReturned: 1, CallsCounted: 1},
	}
	data, _ := json.Marshal(lagged)
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	var lines []byte
	for i := 0; i < 3; i++ {
		line, _ := json.Marshal(Event{TS: ts.Add(time.Duration(i) * time.Minute), Tool: "t", Returned: 1, Saved: 10})
		lines = append(append(lines, line...), '\n')
	}
	if err := os.WriteFile(jsonlPath, lines, 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 3 || snap.Totals.TokensSaved != 30 {
		t.Errorf("totals = %+v, want floored at the events' calls=3 saved=30", snap.Totals)
	}
}

// A hard event-log read error aborts the import without marking or
// renaming, so the next open retries instead of permanently losing the
// unread tail.
func TestImportLegacy_UnreadableEventsAborts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no POSIX mode bits: a 0000 file is still readable, so the read cannot be made to fail")
	}
	if os.Getuid() == 0 {
		t.Skip("permission bits don't bind as root")
	}
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	jsonlPath := filepath.Join(legacyDir, "savings.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o000); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err == nil {
		t.Fatal("unreadable event log must abort the import")
	}
	if _, err := os.Stat(jsonlPath); err != nil {
		t.Errorf("aborted import must not rename the event log: %v", err)
	}
	// A later open (permissions fixed) imports successfully.
	if err := os.Chmod(jsonlPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("retry after fixing permissions: %v", err)
	}
}
