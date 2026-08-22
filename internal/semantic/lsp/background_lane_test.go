package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// The LSP provider is the background lane's first tenant.
var _ semantic.BackgroundEnricher = (*Provider)(nil)

// markerStore wraps an in-memory graph with a durable-marker table so the
// lane's census logic (which keys off enrichment-state markers) is testable
// without a SQLite store.
type markerStore struct {
	graph.Store
	mu      sync.Mutex
	markers map[string]graph.EnrichmentState
}

func newMarkerStore(g graph.Store) *markerStore {
	return &markerStore{Store: g, markers: map[string]graph.EnrichmentState{}}
}

func (s *markerStore) GetEnrichmentState(repoPrefix, provider string) (graph.EnrichmentState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.markers[repoPrefix+"\x00"+provider]
	return st, ok, nil
}

func (s *markerStore) SetEnrichmentState(st graph.EnrichmentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markers[st.RepoPrefix+"\x00"+st.Provider] = st
	return nil
}

// HasBackgroundWork: only GORTEX_LSP_HEAVY=background opens the lane; a
// spec-less provider cannot spawn a drain instance; a current lane marker
// (same sha as the fast tier's marker) means the tier is drained.
func TestLSPProvider_HasBackgroundWork(t *testing.T) {
	spec := SpecByName("omnisharp")
	require.NotNil(t, spec)

	newP := func() *Provider {
		p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
		p.spec = spec
		return p
	}

	t.Run("only background mode opens the lane", func(t *testing.T) {
		for _, v := range []string{"", "on", "off", "bogus"} {
			t.Setenv(HeavyRequestsEnv, v)
			assert.False(t, newP().HasBackgroundWork(graph.New(), ""), "env %q", v)
		}
		t.Setenv(HeavyRequestsEnv, "background")
		assert.True(t, newP().HasBackgroundWork(graph.New(), ""))
	})

	t.Run("spec-less provider has no lane", func(t *testing.T) {
		t.Setenv(HeavyRequestsEnv, "background")
		p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
		assert.False(t, p.HasBackgroundWork(graph.New(), ""))
	})

	t.Run("marker census", func(t *testing.T) {
		t.Setenv(HeavyRequestsEnv, "background")
		p := newP()
		ms := newMarkerStore(graph.New())
		assert.True(t, p.HasBackgroundWork(ms, "repo"), "no markers at all → drain")

		require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
			RepoPrefix: "repo", Provider: p.Name(), IndexedSHA: "sha-1"}))
		assert.True(t, p.HasBackgroundWork(ms, "repo"), "fast marker without a lane marker → drain")

		require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
			RepoPrefix: "repo", Provider: p.Name() + backgroundMarkerSuffix, IndexedSHA: "sha-0"}))
		assert.True(t, p.HasBackgroundWork(ms, "repo"), "stale lane marker → drain")

		require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
			RepoPrefix: "repo", Provider: p.Name() + backgroundMarkerSuffix, IndexedSHA: "sha-1"}))
		assert.False(t, p.HasBackgroundWork(ms, "repo"), "lane marker at the fast tier's sha → drained")
	})

	t.Run("mutation invalidates the drained claim", func(t *testing.T) {
		// A repository mutation re-parses files, and the re-parse drops
		// their progress stamps — the drained-completion marker no longer
		// describes the store. InvalidateBackground must flip
		// HasBackgroundWork back to true so the post-mutation requeue works;
		// the fast marker is left alone (it is the fast tier's claim, not
		// the lane's to revoke).
		t.Setenv(HeavyRequestsEnv, "background")
		p := newP()
		ms := newMarkerStore(graph.New())
		require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
			RepoPrefix: "repo", Provider: p.Name(), IndexedSHA: "sha-1"}))
		require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
			RepoPrefix: "repo", Provider: p.Name() + backgroundMarkerSuffix, IndexedSHA: "sha-1"}))
		require.False(t, p.HasBackgroundWork(ms, "repo"), "fixture sanity: drained before the mutation")

		p.InvalidateBackground(ms, "repo")
		assert.True(t, p.HasBackgroundWork(ms, "repo"), "the mutation revoked the drained claim")
		fast, found, err := ms.GetEnrichmentState("repo", p.Name())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "sha-1", fast.IndexedSHA, "the fast tier's marker is untouched")
	})
}

