package mcp

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	exploreCausalChangeFileNodeCap     = 96
	exploreCausalConsumerSeedLimit     = 5
	exploreCausalConsumerDepth         = 3
	exploreCausalConsumerFrontierCap   = 24
	exploreCausalConsumerNodeFanoutCap = 8
)

type exploreCausalChangeCandidate struct {
	node         *graph.Node
	continuation *graph.Node
	parentID     string
	parentRank   int
	direction    string
	hop          int
	delegated    bool
	consumer     bool
	crossFile    bool
	overlap      int
	longest      int
}

type exploreCausalChangeHydrator func(*graph.Node) ([]*graph.Node, bool)

// promoteExploreCausalChangeTargets reserves one graph-proven change site
// before localization packing. It starts only from hydrated, task-aligned
// production callables and follows their already-bounded direct relations or
// depth-two callee projection. This makes a wrapper's implementation and a
// task-aligned cross-file caller/callee compete on causality instead of raw
// keyword rank.
//
// When the promoted callable crosses a file boundary, one same-file owning
// type is admitted as well. A uniquely returned type wins over the callable's
// enclosing type because it owns the state constructed by builder/factory
// methods; otherwise the enclosing type is the natural change owner. The lane
// never scans a repository, reads at most one source body, and replaces only
// unprotected retrieval-tail rows.
func promoteExploreCausalChangeTargets(
	task string,
	targets []exploreTarget,
	store graph.Store,
	maxSymbols int,
	readSource func(*graph.Node) string,
	hydrateBridges ...exploreCausalChangeHydrator,
) []exploreTarget {
	if len(targets) == 0 || !exploreQueryIsConceptTask(task) {
		return targets
	}
	var hydrateBridge exploreCausalChangeHydrator
	if len(hydrateBridges) > 0 {
		hydrateBridge = hydrateBridges[0]
	}
	candidate, ok := selectExploreCausalChangeTargetWithConsumers(task, targets, store)
	if !ok {
		return targets
	}

	bridge, bridgePresent := exploreTargetByID(targets, candidate.node.ID)
	bridge.node = candidate.node
	bridge.causalChangeBridge = true
	bridge.causalChangeLeaf = false
	if candidate.hop == 1 && (candidate.direction == "caller" || candidate.direction == "callee") {
		bridge.localizationRelation = "direct_" + candidate.direction
	}
	if strings.TrimSpace(bridge.source) == "" && readSource != nil {
		bridge.source = readSource(candidate.node)
	}
	if !bridge.directCalleesComplete && hydrateBridge != nil {
		bridge.callees, bridge.directCalleesComplete = hydrateBridge(candidate.node)
	}

	leaf := bridge
	leafPresent := bridgePresent
	bridgeNeeded := false
	if continuation := selectExploreCausalContinuation(task, bridge, candidate.continuation); continuation != nil {
		leaf, leafPresent = exploreTargetByID(targets, continuation.ID)
		leaf.node = continuation
		leaf.causalChangeBridge = false
		leaf.causalChangeLeaf = true
		leaf.localizationRelation = "direct_callee"
		bridgeNeeded = true
	} else {
		leaf.causalChangeBridge = false
		leaf.causalChangeLeaf = true
	}

	var owner exploreTarget
	ownerPresent := false
	if candidate.crossFile {
		if node := exploreCausalChangeOwner(task, candidate.node, store); node != nil {
			owner, ownerPresent = exploreTargetByID(targets, node.ID)
			owner.node = node
			owner.causalChangeOwner = true
		}
	}

	missing := 0
	if bridgeNeeded && !bridgePresent {
		missing++
	}
	if !leafPresent {
		missing++
	}
	if owner.node != nil && !ownerPresent {
		missing++
	}
	if maxSymbols <= 0 {
		maxSymbols = len(targets)
	}
	overflow := len(targets) + missing - maxSymbols
	if overflow < 0 {
		overflow = 0
	}

	evict := make(map[int]struct{}, overflow)
	for index := len(targets) - 1; index >= 0 && len(evict) < overflow; index-- {
		if exploreCausalChangeAdmissionProtected(task, targets, index, candidate.parentID) {
			continue
		}
		evict[index] = struct{}{}
	}
	if len(evict) != overflow {
		return targets
	}

	promoted := make([]exploreTarget, 0, len(targets)-len(evict)+missing)
	for index, target := range targets {
		if _, drop := evict[index]; drop {
			continue
		}
		switch {
		case target.node != nil && target.node.ID == leaf.node.ID:
			promoted = append(promoted, leaf)
		case bridgeNeeded && target.node != nil && target.node.ID == bridge.node.ID:
			promoted = append(promoted, bridge)
		case owner.node != nil && target.node != nil && target.node.ID == owner.node.ID:
			promoted = append(promoted, owner)
		default:
			promoted = append(promoted, target)
		}
	}
	if bridgeNeeded && !bridgePresent {
		promoted = append(promoted, bridge)
	}
	if !leafPresent {
		promoted = append(promoted, leaf)
	}
	if owner.node != nil && !ownerPresent {
		promoted = append(promoted, owner)
	}
	return promoted
}

