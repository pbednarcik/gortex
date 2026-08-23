package lsp

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// enrichSweepReserveFraction is the slice of a repo's enrichment deadline held
// back from the targeted-edge passes (implementations + reference confirm) for
// the post-confirm hover / hierarchy sweep. Reserving it stops a round-trip-
// bound confirm pass from consuming the whole window and starving the add
// phase (edges_added stuck at 0). 0.4 keeps confirm-first — the majority of the
// window still confirms tiers — while guaranteeing the sweep runs.
const enrichSweepReserveFraction = 0.4

// enrichTarget pairs an ambiguous edge with its (repo+language) source node —
// the unit the reference-confirm pass promotes or refutes.
type enrichTarget struct {
	node *graph.Node
	edge *graph.Edge
}

// confirmableEdgeKind reports whether the reference-confirm pass and its
// definition-rebind fallback can meaningfully adjudicate an edge kind.
//
// The pass matches an edge's SITE line against the reference list of its target
// declaration, and on a miss asks the server what the site resolves to and
// rebinds. That is meaningful for every edge that anchors a use / reference /
// flow site the server can resolve:
//
//   - use sites: calls, references, field reads/writes, type instantiations;
//   - type positions: typed_as / returns — when a concrete impl and the
//     interface it satisfies share a name the resolver may pick the wrong one,
//     and the server's definition rebinds to the declared type (measured: it
//     binds a param / return typed as an interface to the interface, not an
//     implementation);
//   - dataflow through a call: arg_of / value_flow / returns_to — the server
//     disambiguates the callee the value flows into (measured: it rebinds an
//     arg_of from the wrong same-named method onto the one the call actually
//     resolves to, e.g. Pool.EnqueueJob rather than Client.EnqueueJob).
//
// It is NOT meaningful for STRUCTURAL CONTAINMENT — a declaration's fixed
// relationship to the structure that encloses it: a method to its type
// (member_of), a symbol to its definer (defines) or container (contains), a
// param to its function (param_of), a file to an imported package (imports), a
// closure to a captured variable (captures). These are AST-deterministic and
// carry no distinct use site, so the reference match wastes a round-trip and,
// on a miss, can feed a correct edge into the definition-rebind fallback and
// mutate its target (observed: a local variable's member_of edge rebound onto
// an unrelated same-named method). member_of alone is the single largest
// confidence<1.0 kind, so skipping this family removes a large slice of the
// confirm round-trips while, if anything, improving correctness.
func confirmableEdgeKind(k graph.EdgeKind) bool {
	switch k {
	case graph.EdgeMemberOf, graph.EdgeDefines, graph.EdgeContains,
		graph.EdgeParamOf, graph.EdgeImports, graph.EdgeCaptures:
		return false
	default:
		return true
	}
}

// confirmGroup is the confirm pass's per-file work unit: every ambiguous
// target whose referent declaration lives in rel. Grouping lets one didOpen of
// rel serve all its targets' reference queries.
type confirmGroup struct {
	rel     string
	targets []enrichTarget
}

// groupConfirmTargets buckets confirm targets by the file of the referent
// declaration (the file findReferences is positioned in) and orders the groups
// highest-yield-first. The ordering is the value-prioritisation lever: when the
// deadline cuts the pass short, the files carrying the most ambiguous edges —
// where a confirmation resolves the most tiers — have already run. Ordering is
// deterministic (count desc, then path) so a resumed / replayed index is
// stable.
//
// skipFile, when non-nil, drops any referent file the predicate rejects —
// the compile-database-degraded pass passes it to keep clangd from opening
// header translation units it cannot resolve without a database.
func (p *Provider) groupConfirmTargets(nodesByID map[string]*graph.Node, targets []enrichTarget, skipFile func(rel string) bool) []*confirmGroup {
	byFile := map[string]*confirmGroup{}
	var order []*confirmGroup
	for _, t := range targets {
		toNode := nodesByID[t.edge.To]
		if toNode == nil {
			continue
		}
		if p.heavyDelta && nodeRefsStamped(toNode) {
			// A prior drain already adjudicated this target's references
			// (or it is statically terminal-unconfirmable) — the identical
			// question buys nothing until a reparse drops the stamp.
			continue
		}
		if _, ok := lspLine(toNode); !ok {
			continue
		}
		rel := nodeRelPath(toNode)
		if rel == "" {
			continue
		}
		if !p.servesFile(rel) {
			continue // never open a referent file this server can't compile
		}
		if skipFile != nil && skipFile(rel) {
			continue // degraded mode: never open this referent (e.g. a header)
		}
		grp := byFile[rel]
		if grp == nil {
			grp = &confirmGroup{rel: rel}
			byFile[rel] = grp
			order = append(order, grp)
		}
		grp.targets = append(grp.targets, t)
	}
	sort.SliceStable(order, func(i, j int) bool {
		if len(order[i].targets) != len(order[j].targets) {
			return len(order[i].targets) > len(order[j].targets)
		}
		return order[i].rel < order[j].rel
	})
	return order
}

// confirmRefMatchesSite reports whether the server's reference list ties the
// edge's own call site to its target declaration — the identity anchor that
// promotes an ambiguous edge to the lsp tier. A recorded site line is matched
// exactly (±1 for wrapped call expressions); an edge without one falls back to
// containment in the caller's span. Pure over its inputs, so it is safe to call
// from the parallel sweep.
func (p *Provider) confirmRefMatchesSite(refs []Location, absRoot, repoPrefix string, t enrichTarget) bool {
	callerRel := viewPathKey(nodeRelPath(t.node))
	siteRel := viewPathKey(edgeSiteRelPath(t.edge, repoPrefix, callerRel))
	siteLine := t.edge.Line
	for _, ref := range refs {
		// uriToPath returns a repo-relative path while node/edge FilePaths are
		// prefixed, so compare against stripped paths — slash-normalized, since
		// store rows may spell separators either way.
		refPath := viewPathKey(uriToPath(ref.URI, absRoot))
		refLine := ref.Range.Start.Line + 1
		if siteLine > 0 {
			if refPath == siteRel && refLine >= siteLine-1 && refLine <= siteLine+1 {
				return true
			}
			continue
		}
		if refPath == callerRel && refLine >= t.node.StartLine && refLine <= t.node.EndLine {
			return true
		}
	}
	return false
}
