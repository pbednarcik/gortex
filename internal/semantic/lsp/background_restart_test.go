package lsp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
)

// The durable-progress contract, end to end on the real store: one clean
// drain into SQLite, then close/reopen, then two restart censuses — each
// must be a complete no-op (no lane server built, no requests, graph
// counts unchanged), because the heavy stamps AND the lane marker both
// survived the reopen. This is the reported regression shape: a restart
// after a clean drain re-classified the repo as undrained and kept adding
// edges, because the frontier could not see the stamps on SQLite.
func TestLSPProvider_BackgroundDrain_SurvivesReopenAndRestartsAreNoOps(t *testing.T) {
	t.Setenv(SweepEnv, "full") // sweep every node so stamps land and the reopened frontier is provably empty
	t.Setenv(HeavyRequestsEnv, "background")
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")

	storePath := filepath.Join(t.TempDir(), "lane-restart.sqlite")
	store, err := store_sqlite.Open(storePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() }) // failure-path safety; the happy path closes explicitly

	// The fixture repo on disk plus its graph rows in SQLite, fast tier
	// already done: semantic_type stamps and the fast marker at a real sha.
	repoRoot, mem, _ := heavyDeltaFixture(t)
	for _, n := range mem.AllNodes() {
		n.RepoPrefix = "repo"
		n.FilePath = "repo/" + n.FilePath
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["semantic_type"] = "func()"
		store.AddNode(n)
	}
	for _, e := range mem.AllEdges() {
		e.FilePath = "repo/" + e.FilePath // ids unchanged; the file anchor moves with the node
		store.AddEdge(e)
	}
	baseNodes := len(store.GetRepoNodes("repo"))
	require.Positive(t, baseNodes, "fixture rows must be visible in the store")

	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	rig.refsResult = []Location{{
		URI:   pathToURI(filepath.Join(repoRoot, "svc.go")),
		Range: Range{Start: Position{Line: 10, Character: 16}, End: Position{Line: 10, Character: 22}},
	}}
	lane, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	lane.heavyDelta = true
	lane.noHeavyRequests = false

	// The command must resolve on PATH — the census consults Available()
	// before enqueuing. "go" is guaranteed present in a Go test run; the
	// injected factory means it is never actually spawned.
	p := NewProvider("go", nil, []string{"go"}, false, 2, zap.NewNop())
	p.laneProviderFactory = func() (*Provider, error) { return lane, nil }
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "repo", Provider: p.Name(), IndexedSHA: "sha-fast"}))

	// First lifetime: the census finds the undrained tier and drains it.
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	mgr.RegisterProvider(p)
	mgr.StartBackgroundLane(context.Background(), store, map[string]string{"repo": repoRoot})
	require.Eventually(t, func() bool {
		lane, found, err := store.GetEnrichmentState("repo", p.Name()+backgroundMarkerSuffix)
		return err == nil && found && lane.IndexedSHA == "sha-fast"
	}, 5*time.Second, 20*time.Millisecond, "the census drain must complete and record the lane marker")
	require.NoError(t, mgr.Close())
	firstDrainRequests := rig.prepareCall.Load() + rig.incoming.Load() + rig.references.Load()
	require.Positive(t, firstDrainRequests, "the first drain must have done real work")

	// Close and reopen: both grains of progress must survive the reopen.
	drainedNodes := len(store.GetRepoNodes("repo"))
	drainedEdges := len(store.GetRepoEdges("repo"))
	require.NoError(t, store.Close())
	reopened, err := store_sqlite.Open(storePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })

	stamped := 0
	for _, n := range reopened.GetRepoNodes("repo") {
		if nodeHeavyStamped(n) {
			stamped++
		}
	}
	require.Positive(t, stamped, "heavy stamps must survive the SQLite reopen")
	laneMarker, found, err := reopened.GetEnrichmentState("repo", p.Name()+backgroundMarkerSuffix)
	require.NoError(t, err)
	require.True(t, found, "the lane marker must survive the SQLite reopen")
	require.Equal(t, "sha-fast", laneMarker.IndexedSHA)

	// The reopened frontier must be empty at node grain too — this is the
	// SQLite resume regression: the light projection cannot see the
	// blob-only stamp, so without the full-node recheck every restart
	// rebuilt the whole frontier.
	frontierProbe := &Provider{languages: []string{"go"}, heavyDelta: true}
	projection, ok := frontierProbe.readLSPRepoProjection(reopened, "repo")
	require.True(t, ok)
	assert.Empty(t, projection.langNodes, "a drained repo's reopened frontier must hold no candidates")

	// Restarts 1 and 2: the census must not enqueue, must not build a lane
	// server, and must not change the graph.
	for restart := 1; restart <= 2; restart++ {
		p2 := NewProvider("go", nil, []string{"go"}, false, 2, zap.NewNop())
		p2.laneProviderFactory = func() (*Provider, error) {
			t.Errorf("restart %d built a lane server for a drained repo", restart)
			return nil, assert.AnError
		}
		mgr2 := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
		mgr2.RegisterProvider(p2)
		mgr2.StartBackgroundLane(context.Background(), reopened, map[string]string{"repo": repoRoot})
		time.Sleep(250 * time.Millisecond) // the census is synchronous; this covers the worker dequeue
		require.NoError(t, mgr2.Close())

		assert.Equal(t, drainedNodes, len(reopened.GetRepoNodes("repo")),
			"restart %d changed the node count of a drained repo", restart)
		assert.Equal(t, drainedEdges, len(reopened.GetRepoEdges("repo")),
			"restart %d changed the edge count of a drained repo", restart)
	}
	assert.Equal(t, firstDrainRequests, rig.prepareCall.Load()+rig.incoming.Load()+rig.references.Load(),
		"restarts must send zero heavy requests")
}