type exploreCausalContinuationMetric struct {
	node      *graph.Node
	hinted    bool
	delegated bool
	sameOwner bool
	compound  bool
	overlap   int
	mentions  int
	segments  int
	shared    int
	longest   int
}

func selectExploreCausalContinuation(task string, bridge exploreTarget, hinted *graph.Node) *graph.Node {
	if bridge.node == nil || !bridge.directCalleesComplete || len(bridge.callees) == 0 {
		return nil
	}
	query := shapeExploreQuery(task)
	if exploreQueryIsConceptTask(query) {
		query = stripLeadingExploreDirective(query)
	}
	terms := exploreTerminalTerms(query)
	seen := make(map[string]struct{}, len(bridge.callees))
	var best exploreCausalContinuationMetric
	tied := false
	compare := func(left, right exploreCausalContinuationMetric) int {
		boolCompare := func(a, b bool) int {
			switch {
			case a && !b:
				return 1
			case !a && b:
				return -1
			default:
				return 0
			}
		}
		for _, pair := range [][2]bool{
			{left.hinted, right.hinted},
			{left.delegated, right.delegated},
			{left.sameOwner, right.sameOwner},
			{left.compound, right.compound},
		} {
			if order := boolCompare(pair[0], pair[1]); order != 0 {
				return order
			}
		}
		for _, pair := range [][2]int{
			{left.overlap, right.overlap},
			{left.mentions, right.mentions},
			{left.segments, right.segments},
			{left.shared, right.shared},
			{left.longest, right.longest},
		} {
			if pair[0] > pair[1] {
				return 1
			}
			if pair[0] < pair[1] {
				return -1
			}
		}
		return 0
	}

	bridgePath := exploreNormalizedPath(nodeDisplayPath(bridge.node))
	for _, node := range bridge.callees {
		if node == nil || node.ID == "" || node.ID == bridge.node.ID ||
			(node.Kind != graph.KindFunction && node.Kind != graph.KindMethod) ||
			exploreDraftIsTestNode(node) || !exploreNodesShareExactScope(bridge.node, node) {
			continue
		}
		if _, duplicate := seen[node.ID]; duplicate {
			continue
		}
		seen[node.ID] = struct{}{}
		sameOwner := exploreSameCallableOwner(bridge.node, node)
		sameFile := bridgePath != "" && bridgePath == exploreNormalizedPath(nodeDisplayPath(node))
		if !sameOwner && !sameFile {
			continue
		}
		delegated := exploreDelegatedImplementationCallee(bridge.node, node)
		mentions := exploreBodyIdentifierMentions(bridge.source, node.Name)
		if !delegated && mentions == 0 {
			continue
		}
		overlap, longest := exploreDraftTermOverlap(terms, node)
		shared, _ := exploreStructuralSignals(bridge.node, node)
		segments := exploreIdentifierSegmentCount(node.Name)
		metric := exploreCausalContinuationMetric{
			node: node, hinted: hinted != nil && hinted.ID == node.ID,
			delegated: delegated, sameOwner: sameOwner, compound: segments >= 2,
			overlap: overlap, mentions: mentions, segments: segments,
			shared: shared, longest: longest,
		}
		if best.node == nil {
			best, tied = metric, false
			continue
		}
		switch compare(metric, best) {
		case 1:
			best, tied = metric, false
		case 0:
			tied = true
		}
	}
	if best.node == nil || tied {
		return nil
	}
	return best.node
}

func selectExploreCausalChangeTargetWithConsumers(
	task string,
	targets []exploreTarget,
	store graph.Store,
) (exploreCausalChangeCandidate, bool) {
	direct, directOK := selectExploreCausalChangeTarget(task, targets)
	consumer, consumerOK := selectExploreCausalConsumerTarget(task, targets, store)
	switch {
	case !consumerOK:
		return direct, directOK
	case !directOK || exploreCausalChangeLess(consumer, direct):
		return consumer, true
	default:
		return direct, true
	}
}

