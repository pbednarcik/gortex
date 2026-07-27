package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

func componentConjunctionTestCandidate(id, name, qualName, file string, kind graph.NodeKind) *rerank.Candidate {
	return &rerank.Candidate{Node: &graph.Node{
		ID:          id,
		Name:        name,
		QualName:    qualName,
		Kind:        kind,
		FilePath:    file,
		WorkspaceID: "workspace",
		ProjectID:   "project",
		RepoPrefix:  "repo",
	}}
}

func componentConjunctionTestIDs(candidates []*rerank.Candidate) []string {
	ids := make([]string, len(candidates))
	for index, candidate := range candidates {
		if candidate != nil && candidate.Node != nil {
			ids[index] = candidate.Node.ID
		}
	}
	return ids
}

func componentConjunctionAssertIDs(t *testing.T, got []*rerank.Candidate, want []string) {
	t.Helper()
	gotIDs := componentConjunctionTestIDs(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("candidate count = %d, want %d: got=%v", len(gotIDs), len(want), gotIDs)
	}
	for index := range want {
		if gotIDs[index] != want[index] {
			t.Fatalf("candidate %d = %q, want %q: got=%v", index, gotIDs[index], want[index], gotIDs)
		}
	}
}

func TestReserveExploreComponentConjunctionPromotesRipgrepPrinterUtility(t *testing.T) {
	utility := componentConjunctionTestCandidate(
		"crates/printer/src/util.rs::Replacer.replace_all",
		"replace_all",
		"Replacer<M>.replace_all",
		"crates/printer/src/util.rs",
		graph.KindMethod,
	)
	utility.Signals = map[string]float64{"prior": 0.25}
	candidates := []*rerank.Candidate{
		componentConjunctionTestCandidate(
			"crates/matcher/src/captures.rs::replace_with_captures",
			"replace_with_captures",
			"Captures.replace_with_captures",
			"crates/matcher/src/captures.rs",
			graph.KindFunction,
		),
		componentConjunctionTestCandidate(
			"crates/printer/src/standard.rs::StandardBuilder.only_matching",
			"only_matching",
			"StandardBuilder.only_matching",
			"crates/printer/src/standard.rs",
			graph.KindMethod,
		),
		componentConjunctionTestCandidate(
			"crates/printer/src/standard.rs::invert_context_only_matching_multi_line",
			"invert_context_only_matching_multi_line",
			"invert_context_only_matching_multi_line",
			"crates/printer/src/standard.rs",
			graph.KindFunction,
		),
		componentConjunctionTestCandidate(
			"crates/core/src/search.rs::search_reader",
			"search_reader",
			"search_reader",
			"crates/core/src/search.rs",
			graph.KindFunction,
		),
		componentConjunctionTestCandidate(
			"crates/core/src/flags.rs::build_flags",
			"build_flags",
			"build_flags",
			"crates/core/src/flags.rs",
			graph.KindFunction,
		),
		utility,
	}
	query := "only matching multi line captures must replace printer text"
	got := reserveExploreComponentConjunction(query, rerank.QueryClassConcept, candidates, 5)

	if len(got) != len(candidates) {
		t.Fatalf("candidate count = %d, want %d", len(got), len(candidates))
	}
	if got[0] != candidates[0] {
		t.Fatal("semantic head changed")
	}
	if got[4].Node.ID != utility.Node.ID {
		t.Fatalf("final visible candidate = %q, want %q", got[4].Node.ID, utility.Node.ID)
	}
	if got[4].Signals[exploreConceptComplementSignal] != 1 {
		t.Fatalf("utility signals = %#v, want component complement", got[4].Signals)
	}
	if got[4].Signals["prior"] != 0.25 {
		t.Fatalf("utility signals = %#v, want prior signal preserved", got[4].Signals)
	}
	if utility.Signals[exploreConceptComplementSignal] != 0 {
		t.Fatal("original utility candidate was mutated")
	}

	wantIDs := componentConjunctionTestIDs(got)
	again := reserveExploreComponentConjunction(query, rerank.QueryClassConcept, got, 5)
	componentConjunctionAssertIDs(t, again, wantIDs)
	if again[4].Signals[exploreConceptComplementSignal] != 1 {
		t.Fatalf("second pass signals = %#v, want stable component complement", again[4].Signals)
	}
}

