package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func TestExploreSyntacticAnchorsDoNotSpendBudgetOnHTTPRouteMethods(t *testing.T) {
	task := `HandleMethodNotAllowed with r.GET("/base/metrics"), r.DELETE("/base/v1/organizations/:id"), then panic in tree.go getValue`
	anchors := exploreSyntacticAnchors(task)
	if len(anchors) != 3 {
		t.Fatalf("anchors = %#v, want three implementation anchors", anchors)
	}
	got := []string{anchors[0].compact, anchors[1].compact, anchors[2].compact}
	want := []string{"handlemethodnotallowed", "tree", "getvalue"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("anchor %d = %q, want %q (all=%v)", index, got[index], want[index], got)
		}
	}
}

func TestExploreSyntacticAnchorsPreserveQualifiedMemberAlongsideOwner(t *testing.T) {
	anchors := exploreSyntacticAnchors(`Find HipChatHandler and HipChatHandler::buildContent()`)
	if len(anchors) != 2 {
		t.Fatalf("anchors = %#v, want owner and qualified member", anchors)
	}
	if anchors[0].compact != "hipchathandler" || anchors[1].compact != "hipchathandlerbuildcontent" {
		t.Fatalf("anchor compacts = [%s, %s], want owner then qualified member", anchors[0].compact, anchors[1].compact)
	}
}

func TestExploreSyntacticAnchorQualifiedMemberDoesNotReuseOwner(t *testing.T) {
	anchors := exploreSyntacticAnchors(`Find HipChatHandler and HipChatHandler::buildContent()`)
	if len(anchors) != 2 {
		t.Fatalf("anchors = %#v, want owner and qualified member", anchors)
	}
	owner := &rerank.Candidate{Node: &graph.Node{
		ID: "HipChatHandler.php::HipChatHandler", Name: "HipChatHandler",
		QualName: "Monolog.Handler.HipChatHandler", Kind: graph.KindType,
	}}
	member := &rerank.Candidate{Node: &graph.Node{
		ID: "HipChatHandler.php::HipChatHandler.buildContent", Name: "buildContent",
		QualName: "Monolog.Handler.HipChatHandler.buildContent", Kind: graph.KindMethod,
	}}
	candidates := []*rerank.Candidate{owner, member}
	if got := exploreSyntacticAnchorReusesProtected(anchors[0], candidates, map[string]struct{}{owner.Node.ID: {}}); got != owner.Node.ID {
		t.Fatalf("owner reuse = %q, want %q", got, owner.Node.ID)
	}
	used := map[string]struct{}{owner.Node.ID: {}, member.Node.ID: {}}
	if got := exploreSyntacticAnchorReusesProtected(anchors[1], candidates, used); got != member.Node.ID {
		t.Fatalf("qualified member reuse = %q, want %q instead of owner", got, member.Node.ID)
	}
}

func TestExploreSourceRangeSpecsPairPathsWithFollowingLines(t *testing.T) {
	task := `monolog/src/Monolog/Handler/FingersCrossedHandler.php
		Lines 185 to 187
		monolog/tests/Monolog/Handler/FingersCrossedHandlerTest.php
		Lines 253 to 265`
	specs := exploreSourceRangeSpecs(task)
	if len(specs) != 2 {
		t.Fatalf("source range specs = %#v, want production and test ranges", specs)
	}
	if specs[0].File != "monolog/src/Monolog/Handler/FingersCrossedHandler.php" || specs[0].StartLine != 185 || specs[0].EndLine != 187 {
		t.Fatalf("production range = %#v", specs[0])
	}
	if specs[1].File != "monolog/tests/Monolog/Handler/FingersCrossedHandlerTest.php" || specs[1].StartLine != 253 || specs[1].EndLine != 265 {
		t.Fatalf("test range = %#v", specs[1])
	}
}

func TestExploreSourceRangeDefinitionsPreferNamedOwnerOverClosure(t *testing.T) {
	idx := &fileSymbolIndex{}
	idx.add(&graph.Node{ID: "handler.php::flushBuffer", Name: "flushBuffer", Kind: graph.KindMethod, StartLine: 170, EndLine: 200})
	idx.add(&graph.Node{ID: "handler.php::flushBuffer#closure", Name: "closure", Kind: graph.KindClosure, StartLine: 185, EndLine: 187})
	idx.finalise()
	got := exploreSourceRangeDefinitions(idx, 185, 187)
	if len(got) != 1 || got[0].ID != "handler.php::flushBuffer" {
		t.Fatalf("source range definitions = %#v, want named enclosing method", got)
	}
}

