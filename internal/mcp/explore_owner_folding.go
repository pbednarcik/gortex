package mcp

import (
	"context"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
)

// Owner folding.
//
// Concept queries about a capability often rank several members of one type
// above the type itself: each member carries the query vocabulary and its
// own fan-in, while the owner splits the same evidence. The benchmark shape
// is a middleware/persist module whose expected answer is the option/state
// TYPES, yet the emitted symbols were the value-level members. When two or
// more top candidates share one owner type, the owner is promoted directly
// ahead of its first member — members stay as evidence, nothing is evicted.
//
// Implementation-intent queries are exempt: they ask for the concrete
// members, and Q2's expansion exists to surface exactly those.

const (
	exploreOwnerFoldScan = 8
	exploreOwnerFoldMax  = 2
)

// foldMemberOwners promotes an owner type above its members when at least
// two of the scanned top candidates belong to it. The owner node is pulled
// from the candidate list when it is already present (its later occurrence
// is removed), or fetched by one bounded member_of hop otherwise.
func (s *Server) foldMemberOwners(ctx context.Context, targets []exploreTarget) []exploreTarget {
	eng := s.engineFor(ctx)
	if eng == nil || len(targets) < 2 {
		return targets
	}
	ownerOf := func(n *graph.Node) *graph.Node {
		if n == nil || (n.Kind != graph.KindMethod && n.Kind != graph.KindFunction && n.Kind != graph.KindField) {
			return nil
		}
		for _, e := range eng.GetOutEdges(n.ID) {
			if e == nil || e.Kind != graph.EdgeMemberOf {
				continue
			}
			if owner := eng.GetSymbol(e.To); owner != nil &&
				(owner.Kind == graph.KindType || owner.Kind == graph.KindInterface) {
				return owner
			}
		}
		return nil
	}

	type ownerGroup struct {
		owner       *graph.Node
		firstMember int
		members     int
	}
	groups := map[string]*ownerGroup{}
	order := make([]string, 0, exploreOwnerFoldScan)
	rankOf := map[string]int{}
	for index, t := range targets {
		if t.node != nil {
			rankOf[t.node.ID] = index
		}
		if index >= exploreOwnerFoldScan || t.node == nil {
			continue
		}
		owner := ownerOf(t.node)
		if owner == nil {
			continue
		}
		g, ok := groups[owner.ID]
		if !ok {
			g = &ownerGroup{owner: owner, firstMember: index}
			groups[owner.ID] = g
			order = append(order, owner.ID)
		}
		g.members++
	}

	folded := 0
	for _, ownerID := range order {
		if folded >= exploreOwnerFoldMax {
			break
		}
		g := groups[ownerID]
		if g.members < 2 {
			continue
		}
		if existing, present := rankOf[ownerID]; present && existing <= g.firstMember {
			continue // the owner already leads its members
		}
		// Remove a lower-ranked occurrence of the owner, then insert it
		// directly ahead of its first member.
		kept := make([]exploreTarget, 0, len(targets)+1)
		var ownerTarget exploreTarget
		found := false
		for _, t := range targets {
			if t.node != nil && t.node.ID == ownerID {
				ownerTarget = t
				found = true
				continue
			}
			kept = append(kept, t)
		}
		if !found {
			ownerTarget = exploreTarget{node: g.owner, foldedOwner: true}
		}
		insertAt := g.firstMember
		if insertAt > len(kept) {
			insertAt = len(kept)
		}
		if insertAt < len(kept) {
			ownerTarget.score = kept[insertAt].score
		}
		targets = append(kept[:insertAt:insertAt], append([]exploreTarget{ownerTarget}, kept[insertAt:]...)...)
		folded++
		rankOf = map[string]int{}
		for index, t := range targets {
			if t.node != nil {
				rankOf[t.node.ID] = index
			}
		}
	}
	return targets
}

// exploreFoldedDraftSelection returns the current targets selected by the same
// bounded draft planner used for the terminal envelope. A synthetic owner may
// displace direct evidence only when the planner selected that owner and there
// are enough non-mandatory, draft-rejected direct rows to absorb the overflow.
// Raw caller/callee adjacency is deliberately insufficient: noisy graph
// neighbors must not turn an optional summary into mandatory evidence.
func exploreFoldedDraftSelection(
	task string,
	targets []exploreTarget,
	limit int,
	reserved map[string]struct{},
) map[string]struct{} {
	if strings.TrimSpace(task) == "" || limit <= 0 || len(targets) <= limit {
		return nil
	}
	targetIDs := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.node != nil {
			targetIDs[target.node.ID] = struct{}{}
		}
	}
	draft := exploreAnswerDraft(task, targets)
	planned := localizationEvidenceTargetsFromDraft(task, "", targets, draft)
	selected := make(map[string]struct{}, min(limit, len(planned)))
	for index, target := range planned {
		if index >= limit {
			break
		}
		if target.node == nil {
			continue
		}
		if _, direct := targetIDs[target.node.ID]; direct {
			selected[target.node.ID] = struct{}{}
		}
	}

	selectedOwners, unselectedOwners, victims := 0, 0, 0
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		_, chosen := selected[target.node.ID]
		if target.foldedOwner {
			if chosen && exploreFoldedOwnerTaskAligned(task, target.node) {
				selectedOwners++
			} else {
				delete(selected, target.node.ID)
				unselectedOwners++
			}
			continue
		}
		if !chosen && !exploreFoldedTargetMandatory(target, reserved) {
			victims++
		}
	}
	if selectedOwners == 0 {
		return nil
	}
	neededVictims := len(targets) - limit - unselectedOwners
	if neededVictims < 0 {
		neededVictims = 0
	}
	if victims < neededVictims {
		return nil
	}
	return selected
}

