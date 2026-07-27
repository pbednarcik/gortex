package mcp

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Strong completion evidence is deliberately narrower than answer readiness.
// Answer readiness is a ranking decision; these bounded, production-proven
// shapes justify a stronger advisory confidence signal after response packing.
// They never create a tool-permission boundary for an ordinary coding session.
const (
	localizationProvenanceSourceLiteralCallee   = "source_literal_callee"
	localizationProvenanceDivergentDefault      = "divergent_default_owner"
	localizationProvenanceDivergentDefaultType  = "divergent_default_type"
	localizationProvenanceImplementationRoute   = "implementation_route"
	localizationProvenanceImplementationTarget  = "implementation_target"
	localizationProvenanceSyntacticAnchor       = "syntactic_anchor"
	localizationProvenanceTypedAnchorProjection = "typed_anchor_projection"
)

type localizationEvidenceProof struct {
	provenance string
	primary    string
	support    []string
}

func localizationStrongSourceLiteralCallee(target exploreTarget) bool {
	return target.sourceLiteral && target.sourceLiteralCallee && target.exactContent &&
		!target.exactContentAmbiguous && exploreHydratedProductionCallable(target)
}

func localizationStrongImplementationRoute(wrapper, implementation exploreTarget) bool {
	if !wrapper.directCalleesComplete ||
		!exploreHydratedProductionCallable(wrapper) ||
		!exploreHydratedProductionCallable(implementation) ||
		!exploreDraftGenericCandidate(wrapper.node, wrapper.source) ||
		exploreDraftGenericCandidate(implementation.node, implementation.source) {
		return false
	}
	matched := false
	for _, callee := range wrapper.callees {
		if callee == nil || callee.ID == "" || callee.ID == wrapper.node.ID ||
			exploreDraftIsTestNode(callee) ||
			(callee.Kind != graph.KindFunction && callee.Kind != graph.KindMethod) {
			continue
		}
		if callee.ID != implementation.node.ID {
			return false
		}
		matched = true
	}
	return matched
}

func localizationStrongEvidenceForCompletion(completion localizationCompletion, targets []exploreTarget) localizationEvidenceProof {
	if completion.State != localizationStateAnswerReady && completion.State != localizationStateNeedsExactRead {
		return localizationEvidenceProof{}
	}

	// A prescribed refinement can retire in the first response because its
	// graph-proven wrapper/implementation pair is already packed. Preserve the
	// same paired proof that an authorized follow-up read would enforce.
	for symbol, route := range completion.refinementRoutes {
		if !route.enforceable {
			continue
		}
		switch {
		case route.proofSymbol != "":
			return localizationEvidenceProof{
				provenance: localizationProvenanceImplementationRoute,
				primary:    route.proofSymbol,
				support:    []string{symbol},
			}
		case route.implementationSymbol != "":
			return localizationEvidenceProof{
				provenance: localizationProvenanceImplementationRoute,
				primary:    symbol,
				support:    []string{route.implementationSymbol},
			}
		}
	}

	ownerID, ownerIndex := "", -1
	typeID := ""
	for index, target := range targets {
		if target.node == nil || target.node.ID == "" {
			continue
		}
		if target.divergentDefaultOwner && exploreHydratedProductionCallable(target) {
			ownerID, ownerIndex = target.node.ID, index
		}
		if target.divergentDefaultType {
			typeID = target.node.ID
		}
	}
	if ownerID != "" && typeID != "" &&
		((completion.ExactSymbol == "" && ownerIndex == 0) || completion.ExactSymbol == ownerID) {
		return localizationEvidenceProof{
			provenance: localizationProvenanceDivergentDefault,
			primary:    ownerID,
			support:    []string{typeID},
		}
	}

	selected := -1
	if completion.ExactSymbol != "" {
		for index, target := range targets {
			if target.node != nil && target.node.ID == completion.ExactSymbol {
				selected = index
				break
			}
		}
	} else if len(targets) > 0 {
		selected = 0
	}
	if selected >= 0 && localizationStrongSourceLiteralCallee(targets[selected]) {
		return localizationEvidenceProof{
			provenance: localizationProvenanceSourceLiteralCallee,
			primary:    targets[selected].node.ID,
		}
	}
	return localizationEvidenceProof{}
}

// localizationRetiredReadByteAllowance caps the extra payload a response may
// spend on the one body that removes a round trip. A complete second model turn
// replays the system prompt, tool schemas, and ranked page before it can return
// the same source. Paying up to one response budget for that source in the first
// page is therefore the smaller bounded cost.
const localizationRetiredReadByteAllowance = 8192

// localizationRetiredReadAllowance bounds the inlined source against what the
// caller already budgeted for the ranked response. It never grows a page by
// more than one additional response budget or the hard byte cap.
func localizationRetiredReadAllowance(maxBytes int) int {
	if maxBytes < exploreMinBudgetTokens*localizationEnvelopeBytesPerToken {
		return 0
	}
	if maxBytes < localizationRetiredReadByteAllowance {
		return maxBytes
	}
	return localizationRetiredReadByteAllowance
}

