package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSelectExploreCausalConsumerTargetBridgesRetainedComponent(t *testing.T) {
	store := graph.New()
	seed := &graph.Node{
		ID: "crates/matcher/src/lib.rs::Matcher.replace_with_captures_at", Name: "replace_with_captures_at",
		Kind: graph.KindMethod, FilePath: "crates/matcher/src/lib.rs",
	}
	callee := &graph.Node{
		ID: "crates/matcher/src/lib.rs::Matcher.captures_iter_at", Name: "captures_iter_at",
		Kind: graph.KindMethod, FilePath: "crates/matcher/src/lib.rs",
	}
	printerPeer := &graph.Node{
		ID: "crates/printer/src/standard.rs::StandardBuilder", Name: "StandardBuilder",
		Kind: graph.KindType, FilePath: "crates/printer/src/standard.rs",
	}
	consumer := &graph.Node{
		ID: "crates/printer/src/util.rs::replace_with_captures_in_context", Name: "replace_with_captures_in_context",
		Kind: graph.KindFunction, FilePath: "crates/printer/src/util.rs",
	}
	for _, node := range []*graph.Node{seed, callee, printerPeer, consumer} {
		store.AddNode(node)
	}
	store.AddEdge(&graph.Edge{From: consumer.ID, To: callee.ID, Kind: graph.EdgeCalls})

	candidate, ok := selectExploreCausalConsumerTarget(
		"panic while using --replace with multiline captures in context",
		[]exploreTarget{{node: seed, callees: []*graph.Node{callee}}, {node: printerPeer}},
		store,
	)
	if !ok {
		t.Fatal("expected a unique cross-file consumer")
	}
	if candidate.node == nil || candidate.node.ID != consumer.ID || !candidate.consumer ||
		candidate.parentID != seed.ID || candidate.hop != 1 {
		t.Fatalf("unexpected consumer candidate: %#v", candidate)
	}
}

func TestSelectExploreCausalConsumerTargetCollapsesSameFileWrappers(t *testing.T) {
	store := graph.New()
	seed := &graph.Node{
		ID: "crates/ignore/src/gitignore.rs::Glob.is_whitelist", Name: "is_whitelist",
		Kind: graph.KindMethod, FilePath: "crates/ignore/src/gitignore.rs",
	}
	wrapper1 := &graph.Node{
		ID: "crates/ignore/src/gitignore.rs::Gitignore.matched_stripped", Name: "matched_stripped",
		Kind: graph.KindMethod, FilePath: "crates/ignore/src/gitignore.rs",
	}
	wrapper2 := &graph.Node{
		ID: "crates/ignore/src/gitignore.rs::Gitignore.matched", Name: "matched",
		Kind: graph.KindMethod, FilePath: "crates/ignore/src/gitignore.rs",
	}
	consumer := &graph.Node{
		ID: "crates/ignore/src/dir.rs::Ignore.matched_ignore", Name: "matched_ignore",
		Kind: graph.KindMethod, FilePath: "crates/ignore/src/dir.rs",
	}
	for _, node := range []*graph.Node{seed, wrapper1, wrapper2, consumer} {
		store.AddNode(node)
	}
	store.AddEdge(&graph.Edge{From: wrapper1.ID, To: seed.ID, Kind: graph.EdgeCalls})
	store.AddEdge(&graph.Edge{From: wrapper2.ID, To: wrapper1.ID, Kind: graph.EdgeCalls})
	store.AddEdge(&graph.Edge{From: consumer.ID, To: wrapper2.ID, Kind: graph.EdgeCalls})

	candidate, ok := selectExploreCausalConsumerTarget(
		"hidden files whitelisted by an ancestor .ignore fail for explicit current directory",
		[]exploreTarget{{node: seed}},
		store,
	)
	if !ok || candidate.node == nil || candidate.node.ID != consumer.ID || candidate.hop != 3 {
		t.Fatalf("same-file wrapper chain did not recover consumer: ok=%v candidate=%#v", ok, candidate)
	}
}

func TestSelectExploreCausalConsumerTargetRejectsAmbiguousFiles(t *testing.T) {
	store := graph.New()
	seed := &graph.Node{
		ID: "crates/matcher/src/lib.rs::Matcher.captures_iter_at", Name: "captures_iter_at",
		Kind: graph.KindMethod, FilePath: "crates/matcher/src/lib.rs",
	}
	printerPeer := &graph.Node{
		ID: "crates/printer/src/standard.rs::StandardBuilder", Name: "StandardBuilder",
		Kind: graph.KindType, FilePath: "crates/printer/src/standard.rs",
	}
	otherPeer := &graph.Node{
		ID: "crates/other/src/standard.rs::OtherBuilder", Name: "OtherBuilder",
		Kind: graph.KindType, FilePath: "crates/other/src/standard.rs",
	}
	first := &graph.Node{
		ID: "crates/printer/src/util.rs::replace_all", Name: "replace_all",
		Kind: graph.KindFunction, FilePath: "crates/printer/src/util.rs",
	}
	second := &graph.Node{
		ID: "crates/other/src/util.rs::replace_each", Name: "replace_each",
		Kind: graph.KindFunction, FilePath: "crates/other/src/util.rs",
	}
	for _, node := range []*graph.Node{seed, printerPeer, otherPeer, first, second} {
		store.AddNode(node)
	}
	store.AddEdge(&graph.Edge{From: first.ID, To: seed.ID, Kind: graph.EdgeCalls})
	store.AddEdge(&graph.Edge{From: second.ID, To: seed.ID, Kind: graph.EdgeCalls})

	if candidate, ok := selectExploreCausalConsumerTarget(
		"replace output is duplicated",
		[]exploreTarget{{node: seed}, {node: printerPeer}, {node: otherPeer}},
		store,
	); ok {
		t.Fatalf("ambiguous cross-file consumers should be rejected: %#v", candidate)
	}
}
