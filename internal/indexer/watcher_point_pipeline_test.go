package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func TestWatcherPointPipelineForcesReceiptCheckedEqualMtime(t *testing.T) {
	dir, idx, watcher := inertTestWatcher(t, "main.go", "package main\n\nfunc Value() int { return 0 }\n")
	path := filepath.Join(dir, "main.go")
	mtime := time.Unix(0, idx.FileMtimes()[idx.relKey(path)])

	var callbackPath string
	watcher.OnSymbolChange(func(filePath string, _, _ []*graph.Node) {
		callbackPath = filePath
	})
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 1 }\nfunc Added() {}\n")
	require.NoError(t, os.Chtimes(path, mtime, mtime))

	require.NoError(t, watcher.patchGraphAfterReceiptCheck(path, ChangeModified))
	require.NotEmpty(t, idx.graph.FindNodesByName("Added"))
	require.Equal(t, idx.RelKey(path), callbackPath)

	event := waitForEvent(t, watcher, time.Second)
	require.Equal(t, path, event.FilePath)
	require.Equal(t, ChangeModified, event.Kind)
	require.Equal(t, "structural", event.Classification)
}

func TestWatcherPointPipelineDeletesOwnedFileWithoutMtime(t *testing.T) {
	dir, idx, watcher := inertTestWatcher(t, "main.go", "package main\n\nfunc Value() int { return 0 }\n")
	path := filepath.Join(dir, "main.go")
	graphPath := idx.prefixPath(idx.relKey(path))
	require.NotEmpty(t, idx.graph.GetFileNodes(graphPath))

	idx.mtimeMu.Lock()
	delete(idx.fileMtimes, idx.relKey(path))
	idx.mtimeMu.Unlock()
	require.NoError(t, os.Remove(path))

	require.NoError(t, watcher.patchGraph(path, ChangeDeleted))
	require.Empty(t, idx.graph.GetFileNodes(graphPath))
	event := waitForEvent(t, watcher, time.Second)
	require.Equal(t, ChangeDeleted, event.Kind)
	require.Positive(t, event.NodesRemoved)
	require.Len(t, watcher.History(), 1)

	// A second absent notification has neither graph rows nor sidecar ownership
	// and therefore is an identity duplicate rather than another deletion.
	require.NoError(t, watcher.patchGraph(path, ChangeDeleted))
	require.Len(t, watcher.History(), 1)
	requireNoWatcherEvent(t, watcher)
}

func TestWatcherPointPipelineFreezesMetadataCallbackBeforeImage(t *testing.T) {
	dir, _, watcher := inertTestWatcher(t, "main.go", "package main\n\n// old docs\nfunc Value() int { return 0 }\n")
	path := filepath.Join(dir, "main.go")

	var oldSymbols, newSymbols []*graph.Node
	watcher.OnSymbolChange(func(_ string, oldPayload, newPayload []*graph.Node) {
		oldSymbols = oldPayload
		newSymbols = newPayload
	})
	writeTestFile(t, path, "package main\n\n// new docs\nfunc Value() int { return 0 }\n")
	require.NoError(t, watcher.patchGraphAfterReceiptCheck(path, ChangeModified))

	require.NotEmpty(t, oldSymbols)
	require.NotEmpty(t, newSymbols)
	require.NotSame(t, oldSymbols[0], newSymbols[0])
	require.Contains(t, oldSymbols[0].Meta, "doc")
	require.Equal(t, "old docs", oldSymbols[0].Meta["doc"])
	require.Equal(t, "new docs", newSymbols[0].Meta["doc"])
	event := waitForEvent(t, watcher, time.Second)
	require.Equal(t, "metadata_only", event.Classification)
}

