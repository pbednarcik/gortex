package lsp

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
)

type lspFrontierCountingStore struct {
	graph.Store
	sqlite *store_sqlite.Store

	fileCountCalls   int
	nodePageCalls    int
	confirmPageCalls int
	kindPageCalls    int
	fanInCalls       int
	inboundKindCalls int
	nodeBatchCalls   int
	pointCalls       int
	globalCalls      int
	maxFiles         int
}

func (s *lspFrontierCountingStore) LSPRepoFileCounts(repoPrefix string, languages []string) (map[string]int, map[string]int) {
	s.fileCountCalls++
	return s.sqlite.LSPRepoFileCounts(repoPrefix, languages)
}

func (s *lspFrontierCountingStore) LSPRepoNodesByFiles(repoPrefix string, languages, files []string, unstampedOnly bool) []*graph.Node {
	s.nodePageCalls++
	s.observeFiles(files)
	return s.sqlite.LSPRepoNodesByFiles(repoPrefix, languages, files, unstampedOnly)
}

func (s *lspFrontierCountingStore) LSPRepoConfirmableEdgesByFiles(repoPrefix string, languages, files []string, ambiguousOnly bool) []*graph.Edge {
	s.confirmPageCalls++
	s.observeFiles(files)
	return s.sqlite.LSPRepoConfirmableEdgesByFiles(repoPrefix, languages, files, ambiguousOnly)
}

func (s *lspFrontierCountingStore) LSPRepoEdgesByFilesAndKinds(repoPrefix string, languages, files []string, kinds []graph.EdgeKind) []*graph.Edge {
	s.kindPageCalls++
	s.observeFiles(files)
	return s.sqlite.LSPRepoEdgesByFilesAndKinds(repoPrefix, languages, files, kinds)
}

func (s *lspFrontierCountingStore) LSPNodeFanInCounts(ids []string) map[string]int {
	s.fanInCalls++
	return s.sqlite.LSPNodeFanInCounts(ids)
}

func (s *lspFrontierCountingStore) LSPInEdgesByNodeIDsAndKinds(ids []string, kinds []graph.EdgeKind) []*graph.Edge {
	s.inboundKindCalls++
	return s.sqlite.LSPInEdgesByNodeIDsAndKinds(ids, kinds)
}

func (s *lspFrontierCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.nodeBatchCalls++
	return s.sqlite.GetNodesByIDs(ids)
}

func (s *lspFrontierCountingStore) GetNode(id string) *graph.Node {
	s.pointCalls++
	return s.sqlite.GetNode(id)
}

func (s *lspFrontierCountingStore) GetFileNodes(path string) []*graph.Node {
	s.pointCalls++
	return s.sqlite.GetFileNodes(path)
}

func (s *lspFrontierCountingStore) GetOutEdges(id string) []*graph.Edge {
	s.pointCalls++
	return s.sqlite.GetOutEdges(id)
}

func (s *lspFrontierCountingStore) GetInEdges(id string) []*graph.Edge {
	s.pointCalls++
	return s.sqlite.GetInEdges(id)
}

func (s *lspFrontierCountingStore) AllNodes() []*graph.Node {
	s.globalCalls++
	return s.sqlite.AllNodes()
}

func (s *lspFrontierCountingStore) AllEdges() []*graph.Edge {
	s.globalCalls++
	return s.sqlite.AllEdges()
}

func (s *lspFrontierCountingStore) GetRepoNodes(repoPrefix string) []*graph.Node {
	s.globalCalls++
	return s.sqlite.GetRepoNodes(repoPrefix)
}

func (s *lspFrontierCountingStore) GetRepoEdges(repoPrefix string) []*graph.Edge {
	s.globalCalls++
	return s.sqlite.GetRepoEdges(repoPrefix)
}

func (s *lspFrontierCountingStore) observeFiles(files []string) {
	if len(files) > s.maxFiles {
		s.maxFiles = len(files)
	}
}

// The semantic_heavy stamp lives in the opaque Meta blob, so the light
// location projection cannot see it — only the full-node re-fetch can. A
// heavyDelta frontier must therefore recheck the stamp on the full nodes,
// or every drain re-issues the deferred tier for the whole repo (SQLite
// resume regression reported against the lane branch).
func TestReadLSPRepoProjectionHeavyDeltaSkipsHeavyStampedNodes(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "lsp-frontier-heavy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Both nodes carry the fast tier's semantic_type stamp — the heavy
	// frontier is exactly the fast-stamped population — and one already
	// completed its heavy drain.
	store.AddBatch([]*graph.Node{
		{
			ID: "drained", RepoPrefix: "repo", Language: "go", Kind: graph.KindFunction,
			Name: "Drained", FilePath: "repo/a.go", StartLine: 1, EndLine: 5,
			Meta: map[string]any{"semantic_type": "func()", "semantic_heavy": "1"},
		},
		{
			ID: "pending", RepoPrefix: "repo", Language: "go", Kind: graph.KindFunction,
			Name: "Pending", FilePath: "repo/a.go", StartLine: 7, EndLine: 11,
			Meta: map[string]any{"semantic_type": "func()"},
		},
	}, nil)

	provider := &Provider{languages: []string{"go"}, heavyDelta: true}
	projection, ok := provider.readLSPRepoProjection(store, "repo")
	if !ok {
		t.Fatal("SQLite projection capability was not selected")
	}

	ids := make([]string, 0, len(projection.langNodes))
	for _, node := range projection.langNodes {
		ids = append(ids, node.ID)
	}
	if fmt.Sprint(ids) != "[pending]" {
		t.Fatalf("heavyDelta frontier candidates=%v, want only [pending]: the drained node's semantic_heavy stamp must survive the light projection", ids)
	}
}

