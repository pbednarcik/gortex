package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestExploreRefinementRoutesPreferConcreteForwardedImplementation(t *testing.T) {
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
			node:                  wrapper,
			directCalleesComplete: true,
			source: `fn replace(&self, haystack: &str) -> String {
				self.replace_all(haystack)
			}`,
			callees: []*graph.Node{implementation},
		},
		{
			node: implementation,
			source: `fn replace_all(&self, haystack: &str) -> String {
				for capture in self.captures(haystack) { output.push_str(capture); }
				output
			}`,
		},
	}

	routes := exploreLocalizationRefinementRoutes(targets)
	if got := routes[wrapper.ID].implementationSymbol; got != implementation.ID {
		t.Fatalf("wrapper implementation route = %q, want %q", got, implementation.ID)
	}
	if !routes[wrapper.ID].enforceable {
		t.Fatal("unique complete wrapper-to-implementation route must retain strong provenance")
	}
	if route, ok := routes[implementation.ID]; !ok || route.implementationSymbol != "" {
		t.Fatalf("concrete implementation route = %#v, present=%v", route, ok)
	} else if !route.enforceable || route.proofSymbol != wrapper.ID {
		t.Fatalf("proven concrete route = %#v, want wrapper proof %q", route, wrapper.ID)
	}
	ordinary := exploreLocalizationRefinementRoutes([]exploreTarget{targets[1]})
	if ordinary[implementation.ID].enforceable || ordinary[implementation.ID].proofSymbol != "" {
		t.Fatalf("ordinary concrete hydration became enforceable: %#v", ordinary[implementation.ID])
	}
	if got := explorePreferredRoutedRefinementSymbol(wrapper.ID, targets, routes); got != implementation.ID {
		t.Fatalf("preferred routed symbol = %q, want concrete %q", got, implementation.ID)
	}
}

func TestExploreRefinementRoutesRejectAmbiguousGenericForwarder(t *testing.T) {
	wrapper := &graph.Node{ID: "repo/wrapper", Name: "replace", Kind: graph.KindMethod, FilePath: "src/wrapper.rs"}
	first := &graph.Node{ID: "repo/first", Name: "replace_all", Kind: graph.KindMethod, FilePath: "src/first.rs"}
	second := &graph.Node{ID: "repo/second", Name: "replace_one", Kind: graph.KindMethod, FilePath: "src/second.rs"}
	concreteSource := `fn replace_value(&self) { for value in self.values() { consume(value); } }`
	targets := []exploreTarget{
		{
			node:                  wrapper,
			directCalleesComplete: true,
			source:                `fn replace(&self) { self.delegate.replace() }`,
			callees:               []*graph.Node{first, second},
		},
		{node: first, source: concreteSource},
		{node: second, source: concreteSource},
	}

	routes := exploreLocalizationRefinementRoutes(targets)
	if _, authorized := routes[wrapper.ID]; authorized {
		t.Fatal("ambiguous generic forwarder was authorized")
	}
}

func TestExplorePreferredRouteFallsBackToFirstRankedConcreteCandidate(t *testing.T) {
	invalid := &graph.Node{ID: "repo/invalid", Name: "replace", Kind: graph.KindMethod, FilePath: "src/wrapper.rs"}
	firstConcrete := &graph.Node{ID: "repo/first", Name: "replace_all", Kind: graph.KindMethod, FilePath: "src/first.rs"}
	secondConcrete := &graph.Node{ID: "repo/second", Name: "replace_one", Kind: graph.KindMethod, FilePath: "src/second.rs"}
	targets := []exploreTarget{
		{node: invalid, source: `fn replace(&self) { self.delegate.replace() }`},
		{node: firstConcrete, source: `fn replace_all(&self) { execute_all(); }`},
		{node: secondConcrete, source: `fn replace_one(&self) { execute_one(); }`},
	}
	routes := exploreLocalizationRefinementRoutes(targets)
	if _, authorized := routes[invalid.ID]; authorized {
		t.Fatal("generic preferred with incomplete direct projection was authorized")
	}
	if got := explorePreferredRoutedRefinementSymbol(invalid.ID, targets, routes); got != firstConcrete.ID {
		t.Fatalf("fallback preferred = %q, want first ranked concrete %q", got, firstConcrete.ID)
	}
}