func TestPromoteExploreSourceRangeCandidatesMapsToEnclosingMethod(t *testing.T) {
	server, store := setupNavServer(t)
	start := store.GetNode(navFindMethod(t, store, "Start"))
	stop := store.GetNode(navFindMethod(t, store, "Stop"))
	task := fmt.Sprintf("svc.go\nLines %d to %d", start.StartLine, start.EndLine)
	ordinary := []*rerank.Candidate{{Node: stop, TextRank: 0, VectorRank: -1}}
	got := server.promoteExploreSourceRangeCandidates(context.Background(), task, ordinary, query.QueryOptions{})
	if len(got) != 2 || got[0].Node.ID != start.ID || got[1].Node.ID != stop.ID {
		t.Fatalf("promoted candidates = %#v, want cited Start then ordinary Stop", got)
	}
	if got[0].Signals[exploreSyntacticAnchorSignal] != 1 || got[0].Signals[exploreConceptComplementSignal] != 1 {
		t.Fatalf("cited candidate signals = %#v, want protected syntactic evidence", got[0].Signals)
	}
}

func TestPromoteExploreSourceRangeCandidatesStripsIsolatedRepoLabel(t *testing.T) {
	server, store := setupNavServer(t)
	start := store.GetNode(navFindMethod(t, store, "Start"))
	stop := store.GetNode(navFindMethod(t, store, "Stop"))
	task := fmt.Sprintf("monolog/svc.go\nLines %d to %d", start.StartLine, start.EndLine)
	ordinary := []*rerank.Candidate{{Node: stop, TextRank: 0, VectorRank: -1}}
	got := server.promoteExploreSourceRangeCandidates(context.Background(), task, ordinary, query.QueryOptions{})
	if len(got) != 2 || got[0].Node.ID != start.ID || got[1].Node.ID != stop.ID {
		t.Fatalf("promoted candidates = %#v, want repo-prefixed citation to resolve indexed svc.go", got)
	}
}

func TestExploreSourceRangeIndexRejectsAmbiguousSuffixes(t *testing.T) {
	exact := &fileSymbolIndex{}
	first := &fileSymbolIndex{}
	second := &fileSymbolIndex{}
	aliases := []string{"repo/pkg/svc.go", "pkg/svc.go", "svc.go"}
	if got := exploreSourceRangeIndex(map[string]*fileSymbolIndex{aliases[0]: exact, aliases[1]: first, aliases[2]: second}, aliases); got != exact {
		t.Fatalf("exact index = %p, want %p", got, exact)
	}
	if got := exploreSourceRangeIndex(map[string]*fileSymbolIndex{aliases[1]: first, aliases[2]: second}, aliases); got != nil {
		t.Fatalf("ambiguous suffix index = %p, want nil", got)
	}
	if got := exploreSourceRangeIndex(map[string]*fileSymbolIndex{aliases[1]: first}, aliases); got != first {
		t.Fatalf("unique suffix index = %p, want %p", got, first)
	}
}

func TestExploreLocalizationTestLaneCandidateKeepsCitedTestRange(t *testing.T) {
	node := &graph.Node{
		ID: "tests/HandlerTest.php::testPassthruOnClose", Name: "testPassthruOnClose",
		Kind: graph.KindMethod, FilePath: "tests/HandlerTest.php", Meta: map[string]any{"is_test": true},
	}
	candidate := &rerank.Candidate{Node: node, Signals: map[string]float64{exploreSourceRangeSignal: 1}}
	if exploreLocalizationTestLaneCandidate("passthru level failure", candidate) {
		t.Fatal("explicitly cited test range was demoted")
	}
	delete(candidate.Signals, exploreSourceRangeSignal)
	if !exploreLocalizationTestLaneCandidate("passthru level failure", candidate) {
		t.Fatal("ordinary test candidate was not demoted")
	}
}

func TestExploreSyntacticAnchorsCollapseAssignedAndBareFlags(t *testing.T) {
	anchors := exploreSyntacticAnchors(`rg panic caused by --replace, --multiline, a particular pattern, and search text containing repeats and newlines. Panic message: "slice index starts at x but ends at y" where x and y are integers and y < x. Reproduction: rg "(^|[^a-z])((([a-z]+)?)\s)?b(\s([a-z]+)?)($|[^a-z])" --replace=x --multiline lines2.txt where lines2.txt contains " b b b b b b b b\nc". Also reproduces with lines5.txt containing " b\nb\nb\nb\nc". Bug is very sensitive to exact pattern and text.`)
	if len(anchors) != 2 {
		t.Fatalf("anchors = %#v, want one replace lane and one multiline lane", anchors)
	}
	if anchors[0].compact != "replace" || anchors[1].compact != "multiline" {
		t.Fatalf("anchor compacts = [%s, %s], want replace then multiline", anchors[0].compact, anchors[1].compact)
	}
}

