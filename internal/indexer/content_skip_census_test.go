package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"
)

func newContentCensusFixture(t *testing.T, imageLimit int64) *sizeCensusFixture {
	t.Helper()
	f := &sizeCensusFixture{t: t, root: t.TempDir(), dbPath: filepath.Join(t.TempDir(), "graph.sqlite")}
	t.Cleanup(f.close)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "main.go"), []byte("package fixture\nfunc Main() {}\n"), 0600))
	writeSizeCensusGIF(t, filepath.Join(f.root, "demo.gif"), 512)
	info, err := os.Stat(filepath.Join(f.root, "demo.gif"))
	require.NoError(t, err)
	f.mtime = info.ModTime()
	f.open(4096) // the content cap, never MaxFileSize, rejects this GIF
	f.idx.config.Content = config.ContentAdmissionConfig{MaxImageBytes: imageLimit}
	_, err = f.idx.IndexCtx(context.Background(), f.root)
	require.NoError(t, err)
	f.idx.RunDeferredPasses(context.Background())
	return f
}

func reopenContentCensusFixture(f *sizeCensusFixture, imageLimit int64) {
	f.reopen(4096)
	f.idx.config.Content = config.ContentAdmissionConfig{MaxImageBytes: imageLimit}
}

func TestContentSkipCensusColdGIFRepeatedRestartNoop(t *testing.T) {
	f := newContentCensusFixture(t, 128)
	stub := f.fileNode()
	require.Equal(t, true, stub.Meta["skipped_due_to_content"])
	require.Equal(t, skipReasonLargeImage, stub.Meta["skip_reason"])
	require.EqualValues(t, 512, stub.Meta["file_size_bytes"])
	require.NotEqual(t, true, stub.Meta["skipped_due_to_size"])
	require.Less(t, int64(512), f.idx.config.MaxFileSize)
	seeded := []*graph.Edge{
		{From: "repo/main.go", To: "repo/demo.gif", Kind: "co_change", FilePath: "repo/main.go"},
		{From: "repo/demo.gif", To: "repo/main.go", Kind: "co_change", FilePath: "repo/demo.gif"},
	}
	f.store.AddBatch(nil, seeded)
	edges := f.store.GetOutEdgesByNodeIDs([]string{"repo/main.go", "repo/demo.gif"})
	for _, want := range seeded {
		found := 0
		for _, got := range edges[want.From] {
			if got.From == want.From && got.To == want.To && got.Kind == want.Kind {
				found++
			}
		}
		require.Equal(t, 1, found, "incoming/outgoing co_change prerequisite")
	}
	before := sizeCensusGraphRows(t, f.store)
	assert.Equal(t, f.mtime.UnixNano(), f.store.LoadFileMtimes("repo")["demo.gif"], "cold content stub needs a published version receipt")
	for restart := 0; restart < 2; restart++ {
		reopenContentCensusFixture(f, 128)
		changed, deleted, detected, err := f.idx.changedSinceMtimesCensus(f.root)
		require.NoError(t, err)
		assert.Empty(t, changed)
		require.Empty(t, deleted)
		require.Equal(t, 2, detected)
		result, err := f.idx.incrementalReindexPathsMode(f.root, []string{"main.go", "demo.gif"}, incrementalPathMode{})
		require.NoError(t, err)
		assert.Zero(t, result.StaleFileCount)
		assert.Equal(t, before, sizeCensusGraphRows(t, f.store), "unchanged content-policy asset must not lose co_change edges")
	}
}

func TestContentSkipCensusEqualMtimePolicyChanges(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after int64
		stale, skip   bool
	}{
		{"cap_increase_admits", 128, 1024, true, false},
		{"strict_boundary_admits", 128, 512, true, false},
		{"cap_decrease_skips", 1024, 128, true, true},
		{"same_rejection_no_work", 128, 256, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContentCensusFixture(t, tc.before)
			f.establishKnownReceipt() // isolate policy comparison from missing cold receipt
			reopenContentCensusFixture(f, tc.after)
			changed, deleted, detected, err := f.idx.changedSinceMtimesCensus(f.root)
			require.NoError(t, err)
			require.Empty(t, deleted)
			require.Equal(t, 2, detected)
			if tc.stale {
				require.Equal(t, []string{"demo.gif"}, changed)
				f.applyCensus(changed)
			} else {
				require.Empty(t, changed)
			}
			node := f.fileNode()
			if tc.skip {
				require.Equal(t, true, node.Meta["skipped_due_to_content"])
				require.Equal(t, skipReasonLargeImage, node.Meta["skip_reason"])
			} else {
				require.NotEqual(t, true, node.Meta["skipped_due_to_content"])
				found := false
				for _, candidate := range f.store.GetRepoNodes("repo") {
					if candidate.FilePath == "repo/demo.gif" && candidate.Kind != graph.KindFile {
						found = true
					}
				}
				require.True(t, found, "eligible real GIF must be parsed, not merely lose a flag")
			}
			reopenContentCensusFixture(f, tc.after)
			f.census(nil, nil, 2)
		})
	}
}

