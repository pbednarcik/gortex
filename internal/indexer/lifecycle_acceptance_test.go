package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
)

type lifecycleFailingBulkStore struct {
	graph.Store
}

func (*lifecycleFailingBulkStore) BeginBulkLoad() {}

func (*lifecycleFailingBulkStore) FlushBulk() error {
	return errors.New("injected lifecycle bulk flush failure")
}

func newLifecycleTestMultiIndexer(t *testing.T) *MultiIndexer {
	t.Helper()
	return NewMultiIndexer(
		graph.New(),
		newTestRegistry(),
		search.NewNull(),
		newTestConfigManager(t),
		zap.NewNop(),
	)
}

func closeLifecycleTestMultiIndexer(t *testing.T, mi *MultiIndexer) {
	t.Helper()
	require.NoError(t, mi.Close(context.Background()))
}

func TestLifecycleIndexRepoClosesReplacedIndexer(t *testing.T) {
	t.Setenv(crashWorkerEnv, "1")
	repo := setupRepoDir(t, "repo")
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)
	old := mi.GetIndexer("repo")
	require.NotNil(t, old)
	pool, _ := old.sharedParsePool()
	require.NotNil(t, pool, "replacement precondition requires a real crashpool")

	_, err = mi.IndexRepo("repo")
	require.NoError(t, err)
	require.NotSame(t, old, mi.GetIndexer("repo"))
	require.Nil(t, old.parsePool, "replacement must release the old crashpool")
	_, err = old.ExtractBuffer("go", "after.go", []byte("package after\n"))
	require.ErrorIs(t, err, ErrIndexerClosed)
}

func TestLifecycleFailedReplacementClosesCandidateIndexerAndPool(t *testing.T) {
	t.Setenv(crashWorkerEnv, "1")
	repo := setupRepoDir(t, "repo")
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)
	old := mi.GetIndexer("repo")
	require.NotNil(t, old)
	mi.graph = &lifecycleFailingBulkStore{Store: mi.graph}

	var candidate *Indexer
	mi.newIndexer = func(g graph.Store, reg *parser.Registry, cfg config.IndexConfig, logger *zap.Logger) *Indexer {
		candidate = New(g, reg, cfg, logger)
		candidate.SetRootPath(repo)
		pool, _ := candidate.sharedParsePool()
		require.NotNil(t, pool, "failure precondition requires a real crashpool")
		return candidate
	}

	_, err = mi.IndexRepo("repo")
	require.Error(t, err)
	require.NotNil(t, candidate)
	require.Same(t, old, mi.GetIndexer("repo"), "failed replacement must preserve the live Indexer")
	require.Nil(t, candidate.parsePool, "failed replacement must release the candidate crashpool")
	_, err = candidate.ExtractBuffer("go", "after.go", []byte("package after\n"))
	require.ErrorIs(t, err, ErrIndexerClosed)

	_, err = old.ExtractBuffer("go", "still-live.go", []byte("package live\n"))
	require.NoError(t, err, "failed replacement must not close the live Indexer")
}

func TestLifecycleUntrackClosesIndexerAndCrashPool(t *testing.T) {
	t.Setenv(crashWorkerEnv, "1")
	repo := setupRepoDir(t, "repo")
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)
	owned := mi.GetIndexer("repo")
	require.NotNil(t, owned)
	pool, _ := owned.sharedParsePool()
	require.NotNil(t, pool, "untrack precondition requires a real crashpool")

	nodesRemoved, _ := mi.UntrackRepo("repo")
	require.Positive(t, nodesRemoved)
	require.Nil(t, mi.GetIndexer("repo"))
	require.Nil(t, owned.parsePool, "untrack must release the repository crashpool")
	_, err = owned.ExtractBuffer("go", "after.go", []byte("package after\n"))
	require.ErrorIs(t, err, ErrIndexerClosed)
}

// laneUntrackProvider is a minimal semantic.Provider + BackgroundEnricher
// whose drain blocks until cancelled, so the test can hold a lane drain in
// flight across an UntrackRepo call.
type laneUntrackProvider struct {
	drained chan string
	block   chan struct{}
	ctxErr  chan error
}