func TestExploreRefinementRoutesRejectSaturatedDirectCalleeProjection(t *testing.T) {
	wrapper := &graph.Node{ID: "repo/wrapper", Name: "replace", Kind: graph.KindMethod, FilePath: "src/wrapper.rs"}
	raw := make([]*graph.Node, 0, exploreRingCap+1)
	targets := []exploreTarget{{
		node:   wrapper,
		source: `fn replace(&self) { self.delegate.replace() }`,
	}}
	for i := 0; i < exploreRingCap+1; i++ {
		callee := &graph.Node{
			ID: fmt.Sprintf("repo/callee-%d", i), Name: fmt.Sprintf("replace_%d", i),
			Kind: graph.KindMethod, FilePath: "src/replace.rs",
		}
		raw = append(raw, callee)
		targets = append(targets, exploreTarget{node: callee, source: `fn concrete(&self) { execute(); }`})
	}
	projected, complete := ringNeighborsProjection(raw, wrapper.ID, exploreRingCap)
	if complete || len(projected) != exploreRingCap {
		t.Fatalf("saturated projection = (%d, complete=%v), want (%d, false)", len(projected), complete, exploreRingCap)
	}
	targets[0].callees = projected
	targets[0].directCalleesComplete = complete
	if _, authorized := exploreLocalizationRefinementRoutes(targets)[wrapper.ID]; authorized {
		t.Fatal("generic wrapper was authorized from saturated direct-callee evidence")
	}
}

func TestExploreRefinementRoutesRequireHydratedProductionCallable(t *testing.T) {
	hydrated := &graph.Node{ID: "repo/hydrated", Name: "run", Kind: graph.KindFunction, FilePath: "src/run.go"}
	unhydrated := &graph.Node{ID: "repo/unhydrated", Name: "run", Kind: graph.KindMethod, FilePath: "src/run.go"}
	nonCallable := &graph.Node{ID: "repo/type", Name: "Runner", Kind: graph.KindType, FilePath: "src/run.go"}
	testCallable := &graph.Node{
		ID: "repo/test", Name: "testRun", Kind: graph.KindFunction, FilePath: "src/run_test.go",
		Meta: map[string]any{"is_test": true},
	}
	routes := exploreLocalizationRefinementRoutes([]exploreTarget{
		{node: hydrated, source: `func run() { executeWork() }`},
		{node: unhydrated},
		{node: nonCallable, source: `type Runner struct{}`},
		{node: testCallable, source: `func testRun() { run() }`},
	})
	if _, ok := routes[hydrated.ID]; !ok {
		t.Fatal("hydrated production callable was not authorized")
	}
	for _, rejected := range []string{unhydrated.ID, nonCallable.ID, testCallable.ID} {
		if _, ok := routes[rejected]; ok {
			t.Fatalf("ineligible candidate %q was authorized", rejected)
		}
	}
}

