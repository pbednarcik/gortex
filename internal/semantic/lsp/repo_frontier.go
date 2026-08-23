package lsp

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

const lspRepoFileFrontierSize = 32

// lspRepoProjectionReader is implemented by the SQLite store. Every method
// pushes repository, language, file, and edge-kind predicates into SQL. The
// file slice is an explicitly bounded frontier supplied by the caller.
type lspRepoProjectionReader interface {
	LSPRepoFileCounts(repoPrefix string, languages []string) (totals, unstamped map[string]int)
	LSPRepoNodesByFiles(repoPrefix string, languages, filePaths []string, unstampedOnly bool) []*graph.Node
	LSPRepoConfirmableEdgesByFiles(repoPrefix string, languages, filePaths []string, ambiguousOnly bool) []*graph.Edge
	LSPRepoEdgesByFilesAndKinds(repoPrefix string, languages, filePaths []string, kinds []graph.EdgeKind) []*graph.Edge
	LSPNodeFanInCounts(nodeIDs []string) map[string]int
	LSPInEdgesByNodeIDsAndKinds(nodeIDs []string, kinds []graph.EdgeKind) []*graph.Edge
}

type lspRepoProjection struct {
	repoNodes                []*graph.Node
	langNodes                []*graph.Node
	repoEdges                []*graph.Edge
	targets                  []enrichTarget
	symbolsTotal             int
	skippedAlreadyStamped    int
	frontierPages            int
	frontierPeakFiles        int
	frontierPeakNodes        int
	frontierPeakEdges        int
	retainedLocationNodes    int
	retainedConfirmableEdges int
	fanInByID                map[string]int
	inboundDispatchEdges     []*graph.Edge
}

