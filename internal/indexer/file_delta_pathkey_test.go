package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

const pathKeyFixtureV1 = "package sub\n\nfunc Helper() int { return 1 }\n"
const pathKeyFixtureV2 = "package sub\n\nfunc Helper() int { return 2 }\n"

// TestFileDeltaReusesColdWalkPathKey pins the fix for #437.
//
// The cold walk and the single-file delta path both stamp their relPath onto
// node IDs, and both must use relKey's slash-separated spelling. When the two
// disagreed, the cold walk stored "sub\file.go::Helper" on Windows and the
// first edit moved the symbol to "sub/file.go::Helper" — a different node
// identity for a file that never moved. On a persistent store the abandoned ID
// stays behind with whatever line numbers it last held.
//
// On POSIX filepath.Rel already yields "/" and the two forms coincide, so this
// is a no-op guard there and a real regression test on Windows.
func TestFileDeltaReusesColdWalkPathKey(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	path := filepath.Join(dir, "sub", "file.go")
	require.NoError(t, os.WriteFile(path, []byte(pathKeyFixtureV1), 0o644))

	store := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(store, registry, cfg.Index, zap.NewNop())
	idx.SetRootPath(dir)

	_, err := idx.Index(dir)
	require.NoError(t, err)
	cold := helperNodeIDs(store)
	require.Len(t, cold, 1, "cold walk must index exactly one Helper node")

	// Re-index through the delta path, the way the watcher does after an edit.
	require.NoError(t, os.WriteFile(path, []byte(pathKeyFixtureV2), 0o644))
	require.NoError(t, idx.IndexFile(path))

	after := helperNodeIDs(store)
	require.Len(t, after, 1, "delta re-index must not add a second Helper node")
	require.Equal(t, cold[0], after[0],
		"a delta re-index must land on the node the cold walk wrote; the ID moved, "+
			"which on a persistent store leaves the old node behind as a stale twin")
}

// TestRelKeyMatchesColdWalkForm states the contract the fix relies on: relKey
// returns the slash-separated form the cold walk stamps, so a delta re-index
// keyed on it lands on the existing node. Asserting the literal is the point —
// deriving the expectation with filepath.Rel would restate the bug on Windows.
func TestRelKeyMatchesColdWalkForm(t *testing.T) {
	dir := t.TempDir()
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)

	abs := filepath.Join(dir, "sub", "file.go")

	require.Equal(t, "sub/file.go", idx.relKey(abs),
		"the graph key is slash-separated on every platform, never OS-native")
}

func helperNodeIDs(store graph.Store) []string {
	var out []string
	for _, node := range store.AllNodes() {
		if strings.HasSuffix(node.ID, "::Helper") {
			out = append(out, node.ID)
		}
	}
	return out
}