func TestExploreSyntacticAnchorCompetitionIsScopedToBareCompoundCallable(t *testing.T) {
	anchor, ok := newExploreSyntacticAnchor("--replace")
	if !ok {
		t.Fatal("replace flag did not produce a syntactic anchor")
	}
	separator := &rerank.Candidate{Node: &graph.Node{
		ID: "crates/printer/src/util.rs::replace_separator", Name: "replace_separator",
		QualName: "replace_separator", Kind: graph.KindFunction, FilePath: "crates/printer/src/util.rs",
		StartLine: 10, EndLine: 210,
	}}
	replacement := &rerank.Candidate{Node: &graph.Node{
		ID: "crates/printer/src/util.rs::Replacer.replacement", Name: "replacement",
		QualName: "Replacer.replacement", Kind: graph.KindMethod, FilePath: "crates/printer/src/util.rs",
		StartLine: 113, EndLine: 124,
	}}
	replaceAll := &rerank.Candidate{Node: &graph.Node{
		ID: "crates/printer/src/util.rs::Replacer.replace_all", Name: "replace_all",
		QualName: "Replacer.replace_all", Kind: graph.KindMethod, FilePath: "crates/printer/src/util.rs",
		StartLine: 55, EndLine: 111,
	}}
	candidates := []*rerank.Candidate{separator, replacement, replaceAll}
	usedIDs, usedFiles := map[string]struct{}{}, map[string]struct{}{}
	if !exploreSyntacticAnchorNeedsLexicalCompetition(anchor, separator) {
		t.Fatal("bare replace anchor prematurely settled on compound replace_separator")
	}
	ordinary := exploreSyntacticAnchorCandidate(
		anchor, candidates, query.QueryOptions{}, usedIDs, usedFiles,
	)
	if ordinary == nil || ordinary.Node.ID != separator.Node.ID {
		t.Fatalf("ordinary selection = %#v, want stable first candidate", ordinary)
	}
	competing := exploreSyntacticAnchorCompetingCandidate(
		anchor, candidates, query.QueryOptions{}, usedIDs, usedFiles,
	)
	if competing == nil || competing.Node.ID != replaceAll.Node.ID {
		t.Fatalf("competing selection = %#v, want specific replace_all implementation despite larger generic body", competing)
	}

	explicit, ok := newExploreSyntacticAnchor("replace_separator")
	if !ok {
		t.Fatal("explicit replace_separator did not produce an anchor")
	}
	if exploreSyntacticAnchorNeedsLexicalCompetition(explicit, separator) {
		t.Fatal("multi-term explicit replace_separator was treated as ambiguous")
	}
	exact := &rerank.Candidate{Node: &graph.Node{
		ID: "crates/printer/src/util.rs::replace", Name: "replace",
		Kind: graph.KindFunction, FilePath: "crates/printer/src/util.rs",
	}}
	if exploreSyntacticAnchorNeedsLexicalCompetition(anchor, exact) {
		t.Fatal("atomic exact replace match was treated as ambiguous")
	}
	if got := exploreSyntacticAnchorFetchLimit(anchor, separator); got != exploreSyntacticAnchorCompetitionFetch {
		t.Fatalf("ambiguous replace fetch = %d, want %d", got, exploreSyntacticAnchorCompetitionFetch)
	}
	if got := exploreSyntacticAnchorFetchLimit(explicit, separator); got != exploreSyntacticAnchorFetch {
		t.Fatalf("exact replace_separator fetch = %d, want %d", got, exploreSyntacticAnchorFetch)
	}
}

func TestMarkExploreSyntacticAnchorCandidateCarriesDedicatedProvenanceSignal(t *testing.T) {
	shared := map[string]float64{"existing": 1}
	candidate := &rerank.Candidate{Node: &graph.Node{ID: "pkg/util.rs::Replacer.replace_all"}, Signals: shared}
	sibling := &rerank.Candidate{Node: &graph.Node{ID: "pkg/lines.rs::lines"}, Signals: shared}
	markExploreSyntacticAnchorCandidate(candidate)
	if candidate.Signals[exploreConceptComplementSignal] != 1 || candidate.Signals[exploreSyntacticAnchorSignal] != 1 {
		t.Fatalf("signals = %#v, want concept retention plus syntactic provenance", candidate.Signals)
	}
	if sibling.Signals[exploreConceptComplementSignal] != 0 || sibling.Signals[exploreSyntacticAnchorSignal] != 0 {
		t.Fatalf("syntactic provenance leaked to unselected sibling: %#v", sibling.Signals)
	}
	if candidate.Signals["existing"] != 1 {
		t.Fatalf("existing signal was lost: %#v", candidate.Signals)
	}
}