func (p *laneUntrackProvider) Name() string        { return "lane-untrack" }
func (p *laneUntrackProvider) Languages() []string { return []string{"go"} }
func (p *laneUntrackProvider) Available() bool     { return true }
func (p *laneUntrackProvider) Close() error        { return nil }
func (p *laneUntrackProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	return &semantic.EnrichResult{}, nil
}
func (p *laneUntrackProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	return nil, nil
}
func (p *laneUntrackProvider) HasBackgroundWork(graph.Store, string) bool { return true }
func (p *laneUntrackProvider) InvalidateBackground(graph.Store, string)   {}
func (p *laneUntrackProvider) EnrichBackground(ctx context.Context, _ graph.Store, repo, _ string) (*semantic.EnrichResult, error) {
	p.drained <- repo
	select {
	case <-p.block:
		return &semantic.EnrichResult{}, nil
	case <-ctx.Done():
		p.ctxErr <- ctx.Err()
		return nil, ctx.Err()
	}
}

// Untracking a repository must also cancel and purge its background lane
// work: a pending or in-flight drain otherwise outlives the graph purge,
// spawns a server at the abandoned root, and writes the removed repo's
// nodes back into the store.
func TestLifecycleUntrackCancelsBackgroundLane(t *testing.T) {
	// The fixture repo is far below the lane admission floor — disable it so
	// the census enqueues (the floor has its own tests in internal/semantic).
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	repo := setupRepoDir(t, "repo")
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)

	bg := &laneUntrackProvider{
		drained: make(chan string, 2),
		block:   make(chan struct{}),
		ctxErr:  make(chan error, 1),
	}
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	mgr.RegisterProvider(bg)
	t.Cleanup(func() { require.NoError(t, mgr.Close()) })
	mi.SetSemanticManager(mgr)

	// The census enqueue is cooldown-free — it starts the drain the test
	// then holds in flight across the untrack.
	mgr.StartBackgroundLane(context.Background(), mi.Graph(), map[string]string{"repo": repo})
	select {
	case r := <-bg.drained:
		require.Equal(t, "repo", r)
	case <-time.After(2 * time.Second):
		t.Fatal("census did not start the drain")
	}

	done := make(chan struct{})
	go func() { mi.UntrackRepo("repo"); close(done) }()
	select {
	case err := <-bg.ctxErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("untrack did not cancel the in-flight background drain")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("UntrackRepo did not return")
	}
	select {
	case r := <-bg.drained:
		t.Fatalf("untracked repo drained again: %q", r)
	case <-time.After(150 * time.Millisecond):
	}
}

// laneTrackProbeProvider records, at drain start, whatever the injected
// probe observes about the store and the indexer state at that moment.
type laneTrackProbeProvider struct {
	probe func(g graph.Store)
}

func (p *laneTrackProbeProvider) Name() string        { return "lane-track-probe" }
func (p *laneTrackProbeProvider) Languages() []string { return []string{"go"} }
func (p *laneTrackProbeProvider) Available() bool     { return true }
func (p *laneTrackProbeProvider) Close() error        { return nil }
func (p *laneTrackProbeProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	return &semantic.EnrichResult{}, nil
}
func (p *laneTrackProbeProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	return nil, nil
}
func (p *laneTrackProbeProvider) HasBackgroundWork(graph.Store, string) bool { return true }
func (p *laneTrackProbeProvider) InvalidateBackground(graph.Store, string)   {}
func (p *laneTrackProbeProvider) EnrichBackground(_ context.Context, g graph.Store, _, _ string) (*semantic.EnrichResult, error) {
	p.probe(g)
	return &semantic.EnrichResult{}, nil
}

