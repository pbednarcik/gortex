package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// Where the runtime planner-statistics check is wired into the index pipeline.
//
// The store's own tests prove the rule; these prove the pipeline actually asks.
// A daemon that never asks is exactly the state issue #651 left behind: the
// statistics were computed when the store held one repository and were never
// recomputed as it grew to hold several.

// writeGoTree writes n single-function Go files under dir and returns dir.
func writeGoTree(t *testing.T, dir, pkg string, n int) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("package %s\n\ntype T%03d struct{}\n\nfunc (t T%03d) M%03d() {}\n\nfunc F%03d() {}\n",
			pkg, i, i, i, i)
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.go", i)), body)
	}
	return dir
}

func plannerHealth(t *testing.T, store *store_sqlite.Store) graph.PlannerStatsFreshness {
	t.Helper()
	h, err := store.PlannerStatsHealth(context.Background())
	require.NoError(t, err)
	return h
}

// A second repository draining into an already populated store is the runtime
// growth this PR exists for: BeginBulkLoad is a no-op on a non-empty store, so
// FlushBulk returns without refreshing, and nothing else re-analyzes until the
// daemon restarts.
func TestMultiIndexer_SecondRepoDrainRefreshesPlannerStats(t *testing.T) {
	base := t.TempDir()
	dirA := writeGoTree(t, filepath.Join(base, "repo-small"), "small", 2)
	dirB := writeGoTree(t, filepath.Join(base, "repo-large"), "large", 60)

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: dirA, Name: "repo-small"},
		{Path: dirB, Name: "repo-large"},
	}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	store := newFTSStore(t)
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewSymbolSearcherBackend(store), cm, zap.NewNop())

	_, err = mi.TrackRepo(config.RepoEntry{Path: dirA, Name: "repo-small"})
	require.NoError(t, err)
	afterA := plannerHealth(t, store)
	require.False(t, afterA.Stale,
		"a tracked repository left the store without usable planner statistics: %s", afterA.Reason)
	require.Positive(t, afterA.Nodes.Believed, "no nodes statistics after the first repository")

	_, err = mi.TrackRepo(config.RepoEntry{Path: dirB, Name: "repo-large"})
	require.NoError(t, err)

	afterB := plannerHealth(t, store)
	require.True(t, afterB.Nodes.Actual >= 2*afterA.Nodes.Actual,
		"fixture did not double the store: %d -> %d nodes; the growth rule cannot be exercised",
		afterA.Nodes.Actual, afterB.Nodes.Actual)
	// The verdict the drain read was the nodes one — the rules stop at the
	// first hit — and it is cleared: nothing costs the node corpus against
	// repo-small's figures any more.
	require.True(t, afterB.Nodes.Believed*2 >= afterB.Nodes.Actual,
		"planner believes %d nodes over a store holding %d", afterB.Nodes.Believed, afterB.Nodes.Actual)

	// A store that outgrew BOTH relations owes one boundary per relation: each
	// growth anchor moves only on its own family's completed pass, because a
	// nodes pass rebuilds no row describing an index on edges and may not claim
	// otherwise (store_sqlite.plannerStatsBaseline). So a residual edges verdict
	// here is the mechanism working; a store that never settles is the defect,
	// and that is what this loop bounds.
	for pass := 1; afterB.Stale; pass++ {
		require.Less(t, pass, 4,
			"the store never settled: still %q after %d further boundaries", afterB.Reason, pass)
		next, err := store.EnsurePlannerStatsFresh(context.Background())
		require.NoError(t, err)
		require.True(t, next.Refreshed,
			"boundary %d left the store stale without refreshing it: %s", pass, next.Reason)
		afterB = plannerHealth(t, store)
	}

	// Quiescent, and quiescent for the right reason. The nodes assertion above
	// runs before the settling loop, so on its own it is satisfied by a store
	// whose edge statistics were never rebuilt at all — which is exactly the
	// state an anchor moved by the wrong pass produces (store_sqlite's
	// notePlannerStatsRefresh / plannerStatsWorkFamilies). Assert the edge
	// relation too, once nothing more is owed: edges_by_kind is what the
	// receiver/edge joins issue #651 is about are costed from.
	require.True(t, afterB.Edges.Believed*2 >= afterB.Edges.Actual,
		"planner believes %d edges over a store holding %d; the edge statistics were never rebuilt",
		afterB.Edges.Believed, afterB.Edges.Actual)
	require.True(t, afterB.Nodes.Believed*2 >= afterB.Nodes.Actual,
		"planner believes %d nodes over a store holding %d", afterB.Nodes.Believed, afterB.Nodes.Actual)
}

