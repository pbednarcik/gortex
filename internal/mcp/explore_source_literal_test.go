package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
	"github.com/zzet/gortex/internal/search/trigram"
)

type exploreSourceLiteralCountingStore struct {
	graph.Store
	allNodesCalls     int
	outEdgeBatchCalls int
	nodeLookupBatches int
}

func (s *exploreSourceLiteralCountingStore) AllNodes() []*graph.Node {
	s.allNodesCalls++
	return s.Store.AllNodes()
}

func (s *exploreSourceLiteralCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.outEdgeBatchCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *exploreSourceLiteralCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.nodeLookupBatches++
	return s.Store.GetNodesByIDs(ids)
}

type exploreSourceLiteralBlockingStore struct {
	graph.Store
	started chan struct{}
}

func (s *exploreSourceLiteralBlockingStore) GetFileNodesContext(ctx context.Context, _ string) []*graph.Node {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil
}

type exploreSourceLiteralOrderedStore struct {
	graph.Store
	nodesByPath map[string][]*graph.Node
	blockPath   string
	calls       []string
}

func (s *exploreSourceLiteralOrderedStore) GetFileNodesContext(ctx context.Context, path string) []*graph.Node {
	s.calls = append(s.calls, path)
	if path == s.blockPath {
		<-ctx.Done()
		return nil
	}
	return s.nodesByPath[path]
}

func (s *exploreSourceLiteralOrderedStore) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	return nil
}

func newExploreSourceLiteralServer(t testing.TB, nodes []*graph.Node) *Server {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.AddBatch(nodes, nil)
	return &Server{graph: store}
}

// pinExploreSourceLiteralRecallBudget takes the bounded source-literal recall
// off the wall clock. In production the recall gets a 75ms slice per anchor
// and reports a deadline when it runs out, which is the right trade for a
// best-effort fallback. Mapping a two-file fixture costs single-digit
// milliseconds, but a loaded CI runner can deschedule it past that slice, and
// a test asserting which owners were mapped then fails against a recall that
// behaved correctly. Tests that assert what the recall finds pin a slice they
// cannot miss; the ones that assert what it does when the slice runs out —
// TestGatherExploreSourceLiteralRecallBoundsMappingByRequestDeadline — keep
// the production default.
func pinExploreSourceLiteralRecallBudget(s *Server) *Server {
	s.sourceLiteralRecallBudgetOverride = 30 * time.Second
	return s
}

func newExploreSourceLiteralGraphServer(
	t testing.TB,
	nodes []*graph.Node,
	edges []*graph.Edge,
) (*Server, *exploreSourceLiteralCountingStore) {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.AddBatch(nodes, edges)
	counting := &exploreSourceLiteralCountingStore{Store: store}
	return &Server{graph: counting}, counting
}

func sourceLiteralNode(id, name, path string, kind graph.NodeKind, start, end int) *graph.Node {
	return &graph.Node{
		ID: id, Name: name, QualName: id, Kind: kind,
		FilePath: path, RepoPrefix: "demo", Language: "csharp",
		StartLine: start, EndLine: end,
	}
}

func TestExploreSourceLiteralCallNameAcrossLanguages(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{name: "csharp", line: `RegisterDefaultFormatter("ku", formatter);`, want: "RegisterDefaultFormatter", ok: true},
		{name: "rust", line: `register_default_formatter("ku", formatter);`, want: "register_default_formatter", ok: true},
		{name: "typescript member", line: `registry.registerDefaultFormatter('ku', formatter);`, want: "registerDefaultFormatter", ok: true},
		{name: "nested", line: `install(resolve("ku"));`, want: "resolve", ok: true},
		{name: "assignment", line: `const locale = "ku";`},
		{name: "control expression", line: `if (locale == "ku") { register(); }`},
		{name: "ambiguous calls", line: `left("ku"); right("ku");`},
		{name: "multiline is conservative", line: `RegisterDefaultFormatter(`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := exploreSourceLiteralCallName(test.line, "ku")
			require.Equal(t, test.ok, ok)
			require.Equal(t, test.want, got)
		})
	}
}

func TestMapExploreSourceLiteralMatchesPromotesUniqueDirectCalleeAcrossLanguages(t *testing.T) {
	tests := []struct {
		name     string
		language string
		line     string
		callee   string
	}{
		{name: "csharp", language: "csharp", line: `RegisterDefaultFormatter("ku");`, callee: "RegisterDefaultFormatter"},
		{name: "rust", language: "rust", line: `register_default_formatter("ku");`, callee: "register_default_formatter"},
		{name: "typescript", language: "typescript", line: `registry.registerDefaultFormatter('ku');`, callee: "registerDefaultFormatter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := "demo/src/registry"
			owner := sourceLiteralNode(path+"::configure", "configure", path, graph.KindMethod, 1, 5)
			callee := sourceLiteralNode(path+"::"+test.callee, test.callee, path, graph.KindMethod, 7, 9)
			owner.Language = test.language
			callee.Language = test.language
			server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{owner, callee}, []*graph.Edge{{
				From: owner.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: path, Line: 3,
			}})

			recall := server.mapExploreSourceLiteralMatches("ku", []trigram.Match{{
				Path: path, Line: 3, Text: test.line,
			}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

			require.Equal(t, []exploreSourceLiteralHit{{nodeID: callee.ID, rank: 0, callee: true}}, recall.hits)
			require.Equal(t, callee.FilePath, recall.ownerFiles[callee.ID])
			require.Zero(t, counting.allNodesCalls, "callee promotion must remain batch- and file-bounded")
			require.Equal(t, 1, counting.outEdgeBatchCalls)
			require.Equal(t, 1, counting.nodeLookupBatches)
		})
	}
}