type contentCensusReadSpy struct {
	graph.Store
	nodes   map[string]*graph.Node
	batches [][]string
}

func (s *contentCensusReadSpy) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.batches = append(s.batches, append([]string(nil), ids...))
	result := make(map[string]*graph.Node)
	for _, id := range ids {
		if node := s.nodes[id]; node != nil {
			result[id] = node
		}
	}
	return result
}

func TestContentSkipHelperScopeAndUntrackedPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*graph.Node)
		stale bool
	}{
		{"valid", func(*graph.Node) {}, false},
		{"untracked_precedes_content_rejection", func(n *graph.Node) { n.Meta["skip_reason"] = skipReasonUntrackedAsset }, false},
		{"wrong_repo", func(n *graph.Node) { n.RepoPrefix = "sibling" }, true},
		{"wrong_id", func(n *graph.Node) { n.ID = "sibling/demo.gif" }, true},
		{"wrong_path", func(n *graph.Node) { n.FilePath = "repo/other.gif" }, true},
		{"wrong_kind", func(n *graph.Node) { n.Kind = graph.KindFunction }, true},
		{"wrong_reason", func(n *graph.Node) { n.Meta["skip_reason"] = skipReasonLargeData }, true},
		{"wrong_size", func(n *graph.Node) { n.Meta["file_size_bytes"] = int64(511) }, true},
		{"false_marker", func(n *graph.Node) { n.Meta["skipped_due_to_content"] = false }, true},
		{"string_marker", func(n *graph.Node) { n.Meta["skipped_due_to_content"] = "true" }, true},
		{"foreign_untracked_not_exempt", func(n *graph.Node) { n.RepoPrefix = "sibling"; n.Meta["skip_reason"] = skipReasonUntrackedAsset }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContentCensusFixture(t, 128)
			node := *f.fileNode()
			node.Meta = map[string]any{"skipped_due_to_content": true, "skip_reason": skipReasonLargeImage, "file_size_bytes": int64(512)}
			tc.alter(&node)
			spy := &contentCensusReadSpy{Store: f.store, nodes: map[string]*graph.Node{"repo/demo.gif": &node}}
			f.idx.graph = spy
			got := f.idx.staleContentPolicyFiles(f.idx.newContentAdmissionGate(), []contentPolicyCensusCandidate{{"demo.gif", node.Language, 512}})
			if tc.stale {
				require.Equal(t, []string{"demo.gif"}, got)
			} else {
				require.Empty(t, got)
			}
			require.Equal(t, [][]string{{"repo/demo.gif"}}, spy.batches)
		})
	}
}

func TestContentSkipHelperAssetClassesAndPolicyToggle(t *testing.T) {
	f := newContentCensusFixture(t, 128)
	for _, tc := range []struct {
		name   string
		class  parser.AssetClass
		reason string
		after  *contentAdmissionGate
		stale  bool
	}{
		{"document_cap_increase", parser.AssetDocument, skipReasonLargeDocument, &contentAdmissionGate{docLimit: 1024, docCapped: true}, true},
		{"data_enabled", parser.AssetData, skipReasonVectorData, &contentAdmissionGate{indexData: true}, true},
		{"data_same_disabled", parser.AssetData, skipReasonVectorData, &contentAdmissionGate{}, false},
		{"data_rejection_reason_changes", parser.AssetData, skipReasonVectorData, &contentAdmissionGate{indexData: true, dataLimit: 128, dataCapped: true}, true},
		{"image_same_rejection", parser.AssetImage, skipReasonLargeImage, &contentAdmissionGate{imageLimit: 256, imageCapped: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := tc.after
			gate.classes = map[string]parser.AssetClass{"asset": tc.class}
			require.True(t, contentPolicyCensusAsset(gate, "asset"))
			require.False(t, contentPolicyCensusAsset(gate, "go"))
			node := &graph.Node{ID: "repo/demo.gif", Kind: graph.KindFile, FilePath: "repo/demo.gif", RepoPrefix: "repo", Meta: map[string]any{"skipped_due_to_content": true, "skip_reason": tc.reason, "file_size_bytes": int64(512)}}
			f.idx.graph = &contentCensusReadSpy{Store: f.store, nodes: map[string]*graph.Node{node.ID: node}}
			got := f.idx.staleContentPolicyFiles(gate, []contentPolicyCensusCandidate{{"demo.gif", "asset", 512}})
			if tc.stale {
				require.Equal(t, []string{"demo.gif"}, got)
			} else {
				require.Empty(t, got)
			}
		})
	}
	require.False(t, contentPolicyCensusAsset(nil, "asset"))
}