// EnrichBackground runs the drain on a DEDICATED lane instance: the
// foreground provider's server sees zero requests, and the lane instance
// does the heavyDelta work.
func TestLSPProvider_EnrichBackground_LaneIsolation(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "")

	repoRoot, g, edge := heavyDeltaFixture(t)

	serverA := newFakeLSPServer()
	rigA := newHeavyDeltaRig(serverA.handle, repoRoot)
	p, cleanupA := providerWithFakeServer(t, serverA, []string{"go"})
	defer cleanupA()

	serverB := newFakeLSPServer()
	rigB := newHeavyDeltaRig(serverB.handle, repoRoot)
	rigB.refsResult = []Location{{
		URI:   pathToURI(filepath.Join(repoRoot, "svc.go")),
		Range: Range{Start: Position{Line: 10, Character: 16}, End: Position{Line: 10, Character: 22}},
	}}
	lane, cleanupB := providerWithFakeServer(t, serverB, []string{"go"})
	defer cleanupB()
	lane.heavyDelta = true
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := p.EnrichBackground(ctx, g, "", repoRoot)
	require.NoError(t, err)
	require.NotNil(t, result)

	// The lane did the deferred work…
	assert.Positive(t, rigB.references.Load())
	assert.Positive(t, rigB.incoming.Load())
	assert.Equal(t, 1.0, edge.Confidence, "the drain confirms through the lane instance")
	// …and the foreground server was never touched.
	total := rigA.hover.Load() + rigA.definition.Load() + rigA.references.Load() +
		rigA.implementations.Load() + rigA.prepareCall.Load() + rigA.outgoing.Load() +
		rigA.incoming.Load() + rigA.prepareTypeHier.Load()
	assert.Zero(t, total, "the foreground instance must see zero requests during the drain")
}

// The lane instance inherits the operator/router configuration the
// foreground provider carries — and never dials an IDE-attached server.
func TestLSPProvider_LaneInheritsForegroundConfig(t *testing.T) {
	spec := SpecByName("omnisharp")
	require.NotNil(t, spec)
	specWithConnect := *spec
	specWithConnect.Connect = &ConnectSpec{Network: "tcp", Address: "127.0.0.1:1"}

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	p.spec = &specWithConnect
	p.excludeGlobs = []string{"**/generated/**"}
	p.sweepMode = "off"
	p.opensDocs = false
	p.workspaceFolders = []string{"D:/extra/root"}
	p.maxParallel = 2
	// The router can augment env post-construction (BUNDLE_GEMFILE for a
	// Gemfile workspace) — rebuilding from the spec alone would drop it.
	p.env = []string{"BUNDLE_GEMFILE=D:/repo/Gemfile"}

	lane, err := p.newLaneProvider()
	require.NoError(t, err)
	assert.True(t, lane.heavyDelta)
	assert.False(t, lane.noHeavyRequests)
	assert.Equal(t, p.excludeGlobs, lane.excludeGlobs, "operator exclude globs must reach the drain")
	assert.Equal(t, p.env, lane.env, "router-augmented env must reach the drain server")
	assert.Equal(t, "off", lane.sweepMode, "configured sweep mode must reach the drain")
	assert.False(t, lane.opensDocs, "didOpen override must reach the drain")
	assert.Equal(t, p.workspaceFolders, lane.workspaceFolders)
	assert.Equal(t, 2, lane.maxParallel, "the foreground width applies when smaller than the lane cap")
	assert.Nil(t, lane.connect, "the drain must never dial an IDE-attached server")

	p.maxParallel = 16
	lane, err = p.newLaneProvider()
	require.NoError(t, err)
	assert.Equal(t, backgroundLaneMaxParallel, lane.maxParallel, "the lane cap bounds a wide foreground width")
}