// localizationEvidenceCarriesPackedBody reports whether the envelope already
// ships the named symbol's source — the only precondition for dropping the
// instruction to go and read it.
func localizationEvidenceCarriesPackedBody(envelope localizationExploreEnvelope, symbol string) bool {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return false
	}
	for _, evidence := range envelope.Evidence {
		if evidence.ID == symbol && strings.TrimSpace(evidence.Source) != "" {
			return true
		}
	}
	return false
}

// localizationPrescriptionHasNothingLeftToChoose reports whether the page asks
// for one specific body and nothing else. A refinement authorizing several
// candidates is asking the caller to pick between them, and shipping one of
// those bodies does not answer that question.
func localizationPrescriptionHasNothingLeftToChoose(completion localizationCompletion) bool {
	switch completion.State {
	case localizationStateNeedsExactRead:
		return true
	case localizationStateNeedsRefinement:
		return len(completion.AllowedSymbols) <= 1
	default:
		return false
	}
}

// localizationCompletionReleasingPrescribedRead drops a prescription the page
// has already answered, without claiming more than the evidence supports. It
// deliberately does not reuse the single-result completion, which asserts there
// is exactly one supported candidate and no competitor — a ranking claim this
// path has not earned.
func localizationCompletionReleasingPrescribedRead(completion localizationCompletion) localizationCompletion {
	return localizationCompletion{
		State:            localizationStateLocalized,
		Scope:            "localization",
		RequiredAction:   "continue_task",
		Instruction:      localizationReleasedReadInstruction,
		AllowedToolCalls: 0,
		ContractVersion:  localizationTerminalContractV2,
		taskLead:         completion.taskLead,
		digest:           completion.digest,
	}
}

const localizationReleasedReadInstruction = "The source this response prescribed is included above, so the read it asked for would return bytes you already hold. Answer or continue from this evidence. This is not a claim that the evidence is complete — every tool remains available if it does not fit the request."

// localizationEnvelopePackingPrescribedBody returns the envelope with the
// prescribed symbol's source packed, if it fits. Only that one body: a
// refinement page that grew by every candidate's source would spend the whole
// budget restating what its one bounded read was going to fetch anyway.
func localizationEnvelopePackingPrescribedBody(
	envelope localizationExploreEnvelope,
	targets []exploreTarget,
	bodyOrder []int,
	prescribed string,
	maxBytes int,
) localizationExploreEnvelope {
	for _, index := range bodyOrder {
		if index >= len(targets) || index >= len(envelope.Evidence) {
			continue
		}
		node := targets[index].node
		if node == nil || node.ID != prescribed || targets[index].source == "" {
			continue
		}
		candidate := envelope
		candidate.Evidence = append([]localizationEvidence(nil), envelope.Evidence...)
		candidate.Evidence[index].Source = targets[index].source
		if localizationEnvelopeFits(candidate, maxBytes) {
			return candidate
		}
		return envelope
	}
	return envelope
}

// localizationPrescribedSymbol names the one symbol a completion is asking the
// caller to go and read, whichever state prescribes it.
func localizationPrescribedSymbol(completion localizationCompletion) string {
	switch completion.State {
	case localizationStateNeedsExactRead:
		return strings.TrimSpace(completion.ExactSymbol)
	case localizationStateNeedsRefinement:
		return strings.TrimSpace(completion.refinementSymbol)
	default:
		return ""
	}
}

// localizationCompletionRetiringPrescribedRead turns a page whose prescribed
// body is already packed into an answer. Inlining the body alone was measured
// to leave most callers reading it again anyway — the page still told them to.
// So the instruction has to go at the same moment the body arrives.
func localizationCompletionRetiringPrescribedRead(completion localizationCompletion) localizationCompletion {
	completed := newLocalizationCompletion(true, "")
	completed.taskLead = completion.taskLead
	completed.enforceableOnAnswerReady = completion.enforceableOnAnswerReady
	completed.digest = completion.digest
	if len(completion.refinementRoutes) > 0 {
		completed.refinementRoutes = make(map[string]localizationRefinementRoute, len(completion.refinementRoutes))
		for symbol, route := range completion.refinementRoutes {
			completed.refinementRoutes[symbol] = route
		}
	}
	return completed
}

// localizationPrescribedReadSatisfiedByEnvelope reports whether the single read
// the completion prescribes would only return evidence this envelope already
// carries. The prescribed symbol must be packed WITH its source body, and that
// body must clear the same lead-alignment test the reserved read applies when
// it completes — so completing here accepts exactly what the round trip would.
func localizationPrescribedReadSatisfiedByEnvelope(task, symbol string, envelope localizationExploreEnvelope) bool {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return false
	}
	for _, evidence := range envelope.Evidence {
		if evidence.ID != symbol {
			continue
		}
		if strings.TrimSpace(evidence.Source) == "" || strings.TrimSpace(evidence.File) == "" {
			return false
		}
		row := localizationDigestRow{
			Rank:       evidence.Rank,
			ID:         evidence.ID,
			Name:       evidence.Name,
			QualName:   evidence.QualName,
			Kind:       evidence.Kind,
			File:       evidence.File,
			Line:       evidence.Line,
			Signature:  evidence.Signature,
			Callers:    append([]string(nil), evidence.Callers...),
			Callees:    append([]string(nil), evidence.Callees...),
			Provenance: evidence.Provenance,
		}
		return localizationReservedReadEvidenceAlignedWithLead(
			task, envelope.Completion.taskLead, symbol, []localizationDigestRow{row})
	}
	return false
}

