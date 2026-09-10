package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

func TestSearchTextEnrichmentAndCaptureShareOneBoundedFetch(t *testing.T) {
	const path = "repo/handler_test.go"
	backing := graph.New()
	owner := &graph.Node{
		ID: path + "::testOwner", Name: "testOwner", Kind: graph.KindFunction,
		FilePath: path, StartLine: 5, EndLine: 30, Meta: map[string]any{"is_test": true},
	}
	backing.AddNode(owner)
	probe := &boundedFileStoreProbe{Store: backing}
	probe.read = func(_ context.Context, gotPath string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		if gotPath != path || limit != localizationFileNodeLimit || scope.ExcludeTests {
			t.Fatalf("bounded search-text fetch = (%q, %#v, %d)", gotPath, scope, limit)
		}
		return backing.FindFileNodesBounded(context.Background(), gotPath, scope, limit)
	}
	server := &Server{graph: probe}
	ctx := withLocalizationPermittedEvidenceCapture(context.Background(), 91)
	enriched, indexes := server.enrichTextMatchesContext(ctx, []trigram.Match{{Path: path, Line: 10, Text: "needle"}}, query.QueryOptions{})
	if len(enriched) != 1 || enriched[0].SymbolID != owner.ID {
		t.Fatalf("test-file owner enrichment = %#v", enriched)
	}
	server.captureLocalizationSearchText(ctx, enriched, indexes)
	if len(probe.calls) != 1 {
		t.Fatalf("enrichment + capture performed %d file fetches, want one", len(probe.calls))
	}
	rows, recorded := localizationEvidenceForPermittedCall(ctx, "search", "text", 91)
	if !recorded || len(rows) != 1 || rows[0].ID != owner.ID {
		t.Fatalf("shared-index capture = %#v, recorded=%v", rows, recorded)
	}
}

func TestSearchTextEnrichmentPreservesFirstSeenFilePriority(t *testing.T) {
	const (
		firstPath  = "repo/z.go"
		secondPath = "repo/a.go"
	)
	backing := graph.New()
	firstOwner := &graph.Node{
		ID: firstPath + "::first", Name: "first", Kind: graph.KindFunction,
		FilePath: firstPath, StartLine: 1, EndLine: 20,
	}
	secondOwner := &graph.Node{
		ID: secondPath + "::second", Name: "second", Kind: graph.KindFunction,
		FilePath: secondPath, StartLine: 1, EndLine: 20,
	}
	backing.AddNode(firstOwner)
	backing.AddNode(secondOwner)
	probe := &boundedFileStoreProbe{Store: backing}
	probe.read = func(ctx context.Context, path string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		return backing.FindFileNodesBounded(ctx, path, scope, limit)
	}
	server := &Server{graph: probe}

	enriched, _ := server.enrichTextMatchesContext(context.Background(), []trigram.Match{
		{Path: firstPath, Line: 3, Text: "first"},
		{Path: secondPath, Line: 4, Text: "second"},
		{Path: firstPath, Line: 5, Text: "first again"},
	}, query.QueryOptions{})

	if len(probe.calls) != 2 || probe.calls[0].path != firstPath || probe.calls[1].path != secondPath {
		t.Fatalf("bounded fetch order = %#v, want first-seen paths [%q %q]", probe.calls, firstPath, secondPath)
	}
	if len(enriched) != 3 || enriched[0].SymbolID != firstOwner.ID || enriched[1].SymbolID != secondOwner.ID || enriched[2].SymbolID != firstOwner.ID {
		t.Fatalf("first-seen enrichment = %#v", enriched)
	}
}

func TestGenericFileSymbolIndexMapOrderingRemainsDeterministic(t *testing.T) {
	probe := &boundedFileStoreProbe{Store: graph.New()}
	probe.read = func(_ context.Context, _ string, _ graph.LocalizationNodeScope, _ int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{}, nil
	}
	server := &Server{graph: probe}
	server.buildFileSymbolIndexForPathsScopedContext(context.Background(), map[string]struct{}{
		"repo/z.go": {},
		"repo/a.go": {},
		"repo/m.go": {},
	}, query.QueryOptions{})

	if len(probe.calls) != 3 || probe.calls[0].path != "repo/a.go" || probe.calls[1].path != "repo/m.go" || probe.calls[2].path != "repo/z.go" {
		t.Fatalf("generic map fetch order = %#v, want alphabetical determinism", probe.calls)
	}
}

func TestLocalizationTextMatchDirectSymbolIDSkipsFileIndex(t *testing.T) {
	store := graph.New()
	node := &graph.Node{ID: "repo/direct.go::direct", Name: "direct", Kind: graph.KindFunction, FilePath: "repo/direct.go"}
	store.AddNode(node)
	server := &Server{graph: store}
	got, provenance := server.localizationTextMatchNode(
		context.Background(),
		enrichedTextMatch{Path: "repo/ignored.go", SymbolID: node.ID},
		map[string]*fileSymbolIndex{"repo/ignored.go": {saturated: true}},
	)
	if got != node || provenance != "permitted_search_text" {
		t.Fatalf("direct SymbolID = (%#v, %q), want unchanged typed lookup", got, provenance)
	}
}

// TestLocalizationTextMatchNormalisesNativePathToGraphKey pins the one
// spelling reconciliation that survives: the index is keyed the way the graph
// keys nodes (forward slashes on every platform), so a natively-spelled match
// path has to be folded onto that key before the owner lookup. Windows-only —
// on POSIX a backslash is an ordinary filename byte and the two paths are
// genuinely different files.
func TestLocalizationTextMatchNormalisesNativePathToGraphKey(t *testing.T) {
	if filepath.Separator == '/' {
		t.Skip("a native path differs from the graph spelling only on Windows")
	}
	server := &Server{graph: graph.New()}
	graphPath := "repo/dir/handler.go"
	nativePath := filepath.FromSlash(graphPath)
	owner := &graph.Node{ID: graphPath + "::owner", Name: "owner", Kind: graph.KindFunction, FilePath: graphPath, StartLine: 1, EndLine: 20}
	indexes := map[string]*fileSymbolIndex{graphPath: {syms: []*graph.Node{owner}}}
	got, provenance := server.localizationTextMatchNode(context.Background(), enrichedTextMatch{Path: nativePath, Line: 10}, indexes)
	if got != owner || provenance != "permitted_search_text_owner" {
		t.Fatalf("native path fold = (%#v, %q), want owner", got, provenance)
	}
}

func TestLocalizationTextMatchFailsClosedForSaturatedExactPath(t *testing.T) {
	server := &Server{graph: graph.New()}
	path := "repo/dense.go"
	wrong := &graph.Node{ID: path + "::wide", Name: "wide", Kind: graph.KindFunction, FilePath: path, StartLine: 1, EndLine: 100}
	got, provenance := server.localizationTextMatchNode(
		context.Background(),
		enrichedTextMatch{Path: path, Line: 50},
		map[string]*fileSymbolIndex{path: {saturated: true, syms: []*graph.Node{wrong}, fileNode: &graph.Node{ID: path, Kind: graph.KindFile, FilePath: path}}},
	)
	if got != nil || provenance != "" {
		t.Fatalf("saturated path misattributed owner/file: (%#v, %q)", got, provenance)
	}
}