// selectExploreCausalConsumerTarget fills one missing implementation slot by
// following reverse call edges from the bounded ranked head. It collapses only
// same-file wrappers and admits only a task-aligned production callable in a
// directory already represented by the evidence page. That directory join is
// the proof that a cross-file caller connects two retrieved feature families;
// it prevents a generic high-fanout caller walk from becoming another broad
// search channel.
func selectExploreCausalConsumerTarget(
	task string,
	targets []exploreTarget,
	store graph.Store,
) (exploreCausalChangeCandidate, bool) {
	if store == nil || len(targets) == 0 {
		return exploreCausalChangeCandidate{}, false
	}
	terms := exploreTerminalTerms(shapeExploreQuery(task))
	if len(terms) == 0 {
		return exploreCausalChangeCandidate{}, false
	}
	targetIDs := make(map[string]struct{}, len(targets))
	targetDirectories := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.node == nil || target.node.ID == "" {
			continue
		}
		targetIDs[target.node.ID] = struct{}{}
		if directory := exploreCausalChangeDirectory(target.node); directory != "" {
			targetDirectories[directory] = struct{}{}
		}
	}
	if len(targetDirectories) == 0 {
		return exploreCausalChangeCandidate{}, false
	}

	byID := make(map[string]exploreCausalChangeCandidate)
	seedLimit := min(len(targets), exploreCausalConsumerSeedLimit)
	for parentRank := 0; parentRank < seedLimit; parentRank++ {
		seed := targets[parentRank]
		if seed.node == nil || seed.node.ID == "" ||
			(seed.node.Kind != graph.KindFunction && seed.node.Kind != graph.KindMethod) ||
			exploreDraftIsTestNode(seed.node) {
			continue
		}
		seedPath := exploreNormalizedPath(nodeDisplayPath(seed.node))
		if seedPath == "" {
			continue
		}
		expanded := make(map[string]struct{}, exploreCausalConsumerFrontierCap)
		frontier := make([]*graph.Node, 0, 1+len(seed.callees))
		appendFrontier := func(node *graph.Node) {
			if node == nil || node.ID == "" || !exploreNodesShareExactScope(seed.node, node) {
				return
			}
			if _, exists := expanded[node.ID]; exists {
				return
			}
			expanded[node.ID] = struct{}{}
			frontier = append(frontier, node)
		}
		appendFrontier(seed.node)
		for _, callee := range seed.callees {
			appendFrontier(callee)
		}

		for depth := 1; depth <= exploreCausalConsumerDepth && len(frontier) > 0; depth++ {
			sort.SliceStable(frontier, func(i, j int) bool { return frontier[i].ID < frontier[j].ID })
			if len(frontier) > exploreCausalConsumerFrontierCap {
				frontier = frontier[:exploreCausalConsumerFrontierCap]
			}
			ids := make([]string, len(frontier))
			for index, node := range frontier {
				ids[index] = node.ID
			}
			incoming := store.GetInEdgesByNodeIDs(ids)
			callerSet := make(map[string]struct{}, exploreCausalConsumerFrontierCap)
			for _, id := range ids {
				local := make(map[string]struct{}, exploreCausalConsumerNodeFanoutCap+1)
				for _, edge := range incoming[id] {
					if edge == nil || edge.Kind != graph.EdgeCalls || edge.From == "" {
						continue
					}
					local[edge.From] = struct{}{}
				}
				if len(local) > exploreCausalConsumerNodeFanoutCap {
					continue
				}
				for callerID := range local {
					callerSet[callerID] = struct{}{}
				}
			}
			callerIDs := make([]string, 0, len(callerSet))
			for id := range callerSet {
				callerIDs = append(callerIDs, id)
			}
			sort.Strings(callerIDs)
			if len(callerIDs) > exploreCausalConsumerFrontierCap {
				callerIDs = callerIDs[:exploreCausalConsumerFrontierCap]
			}
			nodes := store.GetNodesByIDs(callerIDs)
			next := make([]*graph.Node, 0, len(callerIDs))
			for _, callerID := range callerIDs {
				if _, exists := expanded[callerID]; exists {
					continue
				}
				node := nodes[callerID]
				if node == nil || !exploreNodesShareExactScope(seed.node, node) ||
					(node.Kind != graph.KindFunction && node.Kind != graph.KindMethod) ||
					exploreDraftIsTestNode(node) {
					continue
				}
				expanded[callerID] = struct{}{}
				nodePath := exploreNormalizedPath(nodeDisplayPath(node))
				if nodePath == seedPath {
					next = append(next, node)
				}
				if nodePath == "" || nodePath == seedPath {
					continue
				}
				if _, alreadyVisible := targetIDs[node.ID]; alreadyVisible {
					continue
				}
				if _, represented := targetDirectories[exploreCausalChangeDirectory(node)]; !represented {
					continue
				}
				overlap, longest := exploreDraftTermOverlap(terms, node)
				if overlap < 2 && (overlap != 1 || longest < 5) {
					continue
				}
				candidate := exploreCausalChangeCandidate{
					node: node, parentID: seed.node.ID, parentRank: parentRank,
					direction: "caller", hop: depth, consumer: true,
					crossFile: true, overlap: overlap, longest: longest,
				}
				if current, exists := byID[node.ID]; !exists || exploreCausalChangeLess(candidate, current) {
					byID[node.ID] = candidate
				}
			}
			frontier = next
		}
	}
	if len(byID) == 0 {
		return exploreCausalChangeCandidate{}, false
	}
	candidates := make([]exploreCausalChangeCandidate, 0, len(byID))
	for _, candidate := range byID {
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return exploreCausalChangeLess(candidates[i], candidates[j])
	})
	best := candidates[0]
	bestPath := exploreNormalizedPath(nodeDisplayPath(best.node))
	for _, candidate := range candidates[1:] {
		if candidate.overlap != best.overlap || candidate.longest != best.longest || candidate.hop != best.hop {
			break
		}
		if exploreNormalizedPath(nodeDisplayPath(candidate.node)) != bestPath {
			return exploreCausalChangeCandidate{}, false
		}
	}
	return best, true
}

