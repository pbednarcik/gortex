package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

func requireLocalizationHostContractMatchesVisible(
	t *testing.T,
	result *mcpgo.CallToolResult,
	envelope localizationExploreEnvelope,
) localizationHostEnvelope {
	t.Helper()
	require.NotNil(t, result)
	require.NotNil(t, result.Meta)
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	require.True(t, ok, "authoritative localization metadata is missing")
	visible := localizationTerminalContract{Completion: envelope.Completion, Terminal: envelope.Terminal}
	visibleJSON, err := json.Marshal(visible)
	require.NoError(t, err)
	hostJSON, err := json.Marshal(host.Contract)
	require.NoError(t, err)
	require.JSONEq(t, string(visibleJSON), string(hostJSON))
	return host
}

func TestLocalizationEvidencePolicyHoldsWeakOwnerForOneBoundedCallFromSQLiteCSharpIndex(t *testing.T) {
	root := t.TempDir()
	rel := "src/Humanizer/Localisation/Localiser.cs"
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`public sealed class Localiser {
    public string FindLocaliser(string culture) {
        return culture == "ku" ? "fallback" : culture;
    }
}
`), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	idx := indexer.New(store, registry, config.IndexConfig{}, zap.NewNop())
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)

	owners := store.FindNodesByName("FindLocaliser")
	require.NotEmpty(t, owners)
	var owner *graph.Node
	for _, candidate := range owners {
		if candidate != nil && candidate.FilePath == rel && candidate.Kind == graph.KindMethod {
			owner = candidate
			break
		}
	}
	require.NotNil(t, owner)

	server := pinExploreSourceLiteralRecallBudget(
		&Server{graph: store, indexer: idx, logger: zap.NewNop()},
	)
	recall := server.gatherExploreSourceLiteralRecall(
		context.Background(), []string{"ku"}, "", query.QueryOptions{},
	)
	var hit *exploreSourceLiteralHit
	for index := range recall.hits {
		if recall.hits[index].nodeID == owner.ID {
			hit = &recall.hits[index]
			break
		}
	}
	require.NotNil(t, hit)
	require.False(t, hit.callee, "an enclosing owner without a resolved call edge is advisory")

	target := exploreTarget{
		node:          owner,
		source:        server.manifestSymbolSource(context.Background(), owner),
		exactContent:  true,
		sourceLiteral: true,
	}
	result, _, _, completion := buildLocalizationExploreResultForTaskFinalized(
		newLocalizationCompletion(true, ""), `FindLocaliser handles the literal "ku"`,
		[]exploreTarget{target}, exploreDefaultBudgetTokens,
	)
	// An advisory owner (no resolved call edge) neither proves the answer nor
	// ends navigation: the page keeps its one bounded call.
	require.Equal(t, localizationStateNeedsRecovery, completion.State)
	require.False(t, completion.Enforceable)
	require.Equal(t, "recover_once", completion.RequiredAction)
	require.Equal(t, 1, completion.AllowedToolCalls)
	require.Equal(t, localizationRecoveryOperations, completion.AllowedOperations)

	body, ok := singleTextContent(result)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	require.False(t, envelope.Terminal)
	require.Equal(t, localizationStateNeedsRecovery, envelope.Completion.State)
	require.False(t, envelope.Completion.Enforceable)
	require.Len(t, envelope.Evidence, 1)
	require.Empty(t, envelope.Evidence[0].Provenance)

	wire, err := json.Marshal(result)
	require.NoError(t, err)
	var roundTrip struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	require.NoError(t, json.Unmarshal(wire, &roundTrip))
	var host localizationHostEnvelope
	require.NoError(t, json.Unmarshal(roundTrip.Meta[localizationHostMetaKey], &host))
	require.Equal(t, localizationContractFor(envelope.Completion), host.Contract)
}

