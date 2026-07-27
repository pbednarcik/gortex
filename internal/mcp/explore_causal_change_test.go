package mcp

import (
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func causalChangeTestNode(id, name, file string, kind graph.NodeKind, signature string) *graph.Node {
	meta := map[string]any{}
	if signature != "" {
		meta["signature"] = signature
	}
	return &graph.Node{
		ID: id, Name: name, QualName: name, Kind: kind, FilePath: file,
		Language: "rust", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project",
		StartLine: 10, EndLine: 20, Meta: meta,
	}
}

func causalChangeTargetIDs(targets []exploreTarget) []string {
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.node != nil {
			ids = append(ids, target.node.ID)
		}
	}
	return ids
}

func TestPromoteExploreCausalChangeTargetsReservesDelegatedLeaf(t *testing.T) {
	wrapper := causalChangeTestNode(
		"repo/router/tree.go::node.resolveCaseInsensitivePath",
		"resolveCaseInsensitivePath", "router/tree.go", graph.KindMethod,
		"func (n *node) resolveCaseInsensitivePath(path string) []byte",
	)
	leaf := causalChangeTestNode(
		"repo/router/tree.go::node.resolveCaseInsensitivePathRec",
		"resolveCaseInsensitivePathRec", "router/tree.go", graph.KindMethod,
		"func (n *node) resolveCaseInsensitivePathRec(path string) []byte",
	)
	targets := []exploreTarget{
		{node: wrapper, source: "func resolveCaseInsensitivePath(path string) []byte { return n.resolveCaseInsensitivePathRec(path) }", callees: []*graph.Node{leaf}},
		{node: causalChangeTestNode("repo/router/engine.go::Engine.handle", "handle", "router/engine.go", graph.KindMethod, "func handle()")},
		{node: causalChangeTestNode("repo/router/config.go::Config.redirect", "redirect", "router/config.go", graph.KindMethod, "func redirect()")},
		{node: causalChangeTestNode("repo/router/log.go::logInvalidPath", "logInvalidPath", "router/log.go", graph.KindFunction, "func logInvalidPath()")},
		{node: causalChangeTestNode("repo/router/errors.go::invalidNodeDiagnostic", "invalidNodeDiagnostic", "router/errors.go", graph.KindFunction, "func invalidNodeDiagnostic()")},
	}
	task := "Request execution panics during case insensitive path fallback instead of returning not found"
	got := promoteExploreCausalChangeTargets(task, targets, nil, len(targets), func(node *graph.Node) string {
		if node.ID == leaf.ID {
			return "func resolveCaseInsensitivePathRec(path string) []byte { return nil }"
		}
		return ""
	})

	if len(got) != len(targets) {
		t.Fatalf("promoted targets = %d, want fixed width %d", len(got), len(targets))
	}
	for index := 0; index < exploreDraftPrimaryLimit; index++ {
		if got[index].node.ID != targets[index].node.ID {
			t.Fatalf("direct head row %d changed from %q to %q", index, targets[index].node.ID, got[index].node.ID)
		}
	}
	leafIndex := exploreTargetIndex(got, leaf.ID)
	if leafIndex < 0 || !got[leafIndex].causalChangeLeaf || got[leafIndex].source == "" {
		t.Fatalf("delegated leaf was not reserved and hydrated: %#v", got)
	}
	planned := localizationEvidenceTargetsFromDraft(task, "", got, nil)
	if len(planned) < 2 || planned[0].node.ID != wrapper.ID || planned[1].node.ID != leaf.ID {
		t.Fatalf("causal leaf did not follow retrieval head before packing: %v", causalChangeTargetIDs(planned))
	}
}

