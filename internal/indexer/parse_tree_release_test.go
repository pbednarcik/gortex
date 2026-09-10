package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// The Go extractor is the only one that hands its tree-sitter tree back
// on the ExtractionResult, and that tree is C memory the Go GC cannot
// reclaim. Every incremental path that takes an extraction must release
// it, or a long-lived daemon accumulates one leaked parse tree per
// re-indexed Go file. These tests pin each of those paths.
//
// A released ParseTree reports a nil Tree(), which is the observable
// stand-in for "the underlying C tree was freed".

func newParseTreeReleaseIndexer(t *testing.T, dir string) *Indexer {
	t.Helper()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(graph.New(), registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return idx
}

// preparedTree returns the live ParseTree the prepared extraction for
// path is holding, failing the test if there is not exactly one.
func preparedTree(t *testing.T, idx *Indexer, path string) *parser.ParseTree {
	t.Helper()
	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	idx.preparedMu.Lock()
	defer idx.preparedMu.Unlock()
	entry := idx.prepared[abs]
	require.NotNil(t, entry, "expected a prepared extraction for %s", path)
	require.NotNil(t, entry.result, "prepared extraction has no result")
	require.NotNil(t, entry.result.Tree, "Go extraction should carry a parse tree")
	require.NotNil(t, entry.result.Tree.Tree(), "parse tree should still be live")
	return entry.result.Tree
}

func TestDiscardPreparedExtractionReleasesParseTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")
	idx := newParseTreeReleaseIndexer(t, dir)

	writeTestFile(t, path, "package main\n\nfunc Value() int { return 1 }\n")
	_, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	tree := preparedTree(t, idx, path)

	idx.discardPreparedExtraction(path)
	require.Nil(t, tree.Tree(), "discarding a prepared extraction must release its parse tree")
}

func TestPrepareFileDeltaReleasesDisplacedParseTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")
	idx := newParseTreeReleaseIndexer(t, dir)

	writeTestFile(t, path, "package main\n\nfunc Value() int { return 1 }\n")
	_, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	first := preparedTree(t, idx, path)

	// A second prepare for the same path before the first is consumed
	// overwrites the map slot; the displaced entry's tree is otherwise
	// unreachable.
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 2 }\n")
	_, ok = idx.prepareFileDelta(path)
	require.True(t, ok)
	second := preparedTree(t, idx, path)

	require.NotSame(t, first, second)
	require.Nil(t, first.Tree(), "displaced prepared extraction must release its parse tree")
	require.NotNil(t, second.Tree(), "the current prepared extraction must keep its tree")

	idx.discardPreparedExtraction(path)
	require.Nil(t, second.Tree())
}

func TestTakePreparedExtractionReleasesParseTreeOnMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")
	idx := newParseTreeReleaseIndexer(t, dir)

	writeTestFile(t, path, "package main\n\nfunc Value() int { return 1 }\n")
	_, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	tree := preparedTree(t, idx, path)

	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	// Source bytes that don't match what was prepared: the entry is
	// dropped rather than handed to the caller.
	result, taken := idx.takePreparedExtraction(
		abs, idx.relKey(abs), "go", []byte("package main\n\nfunc Other() {}\n"),
	)
	require.False(t, taken)
	require.Nil(t, result)
	require.Nil(t, tree.Tree(), "a rejected prepared extraction must release its parse tree")
}

func TestIndexFileReleasesPreparedParseTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")
	idx := newParseTreeReleaseIndexer(t, dir)

	writeTestFile(t, path, "package main\n\nfunc Value() int { return 1 }\n")
	_, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	tree := preparedTree(t, idx, path)

	require.NoError(t, idx.indexFile(path, true))
	require.Nil(t, tree.Tree(), "indexFile must release the parse tree it consumed")
}

func TestIndexFileReleasesParseTreeWithoutPrepare(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")
	idx := newParseTreeReleaseIndexer(t, dir)

	// No prepare: indexFile extracts for itself, so it owns the tree.
	// Observed indirectly — the prepared map must stay empty and the
	// re-index must still succeed without retaining anything.
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 1 }\n")
	require.NoError(t, idx.indexFile(path, true))

	idx.preparedMu.Lock()
	remaining := len(idx.prepared)
	idx.preparedMu.Unlock()
	require.Zero(t, remaining)
}

func TestApplyPreparedMetadataRefreshReleasesParseTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")
	idx := newParseTreeReleaseIndexer(t, dir)

	// A comment-only edit keeps the node shape identical, which is the
	// case the metadata-refresh fast path is built for.
	writeTestFile(t, path, "package main\n\n// Value returns a number.\nfunc Value() int { return 0 }\n")
	_, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	tree := preparedTree(t, idx, path)

	prior := idx.graph.GetFileNodes(idx.relKey(path))
	idx.applyPreparedMetadataRefresh(path, prior)
	require.Nil(t, tree.Tree(), "the metadata refresh must release the tree it took")
}

func TestStructuralSymbolsReleasesParseTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")
	idx := newParseTreeReleaseIndexer(t, dir)

	// StructuralSymbols owns its extraction end to end, so the tree is
	// unobservable from outside; assert it returns the symbols it
	// promises and leaves nothing prepared behind.
	syms, ok := idx.StructuralSymbols(path)
	require.True(t, ok)
	require.NotEmpty(t, syms)

	idx.preparedMu.Lock()
	remaining := len(idx.prepared)
	idx.preparedMu.Unlock()
	require.Zero(t, remaining)
}