// newLaneProvider builds the drain instance by copying configuration
// field-by-field — the classic source of "a new Provider field silently
// never reaches the lane" (it happened twice already: the lane dropped the
// operator config in review, then the router's env augmentation after).
// This inventory forces the decision: every Provider field is classified,
// and a new field fails the test until it is placed —
//
//	inherited:   newLaneProvider must copy it from the foreground instance
//	             (and TestLSPProvider_LaneInheritsForegroundConfig must
//	             assert the copy actually lands),
//	overridden:  newLaneProvider sets a deliberate lane value,
//	constructed: NewProviderFromSpec derives it from spec + logger,
//	runtime:     per-instance state a fresh lane must NOT share.
func TestLSPProvider_NewLaneProvider_FieldInventoryPinned(t *testing.T) {
	classified := map[string]string{
		"env":              "inherited",
		"workspaceFolders": "inherited",
		"excludeGlobs":     "inherited",
		"sweepMode":        "inherited",
		"opensDocs":        "inherited",
		"maxParallel":      "inherited", // foreground width, capped at backgroundLaneMaxParallel

		"heavyDelta":      "overridden", // true: the drain runs only the deferred classes
		"noHeavyRequests": "overridden", // false: the drain exists to run them
		"connect":         "overridden", // nil: never dial an IDE-attached server

		"command":            "constructed",
		"args":               "constructed",
		"languages":          "constructed",
		"daemon":             "constructed",
		"logger":             "constructed",
		"spec":               "constructed",
		"altInitOptions":     "constructed",
		"altInitOptionsFunc": "constructed",
		"dialBackoffStart":   "constructed",
		"maxDialBackoff":     "constructed",

		"laneProviderFactory": "runtime", // a lane must never spawn lanes
		"client":              "runtime",
		"sourceCache":         "runtime",
		"docMu":               "runtime",
		"docVersions":         "runtime",
		"openDocs":            "runtime",
		"lastDiag":            "runtime",
		"diagWaitersMu":       "runtime",
		"diagWaiters":         "runtime",
		"diagHookMu":          "runtime",
		"diagHook":            "runtime",
		"capsMu":              "runtime",
		"caps":                "runtime",
		"dynamicCaps":         "runtime",
		"dialBackoff":         "runtime",
		"reconnectMu":         "runtime",
		"reconnectAttempts":   "runtime",
		"connectOnce":         "runtime",
		"reqStats":            "runtime",
	}

	tp := reflect.TypeOf(Provider{})
	seen := map[string]bool{}
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		seen[name] = true
		if _, ok := classified[name]; !ok {
			t.Errorf("Provider field %q is not classified against the background lane — decide whether newLaneProvider must copy it (inherited), override it, or leave it to construction/runtime, then record it here; an inherited field also needs its copy asserted in TestLSPProvider_LaneInheritsForegroundConfig", name)
		}
	}
	for name := range classified {
		if !seen[name] {
			t.Errorf("classified field %q no longer exists on Provider — drop it from the inventory", name)
		}
	}
}

// laneDrainClean is the marker predicate: only a completed, uncancelled,
// non-partial, breaker-clean drain may claim the tier is drained.
func TestLaneDrainClean(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ok := context.Background()
	clean := &semantic.EnrichResult{}
	assert.True(t, laneDrainClean(ok, clean, nil))
	assert.False(t, laneDrainClean(ok, clean, assert.AnError), "an errored drain records nothing")
	assert.False(t, laneDrainClean(cancelled, clean, nil), "a cancelled drain records nothing")
	assert.False(t, laneDrainClean(ok, nil, nil), "a nil result records nothing")
	assert.False(t, laneDrainClean(ok, &semantic.EnrichResult{Partial: true}, nil), "a partial drain records nothing")
	assert.False(t, laneDrainClean(ok, &semantic.EnrichResult{BreakerTripped: true}, nil),
		"a breaker-tripped drain answered errors, not emptiness — it proves nothing about the tier")
}

// A server that never finishes loading its workspace must not be drained:
// it answers every heavy request empty-but-successfully, which would stamp
// every node and record the marker with zero yield — the exact pathology
// the foreground readiness gate exists to prevent.
func TestLSPProvider_EnrichBackground_NotReadyRecordsNothing(t *testing.T) {
	repoRoot, g, _ := heavyDeltaFixture(t)
	ms := newMarkerStore(g)

	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	lane, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	lane.heavyDelta = true

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }
	require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "", Provider: p.Name(), IndexedSHA: "sha-fast"}))

	prev := laneWaitReady
	laneWaitReady = func(context.Context, *Provider, string) error { return semantic.ErrWorkspaceNotReady }
	defer func() { laneWaitReady = prev }()

	_, err := p.EnrichBackground(context.Background(), ms, "", repoRoot)
	require.ErrorIs(t, err, semantic.ErrWorkspaceNotReady)

	total := rig.references.Load() + rig.prepareCall.Load() + rig.incoming.Load()
	assert.Zero(t, total, "a not-ready server must receive no drain requests")
	_, found, _ := ms.GetEnrichmentState("", p.Name()+backgroundMarkerSuffix)
	assert.False(t, found, "no marker for a drain that never ran")
}