func TestPromoteExploreCausalChangeTargetsPrefersGraphCallerOverKeywordOnlyTail(t *testing.T) {
	matcher := causalChangeTestNode(
		"repo/matcher/api.rs::Matcher.replace_with_captures",
		"replace_with_captures", "matcher/api.rs", graph.KindMethod,
		"fn replace_with_captures(&self) -> Result<()> ",
	)
	leaf := causalChangeTestNode(
		"repo/output/rewrite.rs::OutputRewriter.apply_replacements",
		"apply_replacements", "output/rewrite.rs", graph.KindMethod,
		"fn apply_replacements(&mut self) -> io::Result<()> ",
	)
	owner := causalChangeTestNode(
		"repo/output/rewrite.rs::OutputRewriter", "OutputRewriter",
		"output/rewrite.rs", graph.KindType, "struct OutputRewriter",
	)
	g := graph.New()
	g.AddNode(leaf)
	g.AddNode(owner)

	directHead := causalChangeTestNode("repo/search/run.rs::Search.run", "runSearch", "search/run.rs", graph.KindMethod, "fn run_search()")
	keywordOnly := causalChangeTestNode(
		"repo/search/diagnostics.rs::multiline_replace_output_diagnostic",
		"multiline_replace_output_diagnostic", "search/diagnostics.rs", graph.KindFunction,
		"fn multiline_replace_output_diagnostic()",
	)
	targets := []exploreTarget{
		{node: matcher, source: "fn replace_with_captures() { /* multiline replace */ }", callers: []*graph.Node{leaf}},
		{node: directHead},
		{node: causalChangeTestNode("repo/search/options.rs::Options.multiline", "multiline", "search/options.rs", graph.KindMethod, "fn multiline()")},
		{node: causalChangeTestNode("repo/search/output.rs::duplicate_output", "duplicate_output", "search/output.rs", graph.KindFunction, "fn duplicate_output()")},
		{node: keywordOnly},
	}
	task := "Multiline replace duplicates output when replacement captures span several lines"
	got := promoteExploreCausalChangeTargets(task, targets, g, len(targets), func(node *graph.Node) string {
		if node.ID == leaf.ID {
			return "fn apply_replacements(&mut self) -> io::Result<()> { Ok(()) }"
		}
		return ""
	})

	if slices.Contains(causalChangeTargetIDs(got), keywordOnly.ID) {
		t.Fatalf("non-causal keyword-only tail survived ahead of graph change site: %v", causalChangeTargetIDs(got))
	}
	leafIndex, ownerIndex := exploreTargetIndex(got, leaf.ID), exploreTargetIndex(got, owner.ID)
	if leafIndex < 0 || ownerIndex < 0 || !got[leafIndex].causalChangeLeaf || !got[ownerIndex].causalChangeOwner {
		t.Fatalf("caller/owner pair not admitted: %#v", got)
	}
	planned := localizationEvidenceTargetsFromDraft(task, "", got, nil)
	want := []string{matcher.ID, owner.ID, leaf.ID}
	if ids := causalChangeTargetIDs(planned); len(ids) < len(want) || !slices.Equal(ids[:len(want)], want) {
		t.Fatalf("planned causal prefix = %v, want %v", ids, want)
	}
}

func TestExploreCausalChangeOwnerPrefersUniquelyReturnedStateType(t *testing.T) {
	leaf := causalChangeTestNode(
		"repo/state/build.rs::RuleBuilder.build_with_context",
		"build_with_context", "state/build.rs", graph.KindMethod,
		"fn build_with_context(&self, root: &Path) -> RuleState",
	)
	builder := causalChangeTestNode(
		"repo/state/build.rs::RuleBuilder", "RuleBuilder", "state/build.rs", graph.KindType, "struct RuleBuilder",
	)
	state := causalChangeTestNode(
		"repo/state/build.rs::RuleState", "RuleState", "state/build.rs", graph.KindType, "struct RuleState",
	)
	g := graph.New()
	g.AddNode(leaf)
	g.AddNode(builder)
	g.AddNode(state)

	got := exploreCausalChangeOwner("parallel roots leak rule state between traversals", leaf, g)
	if got == nil || got.ID != state.ID {
		t.Fatalf("change owner = %#v, want uniquely returned state type %q", got, state.ID)
	}
}

func causalChangeRouteNode(id, name, qualName, file string, kind graph.NodeKind) *graph.Node {
	node := causalChangeTestNode(id, name, file, kind, "")
	node.QualName = qualName
	node.Language = "go"
	return node
}