// A live track's inline enrichment enqueues the repo's background drain
// from INSIDE the mutation, while a first index on a bulk-loading store
// still holds every row in the indexer's local shadow graph — invisible to
// the durable store the lane drains against. The track must bracket the
// lane like every other repository mutation: hold first, so the drain can
// only start after the shadow has landed and the repo is installed.
func TestLifecycleTrackParksBackgroundDrainUntilRowsVisible(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "lifecycle-track.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	repo := setupRepoDir(t, "repo")
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	type drainObs struct {
		rowsVisible bool
		installed   bool
	}
	seen := make(chan drainObs, 2)
	bg := &laneTrackProbeProvider{probe: func(g graph.Store) {
		seen <- drainObs{
			rowsVisible: len(g.GetFileNodes("repo/main.go")) > 0,
			installed:   mi.GetIndexer("repo") != nil,
		}
	}}
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	mgr.RegisterProvider(bg)
	t.Cleanup(func() { require.NoError(t, mgr.Close()) })
	mi.SetSemanticManager(mgr)

	// Lane running with no repos yet — the drain under test is the one the
	// track's own pass-end enqueue produces, not a census enqueue.
	mgr.StartBackgroundLane(context.Background(), mi.Graph(), map[string]string{})

	_, err = mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)

	select {
	case obs := <-seen:
		require.True(t, obs.rowsVisible,
			"the drain started against a store with no rows for the tracked repo — the pass-end enqueue escaped the mutation bracket")
		require.True(t, obs.installed,
			"the drain started before the tracked repo was installed")
	case <-time.After(5 * time.Second):
		t.Fatal("the track's pass-end enqueue never produced a drain")
	}
}

// laneBracketProvider records the mutation-vs-lane ordering signals as a
// single event log: drain start, drain cancellation (with whether the
// mutated file's pre-mutation rows were still in the graph at that moment),
// the foreground incremental enrichment, and the requeue's claim revocation.
// The executor bracket's whole contract is the order of these events.
type laneBracketProvider struct {
	mu      sync.Mutex
	events  []string
	watched string // graph path whose rows the drain would have read

	drainStarted chan struct{}
	cancelled    chan struct{}
	release      chan struct{}
}

func newLaneBracketProvider(watched string) *laneBracketProvider {
	return &laneBracketProvider{
		watched:      watched,
		drainStarted: make(chan struct{}, 4),
		cancelled:    make(chan struct{}, 4),
		release:      make(chan struct{}),
	}
}

func (p *laneBracketProvider) record(event string) {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.mu.Unlock()
}

func (p *laneBracketProvider) eventLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *laneBracketProvider) Name() string        { return "lane-bracket" }
func (p *laneBracketProvider) Languages() []string { return []string{"go"} }
func (p *laneBracketProvider) Available() bool     { return true }
func (p *laneBracketProvider) Close() error        { return nil }
func (p *laneBracketProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	p.record("enrich")
	return &semantic.EnrichResult{}, nil
}
func (p *laneBracketProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	p.record("enrich")
	return nil, nil
}
func (p *laneBracketProvider) HasBackgroundWork(graph.Store, string) bool { return true }
func (p *laneBracketProvider) InvalidateBackground(graph.Store, string) {
	p.record("invalidate")
}
func (p *laneBracketProvider) EnrichBackground(ctx context.Context, g graph.Store, _, _ string) (*semantic.EnrichResult, error) {
	p.record("drain-start")
	p.drainStarted <- struct{}{}
	select {
	case <-p.release:
		return &semantic.EnrichResult{}, nil
	case <-ctx.Done():
		// The moment of cancellation is the bracket's promise: the mutation
		// is still waiting on this drain, so the rows it read must still be
		// in the store — nothing was evicted or rewritten under it.
		if len(g.GetFileNodes(p.watched)) > 0 {
			p.record("cancelled-before-first-write")
		} else {
			p.record("cancelled-after-write")
		}
		p.cancelled <- struct{}{}
		return nil, ctx.Err()
	}
}

// laneBracketHarness tracks one repo, registers a blocking background
// provider, and holds a lane drain in flight so a test can assert how a
// mutation path brackets it.
func laneBracketHarness(t *testing.T) (*MultiIndexer, *laneBracketProvider, string) {
	t.Helper()
	return laneBracketHarnessWith(t, "repo/main.go", nil)
}