func TestMultiWatcherPointPublishesAfterCurrentRawTail(t *testing.T) {
	const prefix = "repo"
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")

	g := graph.New()
	oldIdx := newTestIndexer(g)
	oldIdx.SetRepoPrefix(prefix)
	oldIdx.SetRootPath(root)
	_, err := oldIdx.Index(root)
	require.NoError(t, err)

	multi := NewMultiIndexer(g, newTestRegistry(), nil, nil, zap.NewNop())
	multi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix, RootPath: root}
	multi.indexers[prefix] = oldIdx
	mw, err := NewMultiWatcher(multi, map[string]config.WatchConfig{
		prefix: {Enabled: false},
	}, zap.NewNop())
	require.NoError(t, err)
	watcher := mw.watchers[prefix]
	require.NotNil(t, watcher)

	newIdx := newTestIndexer(g)
	newIdx.SetRepoPrefix(prefix)
	newIdx.SetRootPath(root)
	newIdx.attachRepositoryMutationCoordinator(multi.repositoryMutationCoordinator(prefix))
	multi.mu.Lock()
	multi.indexers[prefix] = newIdx
	multi.mu.Unlock()

	oldTail := make(chan struct{}, 1)
	oldIdx.incrementalCatchupHook = func(string, []string) {
		select {
		case oldTail <- struct{}{}:
		default:
		}
	}
	derivedEntered := make(chan struct{})
	releaseDerived := make(chan struct{})
	var derivedOnce sync.Once
	var phasesMu sync.Mutex
	var phases []string
	newIdx.incrementalCatchupHook = func(phase string, _ []string) {
		phasesMu.Lock()
		phases = append(phases, phase)
		phasesMu.Unlock()
		if phase == "derived" {
			derivedOnce.Do(func() { close(derivedEntered) })
			<-releaseDerived
		}
	}

	callback := make(chan string, 1)
	watcher.OnSymbolChange(func(filePath string, _, _ []*graph.Node) {
		callback <- filePath
	})
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 1 }\nfunc Added() {}\n")
	pointDone := make(chan error, 1)
	go func() {
		pointDone <- watcher.patchGraphAfterReceiptCheck(path, ChangeModified)
	}()

	select {
	case <-derivedEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Multi raw point tail did not reach derived catch-up")
	}
	phasesMu.Lock()
	observed := append([]string(nil), phases...)
	phasesMu.Unlock()
	require.Contains(t, observed, "resolve")
	require.Contains(t, observed, "derived")
	select {
	case <-oldTail:
		t.Fatal("retired Indexer executed the point tail")
	default:
	}
	require.Empty(t, watcher.History(), "event history published before the Multi tail completed")
	select {
	case event := <-watcher.Events():
		t.Fatalf("event published before the Multi tail completed: %+v", event)
	default:
	}
	select {
	case callbackPath := <-callback:
		t.Fatalf("callback ran before the repository lane released: %s", callbackPath)
	default:
	}

	close(releaseDerived)
	select {
	case err := <-pointDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("point mutation did not finish after derived catch-up")
	}
	require.Equal(t, newIdx.RelKey(path), <-callback)
	event := waitForEvent(t, watcher, time.Second)
	require.Equal(t, ChangeModified, event.Kind)
	require.NotEmpty(t, newIdx.graph.FindNodesByName("Added"))
}

func TestPointMutationClassification(t *testing.T) {
	tests := []struct {
		name           string
		kind           ChangeKind
		plan           DerivedInvalidationPlan
		prior          fileDeltaFingerprints
		fresh          fileDeltaFingerprints
		classification string
	}{
		{name: "inert", kind: ChangeModified, plan: DerivedInvalidationPlan{InertFiles: 1}, classification: "inert"},
		{name: "metadata", kind: ChangeModified, plan: DerivedInvalidationPlan{MetadataOnlyFiles: 1}, classification: "metadata_only"},
		{name: "artifact", kind: ChangeModified, prior: fileDeltaFingerprints{core: "same"}, fresh: fileDeltaFingerprints{core: "same"}, classification: "artifact_only"},
		{name: "structural", kind: ChangeModified, prior: fileDeltaFingerprints{core: "before"}, fresh: fileDeltaFingerprints{core: "after"}, classification: "structural"},
		{name: "delete", kind: ChangeDeleted, plan: DerivedInvalidationPlan{InertFiles: 1}, classification: "structural"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.classification, pointMutationClassification(test.kind, test.plan, test.prior, test.fresh))
		})
	}
}

func TestIncrementalTopologyChangedExcludesOnlySafePointPlans(t *testing.T) {
	tests := []struct {
		name   string
		result *IndexResult
		want   bool
	}{
		{name: "nil", result: nil, want: false},
		{name: "inert", result: &IndexResult{StaleFileCount: 1, DerivedInvalidation: DerivedInvalidationPlan{InertFiles: 1}}, want: false},
		{name: "metadata", result: &IndexResult{StaleFileCount: 1, DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"main.go"}, MetadataOnlyFiles: 1}}, want: false},
		{name: "metadata topology frontier", result: &IndexResult{StaleFileCount: 1, DerivedInvalidation: DerivedInvalidationPlan{Flags: DerivedInvalidationFlags(1), MetadataOnlyFiles: 1}}, want: true},
		{name: "structural", result: &IndexResult{StaleFileCount: 1}, want: true},
		{name: "artifact", result: &IndexResult{StaleFileCount: 1, DerivedInvalidation: DerivedInvalidationPlan{BodyOnlyFiles: 1}}, want: true},
		{name: "delete", result: &IndexResult{DeletedFileCount: 1}, want: true},
		{name: "full", result: &IndexResult{FullRetrack: true}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, incrementalTopologyChanged(test.result))
		})
	}
}
