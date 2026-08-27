package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
)

// TestIncrementalReindex_EvictsExcludedFiles is the regression for #321:
// when a previously-indexed file becomes excluded, IncrementalReindex must
// purge its nodes even though the file still exists on disk. Otherwise
// file_count drops while node_count/search keep serving orphans.
func TestIncrementalReindex_EvictsExcludedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.go"), `package main

func Kept() {}
`)
	writeFile(t, filepath.Join(dir, "drop.go"), `package main

func Dropped() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Kept"))
	require.NotEmpty(t, g.FindNodesByName("Dropped"))

	// Mid-flight exclusion: drop.go remains on disk but must leave the graph.
	idx.SetExcludePatterns(append(append([]string{}, excludes.Builtin...), "drop.go"))

	res, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, res.DeletedFileCount, "excluded-but-present file must count as deleted")

	assert.NotEmpty(t, g.FindNodesByName("Kept"), "kept.go was not excluded; nodes must survive")
	assert.Empty(t, g.FindNodesByName("Dropped"), "drop.go is excluded; nodes must be evicted")
}

// TestIncrementalReindex_EvictsTrulyDeletedFiles is the control case: a
// file that has actually been removed from disk should still be evicted.
func TestIncrementalReindex_EvictsTrulyDeletedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.go"), `package main

func Kept() {}
`)
	gonePath := filepath.Join(dir, "gone.go")
	writeFile(t, gonePath, `package main

func Gone() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Gone"))

	require.NoError(t, os.Remove(gonePath))

	_, err = idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	assert.NotEmpty(t, g.FindNodesByName("Kept"))
	assert.Empty(t, g.FindNodesByName("Gone"), "gone.go was deleted from disk; nodes must be evicted")
}

// canonicalGraph renders a graph as a deterministic, sorted projection
// of its structural identity (node identities + edge triples). Two
// graphs with an equal projection are byte-identical for every query
// the engine can answer.
func canonicalGraph(g graph.Store) string {
	var lines []string
	for _, n := range g.AllNodes() {
		if n == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("N|%s|%s|%s|%s|%d|%d|%s",
			n.ID, n.Kind, n.Name, n.FilePath, n.StartLine, n.EndLine, n.Language))
	}
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("E|%s|%s|%s", e.From, e.To, e.Kind))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// bumpMtime rewrites a file and pushes its mtime forward so the
// mtime-keyed staleness check always classifies it as changed,
// regardless of filesystem timestamp resolution.
func bumpMtime(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
}

// TestIncrementalReindex_ConvergesToFullIndex is the consistency
// invariant: a graph built incrementally — a cold index followed by
// per-file edits each reconciled with IncrementalReindex — must equal
// a single cold index of the same final disk state. Incremental
// reindex that drifted from a full index would silently serve a stale
// or wrong graph.
func TestIncrementalReindex_ConvergesToFullIndex(t *testing.T) {
	build := func(dir string) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
		writeFile(t, filepath.Join(dir, "main.go"),
			"package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n")
		writeFile(t, filepath.Join(dir, "pkg", "util.go"),
			"package pkg\n\ntype Config struct{ Port int }\n\nfunc New() *Config { return &Config{} }\n")
		writeFile(t, filepath.Join(dir, "extra.go"),
			"package main\n\nfunc Extra() {}\n")
	}

	// Path A: incremental — a cold index, then a sequence of edits
	// each reconciled with IncrementalReindex.
	dir := t.TempDir()
	build(dir)
	gA := graph.New()
	idxA := newTestIndexer(gA)
	_, err := idxA.Index(dir)
	require.NoError(t, err)

	bumpMtime(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc main() { helper(); helper() }\n\nfunc helper() {}\n")
	_, err = idxA.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	bumpMtime(t, filepath.Join(dir, "pkg", "util.go"),
		"package pkg\n\ntype Config struct{ Port int }\n\nfunc New() *Config { return &Config{} }\n\nfunc Reset(c *Config) {}\n")
	_, err = idxA.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(dir, "extra.go")))
	_, err = idxA.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	// Path B: a single cold index of the same final disk state.
	gB := graph.New()
	idxB := newTestIndexer(gB)
	_, err = idxB.Index(dir)
	require.NoError(t, err)

	assert.Equal(t, canonicalGraph(gB), canonicalGraph(gA),
		"incremental reindex must converge to the same graph as a full index")
}

// TestIncrementalReindex_FailedFileSurfacedAndRetried checks the
// failed-chunk replay surface: a stale file that cannot be indexed is
// reported on IndexResult.FailedFiles (after one in-pass retry), its
// mtime is left unrecorded so it stays stale, and a later pass
// recovers it once the obstruction clears.
func TestIncrementalReindex_FailedFileSurfacedAndRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("an unreadable-file test is meaningless as root")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.go"), "package main\n\nfunc OK() {}\n")
	bad := filepath.Join(dir, "bad.go")
	writeFile(t, bad, "package main\n\nfunc Bad() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Bad"))

	// Make bad.go unreadable and stale: the incremental pass discovers
	// it (stat works) but fails to read its content.
	require.NoError(t, os.Chmod(bad, 0o000))
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(bad, future, future))

	res, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	assert.Contains(t, res.FailedFiles, bad,
		"an unreadable stale file must be surfaced on FailedFiles")

	// Readable again: the file is still stale (its failed pass never
	// recorded an mtime), so the next incremental pass recovers it.
	require.NoError(t, os.Chmod(bad, 0o644))
	res2, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	assert.Empty(t, res2.FailedFiles, "the file indexes cleanly once readable")
	assert.NotEmpty(t, g.FindNodesByName("Bad"))
}