// fullUnstampedCandidates re-fetches candidate nodes in full and drops any
// the tier stamp already covers. Light scans restore only promoted Meta
// columns, so a blob-only stamp (semantic_heavy) is invisible until this
// batch — without the recheck every heavy drain rebuilds the whole frontier.
func (p *Provider) fullUnstampedCandidates(g graph.Store, candidateIDs []string) []*graph.Node {
	if len(candidateIDs) == 0 {
		return nil
	}
	full := g.GetNodesByIDs(candidateIDs)
	nodes := make([]*graph.Node, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		node := full[id]
		if node == nil || p.tierStamped(node) {
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// readLSPRepoProjection builds the smallest exact projection consumed by the
// existing enrichment phases. It deliberately retains only language symbol
// locations and LSP-adjudicable adjacency; file/import nodes and opaque Meta
// blobs do not enter the location index, while structural containment is
// limited to member_of (the one structural kind hierarchy matching needs).
// SQLite is read in fixed file frontiers so transient decode memory is bounded.
func (p *Provider) readLSPRepoProjection(g graph.Store, repoPrefix string) (*lspRepoProjection, bool) {
	reader, ok := g.(lspRepoProjectionReader)
	if !ok {
		return nil, false
	}

	totalsByFile, unstampedByFile := reader.LSPRepoFileCounts(repoPrefix, p.languages)
	files := make([]string, 0, len(totalsByFile))
	projection := &lspRepoProjection{}
	for filePath, total := range totalsByFile {
		if total <= 0 || semantic.IsLowValueForEnrichment(filePath, p.excludeGlobs) {
			continue
		}
		files = append(files, filePath)
		projection.symbolsTotal += total
		projection.skippedAlreadyStamped += total - unstampedByFile[filePath]
	}
	sort.Strings(files)

	candidateSeen := make(map[string]struct{})
	for start := 0; start < len(files); start += lspRepoFileFrontierSize {
		end := start + lspRepoFileFrontierSize
		if end > len(files) {
			end = len(files)
		}
		frontier := files[start:end]
		projection.frontierPages++
		if len(frontier) > projection.frontierPeakFiles {
			projection.frontierPeakFiles = len(frontier)
		}

		locations := reader.LSPRepoNodesByFiles(repoPrefix, p.languages, frontier, false)
		if len(locations) > projection.frontierPeakNodes {
			projection.frontierPeakNodes = len(locations)
		}
		projection.repoNodes = append(projection.repoNodes, locations...)

		candidateIDs := make([]string, 0, unstampedCount(frontier, unstampedByFile))
		for _, node := range locations {
			if node == nil || p.tierStamped(node) {
				continue
			}
			if _, seen := candidateSeen[node.ID]; seen {
				continue
			}
			candidateSeen[node.ID] = struct{}{}
			candidateIDs = append(candidateIDs, node.ID)
		}
		// The location projection intentionally omits opaque Meta. Re-fetch
		// only the unstamped candidates that may be enriched or ranked; one
		// bounded batch per frontier preserves receiver/trait markers exactly.
		projection.langNodes = append(projection.langNodes, p.fullUnstampedCandidates(g, candidateIDs)...)

		confirmable := reader.LSPRepoConfirmableEdgesByFiles(repoPrefix, p.languages, frontier, false)
		memberOf := reader.LSPRepoEdgesByFilesAndKinds(repoPrefix, p.languages, frontier, []graph.EdgeKind{graph.EdgeMemberOf})
		pageEdges := len(confirmable) + len(memberOf)
		if pageEdges > projection.frontierPeakEdges {
			projection.frontierPeakEdges = pageEdges
		}
		projection.repoEdges = append(projection.repoEdges, confirmable...)
		projection.repoEdges = append(projection.repoEdges, memberOf...)
	}

	candidateIDs := make([]string, 0, len(projection.langNodes))
	inboundIDSet := make(map[string]struct{}, len(projection.langNodes))
	for _, node := range projection.langNodes {
		candidateIDs = append(candidateIDs, node.ID)
		inboundIDSet[node.ID] = struct{}{}
	}
	// Dispatch classification also inspects the candidate method's parent
	// type. member_of is already projected, so extend the inbound frontier
	// with those parent IDs without loading any unrelated adjacency.
	for _, edge := range projection.repoEdges {
		if edge != nil && edge.Kind == graph.EdgeMemberOf {
			if _, candidate := candidateSeen[edge.From]; candidate {
				inboundIDSet[edge.To] = struct{}{}
			}
		}
	}
	if len(candidateIDs) > 0 {
		projection.fanInByID = reader.LSPNodeFanInCounts(candidateIDs)
	}
	inboundIDs := make([]string, 0, len(inboundIDSet))
	for id := range inboundIDSet {
		inboundIDs = append(inboundIDs, id)
	}
	sort.Strings(inboundIDs)
	if len(inboundIDs) > 0 {
		projection.inboundDispatchEdges = reader.LSPInEdgesByNodeIDsAndKinds(inboundIDs, []graph.EdgeKind{
			graph.EdgeOverrides, graph.EdgeImplements, graph.EdgeExtends,
		})
	}

	// Confirmation needs full source/target nodes, including the rare
	// file/import source and cross-repository target omitted from the compact
	// location projection. Resolve the complete endpoint set in one batch.
	endpointSet := make(map[string]struct{})
	for _, edge := range projection.repoEdges {
		if edge == nil || edge.Confidence >= 1 || !confirmableEdgeKind(edge.Kind) {
			continue
		}
		endpointSet[edge.From] = struct{}{}
		endpointSet[edge.To] = struct{}{}
	}
	nodesByID := make(map[string]*graph.Node, len(projection.repoNodes)+len(projection.langNodes))
	for _, node := range projection.repoNodes {
		if node != nil {
			nodesByID[node.ID] = node
		}
	}
	for _, node := range projection.langNodes {
		if node != nil {
			nodesByID[node.ID] = node
		}
	}
	// Confirmation reads blob-only node state — the heavy stamp, the
	// banked references verdict, annotation stamps — and the location
	// projection is blob-blind: a light endpoint hides a banked verdict,
	// so the target is re-asked on every drain as if it never earned one.
	// Resolve EVERY confirmable endpoint in full, not only the ones the
	// projection omitted; frontier candidates are already full and are
	// simply refreshed by the same batch.
	endpointIDs := make([]string, 0, len(endpointSet))
	for id := range endpointSet {
		endpointIDs = append(endpointIDs, id)
	}
	sort.Strings(endpointIDs)
	var fullEndpoints map[string]*graph.Node
	if len(endpointIDs) > 0 {
		fullEndpoints = g.GetNodesByIDs(endpointIDs)
	}
	missingIDs := make([]string, 0)
	for _, id := range endpointIDs {
		if nodesByID[id] == nil {
			missingIDs = append(missingIDs, id)
		}
	}
	// Swap the full rows into the slice too, not only the map: the enrich
	// pass rebuilds its own view from projection.repoNodes, so a light
	// entry left in the slice would resurface there.
	for i, node := range projection.repoNodes {
		if node == nil {
			continue
		}
		if full := fullEndpoints[node.ID]; full != nil {
			projection.repoNodes[i] = full
		}
	}
	projection.repoNodes = append(projection.repoNodes, orderedNodes(missingIDs, fullEndpoints)...)
	for _, id := range endpointIDs {
		if node := fullEndpoints[id]; node != nil {
			nodesByID[id] = node
		}
	}
	for _, edge := range projection.repoEdges {
		if edge == nil || edge.Confidence >= 1 || !confirmableEdgeKind(edge.Kind) {
			continue
		}
		if source := nodesByID[edge.From]; source != nil {
			projection.targets = append(projection.targets, enrichTarget{node: source, edge: edge})
		}
	}

	projection.retainedLocationNodes = len(projection.repoNodes)
	projection.retainedConfirmableEdges = len(projection.repoEdges)
	return projection, true
}

func unstampedCount(files []string, counts map[string]int) int {
	total := 0
	for _, filePath := range files {
		total += counts[filePath]
	}
	return total
}

func orderedNodes(ids []string, nodes map[string]*graph.Node) []*graph.Node {
	out := make([]*graph.Node, 0, len(ids))
	for _, id := range ids {
		if node := nodes[id]; node != nil {
			out = append(out, node)
		}
	}
	return out
}