// The readiness gate aborts on ANY error, not only ErrWorkspaceNotReady: a
// lane server that failed to spawn (WaitReady's ensureClient leg), a
// cancelled context, or an unexpected probe failure must not be drained —
// running the pass anyway would issue requests against a dead or unready
// server and mask the original error with a later, less specific one.
func TestLSPProvider_EnrichBackground_ReadinessErrorAbortsDrain(t *testing.T) {
	repoRoot, g, _ := heavyDeltaFixture(t)
	ms := newMarkerStore(g)

	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	lane, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	lane.heavyDelta = true

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }
	require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "", Provider: p.Name(), IndexedSHA: "sha-fast"}))

	spawnErr := errors.New("lane server failed to spawn")
	prev := laneWaitReady
	laneWaitReady = func(context.Context, *Provider, string) error { return spawnErr }
	defer func() { laneWaitReady = prev }()

	_, err := p.EnrichBackground(context.Background(), ms, "", repoRoot)
	require.ErrorIs(t, err, spawnErr)

	total := rig.references.Load() + rig.prepareCall.Load() + rig.incoming.Load()
	assert.Zero(t, total, "a drain must not run behind a failed readiness gate")
	_, found, _ := ms.GetEnrichmentState("", p.Name()+backgroundMarkerSuffix)
	assert.False(t, found, "no marker for a drain that never ran")
}

// The readiness leg can wedge inside a non-cancellable call — WaitReady's
// spawn / initialize / package-restore legs take no context. The budget
// must bound the WHOLE phase, not just the poll: on expiry the lane
// instance is closed (which unblocks a stuck LSP Call via the client's
// done channel and reaps the server), and if even that cannot free the
// prober it is abandoned — the drain, and the cancelRepo waiter behind
// it, must return rather than block a repository mutation.
func TestLSPProvider_EnrichBackground_WedgedReadinessDoesNotBlock(t *testing.T) {
	repoRoot, g, _ := heavyDeltaFixture(t)
	ms := newMarkerStore(g)

	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	lane, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	lane.heavyDelta = true

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }
	require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "", Provider: p.Name(), IndexedSHA: "sha-fast"}))

	prevBudget, prevGrace := backgroundLaneReadinessBudget, laneReadinessTeardownGrace
	backgroundLaneReadinessBudget = 50 * time.Millisecond
	laneReadinessTeardownGrace = 50 * time.Millisecond
	defer func() {
		backgroundLaneReadinessBudget, laneReadinessTeardownGrace = prevBudget, prevGrace
	}()

	block := make(chan struct{})
	defer close(block) // free the abandoned prober goroutine at test end
	prev := laneWaitReady
	laneWaitReady = func(context.Context, *Provider, string) error { <-block; return nil }
	defer func() { laneWaitReady = prev }()

	done := make(chan error, 1)
	go func() {
		_, err := p.EnrichBackground(context.Background(), ms, "", repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "a wedged readiness phase must surface an error")
	case <-time.After(2 * time.Second):
		t.Fatal("EnrichBackground blocked behind a wedged readiness probe")
	}

	total := rig.references.Load() + rig.prepareCall.Load() + rig.incoming.Load()
	assert.Zero(t, total, "no drain runs behind a failed readiness gate")
	_, found, _ := ms.GetEnrichmentState("", p.Name()+backgroundMarkerSuffix)
	assert.False(t, found, "no marker for a drain that never ran")
}

// The lane marker records the fast tier's sha AS OF DRAIN START — a fast
// pass finishing mid-drain moves the fast marker to a sha whose re-parsed
// state this drain never visited.
func TestLSPProvider_EnrichBackground_MarkerUsesDrainStartSHA(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "")

	repoRoot, g, _ := heavyDeltaFixture(t)
	ms := newMarkerStore(g)

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "", Provider: p.Name(), IndexedSHA: "sha-1"}))

	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		// Mid-drain, a fast pass lands a new sha.
		_ = ms.SetEnrichmentState(graph.EnrichmentState{
			RepoPrefix: "", Provider: p.Name(), IndexedSHA: "sha-2"})
		rig.references.Add(1)
		return []Location{}, nil
	})
	lane, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	lane.heavyDelta = true
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }

	_, err := p.EnrichBackground(context.Background(), ms, "", repoRoot)
	require.NoError(t, err)
	require.Positive(t, rig.references.Load(), "fixture sanity: the mid-drain move must have happened")

	laneMarker, found, err := ms.GetEnrichmentState("", p.Name()+backgroundMarkerSuffix)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "sha-1", laneMarker.IndexedSHA,
		"the marker claims only the state the drain actually visited")
}

