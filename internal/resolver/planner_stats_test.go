package resolver

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The whole-graph attribution tail is the pass issue #651 misplanned: the Go
// receiver rebind and the bare-name / builtin passes join the node and edge
// corpora, and a planner costing them against a fraction of the store's real
// cardinality hoists the wrong relation to the outer loop. A daemon reaches
// this pass many times over the life of a store that keeps growing, so it is
// the natural place to notice that the statistics no longer describe it.
func TestResolveAll_RefreshesStalePlannerStatsBeforeAttribution(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "planner-stats.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	seed := func(namespace string, types int) {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for i := 0; i < types; i++ {
			dir := fmt.Sprintf("repo/%s/p%03d", namespace, i)
			name := fmt.Sprintf("%sT%03d", namespace, i)
			typeFile := dir + "/types.go"
			methodFile := dir + "/methods.go"
			methodID := methodFile + "::" + name + ".M"
			nodes = append(nodes,
				&graph.Node{ID: typeFile + "::" + name, Name: name, Kind: graph.KindType, FilePath: typeFile, Language: "go", RepoPrefix: "repo"},
				&graph.Node{ID: methodID, Name: "M", Kind: graph.KindMethod, FilePath: methodFile, Language: "go", RepoPrefix: "repo"},
			)
			edges = append(edges,
				&graph.Edge{
					From: methodID, To: methodFile + "::" + name, Kind: graph.EdgeMemberOf,
					FilePath: methodFile, Line: i + 1,
				},
				// One genuinely pending edge per package: a resolve with an
				// empty unresolved frontier returns before it ever reaches the
				// whole-graph attribution tail this test is about.
				&graph.Edge{
					From: methodID, To: "unresolved::" + name + "Helper", Kind: graph.EdgeCalls,
					FilePath: methodFile, Line: i + 2,
				})
		}
		store.AddBatch(nodes, edges)
	}
	counters := func(nodes, edges int) {
		require.NoError(t, store.SetRepoIndexState(graph.RepoIndexState{
			RepoPrefix: "repo", NodeCount: nodes, EdgeCount: edges,
		}))
	}

	// Statistics that describe the store as it is now.
	seed("a", 100)
	counters(200, 100)
	baseline, err := store.EnsurePlannerStatsFresh(context.Background())
	require.NoError(t, err)
	require.True(t, baseline.Refreshed, "fixture did not establish planner statistics")

	// Runtime growth with nothing to re-analyze it: exactly what a daemon does
	// between restarts.
	seed("b", 150)
	counters(500, 250)
	stale, err := store.PlannerStatsHealth(context.Background())
	require.NoError(t, err)
	require.True(t, stale.Stale, "fixture did not go stale: %s", stale.Reason)

	New(store).ResolveAll()

	after, err := store.PlannerStatsHealth(context.Background())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(after.LastRefreshReason, "growth:"),
		"last refresh reason = %q, want the growth verdict the resolve should have acted on", after.LastRefreshReason)
	require.True(t, after.Refreshes > baseline.Refreshes,
		"no refresh happened during the resolve (%d refreshes before, %d after)", baseline.Refreshes, after.Refreshes)
	require.True(t, after.Nodes.Believed*2 >= after.Nodes.Actual,
		"the resolve left the planner believing %d nodes over a store holding %d",
		after.Nodes.Believed, after.Nodes.Actual)

	// The resolve clears the verdict it READ, and the rules stop at the first
	// hit, so a fixture that outgrew both relations is left owing the edges
	// family one more boundary: each growth anchor moves only on its own
	// family's completed pass (store_sqlite.plannerStatsBaseline). That residue
	// is the mechanism working — what must not happen is a store that never
	// settles, which is what this bounded loop asserts.
	for pass := 1; after.Stale; pass++ {
		require.Less(t, pass, 4,
			"the store never settled: still %q after %d further boundaries", after.Reason, pass)
		next, err := store.EnsurePlannerStatsFresh(context.Background())
		require.NoError(t, err)
		require.True(t, next.Refreshed,
			"boundary %d left the store stale without refreshing it: %s", pass, next.Reason)
		after, err = store.PlannerStatsHealth(context.Background())
		require.NoError(t, err)
	}
}