func TestLocalizationEvidencePolicyKeepsCompleteImplementationRouteAfterPacking(t *testing.T) {
	wrapper := &graph.Node{
		ID: "repo/src/replace.rs::Replacer.replace", Name: "replace",
		Kind: graph.KindMethod, FilePath: "src/replace.rs",
	}
	implementation := &graph.Node{
		ID: "repo/src/replace.rs::Replacer.replace_all", Name: "replace_all",
		Kind: graph.KindMethod, FilePath: "src/replace.rs",
	}
	targets := []exploreTarget{
		{
			node: wrapper, directCalleesComplete: true,
			source:  "fn replace(&self, text: &str) -> String { self.replace_all(text) }",
			callees: []*graph.Node{implementation},
		},
		{
			node: implementation,
			source: `fn replace_all(&self, text: &str) -> String {
				for capture in self.captures(text) { output.push_str(capture); }
				output
			}`,
		},
	}
	routes := exploreLocalizationRefinementRoutes(targets)
	result, completion, bounded, _ := buildLocalizationRefinementResultForTask(
		implementation.ID, "find the replacement implementation", targets,
		exploreDefaultBudgetTokens, routes,
	)
	if completion.State != localizationStateAnswerReady {
		t.Fatalf("packed implementation route did not conclude in one call: completion=%#v bounded=%#v", completion, bounded)
	}
	require.False(t, completion.Enforceable,
		"a task-aligned body may conclude localization without upgrading bounded evidence to hard proof")
	require.Zero(t, completion.AllowedToolCalls)
	require.True(t, bounded[implementation.ID].enforceable)
	require.Equal(t, wrapper.ID, bounded[implementation.ID].proofSymbol)

	body, ok := singleTextContent(result)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	require.True(t, envelope.Terminal)
	require.Equal(t, localizationStateAnswerReady, envelope.Completion.State)
	require.False(t, envelope.Completion.Enforceable)
	require.Contains(t, envelope.Completion.FinalResponse, localizationBoundedHeading)
	provenance := make(map[string]string, len(envelope.Evidence))
	for _, row := range envelope.Evidence {
		provenance[row.ID] = row.Provenance
	}
	require.Equal(t, localizationProvenanceImplementationRoute, provenance[wrapper.ID])
	require.Equal(t, localizationProvenanceImplementationTarget, provenance[implementation.ID])
	requireLocalizationHostContractMatchesVisible(t, result, envelope)
}

func TestLocalizationEvidencePolicyPrefersImplementationRouteForMixedStrongProofs(t *testing.T) {
	wrapper := &graph.Node{
		ID: "repo/src/replace.rs::Replacer.replace", Name: "replace",
		Kind: graph.KindMethod, FilePath: "src/replace.rs",
	}
	implementation := &graph.Node{
		ID: "repo/src/replace.rs::Replacer.replace_all", Name: "replace_all",
		Kind: graph.KindMethod, FilePath: "src/replace.rs",
	}
	targets := []exploreTarget{
		{
			node: wrapper, directCalleesComplete: true,
			source:  "fn replace(&self, text: &str) -> String { self.replace_all(text) }",
			callees: []*graph.Node{implementation},
		},
		{
			node: implementation,
			source: `fn replace_all(&self, text: &str) -> String {
				for capture in self.captures(text) { output.push_str(capture); }
				output
			}`,
			sourceLiteral:       true,
			sourceLiteralCallee: true,
			exactContent:        true,
		},
	}
	routes := exploreLocalizationRefinementRoutes(targets)
	require.True(t, routes[implementation.ID].enforceable)
	require.Equal(t, wrapper.ID, routes[implementation.ID].proofSymbol)

	result, completion, bounded, _ := buildLocalizationRefinementResultForTask(
		implementation.ID, "find the replacement implementation", targets,
		exploreDefaultBudgetTokens, routes,
	)
	require.True(t, bounded[implementation.ID].enforceable)
	require.Equal(t, localizationStateAnswerReady, completion.State)
	require.Zero(t, completion.AllowedToolCalls)
	require.False(t, completion.Enforceable)
	body, ok := singleTextContent(result)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	require.True(t, envelope.Terminal)
	require.Contains(t, envelope.Completion.FinalResponse, localizationBoundedHeading)
	provenance := make(map[string]string, len(envelope.Evidence))
	for _, row := range envelope.Evidence {
		provenance[row.ID] = row.Provenance
	}
	require.Equal(t, localizationProvenanceImplementationRoute, provenance[wrapper.ID])
	require.Equal(t, localizationProvenanceImplementationTarget, provenance[implementation.ID])
}