func requireCausalChangeTarget(t *testing.T, targets []exploreTarget, id string) exploreTarget {
	t.Helper()
	index := exploreTargetIndex(targets, id)
	if index < 0 {
		t.Fatalf("target %q missing from %v", id, causalChangeTargetIDs(targets))
		return exploreTarget{}
	}
	return targets[index]
}

func TestPromoteExploreCausalChangeTargetsCarriesCrossFileBridgeToContinuationAndOwner(t *testing.T) {
	seed := causalChangeRouteNode(
		"repo/crates/matcher/src/matcher.rs::Matcher.replace_with_captures_at",
		"replace_with_captures_at", "Matcher.replace_with_captures_at",
		"crates/matcher/src/matcher.rs", graph.KindMethod,
	)
	bridge := causalChangeRouteNode(
		"repo/crates/printer/src/util.rs::Replacer.replace_all",
		"replace_all", "Replacer.replace_all", "crates/printer/src/util.rs", graph.KindMethod,
	)
	continuation := causalChangeRouteNode(
		"repo/crates/printer/src/util.rs::replace_with_captures_in_context",
		"replace_with_captures_in_context", "replace_with_captures_in_context",
		"crates/printer/src/util.rs", graph.KindFunction,
	)
	owner := causalChangeRouteNode(
		"repo/crates/printer/src/util.rs::Replacer",
		"Replacer", "Replacer", "crates/printer/src/util.rs", graph.KindType,
	)
	store := graph.New()
	store.AddNode(bridge)
	store.AddNode(continuation)
	store.AddNode(owner)

	readCalls, hydrateCalls := 0, 0
	task := "The replace with captures at path should replace all matches while preserving captures in their surrounding context"
	got := promoteExploreCausalChangeTargets(task, []exploreTarget{{
		node: seed, source: "replace_with_captures_at replaces captures", callers: []*graph.Node{bridge},
	}}, store, 4, func(node *graph.Node) string {
		readCalls++
		if node.ID != bridge.ID {
			t.Fatalf("source read for %q, want bridge %q", node.ID, bridge.ID)
		}
		return "func (r *Replacer) replace_all() { replace_with_captures_in_context() }"
	}, func(node *graph.Node) ([]*graph.Node, bool) {
		hydrateCalls++
		if node.ID != bridge.ID {
			t.Fatalf("hydrated %q, want bridge %q", node.ID, bridge.ID)
		}
		return []*graph.Node{continuation}, true
	})

	if readCalls != 1 || hydrateCalls != 1 {
		t.Fatalf("bounded reads = source:%d adjacency:%d, want 1 each", readCalls, hydrateCalls)
	}
	if len(got) != 4 {
		t.Fatalf("promoted targets = %d, want seed + owner + bridge + continuation", len(got))
	}
	bridgeTarget := requireCausalChangeTarget(t, got, bridge.ID)
	leafTarget := requireCausalChangeTarget(t, got, continuation.ID)
	ownerTarget := requireCausalChangeTarget(t, got, owner.ID)
	if !bridgeTarget.causalChangeBridge || bridgeTarget.causalChangeLeaf {
		t.Fatalf("bridge flags = bridge:%t leaf:%t", bridgeTarget.causalChangeBridge, bridgeTarget.causalChangeLeaf)
	}
	if !exploreFoldedTargetMandatory(bridgeTarget, nil) {
		t.Fatal("causal bridge was not protected from owner folding")
	}
	if !leafTarget.causalChangeLeaf || leafTarget.causalChangeBridge || !ownerTarget.causalChangeOwner {
		t.Fatalf("continuation/owner flags were not preserved: %#v %#v", leafTarget, ownerTarget)
	}
	planned := localizationEvidenceTargetsFromDraft(task, "", got, nil)
	want := []string{seed.ID, owner.ID, bridge.ID, continuation.ID}
	if ids := causalChangeTargetIDs(planned); len(ids) < len(want) || !slices.Equal(ids[:len(want)], want) {
		t.Fatalf("planned causal route = %v, want prefix %v", ids, want)
	}
}