func TestExploreQueryPathAnchorsIgnoreHTTPRouteExamples(t *testing.T) {
	query := `HandleMethodNotAllowed r.GET("/base/metrics") r.GET("/base/v1/:id/devices") panic in github.com/gin-gonic/gin.(*node).getValue at tree.go:637`
	syntactic := exploreSyntacticAnchors(query)
	if len(syntactic) != 3 || syntactic[2].compact != "tree" {
		t.Fatalf("stack source location anchor = %#v, want trailing tree", syntactic)
	}
	anchors, hasDirectory := exploreQueryPathAnchors(query)
	if hasDirectory || len(anchors) != 0 {
		t.Fatalf("runtime routes became source paths: anchors=%v hasDirectory=%v", anchors, hasDirectory)
	}
	node := &graph.Node{
		ID:       "tree.go::node.getValue",
		Name:     "getValue",
		QualName: "node.getValue",
		Kind:     graph.KindMethod,
		FilePath: "tree.go",
	}
	if !exploreLocalizationExplicitAnchor(query, node) {
		t.Fatal("explicit tree.go/getValue anchor was hidden by HTTP route examples")
	}
}

func TestLocalizationEvidenceTargetsPreserveProtectedSyntacticAnchorBeforeDraftRelations(t *testing.T) {
	head := exploreTarget{node: &graph.Node{
		ID: "crates/core/flags/hiargs.rs::suggest_multiline", Name: "suggest_multiline",
		QualName: "suggest_multiline", Kind: graph.KindFunction, FilePath: "crates/core/flags/hiargs.rs",
	}}
	unrelated := exploreTarget{node: &graph.Node{
		ID: "crates/core/flags/hiargs.rs::BinaryMode.Auto", Name: "Auto",
		QualName: "BinaryMode.Auto", Kind: graph.KindMethod, FilePath: "crates/core/flags/hiargs.rs",
	}}
	replaceAll := exploreTarget{
		node: &graph.Node{
			ID: "crates/printer/src/util.rs::Replacer.replace_all", Name: "replace_all",
			QualName: "Replacer.replace_all", Kind: graph.KindMethod, FilePath: "crates/printer/src/util.rs",
		},
		conceptComplement: true,
	}
	targets := []exploreTarget{head, unrelated, replaceAll}
	selected := localizationEvidenceTargetsFromDraft(
		"--replace with --multiline panics while replacing repeated matches",
		"",
		targets,
		[]exploreDraftEntry{{node: head.node}, {node: unrelated.node}, {node: replaceAll.node}},
	)
	if len(selected) != len(targets) {
		t.Fatalf("selected = %#v, want all targets", selected)
	}
	if selected[0].node.ID != head.node.ID || selected[1].node.ID != replaceAll.node.ID {
		t.Fatalf("selected order = [%s, %s], want retrieval head then protected replace anchor", selected[0].node.ID, selected[1].node.ID)
	}
}

func TestLocalizationEvidenceTargetsPrioritizeExplicitConsumerBeforeCausalOwner(t *testing.T) {
	explicit := exploreTarget{node: &graph.Node{
		ID:       "src/Monolog/Handler/StreamHandler.php::StreamHandler.write",
		Name:     "write",
		QualName: "StreamHandler.write",
		Kind:     graph.KindMethod,
		FilePath: "src/Monolog/Handler/StreamHandler.php",
	}}
	owner := exploreTarget{
		node: &graph.Node{
			ID:       "src/Monolog/Handler/RotatingFileHandler.php::RotatingFileHandler.__construct",
			Name:     "__construct",
			QualName: "RotatingFileHandler.__construct",
			Kind:     graph.KindMethod,
			FilePath: "src/Monolog/Handler/RotatingFileHandler.php",
		},
		divergentDefaultOwner: true,
	}
	ownerType := exploreTarget{
		node: &graph.Node{
			ID:       "src/Monolog/Handler/RotatingFileHandler.php::RotatingFileHandler",
			Name:     "RotatingFileHandler",
			QualName: "RotatingFileHandler",
			Kind:     graph.KindType,
			FilePath: "src/Monolog/Handler/RotatingFileHandler.php",
		},
		divergentDefaultType: true,
	}
	targets := []exploreTarget{owner, ownerType, explicit}
	selected := localizationEvidenceTargetsFromDraft(
		"chmod failure in src/Monolog/Handler/StreamHandler.php::StreamHandler.write",
		owner.node.ID,
		targets,
		[]exploreDraftEntry{{node: owner.node}, {node: ownerType.node}, {node: explicit.node}},
	)
	if len(selected) < 2 {
		t.Fatalf("selected = %#v, want explicit target plus causal owner", selected)
	}
	if selected[0].node.ID != explicit.node.ID || selected[1].node.ID != owner.node.ID {
		t.Fatalf("selected order = [%s, %s], want explicit consumer then causal owner", selected[0].node.ID, selected[1].node.ID)
	}
}
