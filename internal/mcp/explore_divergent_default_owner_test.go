package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

const monologForwardingConstructorSource = `public function __construct($filename, $maxFiles = 0, $filePermission = 0644) { parent::__construct($filename, $filePermission); }`

type divergentDefaultFixture struct {
	store                     graph.Store
	targets                   []exploreTarget
	write, baseType, baseCtor *graph.Node
	childType, childCtor      *graph.Node
}

type countingDivergentDefaultStore struct {
	graph.Store
	nodeBatchCalls int
	fileBatchCalls int
	inBatchCalls   int
	nodeBatchDelay time.Duration
}

func (s *countingDivergentDefaultStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.nodeBatchCalls++
	if s.nodeBatchDelay > 0 {
		time.Sleep(s.nodeBatchDelay)
	}
	return s.Store.GetNodesByIDs(ids)
}

func (s *countingDivergentDefaultStore) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	s.fileBatchCalls++
	return s.Store.GetFileNodesByPaths(paths)
}

func (s *countingDivergentDefaultStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.inBatchCalls++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func divergentDefaultTestSource(node *graph.Node) string {
	if node != nil && node.Name == "__construct" {
		return monologForwardingConstructorSource
	}
	return ""
}

func divergentDefaultTestNode(id string, kind graph.NodeKind, name, path, signature string) *graph.Node {
	meta := map[string]any{}
	if signature != "" {
		meta["signature"] = signature
	}
	return &graph.Node{
		ID: id, Kind: kind, Name: name, QualName: strings.TrimPrefix(id[strings.LastIndex(id, "::")+2:], "."), FilePath: path,
		Language: "php", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project", Meta: meta,
	}
}

func monologDivergentDefaultFixture() divergentDefaultFixture {
	basePath := "src/Monolog/Handler/StreamHandler.php"
	childPath := "src/Monolog/Handler/RotatingFileHandler.php"
	write := divergentDefaultTestNode(basePath+"::StreamHandler.write", graph.KindMethod, "write", basePath, "protected function write(array $record)")
	baseType := divergentDefaultTestNode(basePath+"::StreamHandler", graph.KindType, "StreamHandler", basePath, "class StreamHandler")
	baseCtor := divergentDefaultTestNode(basePath+"::StreamHandler.__construct", graph.KindMethod, "__construct", basePath,
		"public function __construct($stream, $level = null, $bubble = true, $filePermission = null)")
	childType := divergentDefaultTestNode(childPath+"::RotatingFileHandler", graph.KindType, "RotatingFileHandler", childPath,
		"class RotatingFileHandler extends StreamHandler")
	childCtor := divergentDefaultTestNode(childPath+"::RotatingFileHandler.__construct", graph.KindMethod, "__construct", childPath,
		"public function __construct($filename, $maxFiles = 0, $level = null, $bubble = true, $filePermission = 0644)")
	store := graph.New()
	store.AddBatch(
		[]*graph.Node{write, baseType, baseCtor, childType, childCtor},
		[]*graph.Edge{
			{From: childCtor.ID, To: baseCtor.ID, Kind: graph.EdgeCalls},
			{From: childType.ID, To: baseType.ID, Kind: graph.EdgeExtends},
		},
	)
	return divergentDefaultFixture{
		store: store,
		targets: []exploreTarget{
			{node: write, source: `fopen($this->url, 'a'); chmod($this->url, $this->filePermission);`, sourceLiteral: true},
			{node: baseType},
			{node: baseCtor},
		},
		write: write, baseType: baseType, baseCtor: baseCtor, childType: childType, childCtor: childCtor,
	}
}

func TestDivergentDefaultOwnerPromotesFromStoreProjectionWithoutEviction(t *testing.T) {
	task := `StreamHandler::write throws UnexpectedValueException "could not be opened: Permission denied" after upgrade, likely due to chmod and file permission handling`
	fixture := monologDivergentDefaultFixture()
	if got := rerank.ClassifyQuery(shapeExploreQuery(task)); got != rerank.QueryClassConcept {
		t.Fatalf("Monolog issue classified as %q, want concept", got)
	}
	protected := divergentDefaultTestNode("src/Protected.php::Protected.run", graph.KindMethod, "run", "src/Protected.php", "function run()")
	fixture.targets = append(fixture.targets, exploreTarget{node: protected, conceptImplementation: true})
	originalCount := len(fixture.targets)
	reads := 0
	promoted := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, len(fixture.targets), func(node *graph.Node) string {
		reads++
		require.Equal(t, fixture.childCtor.ID, node.ID)
		return monologForwardingConstructorSource
	})
	require.Len(t, promoted, originalCount, "promotion must replace the represented base pair, not append and truncate")
	require.Equal(t, fixture.childCtor.ID, promoted[0].node.ID)
	require.True(t, promoted[0].divergentDefaultOwner)
	require.Equal(t, fixture.childType.ID, promoted[1].node.ID)
	require.True(t, promoted[1].divergentDefaultType)
	require.Equal(t, fixture.write.ID, promoted[2].node.ID)
	require.Equal(t, protected.ID, promoted[3].node.ID, "unrelated protected candidate was evicted")
	require.Equal(t, 1, reads)
	require.Equal(t, fixture.childCtor.ID, explorePreferredRefinementSymbol(task, promoted))

	promoted = materializeExploreStructuralSourceWithReader(
		context.Background(), task, promoted,
		query.QueryOptions{WorkspaceID: "workspace", ProjectID: "project", RepoAllow: map[string]bool{"repo": true}},
		func(_ context.Context, _ *graph.Node) string {
			reads++
			return "unexpected second read"
		},
	)
	require.Equal(t, 1, reads, "proof source must be reused by materialization")

	completion := newLocalizationRefinementCompletion(fixture.childCtor.ID)
	result, _, _ := buildLocalizationExploreResultForTask(completion, task, promoted, exploreDefaultBudgetTokens)
	body, ok := singleTextContent(result)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	require.GreaterOrEqual(t, len(envelope.Files), 3)
	// The prescribed causal owner leads a refinement response, followed by its
	// owning type and the task-named consumer. Files, Symbols, and Evidence keep
	// this exact positional order.
	require.Equal(t, fixture.childCtor.FilePath, envelope.Files[0])
	require.Equal(t, fixture.childType.FilePath, envelope.Files[1])
	require.Equal(t, fixture.write.FilePath, envelope.Files[2])
	require.GreaterOrEqual(t, len(envelope.Symbols), 3)
	require.Equal(t, []string{fixture.childCtor.ID, fixture.childType.ID, fixture.write.ID}, envelope.Symbols[:3])
}