func localizationFinalizeCompletionEvidence(
	task string,
	completion localizationCompletion,
	targets []exploreTarget,
	envelope localizationExploreEnvelope,
) localizationCompletion {
	// Never trust an upstream or caller-supplied verdict. The policy is the
	// sole producer of enforceability for initial localization responses.
	completion.Enforceable = false
	completion.enforceableOnAnswerReady = false
	proof := localizationStrongEvidenceForCompletion(completion, targets)
	if !localizationEvidenceProofVisible(proof, envelope) {
		// A ranked head that is answer-ready stays terminal without one of the
		// hard provenance shapes: its evidence and ready-to-emit answer are
		// already packed here, so an extra bounded call only re-reads what the
		// caller can see. The proof keeps gating hard enforcement. Evidence that
		// does not carry the request's lead is the exception — unproven AND
		// unaligned is where ranked confidence has been wrong, so that page
		// keeps its one bounded call.
		if completion.State == localizationStateAnswerReady &&
			(len(envelope.Evidence) > 0 || len(envelope.Symbols) > 0) {
			recovery := newLocalizationRecoveryCompletion()
			recovery.digest = completion.digest
			return recovery
		}
		return completion
	}
	switch completion.State {
	case localizationStateAnswerReady:
		completion.Enforceable = true
	case localizationStateNeedsExactRead:
		completion.enforceableOnAnswerReady = true
	}
	return completion
}

func localizationBoundRouteEvidence(
	routes map[string]localizationRefinementRoute,
	envelope localizationExploreEnvelope,
) map[string]localizationRefinementRoute {
	for symbol, route := range routes {
		if !route.enforceable {
			continue
		}
		proof := localizationEvidenceProof{
			provenance: localizationProvenanceSourceLiteralCallee,
			primary:    symbol,
		}
		switch {
		case route.proofSymbol != "":
			proof.provenance = localizationProvenanceImplementationRoute
			proof.primary = route.proofSymbol
			proof.support = []string{symbol}
		case route.implementationSymbol != "":
			proof.provenance = localizationProvenanceImplementationRoute
			proof.support = []string{route.implementationSymbol}
		}
		if !localizationEvidenceProofVisible(proof, envelope) {
			route.enforceable = false
			routes[symbol] = route
		}
	}
	return routes
}

func localizationEvidenceProofVisible(proof localizationEvidenceProof, envelope localizationExploreEnvelope) bool {
	if proof.provenance == "" || proof.primary == "" {
		return false
	}
	visible := make(map[string]string, len(envelope.Evidence))
	for _, evidence := range envelope.Evidence {
		if evidence.ID != "" {
			visible[evidence.ID] = evidence.Provenance
		}
	}
	if visible[proof.primary] != proof.provenance {
		return false
	}
	for _, support := range proof.support {
		expected := ""
		switch proof.provenance {
		case localizationProvenanceDivergentDefault:
			expected = localizationProvenanceDivergentDefaultType
		case localizationProvenanceImplementationRoute:
			expected = localizationProvenanceImplementationTarget
		}
		if support == "" || visible[support] != expected {
			return false
		}
	}
	return true
}

func localizationTargetProvenance(completion localizationCompletion, target exploreTarget) string {
	if target.divergentDefaultOwner {
		return localizationProvenanceDivergentDefault
	}
	if target.divergentDefaultType {
		return localizationProvenanceDivergentDefaultType
	}
	if target.node == nil {
		return ""
	}
	// Refinement routes need both distinct role markers on the packed wire.
	// Give that paired proof priority over any independent literal role the
	// same target may also carry.
	for symbol, route := range completion.refinementRoutes {
		if !route.enforceable {
			continue
		}
		if route.proofSymbol != "" {
			if target.node.ID == route.proofSymbol {
				return localizationProvenanceImplementationRoute
			}
			if target.node.ID == symbol {
				return localizationProvenanceImplementationTarget
			}
		}
		if target.node.ID == symbol && route.implementationSymbol != "" {
			return localizationProvenanceImplementationRoute
		}
		if target.node.ID == route.implementationSymbol {
			return localizationProvenanceImplementationTarget
		}
	}
	if localizationStrongSourceLiteralCallee(target) {
		return localizationProvenanceSourceLiteralCallee
	}
	if target.syntacticAnchor {
		return localizationProvenanceSyntacticAnchor
	}
	if target.typedAnchorProjection {
		return localizationProvenanceTypedAnchorProjection
	}
	return ""
}