func TestPromoteExploreCausalChangeTargetsUsesNestedDelegationHint(t *testing.T) {
	seed := causalChangeRouteNode(
		"repo/gin.go::Engine.redirectFixedPath", "redirectFixedPath", "Engine.redirectFixedPath",
		"gin.go", graph.KindMethod,
	)
	cleanPath := causalChangeRouteNode(
		"repo/path.go::cleanPath", "cleanPath", "cleanPath", "gin.go", graph.KindFunction,
	)
	bridge := causalChangeRouteNode(
		"repo/tree.go::node.findCaseInsensitivePath", "findCaseInsensitivePath", "node.findCaseInsensitivePath",
		"tree.go", graph.KindMethod,
	)
	continuation := causalChangeRouteNode(
		"repo/tree.go::node.findCaseInsensitivePathRec", "findCaseInsensitivePathRec", "node.findCaseInsensitivePathRec",
		"tree.go", graph.KindMethod,
	)
	targets := []exploreTarget{{
		node:          seed,
		source:        "redirectFixedPath follows the case insensitive path fallback",
		callees:       []*graph.Node{cleanPath, bridge},
		causalCallees: []exploreCausalNeighbor{{node: bridge, hop: 1}, {node: continuation, hop: 2}},
	}}
	task := "The redirect fixed path fallback should follow case insensitive path lookup recursively without cleaning away valid request segments"
	got := promoteExploreCausalChangeTargets(task, targets, nil, 3, func(node *graph.Node) string {
		if node.ID != bridge.ID {
			t.Fatalf("source read for %q, want hinted bridge %q", node.ID, bridge.ID)
		}
		return "func (n *node) findCaseInsensitivePath() { n.findCaseInsensitivePathRec() }"
	}, func(node *graph.Node) ([]*graph.Node, bool) {
		if node.ID != bridge.ID {
			t.Fatalf("hydrated %q, want hinted bridge %q", node.ID, bridge.ID)
		}
		return []*graph.Node{continuation}, true
	})

	if len(got) != 3 {
		t.Fatalf("promoted targets = %d, want seed + bridge + continuation", len(got))
	}
	if slices.Contains(causalChangeTargetIDs(got), cleanPath.ID) {
		t.Fatalf("unrelated sibling callee was promoted: %v", causalChangeTargetIDs(got))
	}
	if !requireCausalChangeTarget(t, got, bridge.ID).causalChangeBridge ||
		!requireCausalChangeTarget(t, got, continuation.ID).causalChangeLeaf {
		t.Fatalf("nested causal route flags missing: %#v", got)
	}
}

func TestPromoteExploreCausalChangeTargetsContinuesInheritedCaller(t *testing.T) {
	seed := causalChangeRouteNode(
		"repo/src/Handler/AbstractHandler.php::AbstractHandler.isHandling",
		"isHandling", "AbstractHandler.isHandling", "src/Handler/AbstractHandler.php", graph.KindMethod,
	)
	genericCaller := causalChangeRouteNode(
		"repo/src/Handler/AbstractProcessingHandler.php::AbstractProcessingHandler.handle",
		"handle", "AbstractProcessingHandler.handle", "src/Handler/AbstractProcessingHandler.php", graph.KindMethod,
	)
	bridge := causalChangeRouteNode(
		"repo/src/Handler/HipChatHandler.php::HipChatHandler.handleBatch",
		"handleBatch", "HipChatHandler.handleBatch", "src/Handler/HipChatHandler.php", graph.KindMethod,
	)
	write := causalChangeRouteNode(
		"repo/src/Handler/HipChatHandler.php::HipChatHandler.write",
		"write", "HipChatHandler.write", "src/Handler/HipChatHandler.php", graph.KindMethod,
	)
	continuation := causalChangeRouteNode(
		"repo/src/Handler/HipChatHandler.php::HipChatHandler.combineRecords",
		"combineRecords", "HipChatHandler.combineRecords", "src/Handler/HipChatHandler.php", graph.KindMethod,
	)
	task := "The Hip Chat handler should handle each batch by combining records before checking handling and writing messages to the API"
	got := promoteExploreCausalChangeTargets(task, []exploreTarget{{
		node: seed, source: "isHandling checks every record", callers: []*graph.Node{genericCaller, bridge},
	}}, nil, 3, func(node *graph.Node) string {
		if node.ID != bridge.ID {
			t.Fatalf("source read for %q, want inherited caller %q", node.ID, bridge.ID)
		}
		return "function handleBatch() { $records = $this->combineRecords(); $this->write($records); }"
	}, func(node *graph.Node) ([]*graph.Node, bool) {
		return []*graph.Node{write, continuation}, true
	})

	if slices.Contains(causalChangeTargetIDs(got), genericCaller.ID) || slices.Contains(causalChangeTargetIDs(got), write.ID) {
		t.Fatalf("generic inherited route won over concrete continuation: %v", causalChangeTargetIDs(got))
	}
	if !requireCausalChangeTarget(t, got, bridge.ID).causalChangeBridge ||
		!requireCausalChangeTarget(t, got, continuation.ID).causalChangeLeaf {
		t.Fatalf("inherited caller route flags missing: %#v", got)
	}
}