func TestDivergentDefaultOwnerRecoversBasePairFromRankedCallableOwner(t *testing.T) {
	task := `StreamHandler::write reports "could not be opened: Permission denied" after a rotating handler applies chmod; find the divergent filePermission default and owning type`
	fixture := monologDivergentDefaultFixture()
	fixture.targets = fixture.targets[:1]
	counted := &countingDivergentDefaultStore{Store: fixture.store}
	reads := 0
	promoted := promoteExploreDivergentDefaultOwner(task, fixture.targets, counted, 3, func(node *graph.Node) string {
		reads++
		require.Equal(t, fixture.childCtor.ID, node.ID)
		return monologForwardingConstructorSource
	})
	require.Len(t, promoted, 3)
	require.Equal(t, []string{fixture.childCtor.ID, fixture.childType.ID, fixture.write.ID}, []string{
		promoted[0].node.ID, promoted[1].node.ID, promoted[2].node.ID,
	})
	require.Equal(t, 2, counted.nodeBatchCalls, "owner discovery and endpoint hydration must each be batched")
	require.Equal(t, 1, counted.fileBatchCalls)
	require.Equal(t, 1, counted.inBatchCalls)
	require.Equal(t, 1, reads)
}

func TestDivergentDefaultOwnerAdmissionAtCapacityEvictsOnlyUnprotectedTail(t *testing.T) {
	task := `StreamHandler::write reports "could not be opened: Permission denied" after a rotating handler applies chmod; find the divergent filePermission default and owning type`
	fixture := monologDivergentDefaultFixture()
	distractorOne := divergentDefaultTestNode("src/Noise.php::Noise.one", graph.KindMethod, "one", "src/Noise.php", "function one()")
	distractorTwo := divergentDefaultTestNode("src/Noise.php::Noise.two", graph.KindMethod, "two", "src/Noise.php", "function two()")
	fixture.targets = []exploreTarget{{node: fixture.write}, {node: distractorOne}, {node: distractorTwo}}

	promoted := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, len(fixture.targets), divergentDefaultTestSource)
	require.Len(t, promoted, len(fixture.targets))
	require.Equal(t, []string{fixture.childCtor.ID, fixture.childType.ID, fixture.write.ID}, []string{
		promoted[0].node.ID, promoted[1].node.ID, promoted[2].node.ID,
	})

	protectedLiteral := exploreTarget{node: distractorOne, sourceLiteral: true}
	protectedImplementation := exploreTarget{node: distractorTwo, conceptImplementation: true}
	fixture.targets = []exploreTarget{{node: fixture.write}, protectedLiteral, protectedImplementation}
	unchanged := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, len(fixture.targets), divergentDefaultTestSource)
	require.Equal(t, []string{fixture.write.ID, distractorOne.ID, distractorTwo.ID}, []string{
		unchanged[0].node.ID, unchanged[1].node.ID, unchanged[2].node.ID,
	})
	require.True(t, unchanged[1].sourceLiteral)
	require.True(t, unchanged[2].conceptImplementation)
}