func TestReserveExploreComponentConjunctionRejectsFalsePositives(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		maxSymbols int
		candidates []*rerank.Candidate
	}{
		{
			name:       "repeated term has no local marginal",
			query:      "only matching must replace output",
			maxSymbols: 2,
			candidates: []*rerank.Candidate{
				componentConjunctionTestCandidate("src/printer/standard.rs::OnlyMatchingPrinter", "OnlyMatchingPrinter", "OnlyMatchingPrinter", "src/printer/standard.rs", graph.KindType),
				componentConjunctionTestCandidate("src/printer/util.rs::matching_only", "matching_only", "matching_only", "src/printer/util.rs", graph.KindFunction),
			},
		},
		{
			name:       "different parent directory",
			query:      "only matching must replace text",
			maxSymbols: 2,
			candidates: []*rerank.Candidate{
				componentConjunctionTestCandidate("src/printer/standard.rs::OnlyMatchingPrinter", "OnlyMatchingPrinter", "OnlyMatchingPrinter", "src/printer/standard.rs", graph.KindType),
				componentConjunctionTestCandidate("src/replacer/util.rs::replace_all", "replace_all", "Replacer.replace_all", "src/replacer/util.rs", graph.KindMethod),
			},
		},
		{
			name:       "generic declaration",
			query:      "only matching handle must replace text",
			maxSymbols: 2,
			candidates: func() []*rerank.Candidate {
				generic := componentConjunctionTestCandidate("src/printer/util.rs::Handler.Handle", "Handle", "Handler.Handle", "src/printer/util.rs", graph.KindMethod)
				generic.Node.Meta = map[string]any{"signature": "interface Handler", "doc": "only matching replacement"}
				return []*rerank.Candidate{
					componentConjunctionTestCandidate("src/printer/standard.rs::OnlyMatchingPrinter", "OnlyMatchingPrinter", "OnlyMatchingPrinter", "src/printer/standard.rs", graph.KindType),
					generic,
				}
			}(),
		},
		{
			name:       "low value terms only",
			query:      "line output diagnostic",
			maxSymbols: 2,
			candidates: []*rerank.Candidate{
				componentConjunctionTestCandidate("src/printer/standard.rs::LineOutputPrinter", "LineOutputPrinter", "LineOutputPrinter", "src/printer/standard.rs", graph.KindType),
				componentConjunctionTestCandidate("src/printer/util.rs::trim_line_output", "trim_line_output", "trim_line_output", "src/printer/util.rs", graph.KindFunction),
			},
		},
		{
			name:       "root component",
			query:      "only matching must replace text",
			maxSymbols: 2,
			candidates: []*rerank.Candidate{
				componentConjunctionTestCandidate("standard.rs::OnlyMatchingPrinter", "OnlyMatchingPrinter", "OnlyMatchingPrinter", "standard.rs", graph.KindType),
				componentConjunctionTestCandidate("util.rs::replace_all", "replace_all", "Replacer.replace_all", "util.rs", graph.KindMethod),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := componentConjunctionTestIDs(test.candidates)
			got := reserveExploreComponentConjunction(test.query, rerank.QueryClassConcept, test.candidates, test.maxSymbols)
			componentConjunctionAssertIDs(t, got, want)
			for _, candidate := range got {
				if candidate != nil && candidate.Signals[exploreConceptComplementSignal] > 0 {
					t.Fatalf("unexpected component complement on %q", candidate.Node.ID)
				}
			}
		})
	}
}

func TestReserveExploreComponentConjunctionRejectsAmbiguousComponents(t *testing.T) {
	candidates := []*rerank.Candidate{
		componentConjunctionTestCandidate("src/a/standard.rs::OnlyMatchingA", "OnlyMatchingA", "OnlyMatchingA", "src/a/standard.rs", graph.KindType),
		componentConjunctionTestCandidate("src/b/standard.rs::OnlyMatchingB", "OnlyMatchingB", "OnlyMatchingB", "src/b/standard.rs", graph.KindType),
		componentConjunctionTestCandidate("src/other/one.rs::parse_config", "parse_config", "parse_config", "src/other/one.rs", graph.KindFunction),
		componentConjunctionTestCandidate("src/other/two.rs::build_config", "build_config", "build_config", "src/other/two.rs", graph.KindFunction),
		componentConjunctionTestCandidate("src/a/util.rs::replace_all", "replace_all", "Replacer.replace_all", "src/a/util.rs", graph.KindMethod),
		componentConjunctionTestCandidate("src/b/util.rs::replace_all", "replace_all", "Replacer.replace_all", "src/b/util.rs", graph.KindMethod),
	}
	want := componentConjunctionTestIDs(candidates)
	got := reserveExploreComponentConjunction("only matching must replace text", rerank.QueryClassConcept, candidates, 4)
	componentConjunctionAssertIDs(t, got, want)
	for _, candidate := range got {
		if candidate.Signals[exploreConceptComplementSignal] > 0 {
			t.Fatalf("ambiguous candidate %q was marked", candidate.Node.ID)
		}
	}
}

func TestReserveExploreComponentConjunctionMarksVisibleCandidateInPlace(t *testing.T) {
	candidates := []*rerank.Candidate{
		componentConjunctionTestCandidate("src/printer/standard.rs::OnlyMatchingPrinter", "OnlyMatchingPrinter", "OnlyMatchingPrinter", "src/printer/standard.rs", graph.KindType),
		componentConjunctionTestCandidate("src/printer/util.rs::replace_all", "replace_all", "Replacer.replace_all", "src/printer/util.rs", graph.KindMethod),
	}
	want := componentConjunctionTestIDs(candidates)
	got := reserveExploreComponentConjunction("only matching must replace text", rerank.QueryClassConcept, candidates, 2)
	componentConjunctionAssertIDs(t, got, want)
	if got[1].Signals[exploreConceptComplementSignal] != 1 {
		t.Fatalf("visible utility signals = %#v, want component complement", got[1].Signals)
	}
	if got[0] != candidates[0] {
		t.Fatal("visible marking changed the semantic head")
	}
}