func TestMapExploreSourceLiteralMatchesDoesNotPromoteAmbiguousCallee(t *testing.T) {
	path := "demo/src/registry.cs"
	owner := sourceLiteralNode(path+"::configure", "configure", path, graph.KindMethod, 1, 5)
	first := sourceLiteralNode(path+"::register-string", "RegisterDefaultFormatter", path, graph.KindMethod, 7, 9)
	second := sourceLiteralNode(path+"::register-provider", "RegisterDefaultFormatter", path, graph.KindMethod, 11, 13)
	server, _ := newExploreSourceLiteralGraphServer(t, []*graph.Node{owner, first, second}, []*graph.Edge{
		{From: owner.ID, To: first.ID, Kind: graph.EdgeCalls, FilePath: path, Line: 3},
		{From: owner.ID, To: second.ID, Kind: graph.EdgeCalls, FilePath: path, Line: 3},
	})

	recall := server.mapExploreSourceLiteralMatches("ku", []trigram.Match{{
		Path: path, Line: 3, Text: `RegisterDefaultFormatter("ku");`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

	require.Equal(t, []exploreSourceLiteralHit{{nodeID: owner.ID, rank: 0}}, recall.hits)
}

func TestMapExploreSourceLiteralMatchesDoesNotPromoteAssignment(t *testing.T) {
	path := "demo/src/registry.cs"
	owner := sourceLiteralNode(path+"::configure", "configure", path, graph.KindMethod, 1, 5)
	callee := sourceLiteralNode(path+"::register", "RegisterDefaultFormatter", path, graph.KindMethod, 7, 9)
	server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{owner, callee}, []*graph.Edge{{
		From: owner.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: path, Line: 3,
	}})

	recall := server.mapExploreSourceLiteralMatches("ku", []trigram.Match{{
		Path: path, Line: 3, Text: `const locale = "ku";`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

	require.Equal(t, []exploreSourceLiteralHit{{nodeID: owner.ID, rank: 0}}, recall.hits,
		"an unrelated same-line edge must not turn an assignment into a callsite")
	require.Zero(t, counting.outEdgeBatchCalls, "non-call literal hits must not query graph adjacency")
	require.Zero(t, counting.nodeLookupBatches, "non-call literal hits must not query callee nodes")
}

func TestExploreSourceLiteralLocalCalleeRejectsCrossRepositoryTarget(t *testing.T) {
	owner := sourceLiteralNode("demo/registry.cs::configure", "configure", "demo/registry.cs", graph.KindMethod, 1, 5)
	callee := sourceLiteralNode("other/registry.cs::register", "RegisterDefaultFormatter", "other/registry.cs", graph.KindMethod, 7, 9)
	callee.RepoPrefix = "other"
	require.False(t, exploreSourceLiteralLocalCallee(owner, callee, "RegisterDefaultFormatter", query.QueryOptions{}))
}

func TestSourceLiteralCalleeRemainsAuthorizedForRefinement(t *testing.T) {
	task := `find where culture "ku" is registered`
	owner := sourceLiteralNode("demo/registry.cs::configure", "configure", "demo/registry.cs", graph.KindMethod, 1, 5)
	callee := sourceLiteralNode("demo/registry.cs::RegisterDefaultFormatter", "RegisterDefaultFormatter", "demo/registry.cs", graph.KindMethod, 7, 10)
	targets := []exploreTarget{
		{node: owner, source: `void configure() { ... }`},
		{node: callee, source: `void RegisterDefaultFormatter(string culture) { ... }`, sourceLiteral: true},
	}
	preferred := explorePreferredRefinementSymbol(task, targets)
	require.Equal(t, callee.ID, preferred)

	result, completion, routes, digest := buildLocalizationRefinementResultForTask(
		preferred, task, targets, exploreDefaultBudgetTokens, exploreLocalizationRefinementRoutes(targets),
	)
	require.Equal(t, localizationStateAnswerReady, completion.State)
	require.False(t, completion.Enforceable)
	require.Zero(t, completion.AllowedToolCalls)
	require.Empty(t, completion.AllowedSymbols)
	require.Contains(t, completion.FinalResponse, localizationBoundedHeading)
	require.Contains(t, routes, callee.ID, "the packed conclusion must retain the chosen implementation route")
	require.NotNil(t, digest)

	text, ok := singleTextContent(result)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(text), &envelope))
	require.True(t, envelope.Terminal)
	foundPackedCallee := false
	for _, evidence := range envelope.Evidence {
		if evidence.ID == callee.ID && evidence.Source != "" {
			foundPackedCallee = true
			break
		}
	}
	require.True(t, foundPackedCallee, "the source-literal callee body must replace the retired refinement read")
}

func TestMapExploreSourceLiteralMatchesFindsCSharpConstructor(t *testing.T) {
	path := "demo/src/FormatterRegistry.cs"
	class := sourceLiteralNode("demo/FormatterRegistry.cs::FormatterRegistry#type", "FormatterRegistry", path, graph.KindType, 8, 42)
	constructor := sourceLiteralNode("demo/FormatterRegistry.cs::FormatterRegistry", "FormatterRegistry", path, graph.KindMethod, 20, 29)
	server := newExploreSourceLiteralServer(t, []*graph.Node{class, constructor})

	recall := server.mapExploreSourceLiteralMatches("ku", []trigram.Match{{
		Path: path, Line: 24, Text: `Register("ku", new CentralKurdishFormatter());`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

	require.Equal(t, []exploreSourceLiteralHit{{nodeID: constructor.ID, rank: 0}}, recall.hits)
	require.False(t, recall.ambiguous)
}

func TestMapExploreSourceLiteralMatchesFallsBackToSingleRepoUnprefixedPath(t *testing.T) {
	graphPath := "src/Humanizer/Configuration/FormatterRegistry.cs"
	matchPath := "humanizer-1059/" + graphPath
	class := sourceLiteralNode("src/FormatterRegistry.cs::FormatterRegistry#type", "FormatterRegistry", graphPath, graph.KindType, 5, 66)
	constructor := sourceLiteralNode("src/FormatterRegistry.cs::FormatterRegistry", "FormatterRegistry", graphPath, graph.KindMethod, 7, 55)
	class.RepoPrefix = ""
	constructor.RepoPrefix = ""
	server := newExploreSourceLiteralServer(t, []*graph.Node{class, constructor})

	recall := server.mapExploreSourceLiteralMatches("ku", []trigram.Match{{
		Path: matchPath, Line: 24, Text: `RegisterDefaultFormatter("ku");`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"humanizer-1059": true}})

	require.Equal(t, []exploreSourceLiteralHit{{nodeID: constructor.ID, rank: 0}}, recall.hits)
	require.False(t, recall.ambiguous)
}

func TestMapExploreSourceLiteralMatchesPrefersExactPathOverAlias(t *testing.T) {
	graphPath := "src/Humanizer/Configuration/FormatterRegistry.cs"
	matchPath := "humanizer-1059/" + graphPath
	alias := sourceLiteralNode("alias::FormatterRegistry", "AliasFormatterRegistry", graphPath, graph.KindMethod, 7, 55)
	alias.RepoPrefix = ""
	exact := sourceLiteralNode("exact::FormatterRegistry", "ExactFormatterRegistry", matchPath, graph.KindMethod, 7, 55)
	exact.RepoPrefix = "humanizer-1059"
	server := newExploreSourceLiteralServer(t, []*graph.Node{alias, exact})

	recall := server.mapExploreSourceLiteralMatches("ku", []trigram.Match{{
		Path: matchPath, Line: 24, Text: `RegisterDefaultFormatter("ku");`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"humanizer-1059": true}})

	require.Equal(t, []exploreSourceLiteralHit{{nodeID: exact.ID, rank: 0}}, recall.hits)
}

func TestMapExploreSourceLiteralMatchesQueriesExactPathsBeforeAliases(t *testing.T) {
	exactA := sourceLiteralNode("demo/src/a.cs::Register", "RegisterA", "demo/src/a.cs", graph.KindMethod, 1, 5)
	exactB := sourceLiteralNode("demo/src/b.cs::Register", "RegisterB", "demo/src/b.cs", graph.KindMethod, 1, 5)
	store := &exploreSourceLiteralOrderedStore{
		nodesByPath: map[string][]*graph.Node{
			exactA.FilePath: {exactA},
			exactB.FilePath: {exactB},
		},
		blockPath: "src/a.cs",
	}
	server := &Server{graph: store}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	recall := server.mapExploreSourceLiteralMatchesContext(ctx, "ku", []trigram.Match{
		{Path: exactB.FilePath, Line: 3, Text: `Register("ku")`},
		{Path: exactA.FilePath, Line: 3, Text: `Register("ku")`},
	}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

	require.Equal(t, []string{"demo/src/a.cs", "demo/src/b.cs", "src/a.cs"}, store.calls)
	require.Equal(t, []exploreSourceLiteralHit{
		{nodeID: exactB.ID, rank: 0},
		{nodeID: exactA.ID, rank: 1},
	}, recall.hits)
	require.True(t, recall.ambiguous)
}

func TestMapExploreSourceLiteralMatchesChoosesSmallestEnclosingSymbol(t *testing.T) {
	path := "demo/src/Registry.cs"
	typeNode := sourceLiteralNode("demo/Registry.cs::Registry#type", "Registry", path, graph.KindType, 1, 80)
	method := sourceLiteralNode("demo/Registry.cs::Registry.RegisterDefaults", "RegisterDefaults", path, graph.KindMethod, 16, 35)
	closure := sourceLiteralNode("demo/Registry.cs::Registry.RegisterDefaults#closure", "closure", path, graph.KindClosure, 22, 25)
	server := newExploreSourceLiteralServer(t, []*graph.Node{typeNode, method, closure})

	recall := server.mapExploreSourceLiteralMatches("ku", []trigram.Match{{
		Path: path, Line: 23, Text: `register("ku")`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

	require.Equal(t, []exploreSourceLiteralHit{{nodeID: closure.ID, rank: 0}}, recall.hits)
}

func TestMapExploreSourceLiteralMatchesKeepsCommonLiteralNonTerminal(t *testing.T) {
	left := sourceLiteralNode("demo/left.cs::Register", "Register", "demo/left.cs", graph.KindMethod, 1, 10)
	right := sourceLiteralNode("demo/right.cs::Register", "Register", "demo/right.cs", graph.KindMethod, 1, 10)
	server := newExploreSourceLiteralServer(t, []*graph.Node{left, right})

	recall := server.mapExploreSourceLiteralMatches("shared", []trigram.Match{
		{Path: left.FilePath, Line: 4, Text: `Register("shared")`},
		{Path: right.FilePath, Line: 5, Text: `Register("shared")`},
	}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

	require.Len(t, recall.hits, 2)
	require.True(t, recall.ambiguous)
	targets := []exploreTarget{
		{node: left, exactContent: true, exactContentAmbiguous: true},
		{node: right, exactContent: true, exactContentAmbiguous: true},
	}
	require.False(t, exploreAnswerReady(`find registration for "shared"`, targets))
}

func TestMapExploreSourceLiteralMatchesHardCapsRecall(t *testing.T) {
	const total = exploreSourceLiteralRecallMaxHits + 7
	nodes := make([]*graph.Node, 0, total)
	matches := make([]trigram.Match, 0, total)
	for i := 0; i < total; i++ {
		path := fmt.Sprintf("demo/registry_%02d.cs", i)
		nodes = append(nodes, sourceLiteralNode(
			fmt.Sprintf("%s::Register", path), "Register", path, graph.KindMethod, 1, 5,
		))
		matches = append(matches, trigram.Match{Path: path, Line: 3, Text: `Register("needle")`})
	}
	server := newExploreSourceLiteralServer(t, nodes)

	recall := server.mapExploreSourceLiteralMatches(
		"needle", matches, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.Len(t, recall.hits, exploreSourceLiteralRecallMaxHits)
	require.Equal(t, exploreSourceLiteralRecallMaxHits-1, recall.hits[len(recall.hits)-1].rank)
	require.True(t, recall.ambiguous)
}

func TestMapDiscoveredExploreSourceLiteralMatchesPreservesHitsAfterDiscoveryDeadline(t *testing.T) {
	path := "demo/src/FormatterRegistry.cs"
	constructor := sourceLiteralNode(
		"demo/src/FormatterRegistry.cs::FormatterRegistry", "FormatterRegistry.<init>",
		path, graph.KindMethod, 2, 4,
	)
	server := newExploreSourceLiteralServer(t, []*graph.Node{constructor})
	search := exploreSourceLiteralSearch{
		matches: []trigram.Match{{
			Path: path, Line: 3, Text: `RegisterDefaultFormatter("ku");`,
		}},
		incomplete: true,
	}

	recall, mappingErr := server.mapDiscoveredExploreSourceLiteralMatches(
		context.Background(), "ku", search,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
		context.DeadlineExceeded,
	)

	require.NoError(t, mappingErr)
	require.Equal(t, []exploreSourceLiteralHit{{nodeID: constructor.ID, rank: 0}}, recall.hits)
	require.True(t, recall.ambiguous, "deadline-truncated discovery must remain non-terminal")
}

func TestExplorePreferredSourceLiteralReservesCompactValue(t *testing.T) {
	require.Equal(t, "ku", explorePreferredSourceLiteral([]string{"registration-key", "ku", "日本"}))
	require.Equal(t, "日本", explorePreferredSourceLiteral([]string{"registration-key", "日本"}))
	require.Empty(t, explorePreferredSourceLiteral(nil))
}

func TestExplorePreferredSourceLiteralRejectsCompactNoise(t *testing.T) {
	for _, noise := range []string{"it", "x-1", "test", "file", "true"} {
		require.Equal(t, "registration-key", explorePreferredSourceLiteral([]string{noise, "registration-key"}), noise)
	}
	// A short prose value remains searchable when it is the only literal; it
	// simply no longer displaces a more selective term.
	require.Equal(t, "test", explorePreferredSourceLiteral([]string{"test"}))
}

func TestExploreSourceLiteralFallbackUsesLongestRemainingTerm(t *testing.T) {
	require.Equal(t, "registration-key", exploreSourceLiteralFallback([]string{"ku", "name", "registration-key"}, "ku"))
	require.Empty(t, exploreSourceLiteralFallback([]string{"ku", "KU"}, "ku"))
}

func TestGatherExploreQuotedContentCandidatesMergesSourceLiteralWithExactContentNode(t *testing.T) {
	root := t.TempDir()
	rel := "src/FormatterRegistry.cs"
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("public sealed class FormatterRegistry {\n    public FormatterRegistry() {\n        Register(\"ku\", new CentralKurdishFormatter());\n    }\n}\n"), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	document := &graph.Node{
		ID: "demo/docs/locales.md::content", Name: "locales", QualName: "docs.locales",
		Kind: graph.KindFile, FilePath: "docs/locales.md", RepoPrefix: "demo",
	}
	typeNode := sourceLiteralNode("demo/src/FormatterRegistry.cs::FormatterRegistry#type", "FormatterRegistry", rel, graph.KindType, 1, 5)
	constructor := sourceLiteralNode("demo/src/FormatterRegistry.cs::FormatterRegistry", "FormatterRegistry", rel, graph.KindMethod, 2, 4)
	store.AddBatch([]*graph.Node{document, typeNode, constructor}, nil)
	counting := &quotedRecallCountingStore{
		Store: store,
		hits: map[string][]graph.ContentHit{
			"ku": {{NodeID: document.ID, FilePath: document.FilePath, Snippet: `locale "ku" reference`}},
		},
	}
	idx := indexer.New(counting, parser.NewRegistry(), config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	idx.SetFileMtimes(map[string]int64{rel: 1})
	store.AddBatch([]*graph.Node{document, typeNode, constructor}, nil)
	server := &Server{graph: counting, indexer: idx}

	candidates := server.gatherExploreQuotedContentCandidates(
		context.Background(), `find registration for "ku"`, nil, 20,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.NotNil(t, candidateByID(candidates, document.ID), "exact content evidence must be retained")
	require.NotNil(t, candidateByID(candidates, constructor.ID), "an exact non-source content hit must not suppress bounded source recall")
}

func TestGatherExploreQuotedContentCandidatesKeepsMissingCompactLiteralBesideExactPeer(t *testing.T) {
	root := t.TempDir()
	rel := "src/Registry.cs"
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("public sealed class Registry {\n    public void Configure() {\n        Register(\"xy\");\n    }\n}\n"), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, parser.NewRegistry(), config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	idx.SetFileMtimes(map[string]int64{rel: 1})

	exactPeer := sourceLiteralNode("demo/src/VisibleType.cs::VisibleType", "VisibleType", "src/VisibleType.cs", graph.KindType, 1, 2)
	configure := sourceLiteralNode("demo/src/Registry.cs::Registry.Configure", "Registry.Configure", rel, graph.KindMethod, 2, 4)
	store.AddBatch([]*graph.Node{exactPeer, configure}, nil)
	server := &Server{graph: store, indexer: idx, logger: zap.NewNop()}

	candidates := server.gatherExploreQuotedContentCandidates(
		context.Background(), `locate where "xy" is registered; "VisibleType" is contextual`,
		[]*rerank.Candidate{{Node: exactPeer, TextRank: 0, VectorRank: -1}}, 20,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.NotNil(t, candidateByID(candidates, configure.ID),
		"an exact metadata peer for one term must not suppress source recall for another quoted value")
}

func TestGatherExploreQuotedContentCandidatesSkipsSourceScanForExactOrdinaryCandidate(t *testing.T) {
	root := t.TempDir()
	rel := "src/RawRegistry.cs"
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("public sealed class RawRegistry {\n    public void Configure() { Register(\"KnownFormatter\"); }\n}\n"), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, parser.NewRegistry(), config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	idx.SetFileMtimes(map[string]int64{rel: 1})

	exact := sourceLiteralNode("demo/src/KnownFormatter.cs::KnownFormatter", "KnownFormatter", "src/KnownFormatter.cs", graph.KindType, 1, 2)
	raw := sourceLiteralNode("demo/src/RawRegistry.cs::RawRegistry.Configure", "RawRegistry.Configure", rel, graph.KindMethod, 1, 3)
	store.AddBatch([]*graph.Node{exact, raw}, nil)
	core, logs := observer.New(zap.DebugLevel)
	server := &Server{graph: store, indexer: idx, logger: zap.New(core)}

	candidates := server.gatherExploreQuotedContentCandidates(
		context.Background(), `find symbol "KnownFormatter"`,
		[]*rerank.Candidate{{Node: exact, TextRank: 0, VectorRank: -1}}, 20,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.Nil(t, candidateByID(candidates, raw.ID), "a distinct raw-source match must not be admitted after an exact ordinary hit")
	require.Zero(t,
		logs.FilterMessage("mcp: explore source literal recall").Len()+
			logs.FilterMessage("mcp: explore source literal recall incomplete").Len(),
		"the miss-only gate must avoid opening source files when ordinary retrieval is already exact",
	)
}

func TestExplorePreferredRefinementSymbolPrefersSourceLiteral(t *testing.T) {
	ordinary := &graph.Node{ID: "repo/ordinary.go::ordinary"}
	source := &graph.Node{ID: "repo/source.go::source"}
	targets := []exploreTarget{{node: ordinary}, {node: source, sourceLiteral: true}}

	require.Equal(t, source.ID, explorePreferredRefinementSymbol("locate the helper", targets))
	require.Equal(t, ordinary.ID, explorePreferredRefinementSymbol("locate the helper", targets[:1]))
}

func TestRetainExploreSourceLiteralOwnersDiversifiesFilesWithinCaps(t *testing.T) {
	recall := exploreSourceLiteralRecall{
		hits: []exploreSourceLiteralHit{
			{nodeID: "first-a", rank: 0},
			{nodeID: "first-b", rank: 1},
			{nodeID: "second", rank: 2},
			{nodeID: "third", rank: 3},
		},
		ownerFiles: map[string]string{
			"first-a": "src/first.go",
			"first-b": "src/first.go",
			"second":  "src/second.go",
			"third":   "src/third.go",
		},
	}

	hits, files, reason := retainExploreSourceLiteralOwners(recall)

	require.Equal(t, []string{"first-a", "second", "first-b"}, []string{
		hits[0].nodeID, hits[1].nodeID, hits[2].nodeID,
	})
	require.Equal(t, exploreSourceLiteralRecallMaxFilesPerTerm, files)
	require.Equal(t, "file_cap", reason)
}

func TestGatherExploreSourceLiteralRecallAggregatesCompactAnchorsAcrossLanguages(t *testing.T) {
	fixtures := []struct {
		name     string
		language string
		ext      string
		first    string
		second   string
	}{
		{
			name: "csharp", language: "csharp", ext: ".cs",
			first:  "public void First() {\n    Register(\"aa\");\n}\n",
			second: "public void Second() {\n    Register(\"aa\");\n    Register(\"bb\");\n}\n",
		},
		{
			name: "rust", language: "rust", ext: ".rs",
			first:  "fn first() {\n    register(\"aa\");\n}\n",
			second: "fn second() {\n    register(\"aa\");\n    register(\"bb\");\n}\n",
		},
		{
			name: "typescript", language: "typescript", ext: ".ts",
			first:  "function first() {\n    register(\"aa\");\n}\n",
			second: "function second() {\n    register(\"aa\");\n    register(\"bb\");\n}\n",
		},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			root := t.TempDir()
			firstRel := "src/first" + fixture.ext
			secondRel := "src/second" + fixture.ext
			for rel, source := range map[string]string{firstRel: fixture.first, secondRel: fixture.second} {
				path := filepath.Join(root, filepath.FromSlash(rel))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
			}

			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			idx := indexer.New(store, parser.NewRegistry(), config.IndexConfig{}, zap.NewNop())
			_, err = idx.IndexCtx(context.Background(), root)
			require.NoError(t, err)
			idx.SetFileMtimes(map[string]int64{firstRel: 1, secondRel: 1})

			first := sourceLiteralNode("demo/first::owner", "first", firstRel, graph.KindFunction, 1, 3)
			second := sourceLiteralNode("demo/second::owner", "second", secondRel, graph.KindFunction, 1, 4)
			first.Language = fixture.language
			second.Language = fixture.language
			store.AddBatch([]*graph.Node{first, second}, nil)
			counting := &exploreSourceLiteralCountingStore{Store: store}
			server := pinExploreSourceLiteralRecallBudget(
				&Server{graph: counting, indexer: idx, logger: zap.NewNop()},
			)

			recall := server.gatherExploreSourceLiteralRecall(
				context.Background(), []string{"aa", "bb"}, "", query.QueryOptions{},
			)
			coverage := make(map[string]map[int]struct{})
			for _, hit := range recall.hits {
				if coverage[hit.nodeID] == nil {
					coverage[hit.nodeID] = make(map[int]struct{})
				}
				coverage[hit.nodeID][hit.anchor] = struct{}{}
			}
			require.Len(t, coverage[first.ID], 1)
			require.Len(t, coverage[second.ID], 2)
			require.Len(t, recall.diagnostics, 2)
			require.Equal(t, []string{"aa", "bb"}, []string{
				recall.diagnostics[0].literal, recall.diagnostics[1].literal,
			})
			require.Equal(t, 2, recall.diagnostics[0].rawHits)
			require.Equal(t, 2, recall.diagnostics[0].mappedOwners)
			require.Equal(t, 2, recall.diagnostics[0].retainedOwners)
			require.Equal(t, 2, recall.diagnostics[0].retainedFiles)
			require.Empty(t, recall.diagnostics[0].reason)
			require.Equal(t, 1, recall.diagnostics[1].rawHits)
			require.Equal(t, 1, recall.diagnostics[1].mappedOwners)
			require.Equal(t, 1, recall.diagnostics[1].retainedOwners)
			require.Equal(t, 1, recall.diagnostics[1].retainedFiles)
			require.Empty(t, recall.diagnostics[1].reason)
			require.Zero(t, counting.allNodesCalls, "anchor aggregation must remain file-bounded")

			candidates := server.gatherExploreQuotedContentCandidates(
				context.Background(), `locate registrations for "aa" and "bb"`, nil, 20,
				query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
			)
			firstCandidate := candidateByID(candidates, first.ID)
			secondCandidate := candidateByID(candidates, second.ID)
			require.NotNil(t, firstCandidate)
			require.NotNil(t, secondCandidate)
			require.Equal(t, float64(1), firstCandidate.Signals[exploreSourceLiteralCoverageSignal])
			require.Equal(t, float64(2), secondCandidate.Signals[exploreSourceLiteralCoverageSignal])
			require.Positive(t, firstCandidate.Signals[exploreContentRecallAmbiguousSignal])
			require.Zero(t, secondCandidate.Signals[exploreContentRecallAmbiguousSignal])
		})
	}
}

func TestGatherExploreSourceLiteralRecallKeepsMultiAnchorOwnerUnderNearCapCompetitors(t *testing.T) {
	root := t.TempDir()
	competitorRel := "src/00_CompetingDefaults.cs"
	targetRel := "src/99_FormatterRegistry.cs"
	competitorSource := "public sealed class CompetingDefaults {\n"
	nodes := make([]*graph.Node, 0, 45)
	line := 2
	for i := 0; i < 44; i++ {
		term := "pl"
		if i%2 == 1 {
			term = "ku"
		}
		name := fmt.Sprintf("Candidate%02d", i)
		competitorSource += fmt.Sprintf("    public void %s() {\n        Register(\"%s\");\n    }\n", name, term)
		node := sourceLiteralNode("demo/competitors::"+name, name, competitorRel, graph.KindMethod, line, line+2)
		node.Language = "csharp"
		nodes = append(nodes, node)
		line += 3
	}
	competitorSource += "}\n"
	targetSource := "public sealed class FormatterRegistry {\n" +
		"    public void RegisterDefaultFormatter() {\n" +
		"        Register(\"pl\");\n" +
		"        Register(\"ku\");\n" +
		"    }\n" +
		"}\n"
	target := sourceLiteralNode(
		"demo/formatter::RegisterDefaultFormatter", "RegisterDefaultFormatter",
		targetRel, graph.KindMethod, 2, 5,
	)
	target.Language = "csharp"
	nodes = append(nodes, target)
	for rel, source := range map[string]string{
		competitorRel: competitorSource,
		targetRel:     targetSource,
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
	}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, parser.NewRegistry(), config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	idx.SetFileMtimes(map[string]int64{competitorRel: 1, targetRel: 1})
	store.AddBatch(nodes, nil)
	counting := &exploreSourceLiteralCountingStore{Store: store}
	// What this test pins down is the retention rule — a second-file owner
	// must survive an anchor page crowded with same-file competitors — so
	// take it off the wall clock.
	server := pinExploreSourceLiteralRecallBudget(
		&Server{graph: counting, indexer: idx, logger: zap.NewNop()},
	)
	scope := query.QueryOptions{RepoAllow: map[string]bool{"demo": true}}

	recall := server.gatherExploreSourceLiteralRecall(context.Background(), []string{"pl", "ku"}, "", scope)
	require.Len(t, recall.diagnostics, 2)
	for _, diagnostic := range recall.diagnostics {
		// Bounded grep collapses repeated lines to one hit per file. The large
		// competitor body still exercises production parsing and both per-anchor
		// mapping passes without multiplying retained owners.
		require.Equal(t, 2, diagnostic.rawHits)
		require.Equal(t, 2, diagnostic.mappedOwners)
		require.Equal(t, 2, diagnostic.retainedOwners)
		require.Equal(t, 2, diagnostic.retainedFiles)
		require.Empty(t, diagnostic.reason)
	}
	coverage := make(map[int]struct{})
	for _, hit := range recall.hits {
		if hit.nodeID == target.ID {
			coverage[hit.anchor] = struct{}{}
		}
	}
	require.Len(t, coverage, 2, "the second-file owner must survive both collision-heavy anchor pages")
	require.Zero(t, counting.allNodesCalls, "near-cap recall must remain file-bounded")

	sourceCandidates := server.gatherExploreQuotedContentCandidates(
		context.Background(), `locate default formatter registrations for "pl" and "ku"`, nil, 20, scope,
	)
	targetCandidate := candidateByID(sourceCandidates, target.ID)
	require.NotNil(t, targetCandidate)
	require.Equal(t, float64(2), targetCandidate.Signals[exploreSourceLiteralCoverageSignal])
	require.Positive(t, targetCandidate.Signals[exploreContentRecallAmbiguousSignal])
	ordinary := []*rerank.Candidate{
		sourcePreservationCandidate("semantic-0", 0, 0),
		sourcePreservationCandidate("semantic-1", 1, 0),
		sourcePreservationCandidate("semantic-2", 2, 0),
	}
	selected := selectFinalExploreCandidates(
		mergeExploreCandidates(ordinary, sourceCandidates, len(ordinary)), nil, 3,
	)
	require.NotNil(t, candidateByID(selected, target.ID), "multi-anchor owner must survive global pruning")
	require.NotNil(t, candidateByID(selected, "semantic-0"))
	require.NotNil(t, candidateByID(selected, "semantic-1"), "ambiguous single-anchor competitors must not consume the second reserve slot")
}

func TestGatherExploreSourceLiteralRecallRecordsTermCapDiagnostic(t *testing.T) {
	// "cc" must be reported as term_cap, not as a deadline the runner caused.
	server := pinExploreSourceLiteralRecallBudget(&Server{logger: zap.NewNop()})
	recall := server.gatherExploreSourceLiteralRecall(
		context.Background(), []string{"aa", "bb", "cc"}, "demo", query.QueryOptions{},
	)

	require.Len(t, recall.diagnostics, 3)
	byLiteral := make(map[string]exploreSourceLiteralDiagnostic, len(recall.diagnostics))
	for _, diagnostic := range recall.diagnostics {
		byLiteral[diagnostic.literal] = diagnostic
	}
	require.Equal(t, "term_cap", byLiteral["cc"].reason)
}

func TestGatherExploreSourceLiteralRecallMapsParsedCSharpConstructor(t *testing.T) {
	root := t.TempDir()
	rel := "src/Humanizer/Configuration/FormatterRegistry.cs"
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("public sealed class FormatterRegistry {\n    public FormatterRegistry() {\n        RegisterDefaultFormatter(\"ku\");\n    }\n\n    private void RegisterDefaultFormatter(string culture) {\n    }\n}\n"), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	idx := indexer.New(store, registry, config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	constructors := store.FindNodesByName("FormatterRegistry.<init>")
	callees := store.FindNodesByName("RegisterDefaultFormatter")
	require.NotEmpty(t, constructors)
	require.NotEmpty(t, callees)
	counting := &exploreSourceLiteralCountingStore{Store: store}
	server := pinExploreSourceLiteralRecallBudget(
		&Server{graph: counting, indexer: idx, logger: zap.NewNop()},
	)

	recall := server.gatherExploreSourceLiteralRecall(
		context.Background(), []string{"ku"}, "", query.QueryOptions{},
	)

	found := false
	for _, hit := range recall.hits {
		node := store.GetNode(hit.nodeID)
		if node != nil && node.FilePath == rel && node.Name == "RegisterDefaultFormatter" {
			found = true
			require.True(t, hit.callee, "unique call-edge promotion must retain strong provenance")
		}
	}
	require.True(t, found, "literal callsite must promote the uniquely resolved invoked method")

	task := `CultureNotFoundException for "ku" culture - code adds/uses "ku" (Kurdish) CultureInfo which is not supported on Xamarin.Android, causing crash. Find where "ku" culture is registered/used in fallback resolution logic.`
	require.Equal(t, rerank.QueryClassConcept, exploreQueryClass(shapeExploreQuery(task)),
		"long parenthesized issue prose must use concept retrieval without losing its unique literal proof")
	engine := query.NewEngine(counting)
	engine.SetSearchProvider(idx.Search)
	fullServer := pinExploreSourceLiteralRecallBudget(
		NewServer(engine, counting, idx, nil, zap.NewNop(), nil),
	)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task": task, "localize": true, "max_symbols": 10,
	}
	result, err := fullServer.handleExplore(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(mcpgo.TextContent)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(text.Text), &envelope))
	require.NotEmpty(t, envelope.Evidence)
	require.Equal(t, "RegisterDefaultFormatter", envelope.Evidence[0].Name, "invoked source evidence must lead the final localization envelope")
	require.Equal(t, localizationProvenanceSourceLiteralCallee, envelope.Evidence[0].Provenance)
	require.Equal(t, localizationStateAnswerReady, envelope.Completion.State)
	require.True(t, envelope.Terminal)
	require.True(t, envelope.Completion.Enforceable)
	require.NotNil(t, result.Meta)
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	require.True(t, ok, "initial explore must attach an authoritative host envelope")
	require.Equal(t, localizationContractFor(envelope.Completion), host.Contract)
	require.NotNil(t, host.Evidence)
	require.NotEmpty(t, host.Evidence.Evidence)
	require.Equal(t, localizationProvenanceSourceLiteralCallee, host.Evidence.Evidence[0].Provenance)
	require.Zero(t, counting.allNodesCalls, "literal mapping must stay bounded to matched files")
}

func TestExploreQuotedRecallCompactMetadataUsesDeclarationEvidenceOnly(t *testing.T) {
	scope := query.QueryOptions{}
	compactName := &graph.Node{
		ID: "src/locale.rs::KU", Name: "KU", QualName: "locale.KU",
		Kind: graph.KindFunction, FilePath: "src/locale.rs", Language: "rust",
	}
	require.True(t, exploreQuotedRecallHasExactSourceNode(`find locale "ku"`, []string{"ku"}, compactName, scope))

	pathOnly := &graph.Node{
		ID: "src/locales/ku/registry.rs::register", Name: "register", QualName: "locales.ku.register",
		Kind: graph.KindFunction, FilePath: "src/locales/ku/registry.rs", Language: "rust",
	}
	require.False(t, exploreQuotedRecallHasExactSourceNode(`find locale "ku"`, []string{"ku"}, pathOnly, scope),
		"a path-derived qualified name must not suppress bounded source recall")
}

func TestExploreQuotedRecallTestMetadataRequiresTestIntent(t *testing.T) {
	testNode := &graph.Node{
		ID: "pkg/locale_test.go::TestKurdish", Name: "TestKurdish", QualName: "tests.ku.TestKurdish",
		Kind: graph.KindFunction, FilePath: "pkg/locale_test.go", Language: "go",
		Meta: map[string]any{"is_test": true, "signature": `func TestKurdish() // "ku"`},
	}
	require.False(t, exploreQuotedRecallHasExactSourceNode(`find where locale "ku" is registered`, []string{"ku"}, testNode, query.QueryOptions{}))
	require.True(t, exploreQuotedRecallHasExactSourceNode(`find the test for locale "ku"`, []string{"ku"}, testNode, query.QueryOptions{}))
}

func TestExploreCompactLiteralIgnoresTestMetadataAndPrefersSpecificProductionCallee(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"src/Humanizer/Configuration/FormatterRegistry.cs": `namespace Humanizer.Configuration {
    internal class FormatterRegistry {
        public FormatterRegistry() { RegisterDefaultFormatter("ku"); }
        private void RegisterDefaultFormatter(string localeCode) { var formatter = new DefaultFormatter(localeCode); }
    }
    internal class DefaultFormatter { public DefaultFormatter(string localeCode) { } }
}`,
		"src/Humanizer/Configuration/NumberToWordsConverterRegistry.cs": `namespace Humanizer.Configuration {
    internal class NumberToWordsConverterRegistry {
        public NumberToWordsConverterRegistry() { Register("ku"); }
        private void Register(string localeCode) { }
    }
}`,
		"src/Humanizer.Tests.Shared/Localisation/ku/NumberToWordsTests.cs": `namespace Humanizer.Tests.Localisation.ku {
    [UseCulture("ku")]
    public class NumberToWordsTests {
        public void ToOrdinalWordsKurdish() { }
    }
}`,
	}
	for rel, source := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
	}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	idx := indexer.New(store, registry, config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	counting := &exploreSourceLiteralCountingStore{Store: store}
	server := pinExploreSourceLiteralRecallBudget(
		&Server{graph: counting, indexer: idx, logger: zap.NewNop()},
	)

	task := `CultureNotFoundException for culture "ku" (Kurdish) thrown when code enumerates or references all supported cultures, e.g. in number-to-words or collection initialization; find where "ku" locale/culture is registered or iterated causing crash on platforms lacking that culture.`
	testNodes := store.FindNodesByName("ToOrdinalWordsKurdish")
	require.NotEmpty(t, testNodes)
	candidates := server.gatherExploreQuotedContentCandidates(
		context.Background(), task,
		[]*rerank.Candidate{{Node: testNodes[0], TextRank: 0, VectorRank: -1}},
		20, query.QueryOptions{},
	)
	specificID := "src/Humanizer/Configuration/FormatterRegistry.cs::FormatterRegistry.RegisterDefaultFormatter"
	genericID := "src/Humanizer/Configuration/NumberToWordsConverterRegistry.cs::NumberToWordsConverterRegistry.Register"
	specificCandidate := candidateByID(candidates, specificID)
	genericCandidate := candidateByID(candidates, genericID)
	require.NotNil(t, specificCandidate, "test metadata must not suppress production literal recall")
	require.NotNil(t, genericCandidate, "both production direct callees must remain available")
	require.Equal(t, 1.0, specificCandidate.Signals[exploreSourceLiteralTaskAlignSignal],
		"construction intent must align with the exact owner that instantiates a value")
	require.Zero(t, genericCandidate.Signals[exploreSourceLiteralTaskAlignSignal],
		"a generic registration helper must not gain construction alignment")

	engine := query.NewEngine(counting)
	engine.SetSearchProvider(idx.Search)
	fullServer := pinExploreSourceLiteralRecallBudget(
		NewServer(engine, counting, idx, nil, zap.NewNop(), nil),
	)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task": task, "localize": true, "max_symbols": 6,
	}
	result, err := fullServer.handleExplore(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(mcpgo.TextContent)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(text.Text), &envelope))

	specificRank := -1
	for rank, evidence := range envelope.Evidence {
		if evidence.ID == specificID {
			specificRank = rank
		}
	}
	require.NotEqual(t, -1, specificRank, "construction-aligned source evidence must survive final packing")
	require.Equal(t, localizationStateAnswerReady, envelope.Completion.State)
	require.True(t, envelope.Terminal)
	require.False(t, envelope.Completion.Enforceable, "ambiguous production literal sites remain advisory")
	require.Zero(t, envelope.Completion.AllowedToolCalls)
	require.Empty(t, envelope.Completion.AllowedSymbols)
	require.Contains(t, envelope.Completion.FinalResponse, localizationBoundedHeading)
}

func TestGatherExploreSourceLiteralRecallBoundsMappingByRequestDeadline(t *testing.T) {
	root := t.TempDir()
	rel := "src/Registry.cs"
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("Register(\"needle\")\n"), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, parser.NewRegistry(), config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	idx.SetFileMtimes(map[string]int64{rel: 1})
	started := make(chan struct{}, 1)
	server := &Server{
		graph:   &exploreSourceLiteralBlockingStore{Store: store, started: started},
		indexer: idx,
		logger:  zap.NewNop(),
	}

	began := time.Now()
	recall := server.gatherExploreSourceLiteralRecall(
		context.Background(), []string{"needle"}, "", query.QueryOptions{},
	)
	elapsed := time.Since(began)

	select {
	case <-started:
	default:
		t.Fatal("mapping did not use the context-aware file-node reader")
	}
	require.Less(t, elapsed, 500*time.Millisecond, "mapping must not outlive the bounded recall budget")
	require.Empty(t, recall.hits)
	require.True(t, recall.ambiguous, "deadline-truncated mapping must remain non-terminal")
}

func TestSearchExploreSourceLiteralFallsBackWhenMultiIndexerDoesNotOwnRepo(t *testing.T) {
	root := t.TempDir()
	rel := "src/FormatterRegistry.cs"
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("Register(\"ku\")\n"), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, parser.NewRegistry(), config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	idx.SetFileMtimes(map[string]int64{rel: 1})
	multi := indexer.NewMultiIndexer(store, parser.NewRegistry(), nil, nil, zap.NewNop())
	server := &Server{graph: store, indexer: idx, multiIndexer: multi}

	search := server.searchExploreSourceLiteral(
		context.Background(), "ku", "", query.QueryOptions{},
	)

	require.Len(t, search.matches, 1)
	require.Equal(t, rel, search.matches[0].Path)
	require.False(t, search.incomplete)
	require.Equal(t, "direct", search.backend)
	require.True(t, search.owned)
}

func TestSearchExploreSourceLiteralDoesNotCrossConfiguredRepository(t *testing.T) {
	directRoot := t.TempDir()
	rel := "src/FormatterRegistry.cs"
	path := filepath.Join(directRoot, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("Register(\"ku\")\n"), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	registry := parser.NewRegistry()
	direct := indexer.New(store, registry, config.IndexConfig{}, zap.NewNop())
	_, err = direct.IndexCtx(context.Background(), directRoot)
	require.NoError(t, err)
	direct.SetFileMtimes(map[string]int64{rel: 1})

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	multi := indexer.NewMultiIndexer(store, registry, search.NewAuto(), manager, zap.NewNop())
	otherRoot := filepath.Join(t.TempDir(), "repo-a")
	require.NoError(t, os.MkdirAll(otherRoot, 0o755))
	_, err = multi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: otherRoot, Force: true})
	require.NoError(t, err)
	server := &Server{graph: store, indexer: direct, multiIndexer: multi}

	search := server.searchExploreSourceLiteral(
		context.Background(), "ku", "repo-b", query.QueryOptions{},
	)

	require.Empty(t, search.matches, "unresolved multi-repo scope must not scan the direct backend")
	require.False(t, search.incomplete)
	require.Equal(t, "multi-unresolved", search.backend)
	require.False(t, search.owned)
}

func TestGatherExploreSourceLiteralRecallDoesNotLogRawLiteral(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	server := &Server{logger: zap.New(core)}
	const secret = "customer-secret-日本"

	server.gatherExploreSourceLiteralRecall(
		context.Background(), []string{secret}, "demo", query.QueryOptions{},
	)

	entries := logs.FilterMessage("mcp: explore source literal recall incomplete").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.NotContains(t, fields, "term")
	require.EqualValues(t, 18, fields["term_runes"])
	require.NotContains(t, entries[0].Message, secret)
	require.NotContains(t, fmt.Sprint(fields), secret)
}

func BenchmarkMapExploreSourceLiteralMatches(b *testing.B) {
	path := "demo/src/FormatterRegistry.cs"
	nodes := []*graph.Node{
		sourceLiteralNode("demo/FormatterRegistry.cs::FormatterRegistry#type", "FormatterRegistry", path, graph.KindType, 8, 42),
		sourceLiteralNode("demo/FormatterRegistry.cs::FormatterRegistry", "FormatterRegistry", path, graph.KindMethod, 20, 29),
	}
	server := newExploreSourceLiteralServer(b, nodes)
	matches := []trigram.Match{{Path: path, Line: 24, Text: `Register("ku")`}}
	scope := query.QueryOptions{RepoAllow: map[string]bool{"demo": true}}
	require.Len(b, server.mapExploreSourceLiteralMatches("ku", matches, scope).hits, 1)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		server.mapExploreSourceLiteralMatches("ku", matches, scope)
	}
}
