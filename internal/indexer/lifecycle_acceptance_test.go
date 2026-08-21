package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
)

type lifecycleFailingBulkStore struct {
	graph.Store
}

func (*lifecycleFailingBulkStore) BeginBulkLoad() {}

func (*lifecycleFailingBulkStore) FlushBulk() error {
	return errors.New("injected lifecycle bulk flush failure")
}

func newLifecycleTestMultiIndexer(t *testing.T) *MultiIndexer {
	t.Helper()
	return NewMultiIndexer(
		graph.New(),
		newTestRegistry(),
		search.NewNull(),
		newTestConfigManager(t),
		zap.NewNop(),
	)
}

func closeLifecycleTestMultiIndexer(t *testing.T, mi *MultiIndexer) {
	t.Helper()
	require.NoError(t, mi.Close(context.Background()))
}

func TestLifecycleIndexRepoClosesReplacedIndexer(t *testing.T) {
	t.Setenv(crashWorkerEnv, "1")
	repo := setupRepoDir(t, "repo")
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)
	old := mi.GetIndexer("repo")
	require.NotNil(t, old)
	pool, _ := old.sharedParsePool()
	require.NotNil(t, pool, "replacement precondition requires a real crashpool")

	_, err = mi.IndexRepo("repo")
	require.NoError(t, err)
	require.NotSame(t, old, mi.GetIndexer("repo"))
	require.Nil(t, old.parsePool, "replacement must release the old crashpool")
	_, err = old.ExtractBuffer("go", "after.go", []byte("package after\n"))
	require.ErrorIs(t, err, ErrIndexerClosed)
}

func TestLifecycleFailedReplacementClosesCandidateIndexerAndPool(t *testing.T) {
	t.Setenv(crashWorkerEnv, "1")
	repo := setupRepoDir(t, "repo")
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)
	old := mi.GetIndexer("repo")
	require.NotNil(t, old)
	mi.graph = &lifecycleFailingBulkStore{Store: mi.graph}

	var candidate *Indexer
	mi.newIndexer = func(g graph.Store, reg *parser.Registry, cfg config.IndexConfig, logger *zap.Logger) *Indexer {
		candidate = New(g, reg, cfg, logger)
		candidate.SetRootPath(repo)
		pool, _ := candidate.sharedParsePool()
		require.NotNil(t, pool, "failure precondition requires a real crashpool")
		return candidate
	}

	_, err = mi.IndexRepo("repo")
	require.Error(t, err)
	require.NotNil(t, candidate)
	require.Same(t, old, mi.GetIndexer("repo"), "failed replacement must preserve the live Indexer")
	require.Nil(t, candidate.parsePool, "failed replacement must release the candidate crashpool")
	_, err = candidate.ExtractBuffer("go", "after.go", []byte("package after\n"))
	require.ErrorIs(t, err, ErrIndexerClosed)

	_, err = old.ExtractBuffer("go", "still-live.go", []byte("package live\n"))
	require.NoError(t, err, "failed replacement must not close the live Indexer")
}

func TestLifecycleUntrackClosesIndexerAndCrashPool(t *testing.T) {
	t.Setenv(crashWorkerEnv, "1")
	repo := setupRepoDir(t, "repo")
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)
	owned := mi.GetIndexer("repo")
	require.NotNil(t, owned)
	pool, _ := owned.sharedParsePool()
	require.NotNil(t, pool, "untrack precondition requires a real crashpool")

	nodesRemoved, _ := mi.UntrackRepo("repo")
	require.Positive(t, nodesRemoved)
	require.Nil(t, mi.GetIndexer("repo"))
	require.Nil(t, owned.parsePool, "untrack must release the repository crashpool")
	_, err = owned.ExtractBuffer("go", "after.go", []byte("package after\n"))
	require.ErrorIs(t, err, ErrIndexerClosed)
}

// laneUntrackProvider is a minimal semantic.Provider + BackgroundEnricher
// whose drain blocks until cancelled, so the test can hold a lane drain in
// flight across an UntrackRepo call.
type laneUntrackProvider struct {
	drained chan string
	block   chan struct{}
	ctxErr  chan error
}

func (p *laneUntrackProvider) Name() string        { return "lane-untrack" }
func (p *laneUntrackProvider) Languages() []string { return []string{"go"} }
func (p *laneUntrackProvider) Available() bool     { return true }
func (p *laneUntrackProvider) Close() error        { return nil }
func (p *laneUntrackProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	return &semantic.EnrichResult{}, nil
}
func (p *laneUntrackProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	return nil, nil
}
func (p *laneUntrackProvider) HasBackgroundWork(graph.Store, string) bool { return true }
func (p *laneUntrackProvider) InvalidateBackground(graph.Store, string)   {}
func (p *laneUntrackProvider) EnrichBackground(ctx context.Context, _ graph.Store, repo, _ string) (*semantic.EnrichResult, error) {
	p.drained <- repo
	select {
	case <-p.block:
		return &semantic.EnrichResult{}, nil
	case <-ctx.Done():
		p.ctxErr <- ctx.Err()
		return nil, ctx.Err()
	}
}

// Untracking a repository must also cancel and purge its background lane
// work: a pending or in-flight drain otherwise outlives the graph purge,
// spawns a server at the abandoned root, and writes the removed repo's
// nodes back into the store.
func TestLifecycleUntrackCancelsBackgroundLane(t *testing.T) {
	repo := setupRepoDir(t, "repo")
	mi := newLifecycleTestMultiIndexer(t)
	t.Cleanup(func() { closeLifecycleTestMultiIndexer(t, mi) })

	_, err := mi.TrackRepo(config.RepoEntry{Path: repo, Name: "repo"})
	require.NoError(t, err)

	bg := &laneUntrackProvider{
		drained: make(chan string, 2),
		block:   make(chan struct{}),
		ctxErr:  make(chan error, 1),
	}
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	mgr.RegisterProvider(bg)
	t.Cleanup(func() { require.NoError(t, mgr.Close()) })
	mi.SetSemanticManager(mgr)

	// The census enqueue is cooldown-free — it starts the drain the test
	// then holds in flight across the untrack.
	mgr.StartBackgroundLane(context.Background(), mi.Graph(), map[string]string{"repo": repo})
	select {
	case r := <-bg.drained:
		require.Equal(t, "repo", r)
	case <-time.After(2 * time.Second):
		t.Fatal("census did not start the drain")
	}

	done := make(chan struct{})
	go func() { mi.UntrackRepo("repo"); close(done) }()
	select {
	case err := <-bg.ctxErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("untrack did not cancel the in-flight background drain")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("UntrackRepo did not return")
	}
	select {
	case r := <-bg.drained:
		t.Fatalf("untracked repo drained again: %q", r)
	case <-time.After(150 * time.Millisecond):
	}
}