func exploreFoldedOwnerTaskAligned(task string, owner *graph.Node) bool {
	if owner == nil {
		return false
	}
	if exploreFoldedOwnerIdentityMentioned(task, owner.Name) ||
		exploreFoldedOwnerIdentityMentioned(task, owner.QualName) ||
		exploreFoldedOwnerIdentityMentioned(task, owner.ID) {
		return true
	}
	queryTerms := exploreTerminalTerms(task)
	ownerTerms := exploreTerminalTerms(owner.Name)
	if len(ownerTerms) == 0 {
		return false
	}
	genericOwnerTerms := map[string]struct{}{
		"class": {}, "config": {}, "configuration": {}, "configur": {},
		"impl": {}, "implement": {}, "implementation": {},
		"interface": {}, "interfac": {}, "manager": {}, "management": {}, "manag": {},
		"option": {}, "registration": {}, "registry": {}, "registrie": {}, "registri": {},
		"schema": {}, "service": {}, "servic": {}, "state": {}, "struct": {}, "type": {},
	}
	distinctive := false
	for term := range ownerTerms {
		if _, matched := queryTerms[term]; !matched {
			return false
		}
		if _, generic := genericOwnerTerms[term]; !generic {
			distinctive = true
		}
	}
	return distinctive
}

func exploreFoldedOwnerIdentityMentioned(task, identity string) bool {
	taskRunes := []rune(strings.ToLower(task))
	identityRunes := []rune(strings.ToLower(strings.TrimSpace(identity)))
	if len(taskRunes) == 0 || len(identityRunes) == 0 || len(identityRunes) > len(taskRunes) {
		return false
	}
	for start := 0; start+len(identityRunes) <= len(taskRunes); start++ {
		if start > 0 && exploreFoldedOwnerIdentityRune(taskRunes[start-1]) {
			continue
		}
		matched := true
		for index := range identityRunes {
			if taskRunes[start+index] != identityRunes[index] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		after := start + len(identityRunes)
		if after == len(taskRunes) || !exploreFoldedOwnerIdentityRune(taskRunes[after]) {
			return true
		}
	}
	return false
}

func exploreFoldedOwnerIdentityRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// exploreFoldedTargetMandatory is the single definition of direct evidence
// that an optional synthetic owner may not displace. Callers add contextual
// reservations (including the retrieval head) through reserved; evidence-role
// flags remain centralized here so selection and eviction cannot drift.
func exploreFoldedTargetMandatory(target exploreTarget, reserved map[string]struct{}) bool {
	if target.node == nil {
		return false
	}
	if _, keep := reserved[target.node.ID]; keep {
		return true
	}
	return target.sourceLiteral || target.sourceLiteralCallee || target.exactContent ||
		target.causalChangeBridge || target.causalChangeLeaf || target.causalChangeOwner ||
		target.conceptImplementation || target.conceptComplement || target.syntacticAnchor || target.typedAnchorProjection ||
		target.divergentDefaultOwner || target.divergentDefaultType
}

// limitExploreFoldedTargets restores the request's symbol cap after owner
// folding inserts a synthetic type. Folding is a post-selection summary:
// synthetic owners are the first overflow victims unless the final draft
// planner selected them and identified weaker direct evidence to displace.
// The ordinary unprotected-tail fallback is retained only as a fail-safe for
// malformed input whose overflow was not produced by folding.
func limitExploreFoldedTargets(task string, targets []exploreTarget, limit int, reserved map[string]struct{}) []exploreTarget {
	if limit <= 0 || len(targets) <= limit {
		return targets
	}
	protected := make(map[string]struct{}, len(reserved)+1)
	for id := range reserved {
		protected[id] = struct{}{}
	}
	if targets[0].node != nil && !targets[0].foldedOwner {
		protected[targets[0].node.ID] = struct{}{}
	}
	draftSelected := exploreFoldedDraftSelection(task, targets, limit, protected)

	bounded := append([]exploreTarget(nil), targets...)
	for len(bounded) > limit {
		remove := -1
		for index := len(bounded) - 1; index >= 0; index-- {
			target := bounded[index]
			if target.node == nil || !target.foldedOwner {
				continue
			}
			if _, chosen := draftSelected[target.node.ID]; chosen {
				continue
			}
			// The tag proves this row was added after the direct window. External
			// reservations cannot preserve it; only the task-aware draft above can.
			remove = index
			break
		}
		if remove >= 0 {
			bounded = append(bounded[:remove], bounded[remove+1:]...)
			continue
		}
		for index := len(bounded) - 1; index >= 0; index-- {
			target := bounded[index]
			if target.node == nil {
				remove = index
				break
			}
			if _, chosen := draftSelected[target.node.ID]; chosen {
				continue
			}
			if !exploreFoldedTargetMandatory(target, protected) {
				remove = index
				break
			}
		}
		if remove < 0 {
			return targets
		}
		bounded = append(bounded[:remove], bounded[remove+1:]...)
	}
	return bounded
}