func TestDivergentDefaultOwnerCallableFallbackFailsClosed(t *testing.T) {
	task := `StreamHandler::write reports "could not be opened: Permission denied" after a rotating handler applies chmod; find the divergent filePermission default and owning type`
	t.Run("no output capacity", func(t *testing.T) {
		fixture := monologDivergentDefaultFixture()
		fixture.targets = fixture.targets[:1]
		got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, 2, divergentDefaultTestSource)
		require.Equal(t, []exploreTarget{fixture.targets[0]}, got)
	})
	t.Run("irrelevant ranked callable", func(t *testing.T) {
		fixture := monologDivergentDefaultFixture()
		fixture.targets = fixture.targets[:1]
		got := promoteExploreDivergentDefaultOwner("Investigate queue retry scheduling and backoff policy", fixture.targets, fixture.store, 3, divergentDefaultTestSource)
		require.Equal(t, fixture.write.ID, got[0].node.ID)
		require.Len(t, got, 1)
	})
	t.Run("ambiguous constructors for exact owner", func(t *testing.T) {
		fixture := monologDivergentDefaultFixture()
		fixture.targets = fixture.targets[:1]
		alternate := divergentDefaultTestNode(
			fixture.baseType.FilePath+"::StreamHandler.StreamHandler",
			graph.KindMethod,
			"StreamHandler",
			fixture.baseType.FilePath,
			`function StreamHandler($filePermission = null)`,
		)
		fixture.store.AddNode(alternate)
		got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, 3, divergentDefaultTestSource)
		require.Equal(t, fixture.write.ID, got[0].node.ID)
		require.Len(t, got, 1)
	})
	t.Run("oversized owner file", func(t *testing.T) {
		fixture := monologDivergentDefaultFixture()
		fixture.targets = fixture.targets[:1]
		for index := 0; index < exploreDefaultOwnerFileNodeCap; index++ {
			fixture.store.AddNode(divergentDefaultTestNode(
				fmt.Sprintf("%s::StreamHandler.helper%02d", fixture.baseType.FilePath, index),
				graph.KindMethod,
				fmt.Sprintf("helper%02d", index),
				fixture.baseType.FilePath,
				"function helper()",
			))
		}
		got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, 3, divergentDefaultTestSource)
		require.Equal(t, fixture.write.ID, got[0].node.ID)
		require.Len(t, got, 1)
	})
	t.Run("owner hydration exceeds admission budget", func(t *testing.T) {
		fixture := monologDivergentDefaultFixture()
		fixture.targets = fixture.targets[:1]
		counted := &countingDivergentDefaultStore{Store: fixture.store, nodeBatchDelay: exploreDefaultOwnerFallbackSLO + time.Millisecond}
		got := promoteExploreDivergentDefaultOwner(task, fixture.targets, counted, 3, divergentDefaultTestSource)
		require.Equal(t, fixture.write.ID, got[0].node.ID)
		require.Len(t, got, 1)
		require.Equal(t, 1, counted.nodeBatchCalls)
		require.Zero(t, counted.fileBatchCalls)
	})
}