// A drain that saw ZERO symbols for its language proves nothing about the
// tier: the store simply held no rows for the repo yet. The live-track
// pass-end enqueue can fire while the repo's nodes still sit in the
// indexer's shadow graph, invisible to the durable store the lane reads —
// claiming completion there would permanently skip the real drain wherever
// a fast marker already exists. No evidence => no marker, and the result
// is partial so the scheduler retries after the rows land.
func TestLSPProvider_EnrichBackground_NoRepoEvidenceIsNotClean(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "background")

	ms := newMarkerStore(graph.New()) // zero nodes anywhere: the repo is not visible yet

	server := newFakeLSPServer()
	lane, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	lane.heavyDelta = true

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }
	require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "", Provider: p.Name(), IndexedSHA: "sha-fast"}))

	result, err := p.EnrichBackground(context.Background(), ms, "", t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.True(t, result.Partial, "zero repo evidence must surface as partial so the scheduler retries")
	_, found, _ := ms.GetEnrichmentState("", p.Name()+backgroundMarkerSuffix)
	assert.False(t, found, "no marker may be recorded for a drain that saw no rows")
	assert.True(t, p.HasBackgroundWork(ms, ""), "the repo must stay drainable")
}

// The inverse must keep working: a repo whose symbols are ALL heavy-stamped
// drains to an empty frontier legitimately — zero requests, clean, marker
// recorded. The no-evidence guard keys on symbols seen, not requests sent.
func TestLSPProvider_EnrichBackground_AllStampedDrainStaysClean(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "background")

	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a", Language: "go", Kind: graph.KindFunction, Name: "A", FilePath: "a.go",
			StartLine: 1, EndLine: 3, Meta: map[string]any{"semantic_type": "func()", "semantic_heavy": "1"}},
		{ID: "b", Language: "go", Kind: graph.KindFunction, Name: "B", FilePath: "a.go",
			StartLine: 5, EndLine: 7, Meta: map[string]any{"semantic_type": "func()", "semantic_heavy": "1"}},
	}, nil)
	ms := newMarkerStore(g)

	server := newFakeLSPServer()
	lane, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	lane.heavyDelta = true

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }
	require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "", Provider: p.Name(), IndexedSHA: "sha-fast"}))

	result, err := p.EnrichBackground(context.Background(), ms, "", t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.False(t, result.Partial, "an all-stamped drain is complete, not partial")
	laneMarker, found, err := ms.GetEnrichmentState("", p.Name()+backgroundMarkerSuffix)
	require.NoError(t, err)
	require.True(t, found, "an all-stamped drain records the lane marker")
	assert.Equal(t, "sha-fast", laneMarker.IndexedSHA)
}

// A clean, uncancelled drain records the lane marker at the fast tier's
// sha; a factory failure records nothing.
func TestLSPProvider_EnrichBackground_Marker(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "")

	repoRoot, g, _ := heavyDeltaFixture(t)
	ms := newMarkerStore(g)

	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	rig.refsResult = []Location{{
		URI:   pathToURI(filepath.Join(repoRoot, "svc.go")),
		Range: Range{Start: Position{Line: 10, Character: 16}, End: Position{Line: 10, Character: 22}},
	}}
	lane, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	lane.heavyDelta = true

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }
	require.NoError(t, ms.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "", Provider: p.Name(), IndexedSHA: "sha-fast"}))

	ctx := context.Background()
	_, err := p.EnrichBackground(ctx, ms, "", repoRoot)
	require.NoError(t, err)

	laneMarker, found, err := ms.GetEnrichmentState("", p.Name()+backgroundMarkerSuffix)
	require.NoError(t, err)
	require.True(t, found, "a clean drain records the lane marker")
	assert.Equal(t, "sha-fast", laneMarker.IndexedSHA, "the lane marker copies the fast tier's sha")

	t.Run("factory failure records nothing", func(t *testing.T) {
		ms2 := newMarkerStore(graph.New())
		p2 := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
		p2.laneProviderFactory = func() (*Provider, error) { return nil, assert.AnError }
		require.NoError(t, ms2.SetEnrichmentState(graph.EnrichmentState{
			RepoPrefix: "", Provider: p2.Name(), IndexedSHA: "sha-fast"}))
		_, err := p2.EnrichBackground(context.Background(), ms2, "", t.TempDir())
		require.Error(t, err)
		_, found, _ := ms2.GetEnrichmentState("", p2.Name()+backgroundMarkerSuffix)
		assert.False(t, found)
	})
}