// The streaming-flush path flushes per chunk, and it is served by the same
// end-of-pass check the direct path is: the chunk loop leaves its rows on disk
// and the counters are written afterwards, so one check at the end of the pass
// sees both. Asking per chunk instead would fire the growth rule O(log N)
// times mid-load, and each ANALYZE is proportional to index pages — tens of
// seconds on a large store.
func TestStreamingFlush_RefreshesPlannerStatsOnce(t *testing.T) {
	// What separates "once at the end of the pass" from "once per chunk" is
	// whether the work depends on the chunk count at all, so the same corpus
	// is indexed twice: once as two chunks, once as one.
	runWithChunkSize := func(t *testing.T, chunkSize string) graph.PlannerStatsFreshness {
		t.Helper()
		t.Setenv("GORTEX_SHADOW_MAX_FILES", "1")
		t.Setenv("GORTEX_STREAMING_FLUSH", "1")
		t.Setenv("GORTEX_STREAMING_CHUNK_SIZE", chunkSize)

		dir := writeGoTree(t, filepath.Join(t.TempDir(), "src"), "src", 4)
		store := newFTSStore(t)
		idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
		_, err := idx.Index(dir)
		require.NoError(t, err)
		return plannerHealth(t, store)
	}

	var chunked, whole graph.PlannerStatsFreshness
	t.Run("two chunks", func(t *testing.T) { chunked = runWithChunkSize(t, "2") })
	t.Run("one chunk", func(t *testing.T) { whole = runWithChunkSize(t, "8") })

	require.Positive(t, chunked.Checks, "the streaming path never consulted the freshness check")
	require.EqualValues(t, 1, chunked.Refreshes,
		"a streaming index rebuilt planner statistics %d times; each rebuild is proportional to index "+
			"pages, so a per-chunk check is tens of seconds per chunk on a large store", chunked.Refreshes)
	require.Equal(t, whole.Checks, chunked.Checks,
		"splitting the same corpus into chunks changed how often planner statistics were consulted "+
			"(%d over one chunk, %d over two): the check is inside the chunk loop", whole.Checks, chunked.Checks)
	require.Equal(t, whole.Refreshes, chunked.Refreshes,
		"splitting the same corpus into chunks changed how often they were rebuilt (%d over one chunk, %d over two)",
		whole.Refreshes, chunked.Refreshes)
	require.False(t, chunked.Stale, "streaming flush left stale planner statistics: %s", chunked.Reason)
}

// The direct-SQLite path with nothing for the resolver to do. A whole-graph
// resolve returns before its attribution tail when the unresolved frontier is
// empty, so on a corpus of plain declarations the end-of-pass counter site is
// the ONLY thing that can notice the store has outgrown its statistics — and
// it is the path a daemon spends its life on.
func TestDirectIndex_RefreshesPlannerStatsWithoutAResolvePass(t *testing.T) {
	// Above the shadow ceiling and with streaming off: the pass writes
	// straight to SQLite, so diskTarget is nil and the counter site has to
	// resolve its target exactly as the counter write does.
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1")

	dir := t.TempDir()
	for i := 0; i < 12; i++ {
		// Declarations only: no call, no reference, no unresolved edge.
		writeFile(t, filepath.Join(dir, fmt.Sprintf("t%03d.go", i)),
			fmt.Sprintf("package decls\n\ntype D%03d struct{}\n", i))
	}

	store := newFTSStore(t)
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	h := plannerHealth(t, store)
	require.Positive(t, h.Refreshes,
		"an index pass with an empty unresolved frontier left the store with no planner statistics at all")
	require.False(t, h.Stale, "planner statistics stale after a direct index: %s", h.Reason)
	require.Positive(t, h.Nodes.Believed)
}

// The direct-SQLite path writes its counters through idx.graph, not through a
// shadow disk target. Selecting the freshness target any other way would skip
// this path entirely — and it is the incremental path a daemon spends its life
// on.
func TestIndexStateTarget_MatchesCounterWriter(t *testing.T) {
	store := newFTSStore(t)
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())

	require.Same(t, store, idx.indexStateTarget(nil),
		"with no shadow disk target the counters are written through idx.graph")

	other := newFTSStore(t)
	require.Same(t, other, idx.indexStateTarget(other),
		"a shadow drain writes its counters to the disk target")
}

// The save-driven incremental path. The git-watcher and the poller both land
// on reconcileRepoIndexState, and it reaches none of the other four sites: it
// never drains a shadow and never runs the full pass that ends at the counter
// site. Without a check here a daemon that only ever reindexes changed files
// keeps the statistics of the cold load that created the store for the rest of
// its life — which is exactly the state issue #651 describes.
func TestReconcileRepoIndexState_RefreshesPlannerStats(t *testing.T) {
	dir := writeGoTree(t, filepath.Join(t.TempDir(), "src"), "src", 4)
	store := newFTSStore(t)
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	before := plannerHealth(t, store)
	require.False(t, before.Stale, "fixture started stale: %s", before.Reason)

	// Runtime growth with nothing to re-analyze it: rows land in the physical
	// tables while sqlite_stat1 still describes the indexed tree.
	var nodes []*graph.Node
	for i := 0; i < 300; i++ {
		file := fmt.Sprintf("grown/p%03d/types.go", i)
		name := fmt.Sprintf("G%03d", i)
		nodes = append(nodes, &graph.Node{
			ID: file + "::" + name, Name: name, Kind: graph.KindType,
			FilePath: file, Language: "go",
		})
	}
	store.AddBatch(nodes, nil)

	idx.reconcileRepoIndexState(context.Background(), dir)

	after := plannerHealth(t, store)
	require.Greater(t, after.Refreshes, before.Refreshes,
		"the incremental reconcile left the store believing %d nodes over the %d it holds; the "+
			"save-driven path is the one a daemon spends its life on and it asked nobody",
		after.Nodes.Believed, after.Nodes.Actual)
	require.False(t, after.Stale, "planner statistics stale after an incremental reconcile: %s", after.Reason)
	require.True(t, after.Nodes.Believed*2 >= after.Nodes.Actual,
		"planner believes %d nodes over a store holding %d", after.Nodes.Believed, after.Nodes.Actual)
}