func TestContentSkipHelperEmptyRepoAndSelectedGeneration(t *testing.T) {
	t.Run("empty_repo", func(t *testing.T) {
		f := newContentCensusFixture(t, 128)
		lang := f.fileNode().Language
		f.idx.SetRepoPrefix("")
		node := &graph.Node{ID: "demo.gif", Kind: graph.KindFile, FilePath: "demo.gif", Meta: map[string]any{"skipped_due_to_content": true, "skip_reason": skipReasonLargeImage, "file_size_bytes": int64(512)}}
		spy := &contentCensusReadSpy{Store: f.store, nodes: map[string]*graph.Node{node.ID: node}}
		f.idx.graph = spy
		require.Empty(t, f.idx.staleContentPolicyFiles(f.idx.newContentAdmissionGate(), []contentPolicyCensusCandidate{{"demo.gif", lang, 512}}))
		require.Equal(t, [][]string{{"demo.gif"}}, spy.batches)
	})
	t.Run("other_generation", func(t *testing.T) {
		f := newContentCensusFixture(t, 128)
		require.Equal(t, true, f.fileNode().Meta["skipped_due_to_content"])
		lang := f.fileNode().Language
		f.close()
		db, err := sql.Open("sqlite", f.dbPath)
		require.NoError(t, err)
		result, err := db.Exec(`UPDATE nodes SET view_gen = 7 WHERE id = ?`, "repo/demo.gif")
		require.NoError(t, err)
		rows, err := result.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, rows)
		require.NoError(t, db.Close())
		reopenContentCensusFixture(f, 128)
		require.Nil(t, f.store.GetNodesByIDs([]string{"repo/demo.gif"})["repo/demo.gif"])
		require.Equal(t, []string{"demo.gif"}, f.idx.staleContentPolicyFiles(f.idx.newContentAdmissionGate(), []contentPolicyCensusCandidate{{"demo.gif", lang, 512}}))
	})
}

func TestContentSkipCensusReadsOnlyAssetsInBoundedBatches(t *testing.T) {
	f := newContentCensusFixture(t, 128)
	nodes := make(map[string]*graph.Node)
	mtimes := f.store.LoadFileMtimes("repo")
	for i := 0; i < 257; i++ {
		rel := fmt.Sprintf("asset-%03d.gif", i)
		path := filepath.Join(f.root, rel)
		require.NoError(t, os.WriteFile(path, make([]byte, 512), 0600))
		info, err := os.Stat(path)
		require.NoError(t, err)
		mtimes[rel] = info.ModTime().UnixNano()
		id := "repo/" + rel
		nodes[id] = &graph.Node{ID: id, Kind: graph.KindFile, FilePath: id, RepoPrefix: "repo", Meta: map[string]any{"skipped_due_to_content": true, "skip_reason": skipReasonLargeImage, "file_size_bytes": int64(512)}}
	}
	// Keep the existing real demo GIF out of this exact 257-candidate frontier.
	require.NoError(t, os.Remove(filepath.Join(f.root, "demo.gif")))
	delete(mtimes, "demo.gif")
	f.idx.SetFileMtimes(mtimes)
	spy := &contentCensusReadSpy{Store: f.store, nodes: nodes}
	f.idx.graph = spy
	changed, deleted, detected, err := f.idx.changedSinceMtimesCensus(f.root)
	require.NoError(t, err)
	require.Empty(t, changed)
	require.Empty(t, deleted)
	require.Equal(t, 258, detected)
	require.Len(t, spy.batches, 3)
	seen := make(map[string]bool)
	for _, batch := range spy.batches {
		require.LessOrEqual(t, len(batch), 128)
		for _, id := range batch {
			require.NotEqual(t, "repo/main.go", id)
			require.False(t, seen[id])
			seen[id] = true
		}
	}
	require.Len(t, seen, 257)
}