func TestDivergentDefaultOwnerFailsClosedOnMissingOrInvalidStoreEvidence(t *testing.T) {
	task := "permission denied after the rotating file handler changed its default permission"
	tests := []struct {
		name   string
		mutate func(divergentDefaultFixture)
	}{
		{"missing call edge", func(f divergentDefaultFixture) { f.store.RemoveEdge(f.childCtor.ID, f.baseCtor.ID, graph.EdgeCalls) }},
		{"missing extends edge", func(f divergentDefaultFixture) { f.store.RemoveEdge(f.childType.ID, f.baseType.ID, graph.EdgeExtends) }},
		{"neutral child default", func(f divergentDefaultFixture) {
			f.childCtor.Meta["signature"] = `function __construct($filePermission = null)`
		}},
		{"irrelevant parameter", func(f divergentDefaultFixture) {
			f.baseCtor.Meta["signature"] = `function __construct($retryCount = null)`
			f.childCtor.Meta["signature"] = `function __construct($retryCount = 3)`
		}},
		{"concrete base default", func(f divergentDefaultFixture) {
			f.baseCtor.Meta["signature"] = `function __construct($filePermission = 0600)`
		}},
		{"cross-repository child", func(f divergentDefaultFixture) {
			f.childCtor.RepoPrefix = "other"
			f.childType.RepoPrefix = "other"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := monologDivergentDefaultFixture()
			test.mutate(fixture)
			got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, len(fixture.targets), divergentDefaultTestSource)
			require.Equal(t, fixture.write.ID, got[0].node.ID)
			require.False(t, got[0].divergentDefaultOwner)
		})
	}
}

func TestDivergentDefaultOwnerRejectsAmbiguousOrOversizedInboundProjection(t *testing.T) {
	task := "permission denied after the rotating file handler changed its default permission"
	t.Run("ambiguous", func(t *testing.T) {
		fixture := monologDivergentDefaultFixture()
		secondType := divergentDefaultTestNode("src/Weekly.php::WeeklyFileHandler", graph.KindType, "WeeklyFileHandler", "src/Weekly.php", "class WeeklyFileHandler extends StreamHandler")
		secondCtor := divergentDefaultTestNode("src/Weekly.php::WeeklyFileHandler.__construct", graph.KindMethod, "__construct", secondType.FilePath, `function __construct($filePermission = 0660)`)
		fixture.store.AddBatch([]*graph.Node{secondType, secondCtor}, []*graph.Edge{
			{From: secondCtor.ID, To: fixture.baseCtor.ID, Kind: graph.EdgeCalls},
			{From: secondType.ID, To: fixture.baseType.ID, Kind: graph.EdgeExtends},
		})
		got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, len(fixture.targets), divergentDefaultTestSource)
		require.Equal(t, fixture.write.ID, got[0].node.ID)
	})
	t.Run("edge cap", func(t *testing.T) {
		fixture := monologDivergentDefaultFixture()
		nodes := make([]*graph.Node, 0, exploreDefaultOwnerEdgeCap)
		edges := make([]*graph.Edge, 0, exploreDefaultOwnerEdgeCap)
		for index := 0; index < exploreDefaultOwnerEdgeCap; index++ {
			node := divergentDefaultTestNode(fmt.Sprintf("src/Extra%02d.php::Extra%02d.__construct", index, index), graph.KindMethod, "__construct", fmt.Sprintf("src/Extra%02d.php", index), `function __construct($filePermission = 0600)`)
			nodes = append(nodes, node)
			edges = append(edges, &graph.Edge{From: node.ID, To: fixture.baseCtor.ID, Kind: graph.EdgeCalls})
		}
		fixture.store.AddBatch(nodes, edges)
		got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, len(fixture.targets), divergentDefaultTestSource)
		require.Equal(t, fixture.write.ID, got[0].node.ID)
	})
}

func TestDivergentDefaultOrderSurvivesOwnerFolding(t *testing.T) {
	fixture := monologDivergentDefaultFixture()
	targets := []exploreTarget{
		{node: fixture.childType, divergentDefaultType: true},
		{node: fixture.write},
		{node: fixture.childCtor, divergentDefaultOwner: true},
	}
	got := preserveExploreDivergentDefaultOrder(targets)
	require.Equal(t, []string{fixture.childCtor.ID, fixture.childType.ID, fixture.write.ID}, []string{got[0].node.ID, got[1].node.ID, got[2].node.ID})
}

