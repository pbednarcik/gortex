package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestIncrementalReindex_NestedFileNoDuplicate guards the
// slash-path duplicate indexing regression on Windows.
//
// The cold bulk walk and the incremental re-index must key a nested
// file's graph nodes under the SAME spelling — relKey's
// "myrepo/pkg/sub/thing.go" — because graph.GetFileNodes / EvictFile
// match the key byte-for-byte, with no separator folding. When the two
// disagreed on Windows the evict missed the cold nodes, the stale set
// survived, and the re-parse appended a second copy under the other
// separator — a duplicate that grew on every save.
//
// On POSIX filepath.Rel already yields '/', so the two key forms
// coincide and this is a Windows-only correction; the assertions below
// hold on every platform (before the fix they failed only on Windows).
func TestIncrementalReindex_NestedFileNoDuplicate(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "pkg", "sub", "thing.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(nested), 0o755))
	const content = "package sub\n\nfunc Thing() {}\n"
	writeFile(t, nested, content)

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("myrepo")
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// Cold index established exactly one node named Thing.
	cold := g.FindNodesByName("Thing")
	require.Len(t, cold, 1, "cold index should define Thing exactly once")
	coldFilePath := cold[0].FilePath

	// A scoped incremental re-index of the same file must REPLACE its
	// node set, not leak a second copy under a different separator form.
	bumpMtime(t, nested, content)
	_, err = idx.IncrementalReindexPaths(dir, []string{nested})
	require.NoError(t, err)

	things := g.FindNodesByName("Thing")
	require.Len(t, things, 1,
		"incremental re-index must replace the nested file's nodes, not duplicate them")

	// The re-indexed node must land on the SAME key the cold walk used,
	// so the graph stays internally consistent and runtime lookups (which
	// build OS-native graph paths) still resolve it after a save.
	assert.Equal(t, coldFilePath, things[0].FilePath,
		"incremental node FilePath must match the cold-walk key form")

	// And the whole file's node set must be reachable under that one key
	// — never split across a slash and a backslash spelling.
	nodesUnderColdKey := g.GetFileNodes(idx.prefixPath(idx.relKey(nested)))
	assert.NotEmpty(t, nodesUnderColdKey,
		"the file's nodes must be retrievable under the graph key form")
}