// seedSlashSpelledTodoRow plants the coverage-domain rows a pre-fix
// store holds on Windows: the builders re-spelled the extractor's
// relPath with forward slashes, so a subdirectory file's todo node and
// annotated edge were keyed by a spelling native eviction never sweeps.
// Returns the stale node ID.
func seedSlashSpelledTodoRow(g *graph.Graph, nativeRel string) string {
	slashRel := filepath.ToSlash(nativeRel)
	staleID := slashRel + "::todo:99"
	g.AddNode(&graph.Node{
		ID:        staleID,
		Kind:      graph.KindTodo,
		Name:      "todo:99",
		FilePath:  slashRel,
		StartLine: 99,
		EndLine:   99,
		Language:  "go",
		Meta:      map[string]any{"tag": "NOTE", "text": "stale spelling"},
	})
	g.AddEdge(&graph.Edge{
		From:     slashRel,
		To:       staleID,
		Kind:     graph.EdgeAnnotated,
		FilePath: slashRel,
		Line:     99,
		Origin:   graph.OriginASTResolved,
	})
	return staleID
}

// edgesTouching returns the graph's edges whose From or To equals id.
func edgesTouching(g graph.Store, id string) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.AllEdges() {
		if e != nil && (e.From == id || e.To == id) {
			out = append(out, e)
		}
	}
	return out
}

// TestIncrementalReindex_SweepsSlashSpelledCoverageRows: stores written
// before the coverage-domain builders preserved the extractor's path
// spelling hold todo rows keyed by the forward-slash spelling of
// subdirectory files. Replacing the file must sweep that twin spelling
// too, or the stale row shadows the file forever on Windows.
func TestIncrementalReindex_SweepsSlashSpelledCoverageRows(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	rel := filepath.Join("pkg", "util.go")
	writeFile(t, filepath.Join(dir, rel),
		"package pkg\n\n// TODO: original marker\nfunc Util() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	staleID := seedSlashSpelledTodoRow(g, rel)
	require.NotNil(t, g.GetNode(staleID))

	bumpMtime(t, filepath.Join(dir, rel),
		"package pkg\n\n// TODO: edited marker\nfunc Util() {}\n")
	_, err = idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	assert.Nil(t, g.GetNode(staleID),
		"the slash-spelled stale todo node must be swept on replacement")
	assert.Empty(t, edgesTouching(g, staleID),
		"the stale annotated edge must be swept with its node")
	assert.NotNil(t, g.GetNode(rel+"::todo:3"),
		"the fresh extraction's todo node rides the native spelling")
}

// TestIncrementalReindex_SweepsSlashSpelledCoverageRowsOnDelete is the
// delete-lane sibling: removing the file from disk must also sweep the
// twin-spelled stale rows.
func TestIncrementalReindex_SweepsSlashSpelledCoverageRowsOnDelete(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	rel := filepath.Join("pkg", "util.go")
	writeFile(t, filepath.Join(dir, rel),
		"package pkg\n\n// TODO: original marker\nfunc Util() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	staleID := seedSlashSpelledTodoRow(g, rel)
	require.NotNil(t, g.GetNode(staleID))

	require.NoError(t, os.Remove(filepath.Join(dir, rel)))
	_, err = idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	assert.Nil(t, g.GetNode(staleID),
		"the slash-spelled stale todo node must be swept on deletion")
	assert.Empty(t, edgesTouching(g, staleID),
		"the stale annotated edge must be swept with its node")
}

// TestIncrementalReindex_MerkleMode exercises the BLAKE3 Merkle change
// detector: a content edit is re-indexed, but a file merely touched
// (new mtime, identical content) is not — the content-addressed tree
// ignores the mtime false positive that the bare-mtime path would
// re-index needlessly.
func TestIncrementalReindex_MerkleMode(t *testing.T) {
	t.Setenv("GORTEX_MERKLE", "1")

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "edited.go"), "package main\n\nfunc Edited() {}\n")
	writeFile(t, filepath.Join(dir, "touched.go"), "package main\n\nfunc Touched() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Edited"))
	require.NotEmpty(t, g.FindNodesByName("Touched"))
	require.FileExists(t, filepath.Join(dir, ".gortex", "merkle.json"),
		"a full index in Merkle mode must persist a baseline tree")

	// Edit one file's content; touch the other without changing it.
	bumpMtime(t, filepath.Join(dir, "edited.go"),
		"package main\n\nfunc Edited() {}\n\nfunc AlsoEdited() {}\n")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "touched.go"), future, future))

	res, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	assert.NotEmpty(t, g.FindNodesByName("AlsoEdited"),
		"a content edit must be re-indexed under Merkle mode")
	assert.Equal(t, 1, res.StaleFileCount,
		"only the content-changed file is stale; a bare touch is not")
}