func TestParseExploreParameterDefaultsAcrossLanguages(t *testing.T) {
	tests := []struct {
		name, signature, parameter, value string
		neutral                           bool
	}{
		{"php", `function __construct($filePermission = null)`, "filePermission", "null", true},
		{"python", `def __init__(self, file_permission: int | None = None)`, "file_permission", "None", true},
		{"typescript", `constructor(filePermission: number | null = 0o644)`, "filePermission", "0o644", false},
		{"csharp", `Handler(int? filePermission = 420)`, "filePermission", "420", false},
		{"kotlin", `constructor(filePermission: Int? = null)`, "filePermission", "null", true},
		{"ruby", `def initialize(file_permission = 0o644)`, "file_permission", "0o644", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defaults := parseExploreParameterDefaults(test.signature)
			got, found := exploreParameterDefaultByName(defaults, test.parameter)
			require.True(t, found)
			require.Equal(t, test.value, got.value)
			require.Equal(t, test.neutral, isNeutralExploreDefault(got.value))
			require.NotEqual(t, test.neutral, isConcreteExploreDefault(got.value))
		})
	}
}

func TestDivergentDefaultOwnerRequiresExecutableForwardingIntoProvenBaseCall(t *testing.T) {
	task := "permission denied after the rotating file handler changed its default permission"
	negative := []string{
		`function __construct($filePermission = 0644) { $this->filePermission = $filePermission; parent::__construct($filename); }`,
		`function __construct($filePermission = 0644) { helper($filePermission); parent::__construct($filename); }`,
		`function __construct($filePermission = 0644) { /* parent::__construct($filePermission); */ parent::__construct($filename); }`,
		`function __construct($filePermission = 0644) { throw new Exception("parent::__construct($filePermission)"); }`,
		`function __construct($filePermission = 0644) { parent::__construct($filename, "$filePermission"); }`,
		`function __construct($filePermission = 0644) { parent::__construct($filename /* $filePermission */); }`,
		"",
	}
	for _, source := range negative {
		fixture := monologDivergentDefaultFixture()
		got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, len(fixture.targets), func(*graph.Node) string { return source })
		require.Equal(t, fixture.write.ID, got[0].node.ID, "non-executable forwarding was accepted: %q", source)
	}
}

func TestConstructorForwardingProofAcrossLanguages(t *testing.T) {
	tests := []struct{ name, source, parameter, base string }{
		{"php", `parent::__construct($path, $filePermission);`, "filePermission", "StreamHandler"},
		{"python", `super().__init__(path, file_permission)`, "file_permission", "StreamHandler"},
		{"typescript", `super(path, filePermission);`, "filePermission", "StreamHandler"},
		{"csharp", `RotatingHandler(string path, int permission = 420) : base(path, permission) {}`, "permission", "StreamHandler"},
		{"kotlin", `class Rotating(path: String, permission: Int = 420) : StreamHandler(path, permission)`, "permission", "StreamHandler"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.True(t, exploreConstructorForwardsParameter(test.source, test.parameter, test.base))
		})
	}
}

