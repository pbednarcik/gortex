package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type blockingAddBatchStore struct {
	graph.Store
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingAddBatchStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if s.armed.Load() {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	s.Store.AddBatch(nodes, edges)
}

func TestPreparedMetadataRefreshDoesNotStampWriteDuringCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")

	base := graph.New()
	store := &blockingAddBatchStore{
		Store: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(store, registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	initial := indexedFileMtime(t, idx, path)
	firstMtime := initial.Add(time.Second)
	secondMtime := initial.Add(2 * time.Second)
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 1 }\n")
	require.NoError(t, os.Chtimes(path, firstMtime, firstMtime))
	_, prepared := idx.prepareFileDelta(path)
	require.True(t, prepared)
	prior := base.GetFileNodes(idx.relKey(path))

	type refreshResult struct {
		applied bool
		fresh   bool
	}
	store.armed.Store(true)
	done := make(chan refreshResult, 1)
	go func() {
		_, applied, fresh := idx.applyPreparedMetadataRefresh(path, prior)
		done <- refreshResult{applied: applied, fresh: fresh}
	}()
	<-store.entered

	writeTestFile(t, path, "package main\n\nfunc Value() int { return 2 }\n")
	require.NoError(t, os.Chtimes(path, secondMtime, secondMtime))
	close(store.release)
	result := <-done
	require.True(t, result.applied)
	require.False(t, result.fresh)
	require.NotEqual(t, secondMtime.UnixNano(), idx.FileMtimes()[idx.relKey(path)])
}

func TestIncrementalBatchDoesNotStampWriteDuringCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Original() int { return 0 }\n")

	base := graph.New()
	store := &blockingAddBatchStore{
		Store: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(store, registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	initial := indexedFileMtime(t, idx, path)
	firstMtime := initial.Add(time.Second)
	secondMtime := initial.Add(2 * time.Second)
	writeTestFile(t, path, "package main\n\nfunc First() int { return 1 }\n")
	require.NoError(t, os.Chtimes(path, firstMtime, firstMtime))

	type chunkResult struct {
		consumed      int
		reparsed      []string
		failed        []string
		versionChange []string
	}
	store.armed.Store(true)
	done := make(chan chunkResult, 1)
	go func() {
		consumed, _, reparsed, failed, versionChange := idx.reindexIncrementalChunk([]string{path}, nil, false)
		done <- chunkResult{
			consumed: consumed, reparsed: reparsed, failed: failed, versionChange: versionChange,
		}
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("incremental batch did not reach graph commit")
	}

	writeTestFile(t, path, "package main\n\nfunc Second() int { return 2 }\n")
	require.NoError(t, os.Chtimes(path, secondMtime, secondMtime))
	close(store.release)
	result := <-done
	require.Equal(t, 1, result.consumed)
	require.Contains(t, result.failed, path)
	require.Contains(t, result.versionChange, path)
	require.NotContains(t, result.reparsed, path)
	require.NotEqual(t, secondMtime.UnixNano(), idx.FileMtimes()[idx.relKey(path)])

	store.armed.Store(false)
	_, _, reparsed, failed, versionChange := idx.reindexIncrementalChunk([]string{path}, nil, false)
	require.Empty(t, failed)
	require.Empty(t, versionChange)
	require.Contains(t, reparsed, path)
	require.Equal(t, secondMtime.UnixNano(), idx.FileMtimes()[idx.relKey(path)])
	assertFileNodeName(t, base, idx.relKey(path), "Second")
}

func TestIncrementalManifestDoesNotStampWriteDuringCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	writeTestFile(t, path, "module example.com/app\n\nrequire example.com/original v1.0.0\n")

	base := graph.New()
	store := &blockingAddBatchStore{
		Store: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)
	idx.extractOneModuleManifest("go.mod", modules.ParseGoMod, readGoModModulePath)
	idx.recordFileMtime("go.mod", path)

	initial := indexedFileMtime(t, idx, path)
	firstMtime := initial.Add(time.Second)
	secondMtime := initial.Add(2 * time.Second)
	writeTestFile(t, path, "module example.com/app\n\nrequire example.com/first v1.0.0\n")
	require.NoError(t, os.Chtimes(path, firstMtime, firstMtime))

	type manifestResult struct {
		failed []string
	}
	store.armed.Store(true)
	done := make(chan manifestResult, 1)
	go func() {
		_, failed := idx.refreshIncrementalContractManifests([]string{path})
		done <- manifestResult{failed: failed}
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("incremental manifest did not reach graph commit")
	}

	writeTestFile(t, path, "module example.com/app\n\nrequire example.com/second v2.0.0\n")
	require.NoError(t, os.Chtimes(path, secondMtime, secondMtime))
	close(store.release)
	result := <-done
	require.Contains(t, result.failed, path)
	require.NotEqual(t, secondMtime.UnixNano(), idx.FileMtimes()[idx.relKey(path)])
	require.NotNil(t, base.GetNode(modules.ModuleNodeID("go", "example.com/first", "v1.0.0")))
	require.Nil(t, base.GetNode(modules.ModuleNodeID("go", "example.com/second", "v2.0.0")))

	store.armed.Store(false)
	_, failed := idx.refreshIncrementalContractManifests([]string{path})
	require.Empty(t, failed)
	require.Equal(t, secondMtime.UnixNano(), idx.FileMtimes()[idx.relKey(path)])
	require.Nil(t, base.GetNode(modules.ModuleNodeID("go", "example.com/first", "v1.0.0")))
	require.NotNil(t, base.GetNode(modules.ModuleNodeID("go", "example.com/second", "v2.0.0")))
}

func assertFileNodeName(t *testing.T, store graph.Store, graphPath, name string) {
	t.Helper()
	for _, node := range store.GetFileNodes(graphPath) {
		if node != nil && node.Name == name {
			return
		}
	}
	t.Fatalf("file %q has no node named %q", graphPath, name)
}