func TestExploreRefinementRoutesTreatEveryPlausibleCallableAsAmbiguity(t *testing.T) {
	wrapper := &graph.Node{ID: "repo/wrapper", Name: "replace", Kind: graph.KindMethod, FilePath: "src/wrapper.rs"}
	concrete := &graph.Node{ID: "repo/concrete", Name: "replace_all", Kind: graph.KindMethod, FilePath: "src/replace.rs"}
	unresolved := &graph.Node{ID: "repo/unresolved", Name: "replace_one", Kind: graph.KindMethod, FilePath: "src/missing.rs"}
	unhydrated := &graph.Node{ID: "repo/unhydrated", Name: "replace_some", Kind: graph.KindMethod, FilePath: "src/replace.rs"}
	wrapperSource := `fn replace(&self) { self.delegate.replace() }`
	concreteSource := `fn replace_all(&self) { for value in self.values() { consume(value); } }`

	for name, extraTargets := range map[string][]exploreTarget{
		"unresolved": nil,
		"unhydrated": {{node: unhydrated}},
	} {
		extraCallee := unresolved
		if name == "unhydrated" {
			extraCallee = unhydrated
		}
		targets := []exploreTarget{
			{node: wrapper, source: wrapperSource, callees: []*graph.Node{concrete, extraCallee}, directCalleesComplete: true},
			{node: concrete, source: concreteSource},
		}
		targets = append(targets, extraTargets...)
		if _, ok := exploreLocalizationRefinementRoutes(targets)[wrapper.ID]; ok {
			t.Fatalf("generic wrapper with %s callable callee was authorized", name)
		}
	}
}

func TestBuildLocalizationRefinementResultKeepsWireAndStateAuthorizationEqual(t *testing.T) {
	preferred := &graph.Node{ID: "repo/concrete", Name: "replace_all", Kind: graph.KindMethod, FilePath: "src/replace.rs"}
	alternate := &graph.Node{ID: "repo/alternate", Name: "replace_one", Kind: graph.KindMethod, FilePath: "src/replace.rs"}
	targets := []exploreTarget{
		{node: preferred, source: `fn replace_all(&self) { execute_all(); }`},
		{node: alternate, source: `fn replace_one(&self) { execute_one(); }`},
	}
	routes := exploreLocalizationRefinementRoutes(targets)
	result, completion, bounded, digest := buildLocalizationRefinementResultForTask(
		preferred.ID, "find the replace implementation", targets, exploreDefaultBudgetTokens, routes,
	)
	if completion.State != localizationStateAnswerReady {
		t.Fatalf("completion state = %q, want %q", completion.State, localizationStateAnswerReady)
	}
	if completion.Enforceable || completion.AllowedToolCalls != 0 {
		t.Fatalf("packed refinement completion = %#v, want non-enforceable terminal conclusion", completion)
	}
	if _, ok := bounded[preferred.ID]; !ok {
		t.Fatalf("bounded route = %#v, want retained preferred refinement choice", bounded)
	}
	if digest == nil {
		t.Fatal("packed refinement has no digest")
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatal("localization refinement result has no single text payload")
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode localization envelope: %v", err)
	}
	if envelope.Completion.State != completion.State || envelope.Completion.AllowedToolCalls != completion.AllowedToolCalls {
		t.Fatalf("wire completion = %#v, returned completion = %#v", envelope.Completion, completion)
	}
	if !envelope.Terminal || !strings.HasPrefix(envelope.Completion.FinalResponse, localizationBoundedHeading+"\n") {
		t.Fatalf("packed refinement envelope = %#v, want terminal bounded-evidence conclusion", envelope)
	}
	if len(envelope.Completion.AllowedSymbols) != 0 {
		t.Fatalf("wire allowed symbols = %v, want none after packed body retires read", envelope.Completion.AllowedSymbols)
	}
	if envelope.Completion.refinementSymbol != "" || len(envelope.Completion.refinementRoutes) != 0 {
		t.Fatalf("wire decoded private routing state: %#v", envelope.Completion)
	}
	foundPackedPreferred := false
	for _, evidence := range envelope.Evidence {
		if evidence.ID == preferred.ID && strings.TrimSpace(evidence.Source) != "" {
			foundPackedPreferred = true
			break
		}
	}
	if !foundPackedPreferred {
		t.Fatalf("preferred evidence body was not packed: %#v", envelope.Evidence)
	}
}