func TestTypedAnchorProvenanceDoesNotOverrideStrongerEvidenceRoles(t *testing.T) {
	node := &graph.Node{
		ID: "repo/src/replace.rs::Replacer.replace_all", Name: "replace_all",
		Kind: graph.KindMethod, FilePath: "src/replace.rs",
	}
	target := exploreTarget{
		node: node, source: "fn replace_all() {}",
		typedAnchorProjection: true,
		sourceLiteral:         true, sourceLiteralCallee: true, exactContent: true,
	}
	if got := localizationTargetProvenance(newLocalizationCompletion(true, ""), target); got != localizationProvenanceSourceLiteralCallee {
		t.Fatalf("source-literal provenance = %q, want %q", got, localizationProvenanceSourceLiteralCallee)
	}

	completion := newLocalizationRefinementCompletionForSymbols(node.ID, []string{node.ID})
	completion.refinementRoutes = map[string]localizationRefinementRoute{
		node.ID: {enforceable: true, implementationSymbol: "repo/src/replace.rs::Replacer.replace_impl"},
	}
	if got := localizationTargetProvenance(completion, target); got != localizationProvenanceImplementationRoute {
		t.Fatalf("implementation-route provenance = %q, want %q", got, localizationProvenanceImplementationRoute)
	}
}

func TestLocalizationEvidencePolicyRequiresEveryPackedProofRole(t *testing.T) {
	owner := &graph.Node{
		ID: "repo/RotatingFileHandler.php::__construct", Name: "__construct",
		Kind: graph.KindMethod, FilePath: "src/RotatingFileHandler.php",
	}
	ownerTarget := exploreTarget{
		node: owner, source: "public function __construct($filePermission = 0644) {}",
		divergentDefaultOwner: true,
	}
	typeNode := &graph.Node{
		ID: "repo/RotatingFileHandler.php::RotatingFileHandler", Name: "RotatingFileHandler",
		Kind: graph.KindType, FilePath: "src/RotatingFileHandler.php",
	}
	typeTarget := exploreTarget{node: typeNode, divergentDefaultType: true}
	targets := []exploreTarget{ownerTarget, typeTarget}
	completion := newLocalizationCompletion(false, owner.ID)

	completeEnvelope := localizationExploreEnvelope{Evidence: []localizationEvidence{
		{ID: owner.ID, Provenance: localizationProvenanceDivergentDefault},
		{ID: typeNode.ID, Provenance: localizationProvenanceDivergentDefaultType},
	}}
	complete := localizationFinalizeCompletionEvidence("owner default permission", completion, targets, completeEnvelope)
	require.True(t, complete.enforceableOnAnswerReady)

	missingSupport := completeEnvelope
	missingSupport.Evidence = missingSupport.Evidence[:1]
	incomplete := localizationFinalizeCompletionEvidence("owner default permission", completion, targets, missingSupport)
	require.False(t, incomplete.enforceableOnAnswerReady)

	ready := newLocalizationCompletion(true, "")
	ready = localizationFinalizeCompletionEvidence("owner default permission", ready, targets, completeEnvelope)
	require.True(t, ready.Enforceable)

	spoofed := newLocalizationCompletion(true, "")
	spoofed.Enforceable = true
	spoofed.enforceableOnAnswerReady = true
	weak := localizationFinalizeCompletionEvidence("owner default permission", spoofed, []exploreTarget{{
		node: owner, source: ownerTarget.source, sourceLiteral: true, exactContent: true,
	}}, localizationExploreEnvelope{Evidence: []localizationEvidence{{ID: owner.ID}}})
	require.False(t, weak.Enforceable)
	require.False(t, weak.enforceableOnAnswerReady)
}