func TestPromoteExploreCausalChangeTargetsContinuationFailsClosed(t *testing.T) {
	seed := causalChangeRouteNode(
		"repo/router/resolve.go::Resolver.resolveRequestPath", "resolveRequestPath", "Resolver.resolveRequestPath",
		"router/resolve.go", graph.KindMethod,
	)
	bridge := causalChangeRouteNode(
		"repo/router/dispatch.go::RouteHandler.dispatchRequest", "dispatchRequest", "RouteHandler.dispatchRequest",
		"router/dispatch.go", graph.KindMethod,
	)
	primary := causalChangeRouteNode(
		"repo/router/dispatch.go::RouteHandler.dispatchPrimaryWork", "dispatchPrimaryWork", "RouteHandler.dispatchPrimaryWork",
		"router/dispatch.go", graph.KindMethod,
	)
	backup := causalChangeRouteNode(
		"repo/router/dispatch.go::RouteHandler.dispatchBackupWork", "dispatchBackupWork", "RouteHandler.dispatchBackupWork",
		"router/dispatch.go", graph.KindMethod,
	)
	task := "The resolve request path flow asks the route handler to dispatch request work before selecting any terminal implementation"

	tests := []struct {
		name     string
		children []*graph.Node
		complete bool
	}{
		{name: "ambiguous", children: []*graph.Node{primary, backup}, complete: true},
		{name: "incomplete", children: []*graph.Node{primary}, complete: false},
		{name: "out_of_scope", children: []*graph.Node{func() *graph.Node {
			outside := *primary
			outside.WorkspaceID = "other-workspace"
			return &outside
		}()}, complete: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := promoteExploreCausalChangeTargets(task, []exploreTarget{{
				node: seed, source: "resolveRequestPath dispatches request work", callers: []*graph.Node{bridge},
			}}, nil, 3, func(*graph.Node) string {
				return "func dispatchRequest() { dispatchPrimaryWork(); dispatchBackupWork() }"
			}, func(*graph.Node) ([]*graph.Node, bool) {
				return test.children, test.complete
			})

			bridgeTarget := requireCausalChangeTarget(t, got, bridge.ID)
			if bridgeTarget.causalChangeBridge || !bridgeTarget.causalChangeLeaf {
				t.Fatalf("uncertain bridge must remain terminal leaf, got bridge:%t leaf:%t", bridgeTarget.causalChangeBridge, bridgeTarget.causalChangeLeaf)
			}
			if slices.Contains(causalChangeTargetIDs(got), primary.ID) || slices.Contains(causalChangeTargetIDs(got), backup.ID) {
				t.Fatalf("uncertain continuation escaped fail-closed selection: %v", causalChangeTargetIDs(got))
			}
		})
	}
}