// Keep the actual SIZE publication/version fixture's seam and durable retry
// assertions, but select content policy: a valid 512B GIF exceeds the128B image
// policy while remaining below4096B MaxFileSize. The active graph at the real
// IndexCtx log seam must prove content=true/reason=large_image_asset and no
// skipped_due_to_size field before cancellation or same-mtime size mutation.
func TestContentSkipFailureBoundaryIndexCtxWiring(t *testing.T) {
	for _, scenario := range []string{"cancel_after_capture", "same_mtime_size_change_after_capture"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			gifPath := filepath.Join(root, "demo.gif")
			const oldSize = 512
			const limit = 128
			const maxFileSize = 4096
			require.Greater(t, oldSize, limit)
			require.Less(t, oldSize, maxFileSize, "fixture must select content policy rather than SIZE")
			sizeFailureBoundaryWriteGIF(t, gifPath, oldSize)
			capturedInfo, err := os.Stat(gifPath)
			require.NoError(t, err)
			oldMtime := capturedInfo.ModTime().UnixNano()
			peerPath := filepath.Join(root, "durable-only.go")
			require.NoError(t, os.WriteFile(peerPath, []byte("package fixture\n"), 0600))
			peerInfo, err := os.Stat(peerPath)
			require.NoError(t, err)
			peerMtime := peerInfo.ModTime().UnixNano()
			dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
			var idx *Indexer
			var store *store_sqlite.Store
			t.Cleanup(func() {
				if idx != nil {
					idx.Close()
				}
				if store != nil {
					require.NoError(t, store.Close())
				}
			})
			store, err = store_sqlite.Open(dbPath)
			require.NoError(t, err)
			newIndexer := func(logger *zap.Logger) *Indexer {
				registry := newTestRegistry()
				registry.Register(languages.NewImageAssetExtractor())
				cfg := config.IndexConfig{MaxFileSize: maxFileSize}
				cfg.Content.MaxImageBytes = limit
				result := New(store, registry, cfg, logger)
				result.SetRepoPrefix("repo")
				result.SetDeferResolve(true)
				result.SetDeferGlobalPasses(true)
				result.storeRootPath(root)
				return result
			}
			// Establish a real previous graph, not only a synthetic file row.
			// Seed the old receipt explicitly so this fixture also works before
			// the separate missing-cold-receipt fix has landed.
			idx = newIndexer(zap.NewNop())
			_, err = idx.IndexCtx(context.Background(), root)
			require.NoError(t, err)
			idx.Close()
			idx = nil
			require.NotNil(t, store.GetNodesByIDs([]string{"repo/demo.gif"})["repo/demo.gif"])
			require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{
				"demo.gif": oldMtime, "durable-only.go": peerMtime,
			}))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callbackHits atomic.Int32
			var callbackErr error
			var observedKind graph.NodeKind
			var observedSkip, observedReason, observedSize any
			var observedSizeFlagPresent bool
			core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.InfoLevel)
			logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
				if entry.Message != "indexer: parse subphases" {
					return nil
				}
				if callbackHits.Add(1) != 1 {
					callbackErr = errors.New("publication callback unexpectedly ran more than once")
					return nil
				}
				if ctx.Err() != nil {
					callbackErr = errors.New("context was cancelled before the intended publication seam")
					return nil
				}
				// Read the active graph: on a shadow path it has not drained
				// to the durable Store yet. This proves content-node emission has
				// occurred without assuming a specific publication backend.
				stub := idx.graph.GetNodesByIDs([]string{"repo/demo.gif"})["repo/demo.gif"]
				if stub == nil {
					callbackErr = errors.New("content skip node absent at the publication callback")
					return nil
				}
				observedKind = stub.Kind
				observedSkip = stub.Meta["skipped_due_to_content"]
				observedSize = stub.Meta["file_size_bytes"]
				observedReason = stub.Meta["skip_reason"]
				_, observedSizeFlagPresent = stub.Meta["skipped_due_to_size"]
				if scenario == "cancel_after_capture" {
					cancel()
					return nil
				}
				// Preserve a valid GIF and its mtime while invalidating the
				// captured size version. The synchronous hook finishes before
				// IndexCtx can execute either receipt finalizer.
				data, err := os.ReadFile(gifPath)
				if err == nil {
					err = os.WriteFile(gifPath, append(data, make([]byte, 256)...), 0600)
				}
				if err == nil {
					err = os.Chtimes(gifPath, capturedInfo.ModTime(), capturedInfo.ModTime())
				}
				callbackErr = err
				return nil
			}))
			idx = newIndexer(logger)
			// The peer is durable-only at entry, so receipt publication must
			// not reconstruct an authoritative old roster from this map.
			idx.SetFileMtimes(map[string]int64{"demo.gif": oldMtime})
			result, indexErr := idx.IndexCtx(ctx, root)
			require.EqualValues(t, 1, callbackHits.Load(), "the source-verified publication seam must actually execute")
			require.NoError(t, callbackErr)
			require.Equal(t, graph.KindFile, observedKind)
			require.Equal(t, true, observedSkip)
			require.EqualValues(t, oldSize, observedSize, "the stub must describe the pre-callback version")
			require.Equal(t, "large_image_asset", observedReason)
			require.False(t, observedSizeFlagPresent, "content-policy control must not be a SIZE skip")
			if scenario == "cancel_after_capture" {
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				// Some publication backends acknowledge this late cancellation
				// with an error, while others have already produced a result.
				// Both must withhold the cancelled attempt's receipt.
				if indexErr != nil {
					require.ErrorIs(t, indexErr, context.Canceled)
				} else {
					require.NotNil(t, result)
				}
			} else {
				require.NoError(t, indexErr)
				require.NotNil(t, result)
				currentInfo, err := os.Stat(gifPath)
				require.NoError(t, err)
				require.EqualValues(t, oldSize+256, currentInfo.Size())
				require.Equal(t, oldMtime, currentInfo.ModTime().UnixNano())
			}
			assert.NotContains(t, idx.publishFileMtimes(), "demo.gif", "the actual IndexCtx must invalidate the old local receipt")
			assert.NotContains(t, store.LoadFileMtimes("repo"), "demo.gif", "the actual IndexCtx must invalidate the old durable receipt")
			assert.Equal(t, peerMtime, store.LoadFileMtimes("repo")["durable-only.go"])
			idx.Close()
			idx = nil
			require.NoError(t, store.Close())
			store = nil
			store, err = store_sqlite.Open(dbPath)
			require.NoError(t, err)
			idx = newIndexer(zap.NewNop())
			idx.SetFileMtimes(store.LoadFileMtimes("repo"))
			idx.loadFileIndexFailures()
			assert.NotContains(t, store.LoadFileMtimes("repo"), "demo.gif")
			assert.Equal(t, peerMtime, store.LoadFileMtimes("repo")["durable-only.go"])
			changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
			require.NoError(t, err)
			assert.Equal(t, []string{"demo.gif"}, changed)
			require.Empty(t, deleted)
			require.Equal(t, 2, detected)
		})
	}
}