func TestBuildLocalizationRefinementResultOffersRecoveryWithoutValidPreferredRoute(t *testing.T) {
	unhydrated := &graph.Node{ID: "repo/unhydrated", Name: "replace", Kind: graph.KindMethod, FilePath: "src/replace.rs"}
	targets := []exploreTarget{{node: unhydrated}}
	result, completion, bounded, _ := buildLocalizationRefinementResultForTask(
		unhydrated.ID, "find the replace implementation", targets, exploreDefaultBudgetTokens,
		exploreLocalizationRefinementRoutes(targets),
	)
	if completion.State != localizationStateNeedsRecovery || completion.RequiredAction != "recover_once" || completion.AllowedToolCalls != 1 {
		t.Fatalf("invalid preferred route completion = %#v, want bounded recovery contract", completion)
	}
	if len(bounded) != 0 {
		t.Fatalf("invalid preferred route retained authorization: %v", bounded)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatal("recovery localization result has no single text payload")
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode recovery localization envelope: %v", err)
	}
	if envelope.Completion.State != localizationStateNeedsRecovery || envelope.Completion.RequiredAction != "recover_once" || len(envelope.Completion.AllowedSymbols) != 0 {
		t.Fatalf("wire completion = %#v, want bounded recovery without a prevalidated route", envelope.Completion)
	}
}

func TestBoundedLocalizationRefinementRoutesCapsWireSetAndKeepsPreferred(t *testing.T) {
	symbols := make([]string, 10)
	routes := make(map[string]localizationRefinementRoute, len(symbols))
	for i := range symbols {
		symbols[i] = fmt.Sprintf("repo/internal/localization/package%02d/file.go::Resolver.Method%02d", i, i)
		routes[symbols[i]] = localizationRefinementRoute{}
	}
	preferred := symbols[9]
	authorized, bounded := boundedLocalizationRefinementRoutes(symbols, routes, preferred)
	if len(authorized) != localizationRefinementAllowedSymbolCap || len(bounded) != localizationRefinementAllowedSymbolCap {
		t.Fatalf("bounded authorization sizes = (%d, %d), want (%d, %d)", len(authorized), len(bounded), localizationRefinementAllowedSymbolCap, localizationRefinementAllowedSymbolCap)
	}
	if authorized[0] != preferred {
		t.Fatalf("preferred symbol = %q at index 0, want %q", authorized[0], preferred)
	}
	if _, rankSevenRetained := bounded[symbols[6]]; !rankSevenRetained {
		t.Fatalf("rank-seven recovery symbol %q was dropped: %v", symbols[6], authorized)
	}
	completion := newLocalizationRefinementCompletionForSymbols(preferred, authorized)
	wire, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("marshal bounded completion: %v", err)
	}
	if len(wire) > 2048 {
		t.Fatalf("bounded completion = %d bytes, want <= 2048: %s", len(wire), wire)
	}
	state := &localizationTerminalState{}
	state.armRefinementRoutesForTask("find implementation", preferred, authorized, bounded, nil)
	if got := state.completionLocked().AllowedSymbols; !reflect.DeepEqual(got, authorized) {
		t.Fatalf("state allowed symbols = %v, wire set = %v", got, authorized)
	}
}

func TestBoundedLocalizationRefinementRoutesRequiresVisibleImplementation(t *testing.T) {
	routes := map[string]localizationRefinementRoute{
		"wrapper":        {implementationSymbol: "implementation"},
		"implementation": {},
	}

	authorized, bounded := boundedLocalizationRefinementRoutes([]string{"wrapper"}, routes, "wrapper")
	if len(authorized) != 0 || len(bounded) != 0 {
		t.Fatalf("hidden implementation retained route: symbols=%v routes=%v", authorized, bounded)
	}

	authorized, bounded = boundedLocalizationRefinementRoutes([]string{"wrapper", "implementation"}, routes, "wrapper")
	if len(authorized) != 2 || bounded["wrapper"].implementationSymbol != "implementation" {
		t.Fatalf("visible implementation route = symbols:%v routes:%v", authorized, bounded)
	}
}