func exploreCausalChangeDirectory(node *graph.Node) string {
	path := exploreNormalizedPath(nodeDisplayPath(node))
	if index := strings.LastIndex(path, "/"); index > 0 {
		return path[:index]
	}
	return ""
}

func selectExploreCausalChangeTarget(task string, targets []exploreTarget) (exploreCausalChangeCandidate, bool) {
	query := shapeExploreQuery(task)
	if exploreQueryIsConceptTask(query) {
		query = stripLeadingExploreDirective(query)
	}
	terms := exploreTerminalTerms(query)
	byID := make(map[string]exploreCausalChangeCandidate)
	for parentRank, target := range targets {
		if !exploreHydratedProductionCallable(target) || !exploreStrongCausalSeed(query, target) {
			continue
		}
		continuationHint := func(bridge *graph.Node) *graph.Node {
			var hinted *graph.Node
			for _, neighbor := range target.causalCallees {
				if neighbor.hop != 2 || neighbor.node == nil ||
					!exploreNodesShareExactScope(bridge, neighbor.node) ||
					!exploreDelegatedImplementationCallee(bridge, neighbor.node) {
					continue
				}
				if hinted != nil && hinted.ID != neighbor.node.ID {
					return nil
				}
				hinted = neighbor.node
			}
			return hinted
		}
		consider := func(node *graph.Node, continuation *graph.Node, direction string, hop int) {
			if node == nil || node.ID == "" || node.ID == target.node.ID ||
				(node.Kind != graph.KindFunction && node.Kind != graph.KindMethod) ||
				exploreDraftIsTestNode(node) || !exploreNodesShareExactScope(target.node, node) {
				return
			}
			delegated := continuation != nil || (direction == "callee" && hop == 1 &&
				exploreDelegatedImplementationCallee(target.node, node))
			crossFile := exploreNormalizedPath(nodeDisplayPath(target.node)) !=
				exploreNormalizedPath(nodeDisplayPath(node))
			overlap, longest := exploreDraftTermOverlap(terms, node)
			if !delegated && (!crossFile || overlap == 0) {
				return
			}
			candidate := exploreCausalChangeCandidate{
				node: node, continuation: continuation,
				parentID: target.node.ID, parentRank: parentRank,
				direction: direction, hop: hop, delegated: delegated,
				crossFile: crossFile, overlap: overlap, longest: longest,
			}
			if current, exists := byID[node.ID]; !exists || exploreCausalChangeLess(candidate, current) {
				byID[node.ID] = candidate
			}
		}
		for _, node := range target.callers {
			consider(node, nil, "caller", 1)
		}
		for _, node := range target.callees {
			consider(node, continuationHint(node), "callee", 1)
		}
		for _, neighbor := range target.causalCallees {
			consider(neighbor.node, nil, "callee", neighbor.hop)
		}
	}
	if len(byID) == 0 {
		return exploreCausalChangeCandidate{}, false
	}
	candidates := make([]exploreCausalChangeCandidate, 0, len(byID))
	for _, candidate := range byID {
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return exploreCausalChangeLess(candidates[i], candidates[j])
	})
	return candidates[0], true
}

