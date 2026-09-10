package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic"
)

type repoStatsCountingStore struct {
	graph.Store
	repoStatsCalls int
}

func (s *repoStatsCountingStore) RepoStats() map[string]graph.GraphStats {
	s.repoStatsCalls++
	return s.Store.RepoStats()
}

func TestRunDeferredPassesAllSkipsIdleRepositoriesBeforeStatsScan(t *testing.T) {
	store := &repoStatsCountingStore{Store: graph.New()}
	mi := &MultiIndexer{
		graph: store,
		indexers: map[string]*Indexer{
			"idle-a": {},
			"idle-b": {},
		},
	}

	if scheduled := mi.RunDeferredPassesAllResult(context.Background()).EnrichScheduled; scheduled != 0 {
		t.Fatalf("scheduled enrichments = %d, want 0", scheduled)
	}
	if store.repoStatsCalls != 0 {
		t.Fatalf("idle deferred pass called RepoStats %d times, want 0", store.repoStatsCalls)
	}
}

type deferredBatchProvider struct {
	batchCalls  int
	singleCalls int
	fullCalls   int
	batches     [][]string
}

func (p *deferredBatchProvider) Name() string        { return "deferred-batch-test" }
func (p *deferredBatchProvider) Languages() []string { return []string{"go"} }
func (p *deferredBatchProvider) Available() bool     { return true }
func (p *deferredBatchProvider) Close() error        { return nil }

func (p *deferredBatchProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	p.fullCalls++
	return &semantic.EnrichResult{Provider: p.Name(), Language: "go"}, nil
}

func (p *deferredBatchProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	p.singleCalls++
	return &semantic.EnrichResult{Provider: p.Name(), Language: "go"}, nil
}

func (p *deferredBatchProvider) EnrichFiles(_ graph.Store, _ string, _ string, files []string) (*semantic.EnrichResult, error) {
	p.batchCalls++
	p.batches = append(p.batches, append([]string(nil), files...))
	return &semantic.EnrichResult{Provider: p.Name(), Language: "go"}, nil
}

type deferredGoFixture struct {
	relPath string
	before  string
	after   string
}

func writeDeferredGoFixture(t *testing.T, root string, fixture deferredGoFixture, after bool) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(fixture.relPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := fixture.before
	if after {
		content = fixture.after
	}
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestDeferredIncrementalGoFrontierUsesOneBatchAndNoRepoPass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []deferredGoFixture
	}{
		{
			name: "same package",
			files: []deferredGoFixture{
				{relPath: "pkg/a.go", before: "package pkg\nfunc A() int { return 1 }\n", after: "package pkg\nfunc A() string { return \"a\" }\n"},
				{relPath: "pkg/b.go", before: "package pkg\nfunc B() int { return 2 }\n", after: "package pkg\nfunc B() string { return \"b\" }\n"},
			},
		},
		{
			name: "different packages",
			files: []deferredGoFixture{
				{relPath: "pkg/a.go", before: "package pkg\nfunc A() int { return 1 }\n", after: "package pkg\nfunc A() string { return \"a\" }\n"},
				{relPath: "other/b.go", before: "package other\nfunc B() int { return 2 }\n", after: "package other\nfunc B() string { return \"b\" }\n"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)

			store := graph.New()
			registry := parser.NewRegistry()
			languages.RegisterAll(registry)
			idx := New(store, registry, config.IndexConfig{}, zap.NewNop())
			idx.rootPath = root
			idx.deferGlobalPasses.Store(true)

			provider := &deferredBatchProvider{}
			manager := semantic.NewManager(semantic.Config{
				Enabled:       true,
				EnrichOnWatch: true,
				Providers: []semantic.ProviderConfig{{
					Name: provider.Name(), Languages: provider.Languages(), Priority: 1, Enabled: true,
				}},
			}, zap.NewNop())
			manager.RegisterProvider(provider)
			idx.SetSemanticManager(manager)

			paths := make([]string, 0, len(tc.files))
			for _, fixture := range tc.files {
				path := writeDeferredGoFixture(t, root, fixture, false)
				paths = append(paths, path)
				require.NoError(t, idx.IndexFile(path))
			}
			*provider = deferredBatchProvider{}

			future := time.Now().Add(time.Minute)
			for i, fixture := range tc.files {
				writeDeferredGoFixture(t, root, fixture, true)
				require.NoError(t, os.Chtimes(paths[i], future, future))
			}

			result, err := idx.IncrementalReindexPaths(root, paths)
			require.NoError(t, err)
			require.Equal(t, len(paths), result.StaleFileCount)
			assert.Zero(t, provider.batchCalls, "deferred mode must not enrich inline")
			assert.Zero(t, provider.singleCalls)
			assert.Zero(t, provider.fullCalls)

			idx.runDeferredEnrich()
			require.Equal(t, 1, provider.batchCalls)
			assert.Zero(t, provider.singleCalls)
			assert.Zero(t, provider.fullCalls, "known Go files must not fall back to repo enrichment")
			require.Len(t, provider.batches, 1)
			// A deferred batch carries graph paths, not OS paths: the indexer
			// keys them with pathkey.Normalize(filepath.ToSlash(rel)), so the
			// fixture's own '/' spelling is the expectation verbatim on every
			// platform. FromSlash here would want `pkg\a.go` on Windows and
			// match nothing.
			wantGraphPaths := make([]string, 0, len(tc.files))
			for _, fixture := range tc.files {
				wantGraphPaths = append(wantGraphPaths, fixture.relPath)
			}
			assert.ElementsMatch(t, wantGraphPaths, provider.batches[0])
			assert.False(t, idx.pendingEnrich.Load())
		})
	}
}

func TestDeferredIncrementalGoFrontierDoesNotWriteWholeRepoMarker(t *testing.T) {
	repo := commitGitRepo(t)
	store := openTestSqlite(t)

	provider := &deferredBatchProvider{}
	manager := semantic.NewManager(semantic.Config{
		Enabled: true,
		Providers: []semantic.ProviderConfig{{
			Name: provider.Name(), Languages: provider.Languages(), Priority: 1, Enabled: true,
		}},
	}, zap.NewNop())
	manager.RegisterProvider(provider)

	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(manager)
	store.AddNode(&graph.Node{
		ID: "r/main.go", Kind: graph.KindFile, Name: "main.go",
		FilePath: "r/main.go", Language: "go", RepoPrefix: "r",
	})
	idx.markPendingEnrichFiles([]string{"r/main.go"})

	idx.runDeferredEnrich()

	require.Equal(t, 1, provider.batchCalls)
	assert.False(t, idx.pendingEnrich.Load(), "the represented file generation was drained")
	sha := repoHead(repo)
	require.NotEmpty(t, sha)
	current, persisted := manager.RepoEnrichmentMarkerState(store, "r", sha)
	assert.True(t, persisted)
	assert.False(t, current,
		"a file-bounded pass must never claim whole-repository enrichment completion")
}
