package lsp

import (
	"context"
	"path/filepath"
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
}

// EnrichBackground runs the drain on a DEDICATED lane instance: the
// foreground provider's server sees zero requests, and the lane instance
// does the heavyDelta work.
func TestLSPProvider_EnrichBackground_LaneIsolation(t *testing.T) {
	t.Setenv(SweepEnv, "")

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

// A clean, uncancelled drain records the lane marker at the fast tier's
// sha; a factory failure records nothing.
func TestLSPProvider_EnrichBackground_Marker(t *testing.T) {
	t.Setenv(SweepEnv, "")

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