func TestHandleExplorePromotesDivergentDefaultOwnerFromSQLitePHPIndex(t *testing.T) {
	server, store := newPHPDivergentDefaultServer(t)
	write := requirePHPFixtureNode(t, store, "StreamHandler.php", "write", graph.KindMethod)
	baseType := requirePHPFixtureNode(t, store, "StreamHandler.php", "StreamHandler", graph.KindType)
	baseCtor := requirePHPFixtureNode(t, store, "StreamHandler.php", "__construct", graph.KindMethod)
	childType := requirePHPFixtureNode(t, store, "RotatingFileHandler.php", "RotatingFileHandler", graph.KindType)
	childCtor := requirePHPFixtureNode(t, store, "RotatingFileHandler.php", "__construct", graph.KindMethod)
	baseContract := requirePHPFixtureNode(t, store, "StreamHandler.php", "BaseContract", graph.KindInterface)
	childContract := requirePHPFixtureNode(t, store, "StreamHandler.php", "ChildContract", graph.KindInterface)
	serializableContract := requirePHPFixtureNode(t, store, "StreamHandler.php", "SerializableContract", graph.KindInterface)
	statusType := requirePHPFixtureNode(t, store, "StreamHandler.php", "HandlerStatus", graph.KindType)
	require.Contains(t, baseCtor.RetrievalMetadata().Signature, "filePermission")
	require.Contains(t, childCtor.RetrievalMetadata().Signature, "0644")
	require.True(t, hasFixtureEdge(store.GetInEdges(baseCtor.ID), childCtor.ID, baseCtor.ID, graph.EdgeCalls), "PHP index must resolve child ctor -> base ctor")
	require.True(t, hasFixtureEdge(store.GetInEdges(baseType.ID), childType.ID, baseType.ID, graph.EdgeExtends), "PHP index must resolve child type -> base type; child out=%v base in=%v", fixtureEdgeStrings(store.GetOutEdges(childType.ID)), fixtureEdgeStrings(store.GetInEdges(baseType.ID)))
	require.True(t, hasFixtureEdge(store.GetInEdges(baseContract.ID), childContract.ID, baseContract.ID, graph.EdgeExtends), "PHP index must resolve interface inheritance")
	require.True(t, hasFixtureEdge(store.GetInEdges(serializableContract.ID), statusType.ID, serializableContract.ID, graph.EdgeImplements), "PHP index must resolve enum conformance")

	task := `StreamHandler::write reports "could not be opened: Permission denied" after a rotating handler applies chmod; find the divergent filePermission default and owning type`
	direct := promoteExploreDivergentDefaultOwner(task, []exploreTarget{{node: write}, {node: baseType}, {node: baseCtor}}, store, 3, func(node *graph.Node) string {
		return server.manifestSymbolSource(context.Background(), node)
	})
	require.Equal(t, childCtor.ID, direct[0].node.ID, "real store projection did not promote child constructor")
	fallback := promoteExploreDivergentDefaultOwner(task, []exploreTarget{{node: write}}, store, 3, func(node *graph.Node) string {
		return server.manifestSymbolSource(context.Background(), node)
	})
	require.Equal(t, childCtor.ID, fallback[0].node.ID, "real store callable-owner fallback did not promote child constructor")

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "explore"
	req.Params.Arguments = map[string]any{"task": task, "localize": true, "max_symbols": 30, "token_budget": 2400}
	ctx := WithSessionID(context.Background(), "php_divergent_default_contract")
	result, err := server.handleExplore(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	body, ok := singleTextContent(result)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &envelope), body)
	require.GreaterOrEqual(t, len(envelope.Symbols), 2, body)
	require.Equal(t, childCtor.ID, envelope.Symbols[0], body)
	require.Equal(t, childType.ID, envelope.Symbols[1], body)
	require.NotEmpty(t, envelope.Files)
	require.True(t, strings.HasSuffix(filepath.ToSlash(envelope.Files[0]), "RotatingFileHandler.php"), body)
	require.Equal(t, envelope.Completion.State == localizationStateAnswerReady, envelope.Terminal)
	provenance := make(map[string]string, len(envelope.Evidence))
	for _, row := range envelope.Evidence {
		provenance[row.ID] = row.Provenance
	}
	require.Equal(t, localizationProvenanceDivergentDefault, provenance[childCtor.ID])
	require.Equal(t, localizationProvenanceDivergentDefaultType, provenance[childType.ID])
	requireLocalizationHostContractMatchesVisible(t, result, envelope)

	switch envelope.Completion.State {
	case localizationStateAnswerReady:
		require.True(t, envelope.Terminal)
		require.True(t, envelope.Completion.Enforceable)
	case localizationStateNeedsExactRead:
		require.False(t, envelope.Terminal)
		require.False(t, envelope.Completion.Enforceable)
		require.Equal(t, childCtor.ID, envelope.Completion.ExactSymbol)
		readRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
			Name: "read",
			Arguments: map[string]any{
				"operation": "source",
				"target":    map[string]any{"symbol": childCtor.ID},
			},
		}}
		readResult, readErr := server.handleFacade(ctx, "read", readRequest)
		require.NoError(t, readErr)
		require.NotNil(t, readResult)
		require.False(t, readResult.IsError)
		readBody, readOK := singleTextContent(readResult)
		require.True(t, readOK)
		require.Contains(t, readBody, `"state":"answer_ready"`)
		require.Contains(t, readBody, `"terminal":true`)
		require.Contains(t, readBody, `"enforceable":true`)
		require.NotNil(t, readResult.Meta)
		readHost, readHostOK := readResult.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
		require.True(t, readHostOK)
		require.True(t, readHost.Contract.Terminal)
		require.True(t, readHost.Contract.Completion.Enforceable)
	default:
		t.Fatalf("divergent-default proof returned unexpected completion: %#v", envelope.Completion)
	}
}