// laneBracketHarnessWith is laneBracketHarness with a custom watched graph
// path and a fixture hook that runs before the repo is tracked.
func laneBracketHarnessWith(t *testing.T, watched string, prepare func(repoDir string)) (*MultiIndexer, *laneBracketProvider, string) {
	t.Helper()
	// The fixture repo is a handful of nodes — keep the enrichment admission
	// floor from silently skipping the tail whose ordering this asserts.
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	repo := setupRepoDir(t, "repo")
	if prepare != nil {
		prepare(repo)
	}
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)

	bg := newLaneBracketProvider(watched)
	mgr := semantic.NewManager(semantic.Config{Enabled: true, EnrichOnWatch: true}, zap.NewNop())
	mgr.RegisterProvider(bg)
	t.Cleanup(func() { require.NoError(t, mgr.Close()) })
	// Unblock any still-parked drain before the manager's mandatory-drain
	// close waits on it (LIFO: this runs before the mgr.Close cleanup).
	t.Cleanup(func() { close(bg.release) })
	mi.SetSemanticManager(mgr)

	mgr.StartBackgroundLane(context.Background(), mi.Graph(), map[string]string{"repo": repo})
	select {
	case <-bg.drainStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("census did not start the drain")
	}
	return mi, bg, repo
}

func requireLaneCancelled(t *testing.T, bg *laneBracketProvider, op string) {
	t.Helper()
	select {
	case <-bg.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not cancel the in-flight background drain", op)
	}
}

// An incremental reindex must wait out the repo's in-flight drain before its
// first store write, and must not hand the lane back (the requeue that
// revokes the drained claim) until its complete tail — the incremental
// semantic enrichment included — has finished writing. A requeue that fires
// between the reparse and the tail re-admits the lane against a store the
// foreground is still mutating.
func TestLifecycleIncrementalReindexBracketsBackgroundLane(t *testing.T) {
	mi, bg, repo := laneBracketHarness(t)

	writeFile(t, filepath.Join(repo, "main.go"), `package main

func Hello() {}
func Added() {}
`)
	_, err := mi.IncrementalReindexRepo("repo", []string{"main.go"})
	require.NoError(t, err)
	requireLaneCancelled(t, bg, "IncrementalReindexRepo")

	events := bg.eventLog()
	require.Contains(t, events, "cancelled-before-first-write",
		"the drain must observe an unmutated store at cancellation: %v", events)
	require.NotContains(t, events, "cancelled-after-write", "events: %v", events)

	// First occurrences on both sides: ANY claim revocation landing before
	// the tail's enrichment re-admits the lane against a store the
	// foreground is still writing.
	enrichAt, invalidateAt := -1, -1
	for i, e := range events {
		if e == "enrich" && enrichAt == -1 {
			enrichAt = i
		}
		if e == "invalidate" && invalidateAt == -1 {
			invalidateAt = i
		}
	}
	require.GreaterOrEqual(t, enrichAt, 0, "the mutation tail must run the incremental enrichment: %v", events)
	require.GreaterOrEqual(t, invalidateAt, 0, "the mutation must revoke the drained claim: %v", events)
	require.Greater(t, invalidateAt, enrichAt,
		"the requeue must not re-admit the lane before the mutation's semantic tail finished: %v", events)
}

// A forced single-file eviction is a repository mutation like any other: it
// must cancel the languages it touches BEFORE removing the rows an in-flight
// drain may have read (or the resumed drain flushes its stale copies back —
// resurrecting the deleted file), and revoke the drained claim after.
func TestLifecycleForcedEvictBracketsBackgroundLane(t *testing.T) {
	mi, bg, _ := laneBracketHarness(t)
	idx := mi.GetIndexer("repo")
	require.NotNil(t, idx)

	done := make(chan struct{})
	var nodesRemoved int
	go func() {
		nodesRemoved, _ = idx.EvictFile("main.go")
		close(done)
	}()
	requireLaneCancelled(t, bg, "EvictFile")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EvictFile did not return")
	}
	require.Positive(t, nodesRemoved)

	events := bg.eventLog()
	require.Contains(t, events, "cancelled-before-first-write",
		"the eviction must wait the drain out before removing the rows it read: %v", events)
	require.NotContains(t, events, "cancelled-after-write", "events: %v", events)
	require.Contains(t, events, "invalidate",
		"the eviction must revoke the drained claim: %v", events)
	require.Empty(t, mi.Graph().GetFileNodes("repo/main.go"), "the eviction itself must still land")
}