func TestReadLSPRepoProjectionBoundsFrontiersAndMatchesLegacyDecisions(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "lsp-frontier.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const fileCount = 70
	nodes := make([]*graph.Node, 0, fileCount+3)
	edges := make([]*graph.Edge, 0, (fileCount-1)*2)
	for i := 0; i < fileCount; i++ {
		id := fmt.Sprintf("node-%03d", i)
		file := fmt.Sprintf("repo/file-%03d.go", i)
		meta := map[string]any{"receiver": "Receiver"}
		if i%2 == 0 {
			meta["semantic_type"] = "func()"
		}
		nodes = append(nodes, &graph.Node{
			ID: id, RepoPrefix: "repo", Language: "go", Kind: graph.KindFunction,
			Name: fmt.Sprintf("Fn%03d", i), FilePath: file, StartLine: 1, EndLine: 5, Meta: meta,
		})
		if i > 0 {
			prev := fmt.Sprintf("node-%03d", i-1)
			edges = append(edges,
				&graph.Edge{From: prev, To: id, Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("repo/file-%03d.go", i-1), Line: 3, Confidence: 0.5},
				&graph.Edge{From: prev, To: id, Kind: graph.EdgeDefines, FilePath: fmt.Sprintf("repo/file-%03d.go", i-1), Line: 1, Confidence: 0.5},
			)
		}
	}
	nodes = append(nodes,
		&graph.Node{ID: "generated", RepoPrefix: "repo", Language: "go", Kind: graph.KindFunction, Name: "Generated", FilePath: "repo/generated.pb.go", StartLine: 1, EndLine: 2},
		&graph.Node{ID: "python", RepoPrefix: "repo", Language: "python", Kind: graph.KindFunction, Name: "Python", FilePath: "repo/python.py", StartLine: 1, EndLine: 2},
		&graph.Node{ID: "other", RepoPrefix: "other", Language: "go", Kind: graph.KindFunction, Name: "Other", FilePath: "other/file.go", StartLine: 1, EndLine: 2},
	)
	store.AddBatch(nodes, edges)

	counting := &lspFrontierCountingStore{Store: store, sqlite: store}
	provider := &Provider{languages: []string{"go"}}
	projection, ok := provider.readLSPRepoProjection(counting, "repo")
	if !ok {
		t.Fatal("SQLite projection capability was not selected")
	}

	if projection.symbolsTotal != fileCount || projection.skippedAlreadyStamped != fileCount/2 {
		t.Fatalf("projection counters total=%d skipped=%d, want %d/%d",
			projection.symbolsTotal, projection.skippedAlreadyStamped, fileCount, fileCount/2)
	}
	if len(projection.langNodes) != fileCount/2 {
		t.Fatalf("unstamped candidates=%d, want %d", len(projection.langNodes), fileCount/2)
	}
	if len(projection.targets) != fileCount-1 {
		t.Fatalf("ambiguous targets=%d, want %d", len(projection.targets), fileCount-1)
	}
	if len(projection.repoEdges) != fileCount-1 {
		t.Fatalf("retained edges=%d, want only %d confirmable calls", len(projection.repoEdges), fileCount-1)
	}

	wantPages := (fileCount + lspRepoFileFrontierSize - 1) / lspRepoFileFrontierSize
	if projection.frontierPages != wantPages || projection.frontierPeakFiles != lspRepoFileFrontierSize || counting.maxFiles > lspRepoFileFrontierSize {
		t.Fatalf("frontier pages=%d peak=%d observed=%d, want pages=%d peak<=%d",
			projection.frontierPages, projection.frontierPeakFiles, counting.maxFiles, wantPages, lspRepoFileFrontierSize)
	}
	if counting.fileCountCalls != 1 || counting.nodePageCalls != wantPages || counting.confirmPageCalls != wantPages || counting.kindPageCalls != wantPages {
		t.Fatalf("projection query shape counts=%d nodes=%d confirm=%d kinds=%d, want 1/%d/%d/%d",
			counting.fileCountCalls, counting.nodePageCalls, counting.confirmPageCalls, counting.kindPageCalls,
			wantPages, wantPages, wantPages)
	}
	if counting.nodeBatchCalls != wantPages {
		t.Fatalf("full-node batch calls=%d, want one candidate batch per frontier (%d)", counting.nodeBatchCalls, wantPages)
	}
	if counting.fanInCalls != 1 || counting.inboundKindCalls != 1 {
		t.Fatalf("compact inbound queries fan-in=%d dispatch=%d, want one each", counting.fanInCalls, counting.inboundKindCalls)
	}
	if counting.pointCalls != 0 || counting.globalCalls != 0 {
		t.Fatalf("forbidden point/global reads used: point=%d global=%d", counting.pointCalls, counting.globalCalls)
	}

	legacyCandidates := make([]string, 0, fileCount/2)
	for _, node := range store.GetRepoNodes("repo") {
		if node.Language != "go" || node.Kind == graph.KindFile || node.Kind == graph.KindImport ||
			semantic.IsLowValueForEnrichment(node.FilePath, nil) || nodeAlreadyStamped(node) {
			continue
		}
		legacyCandidates = append(legacyCandidates, node.ID)
	}
	projectedCandidates := make([]string, 0, len(projection.langNodes))
	for _, node := range projection.langNodes {
		projectedCandidates = append(projectedCandidates, node.ID)
	}
	sort.Strings(legacyCandidates)
	sort.Strings(projectedCandidates)
	if fmt.Sprint(projectedCandidates) != fmt.Sprint(legacyCandidates) {
		t.Fatalf("candidate parity mismatch\nprojected=%v\nlegacy=%v", projectedCandidates, legacyCandidates)
	}
}