func newPHPDivergentDefaultServer(t *testing.T) (*Server, graph.Store) {
	t.Helper()
	root := t.TempDir()
	handlerDir := filepath.Join(root, "src", "Monolog", "Handler")
	require.NoError(t, os.MkdirAll(handlerDir, 0o755))
	stream := `<?php
namespace Monolog\Handler;
interface BaseContract {}
interface ChildContract extends BaseContract {}
interface SerializableContract {}
enum HandlerStatus implements SerializableContract { case Ready; }
/** Stream handler that opens files and applies the configured file permission. */
class StreamHandler {
    /** Configure the neutral file permission default later consumed by write. */
    public function __construct($stream, $level = null, $bubble = true, $filePermission = null) {}
    protected function write(array $record): void {
        chmod($record['path'], $this->filePermission);
        throw new \UnexpectedValueException('could not be opened: Permission denied');
    }
}
`
	rotating := `<?php
namespace Monolog\Handler;
/** Rotating handler with a concrete file permission constructor default. */
class RotatingFileHandler extends StreamHandler {
    /** Forward the concrete file permission default into the stream handler. */
    public function __construct($filename, $maxFiles = 0, $level = null, $bubble = true, $filePermission = 0644) {
        parent::__construct($filename, $level, $bubble, $filePermission);
    }
}
`
	require.NoError(t, os.WriteFile(filepath.Join(handlerDir, "StreamHandler.php"), []byte(stream), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(handlerDir, "RotatingFileHandler.php"), []byte(rotating), 0o644))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cfg := config.Default()
	idx := indexer.New(store, testRegistry(), cfg.Index, zap.NewNop())
	_, err = idx.Index(root)
	require.NoError(t, err)
	server := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil)
	return server, store
}

func requirePHPFixtureNode(t *testing.T, store graph.Store, fileSuffix, name string, kind graph.NodeKind) *graph.Node {
	t.Helper()
	for _, node := range store.FindNodesByName(name) {
		if node != nil && node.Kind == kind && strings.HasSuffix(filepath.ToSlash(node.FilePath), fileSuffix) {
			return node
		}
	}
	t.Fatalf("missing PHP fixture node %s %s in %s", kind, name, fileSuffix)
	return nil
}

func hasFixtureEdge(edges []*graph.Edge, from, to string, kind graph.EdgeKind) bool {
	for _, edge := range edges {
		if edge != nil && edge.From == from && edge.To == to && edge.Kind == kind {
			return true
		}
	}
	return false
}

func fixtureEdgeStrings(edges []*graph.Edge) []string {
	result := make([]string, 0, len(edges))
	for _, edge := range edges {
		if edge != nil {
			result = append(result, fmt.Sprintf("%s --%s--> %s", edge.From, edge.Kind, edge.To))
		}
	}
	return result
}

func BenchmarkPromoteExploreDivergentDefaultOwner24(b *testing.B) {
	fixture := monologDivergentDefaultFixture()
	for index := len(fixture.targets); index < 24; index++ {
		node := divergentDefaultTestNode(fmt.Sprintf("src/Distractor%02d.php::Distractor%02d.run", index, index), graph.KindMethod, "run", fmt.Sprintf("src/Distractor%02d.php", index), "function run($value = null)")
		fixture.targets = append(fixture.targets, exploreTarget{node: node})
	}
	task := "permission denied after the rotating file handler changed its default permission"
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, len(fixture.targets), divergentDefaultTestSource); !got[0].divergentDefaultOwner {
			b.Fatal("expected promotion")
		}
	}
}

func BenchmarkPromoteExploreDivergentDefaultOwnerFromCallable24(b *testing.B) {
	fixture := monologDivergentDefaultFixture()
	fixture.targets = fixture.targets[:1]
	for index := len(fixture.targets); index < 24; index++ {
		node := divergentDefaultTestNode(fmt.Sprintf("src/Distractor%02d.php::Distractor%02d.run", index, index), graph.KindMethod, "run", fmt.Sprintf("src/Distractor%02d.php", index), "function run($value = null)")
		fixture.targets = append(fixture.targets, exploreTarget{node: node})
	}
	task := `StreamHandler::write reports "could not be opened: Permission denied" after a rotating handler applies chmod; find the divergent filePermission default and owning type`
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if got := promoteExploreDivergentDefaultOwner(task, fixture.targets, fixture.store, 26, divergentDefaultTestSource); !got[0].divergentDefaultOwner {
			b.Fatal("expected promotion")
		}
	}
}