// MultiIndexer.IndexRepo rewrites every row of the repository — the same
// full-reindex shape IndexCtx already brackets. Its separate raw path must
// cancel the repo's in-flight drain before EvictRepo, or the old drain
// flushes its pre-eviction snapshot over the rebuilt graph.
func TestLifecycleIndexRepoCancelsBackgroundLane(t *testing.T) {
	mi, bg, _ := laneBracketHarness(t)

	done := make(chan struct{})
	var indexErr error
	go func() {
		_, indexErr = mi.IndexRepo("repo")
		close(done)
	}()
	requireLaneCancelled(t, bg, "IndexRepo")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("IndexRepo did not return")
	}
	require.NoError(t, indexErr)

	events := bg.eventLog()
	require.Contains(t, events, "cancelled-before-first-write",
		"the full reindex must wait the drain out before evicting the repo: %v", events)
	require.NotContains(t, events, "cancelled-after-write", "events: %v", events)
}

// The mutation's own semantic tail ends with the manager's pass-end
// enqueue, and that task is born with no cooldown. Cancellation cannot
// stop it — it does not exist yet when the mutation cancels — so the
// executor must park the repo at the scheduler's dequeue gate for the
// whole mutation. Without the hold, the lane starts a drain against a
// store the resolver/derived tail is still writing.
func TestLifecycleMutationHoldsPassEndDrain(t *testing.T) {
	mi, bg, repo := laneBracketHarness(t)

	writeFile(t, filepath.Join(repo, "main.go"), `package main

func Hello() {}
func Grown() {}
`)
	_, err := mi.IncrementalReindexRepo("repo", []string{"main.go"})
	require.NoError(t, err)
	requireLaneCancelled(t, bg, "IncrementalReindexRepo")

	// Settle window: a drain leaked mid-tail records its start within it.
	time.Sleep(300 * time.Millisecond)
	events := bg.eventLog()
	starts := 0
	for _, e := range events {
		if e == "drain-start" {
			starts++
		}
	}
	require.Equal(t, 1, starts,
		"the pass-end enqueue born inside the mutation tail must stay parked until the mutation completes: %v", events)
}

// A zero-change full-root reconcile (the watcher's overflow recovery
// passes nil paths) writes nothing, so there is nothing to protect:
// cancelling the active drain would throw away a long server warmup, and
// revoking the drained claim would erase real progress for no data change.
func TestLifecycleZeroChangeReconcilePreservesLaneWork(t *testing.T) {
	mi, bg, _ := laneBracketHarness(t)

	_, err := mi.IncrementalReindexRepo("repo", nil)
	require.NoError(t, err)

	select {
	case <-bg.cancelled:
		t.Fatalf("a zero-change reconcile cancelled the active drain: %v", bg.eventLog())
	case <-time.After(300 * time.Millisecond):
	}
	require.NotContains(t, bg.eventLog(), "invalidate",
		"a zero-change reconcile must not revoke the drained claim")
}

// The watcher's new-directory discovery passes DIRECTORY paths, and a
// scoped reindex may too. The raw path derives no languages — only
// classification knows which files inside changed — so the precise cancel
// must come from the real stale set, not the caller's path list.
func TestLifecycleDirectoryScopeReindexCancelsLaneDrain(t *testing.T) {
	// The graph keys subdirectory files with OS-native separators (see
	// graphRelKey) — the watched path must match the stored form.
	mi, bg, repo := laneBracketHarnessWith(t, "repo/"+filepath.FromSlash("sub/lib.go"), func(dir string) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
		writeFile(t, filepath.Join(dir, "sub", "lib.go"), "package sub\n\nfunc Lib() {}\n")
	})

	writeFile(t, filepath.Join(repo, "sub", "lib.go"), "package sub\n\nfunc Lib() {}\nfunc Grown() {}\n")
	_, err := mi.IncrementalReindexRepo("repo", []string{"sub"})
	require.NoError(t, err)
	requireLaneCancelled(t, bg, "directory-scoped IncrementalReindexRepo")

	events := bg.eventLog()
	require.Contains(t, events, "cancelled-before-first-write", "events: %v", events)
	require.NotContains(t, events, "cancelled-after-write", "events: %v", events)
	require.Contains(t, events, "invalidate",
		"the mutated language's drained claim must be revoked: %v", events)
}