func exploreCausalChangeLess(left, right exploreCausalChangeCandidate) bool {
	if left.delegated != right.delegated {
		return left.delegated
	}
	if left.consumer != right.consumer {
		return left.consumer
	}
	if left.overlap != right.overlap {
		return left.overlap > right.overlap
	}
	if (left.direction == "caller") != (right.direction == "caller") {
		return left.direction == "caller"
	}
	if left.crossFile != right.crossFile {
		return left.crossFile
	}
	if left.longest != right.longest {
		return left.longest > right.longest
	}
	if left.hop != right.hop {
		return left.hop > right.hop
	}
	if left.parentRank != right.parentRank {
		return left.parentRank < right.parentRank
	}
	return exploreDraftNodeKey(left.node) < exploreDraftNodeKey(right.node)
}

func exploreCausalChangeOwner(task string, leaf *graph.Node, store graph.Store) *graph.Node {
	if leaf == nil || leaf.FilePath == "" || store == nil {
		return nil
	}
	fileNodes := store.GetFileNodesByPaths([]string{leaf.FilePath})[leaf.FilePath]
	if len(fileNodes) == 0 || len(fileNodes) > exploreCausalChangeFileNodeCap {
		return nil
	}
	types := make([]*graph.Node, 0, 8)
	for _, node := range fileNodes {
		if node == nil || node.Kind != graph.KindType || exploreDraftIsTestNode(node) ||
			!exploreNodesShareExactScope(leaf, node) {
			continue
		}
		types = append(types, node)
	}
	if len(types) == 0 {
		return nil
	}
	sort.SliceStable(types, func(i, j int) bool {
		return exploreDraftNodeKey(types[i]) < exploreDraftNodeKey(types[j])
	})

	returnText := exploreCallableReturnText(leaf.RetrievalMetadata().Signature)
	returned := make([]*graph.Node, 0, 2)
	for _, node := range types {
		if exploreContainsIdentifier(returnText, node.Name) {
			returned = append(returned, node)
		}
	}
	terms := exploreTerminalTerms(shapeExploreQuery(task))
	if len(returned) == 1 {
		if overlap, _ := exploreDraftTermOverlap(terms, returned[0]); overlap > 0 {
			return returned[0]
		}
	}
	if len(returned) > 1 {
		var aligned *graph.Node
		bestOverlap := 0
		tied := false
		for _, node := range returned {
			overlap, _ := exploreDraftTermOverlap(terms, node)
			switch {
			case overlap > bestOverlap:
				aligned, bestOverlap, tied = node, overlap, false
			case overlap == bestOverlap && overlap > 0:
				tied = true
			}
		}
		if aligned != nil && !tied {
			return aligned
		}
	}

	ownerID, _ := graph.EnclosingFromID(leaf.ID, leaf.Kind)
	for _, node := range types {
		if node.ID == ownerID {
			return node
		}
	}
	return nil
}

func exploreCallableReturnText(signature string) string {
	signature = strings.TrimSpace(signature)
	open := strings.IndexByte(signature, '(')
	if open < 0 {
		return ""
	}
	close, ok := exploreMatchingParen(signature, open)
	if !ok || close+1 >= len(signature) {
		return ""
	}
	return strings.TrimSpace(signature[close+1:])
}

func exploreCausalChangeAdmissionProtected(task string, targets []exploreTarget, index int, parentID string) bool {
	if index < 0 || index >= len(targets) {
		return true
	}
	target := targets[index]
	if target.node == nil || index < exploreDraftPrimaryLimit || target.node.ID == parentID {
		return true
	}
	return target.causalChangeBridge || target.causalChangeLeaf || target.causalChangeOwner ||
		target.divergentDefaultOwner || target.divergentDefaultType ||
		target.conceptImplementation || target.conceptComplement ||
		target.exactContent || target.exactContentAmbiguous ||
		target.sourceLiteral || target.typedAnchorProjection ||
		exploreLocalizationExplicitAnchor(task, target.node)
}