func TestContentSkipReconcileUsesExplicitPolicyFrontier(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "mtime"
		if enabled {
			name = "merkle"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("GORTEX_MERKLE", "")
			f := newSizeCensusFixture(t, 4096)
			// Keep one asset policy transition below the full-retrack threshold.
			// The sentinel below proves unchanged files are not reindexed.
			for i := 1; i <= 3; i++ {
				body := fmt.Sprintf("package fixture\nfunc Stable%d() {}\n", i)
				require.NoError(t, os.WriteFile(filepath.Join(f.root, fmt.Sprintf("stable%d.go", i)), []byte(body), 0600))
			}
			writeConfig := func(imageLimit int64) {
				cfg := config.Default()
				cfg.Index.MaxFileSize = 4096
				cfg.Index.Merkle = enabled
				cfg.Index.Content.MaxImageBytes = imageLimit
				encoded, err := yaml.Marshal(cfg)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(f.root, ".gortex.yaml"), encoded, 0600))
			}
			writeConfig(1024)
			f.idx.config.Merkle = enabled
			f.idx.config.Content.MaxImageBytes = 1024
			require.Equal(t, enabled, f.idx.merkleEnabled())
			_, err := f.idx.IndexCtx(context.Background(), f.root)
			require.NoError(t, err)
			f.idx.RunDeferredPasses(context.Background())
			f.establishKnownReceipt()
			if enabled {
				encoded, err := os.ReadFile(merkleTreeFile(f.root))
				require.NoError(t, err)
				var baseline map[string]any
				require.NoError(t, json.Unmarshal(encoded, &baseline))
				files, ok := baseline["files"].(map[string]any)
				require.True(t, ok, "test-created baseline must contain its files map")
				leaf, ok := files["demo.gif"].(map[string]any)
				require.True(t, ok, "cold eligible GIF must have an actual leaf before content cap decrease")
				hash, ok := leaf["hash"].(string)
				require.True(t, ok)
				require.Len(t, hash, 64)
			}
			require.GreaterOrEqual(t, len(f.store.LoadFileMtimes("repo")), 5)
			assertPolicyOutcome := func(imageLimit int64) {
				node := f.fileNode()
				require.NotEqual(t, true, node.Meta["skipped_due_to_size"], "global size cap remains above the unchanged 512-byte GIF")
				nodes := f.store.GetFileNodesByPaths([]string{"repo/demo.gif"})["repo/demo.gif"]
				assetNodes := 0
				for _, candidate := range nodes {
					if candidate != nil && candidate.Kind != graph.KindFile {
						assetNodes++
					}
				}
				if imageLimit < 512 {
					require.Equal(t, true, node.Meta["skipped_due_to_content"])
					require.Equal(t, "large_image_asset", node.Meta["skip_reason"])
					require.EqualValues(t, 512, node.Meta["file_size_bytes"])
					require.Zero(t, assetNodes, "content-ineligible GIF must retain only its file stub")
				} else {
					require.NotEqual(t, true, node.Meta["skipped_due_to_content"])
					require.NotEqual(t, "large_image_asset", node.Meta["skip_reason"])
					require.Positive(t, assetNodes, "eligible GIF must have an actually parsed non-file image asset node")
				}
				require.Equal(t, f.mtime.UnixNano(), f.store.LoadFileMtimes("repo")["demo.gif"])
			}
			assertPolicyOutcome(1024)

			unchanged := &graph.Edge{From: "repo/stable1.go", To: "repo/stable2.go",
				Kind: "co_change", FilePath: "repo/stable1.go", Line: 777}
			f.store.AddBatch(nil, []*graph.Edge{unchanged})
			assertUnchangedEdge := func() {
				edges := f.store.GetOutEdgesByNodeIDs([]string{unchanged.From})[unchanged.From]
				found := 0
				for _, edge := range edges {
					if edge.From == unchanged.From && edge.To == unchanged.To && edge.Kind == unchanged.Kind && edge.Line == unchanged.Line {
						found++
					}
				}
				require.Equal(t, 1, found, "scoped content policy refresh must retain unchanged-file edge")
			}
			assertUnchangedEdge()

			entry := config.RepoEntry{Path: f.root, Name: "repo"}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			global := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
			global.SetConfigPath(configPath)
			require.NoError(t, global.Save())
			reconcile := func(imageLimit int64, wantStale bool) {
				f.reopen(4096)
				f.idx.config.Content.MaxImageBytes = imageLimit
				f.idx.Close()
				f.idx = nil
				manager, err := config.NewConfigManager(configPath)
				require.NoError(t, err)
				registry := newTestRegistry()
				registry.Register(languages.NewImageAssetExtractor())
				mi := NewMultiIndexer(f.store, registry, search.NewNull(), manager, zap.NewNop())
				// A fresh MI must load real repo config and reconcile persisted state.
				result, err := mi.ReconcileRepoCtx(context.Background(), entry, f.store.LoadFileMtimes("repo"))
				f.idx = mi.indexers["repo"]
				require.NoError(t, err)
				require.NotNil(t, result)
				require.NotNil(t, f.idx)
				require.EqualValues(t, 4096, f.idx.config.MaxFileSize)
				require.EqualValues(t, imageLimit, f.idx.config.Content.MaxImageBytes, "real repo content policy override must have loaded")
				require.Equal(t, enabled, f.idx.merkleEnabled())
				if wantStale {
					require.Positive(t, result.StaleFileCount)
				} else {
					require.Zero(t, result.StaleFileCount)
				}
				assertPolicyOutcome(imageLimit)
				assertUnchangedEdge()
			}
			writeConfig(128)
			reconcile(128, true)
			reconcile(128, false)
			writeConfig(1024)
			reconcile(1024, true)
			reconcile(1024, false)
		})
	}
}
