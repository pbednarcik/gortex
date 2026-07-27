package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const (
	localizationStateInactive          = ""
	localizationStateNeedsExactRead    = "needs_exact_read"
	localizationStateNeedsRefinement   = "needs_refinement"
	localizationStateNeedsRecovery     = "needs_recovery"
	localizationStateRefineInFlight    = "refinement_in_flight"
	localizationStateExactReadInFlight = "exact_read_in_flight"
	localizationStateRecoveryInFlight  = "recovery_in_flight"
	localizationStateLocalized         = "localized"
	localizationStateAnswerReady       = "answer_ready"
	localizationTerminalContractV2     = 2
)

var localizationRecoveryOperations = []string{"search.text", "search.symbols", "read.source"}

const localizationSingleResultInstruction = "The indexed localization pass is complete for this request. Its bounded evidence contains exactly one supported primary production implementation candidate and no competing direct candidate. Continue the requested coding work using this result. Editing, building, testing, and other task tools remain available; further localization is unnecessary unless they reveal contradictory evidence."

// localizationCompletion is the host-neutral terminality contract returned by
// explore(operation:"localize"). Hosts may stop the turn from this payload;
// the server also enforces it for later Gortex navigation calls in the same
// MCP session. AllowedToolCalls bounds only follow-up localization calls
// prescribed by this completion; it never limits editing, building, testing,
// or other task-execution tools.
type localizationCompletion struct {
	State             string   `json:"state"`
	Scope             string   `json:"scope"`
	RequiredAction    string   `json:"required_action"`
	Instruction       string   `json:"instruction,omitempty"`
	FinalResponse     string   `json:"final_response,omitempty"`
	AllowedToolCalls  int      `json:"allowed_tool_calls"`
	ContractVersion   int      `json:"contract_version"`
	Enforceable       bool     `json:"enforceable"`
	ExactSymbol       string   `json:"exact_symbol,omitempty"`
	AllowedSymbols    []string `json:"allowed_symbols,omitempty"`
	AllowedOperations []string `json:"allowed_operations,omitempty"`

	// Route hops stay session-only, while AllowedSymbols exposes the exact
	// bounded authorization set carried by the wire contract.
	refinementSymbol  string
	refinementSymbols []string
	refinementRoutes  map[string]localizationRefinementRoute
	// correctionSymbol is the one ranked alternate fixed when the contract is
	// armed. It is session-only: after a successful advisory read the wire
	// contract exposes it as ExactSymbol instead of opening another search.
	correctionSymbol string
	correctionRoute  localizationRefinementRoute
	// enforceableOnAnswerReady is session-only provenance. A non-terminal
	// completion may carry a prevalidated future verdict through its one
	// authorized read without claiming that the current response is terminal.
	// It defaults false until the evidence policy explicitly opts in.
	enforceableOnAnswerReady bool
	// taskLead is the bounded, normalized first issue line used only to check
	// whether an advisory read covers the task's primary claim. The full prompt
	// is never retained here.
	taskLead string
	// recoveryOperation and recoveryAnchor are a session-only refinement plan.
	// They intentionally have no JSON fields: the existing completion contract
	// renders the exact required call without expanding the wire schema.
	recoveryOperation string
	recoveryAnchor    string

	// digest is the bounded evidence projection carried session-only through
	// reservation staging (see localization_digest.go). Post-terminal results
	// expose it only through host-only MCP _meta. It rides the
	// completion through reservation staging into commitLocalizationLocked,
	// which covers the direct-arm and facade finishLocalize paths alike.
	digest *localizationEvidenceDigest
}

// localizationRefinementRoute is session-only. A zero implementation symbol
// marks a concrete refinement candidate; a non-empty symbol is the one exact
// concrete hop prevalidated for a generic forwarder.
type localizationRefinementRoute struct {
	implementationSymbol string
	// proofSymbol names the generic wrapper that uniquely and completely
	// resolved to this concrete target. It is empty for ordinary concrete
	// hydration and for the wrapper side of the same route.
	proofSymbol string
	// enforceable is set only by the centralized evidence policy after it has
	// proved the entire route. A successful read alone never upgrades trust.
	enforceable bool
}

// localizationTerminalContract is the single wire shape used in visible MCP
// payloads and authenticated host metadata. The metadata lets a host correlate
// advisory context with the server response; it is never a permission boundary.
// The visible copy remains useful to agents and legacy harnesses.
type localizationTerminalContract struct {
	Completion localizationCompletion `json:"completion"`
	Terminal   bool                   `json:"terminal"`
}

func localizationContractFor(completion localizationCompletion) localizationTerminalContract {
	if completion.ContractVersion == 0 {
		completion.ContractVersion = localizationTerminalContractV2
	}
	if completion.State != localizationStateAnswerReady {
		completion.Enforceable = false
	}
	// Session-only evidence remains on localizationTerminalState and in the
	// authenticated host envelope. The wire completion carries only its bounded
	// final_response, never a live digest pointer.
	completion.digest = nil
	return localizationTerminalContract{
		Completion: completion,
		Terminal:   completion.State == localizationStateAnswerReady,
	}
}

func newLocalizationRecoveryCompletion() localizationCompletion {
	return localizationCompletion{
		State:             localizationStateNeedsRecovery,
		Scope:             "localization",
		RequiredAction:    "recover_once",
		Instruction:       `Make one accepted, bounded Gortex MCP recovery call: search(operation:"text" or "symbols", query:<specific task anchor>) or read(operation:"source", target:{symbol:<exact id>}). If Gortex explicitly rejects an overbroad request and preserves the recovery allowance, correct it using only Gortex MCP search or read; the rejected request does not count as the accepted recovery. Do not call host Read, Grep, Glob, Bash, or any other non-Gortex tool. Then respond from the accepted result and follow its completion.`,
		AllowedToolCalls:  1,
		ContractVersion:   localizationTerminalContractV2,
		AllowedOperations: append([]string(nil), localizationRecoveryOperations...),
	}
}

func newLocalizationPlannedRecoveryCompletion(operation, anchor string) localizationCompletion {
	operation = strings.TrimSpace(operation)
	anchor = strings.TrimSpace(anchor)
	return localizationCompletion{
		State:             localizationStateNeedsRecovery,
		Scope:             "localization",
		RequiredAction:    fmt.Sprintf(`%s(%q)`, operation, anchor),
		Instruction:       fmt.Sprintf(`Call Gortex MCP search(operation:"symbols", query:%q); then respond from the accepted result and follow its completion.`, anchor),
		AllowedToolCalls:  1,
		ContractVersion:   localizationTerminalContractV2,
		AllowedOperations: []string{operation},
		recoveryOperation: operation,
		recoveryAnchor:    anchor,
	}
}

// localizationAnswerReadyInstruction states the emit contract in the field every
// non-terminal completion already uses, so a host that reads only
// completion.instruction carries the same obligation as one that reads the
// rendered page. required_action stays "respond" — the terminal gate matches
// that exact value.
const localizationAdvisorySelfCheck = "For that localization-only request, before another tool call, name a concrete requested file or symbol missing from EVIDENCE. If none is missing, respond now; wanting more context is not a missing artifact."

const localizationAnswerReadyInstruction = "Localization is complete: the bounded search examined the supplied anchors, and completion.final_response is the full retained result. For a localization-only request, answer now from completion.final_response using compact FILES, SYMBOLS, and EVIDENCE sections, preserving each PRIMARY tuple once in EVIDENCE order. Do not call another tool just to gain confidence, repeat, or cross-check this localization: PRIMARY rows are the best-supported answer, and SUPPORTING rows are context, not a signal that the search is unfinished. " + localizationAdvisorySelfCheck + " On a broader coding task, this closes only localization; if the user's request includes diagnosis, implementation, or verification, continue normally, and all tools remain available, including editing, building, testing, and verification."

func newLocalizationCompletion(answerReady bool, exactSymbol string) localizationCompletion {
	if answerReady {
		return localizationCompletion{
			State:            localizationStateAnswerReady,
			Scope:            "localization",
			RequiredAction:   "respond",
			Instruction:      localizationAnswerReadyInstruction,
			AllowedToolCalls: 0,
			ContractVersion:  localizationTerminalContractV2,
		}
	}
	return newLocalizationExactReadCompletion(exactSymbol, false)
}

const localizationBoundedConclusionInstruction = "The bounded localization search is exhausted; completion.final_response contains the full retained task-supported result. For a localization-only request, answer now from completion.final_response using compact FILES, SYMBOLS, and EVIDENCE sections, preserving each PRIMARY tuple once in EVIDENCE order. Do not call another tool just to gain confidence, repeat, or cross-check this localization: PRIMARY rows are the best-supported answer, and SUPPORTING rows are context, not a signal that the search is unfinished. " + localizationAdvisorySelfCheck + " On a broader coding task, this closes only localization; if the user's request includes diagnosis, implementation, or verification, continue normally, and all tools remain available, including editing, building, testing, and verification."

const localizationBoundedConclusionDirective = "The bounded localization search is exhausted; this is the full retained result supported by the indexed task evidence. For a localization-only request, answer now using compact FILES, SYMBOLS, and EVIDENCE sections, preserving each PRIMARY tuple once in EVIDENCE order. Do not call another tool just to gain confidence, repeat, or cross-check this localization: PRIMARY rows are the best-supported answer, and SUPPORTING rows are context, not a signal that the search is unfinished. " + localizationAdvisorySelfCheck + " On a broader coding task, this closes only localization; if the user's request includes diagnosis, implementation, or verification, continue normally, and all tools remain available, including editing, building, testing, and verification."

// newLocalizationBoundedConclusionCompletion terminalizes localization
// navigation without promoting bounded best-effort evidence to a confirmed
// answer. The digest clone preserves canonical row order and leaves the caller
// free to continue diagnosis, implementation, and verification.
func newLocalizationBoundedConclusionCompletion(digest *localizationEvidenceDigest) localizationCompletion {
	boundedDigest := &localizationEvidenceDigest{}
	if digest != nil {
		*boundedDigest = *digest
	}
	response := boundedDigest.finalResponse
	if response == "" {
		response = renderLocalizationFinalResponse(boundedDigest.Evidence)
	}
	response = strings.TrimSpace(response)
	switch {
	case strings.HasPrefix(response, localizationAnswerHeading):
		response = localizationBoundedHeading + strings.TrimPrefix(response, localizationAnswerHeading)
	case strings.HasPrefix(response, localizationProvisionalHeading):
		response = localizationBoundedHeading + strings.TrimPrefix(response, localizationProvisionalHeading)
	case !strings.HasPrefix(response, localizationBoundedHeading):
		response = localizationBoundedHeading + "\n" + response
	}
	for _, directive := range []string{localizationAnswerReadyDirective, localizationProvisionalDirective, localizationBoundedConclusionDirective} {
		if strings.HasSuffix(response, directive) {
			response = strings.TrimSpace(strings.TrimSuffix(response, directive))
		}
	}
	response += "\n\n" + localizationBoundedConclusionDirective
	boundedDigest.finalResponse = response
	boundedDigest.provisionalResponse = response

	completion := newLocalizationCompletion(true, "")
	completion.Instruction = localizationBoundedConclusionInstruction
	completion.FinalResponse = response
	completion.digest = boundedDigest
	return localizationCompletionWithDigest(completion, boundedDigest)
}

// newLocalizationSingleResultCompletion reports that localization has enough
// unique implementation evidence to continue the task without arming terminal
// state. The model gets a strong completeness cue, while every coding and
// navigation tool remains available if later implementation evidence disagrees.
func newLocalizationSingleResultCompletion() localizationCompletion {
	return localizationCompletion{
		State:            localizationStateLocalized,
		Scope:            "localization",
		RequiredAction:   "continue_task",
		Instruction:      localizationSingleResultInstruction,
		AllowedToolCalls: 0,
		ContractVersion:  localizationTerminalContractV2,
	}
}

func newLocalizationExactReadCompletion(exactSymbol string, correction bool) localizationCompletion {
	instruction := fmt.Sprintf(`Call Gortex MCP read(operation:"source", target:{symbol:%q}); then respond.`, exactSymbol)
	if correction {
		instruction = fmt.Sprintf(`Call Gortex MCP read(operation:"source", target:{symbol:%q}); this is the only permitted corrective read; then follow the returned completion.`, exactSymbol)
	}
	return localizationCompletion{
		State:            localizationStateNeedsExactRead,
		Scope:            "localization",
		RequiredAction:   "read_exact",
		Instruction:      instruction,
		AllowedToolCalls: 1,
		ContractVersion:  localizationTerminalContractV2,
		ExactSymbol:      exactSymbol,
	}
}

func newLocalizationOpenCompletion() localizationCompletion {
	return localizationCompletion{
		State:            localizationStateInactive,
		Scope:            "localization",
		RequiredAction:   "continue",
		AllowedToolCalls: 0,
		ContractVersion:  localizationTerminalContractV2,
	}
}

// newLocalizationRefinementCompletion keeps uncertain localization successful
// and bounded. The ranked evidence remains usable, while the server permits
// exactly one source read selected from the explicit wire authorization set.
func newLocalizationRefinementCompletion(preferredSymbol string) localizationCompletion {
	return newLocalizationRefinementCompletionForSymbols(preferredSymbol, []string{preferredSymbol})
}

func newLocalizationRefinementCompletionForSymbols(preferredSymbol string, allowedSymbols []string) localizationCompletion {
	preferredSymbol = strings.TrimSpace(preferredSymbol)
	allowedSymbols = append([]string(nil), allowedSymbols...)
	return localizationCompletion{
		State:             localizationStateNeedsRefinement,
		Scope:             "localization",
		RequiredAction:    fmt.Sprintf(localizationRefinementRequiredActionFormat, preferredSymbol),
		AllowedToolCalls:  1,
		ContractVersion:   localizationTerminalContractV2,
		AllowedSymbols:    allowedSymbols,
		refinementSymbol:  preferredSymbol,
		refinementSymbols: append([]string(nil), allowedSymbols...),
	}
}

func localizationRankedCorrection(
	preferredSymbol string,
	symbols []string,
	routes map[string]localizationRefinementRoute,
) (string, localizationRefinementRoute) {
	for _, symbol := range symbols {
		symbol = strings.TrimSpace(symbol)
		if symbol == "" || symbol == preferredSymbol {
			continue
		}
		route, authorized := routes[symbol]
		if !authorized || (!route.enforceable && route.implementationSymbol == "") {
			continue
		}
		return symbol, route
	}
	return "", localizationRefinementRoute{}
}

// localizationTerminalState is intentionally session-local. It bounds only
// localization navigation; mutation, workspace, session, memory, and
// capability tools remain usable after answer_ready and across later work.
type localizationTerminalState struct {
	mu                sync.Mutex
	state             string
	exactSymbol       string
	refinementSymbol  string
	refinementSymbols []string
	refinementRoutes  map[string]localizationRefinementRoute
	correctionSymbol  string
	correctionRoute   localizationRefinementRoute
	// inFlightImplementationSymbol is selected from refinementRoutes when the
	// actual requested candidate is authorized. It is never inferred from the
	// read result.
	inFlightImplementationSymbol string
	inFlightEnforceable          bool
	inFlightCorrectionSymbol     string
	inFlightRecoveryAnchor       string
	inFlightRecoveryOperation    string
	// recoveryOperation/recoveryAnchor retain an exact, session-only recovery
	// plan while its permitted search is pending or in flight.
	recoveryOperation          string
	recoveryAnchor             string
	exactReadIsCorrection      bool
	exactReadRoute             localizationRefinementRoute
	correctionRetriesRemaining uint8
	refinementRetriesRemaining uint8
	recoveryRetriesRemaining   uint8
	// Read reservations are tokenized independently of localization calls. A
	// reset or newly armed task invalidates an old token, so a late read cannot
	// finish (or decorate itself with) a newer task's contract.
	nextReadReservation  uint64
	readReservationToken uint64
	readReservationGen   uint64
	// enforceableOnAnswerReady persists a proven verdict across an authorized
	// exact/refinement read. Its zero value is deliberately advisory.
	enforceableOnAnswerReady bool
	taskFingerprint          string
	taskLead                 string
	generation               uint64
	nextReservation          uint64
	reservation              *localizationReservation
	// digest is the evidence retained for the live contract; nil when the
	// contract is inactive or predates digest capture. Promotions through
	// finishReservedRead keep it — the evidence was stashed when the
	// contract was armed, before the permitted read ran.
	digest *localizationEvidenceDigest
}

type localizationReservation struct {
	token                  uint64
	generation             uint64
	pendingCompletion      localizationCompletion
	pendingTaskFingerprint string
	staged                 bool
}

func newLocalizationTerminalState() *localizationTerminalState {
	return &localizationTerminalState{}
}

func (s *localizationTerminalState) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.generation++
	s.state = localizationStateInactive
	s.exactSymbol = ""
	s.refinementSymbol = ""
	s.refinementSymbols = nil
	s.refinementRoutes = nil
	s.correctionSymbol = ""
	s.correctionRoute = localizationRefinementRoute{}
	s.inFlightImplementationSymbol = ""
	s.inFlightEnforceable = false
	s.inFlightCorrectionSymbol = ""
	s.inFlightRecoveryAnchor = ""
	s.inFlightRecoveryOperation = ""
	s.recoveryOperation = ""
	s.recoveryAnchor = ""
	s.exactReadIsCorrection = false
	s.exactReadRoute = localizationRefinementRoute{}
	s.correctionRetriesRemaining = 0
	s.refinementRetriesRemaining = 0
	s.recoveryRetriesRemaining = 0
	s.readReservationToken = 0
	s.readReservationGen = 0
	s.enforceableOnAnswerReady = false
	s.taskFingerprint = ""
	s.taskLead = ""
	s.digest = nil
	// Keep an in-flight reservation until its owner finishes. Its captured
	// generation is now stale, so finishLocalize cannot commit it, while a
	// second localization cannot race ahead of the still-running handler.
	s.mu.Unlock()
}

func (s *localizationTerminalState) arm(completion localizationCompletion) {
	s.armForTask(completion, "")
}

func (s *localizationTerminalState) armForTask(completion localizationCompletion, task string) {
	if s == nil {
		return
	}
	// `localized` is a wire-only confidence cue. Normalize it at the state
	// boundary as a second line of defense so no current or future caller can
	// accidentally turn the non-blocking cue into an unknown blocking state.
	if completion.State == localizationStateLocalized {
		completion = newLocalizationOpenCompletion()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fingerprint := localizationTaskFingerprint(task)
	if completion.State != localizationStateInactive {
		completion.taskLead = localizationTaskLead(task)
	}
	if s.reservation != nil {
		s.reservation.pendingCompletion = completion
		s.reservation.pendingTaskFingerprint = fingerprint
		s.reservation.staged = true
		return
	}
	s.commitLocalizationLocked(completion, fingerprint)
}

// keepOpenForTask transactionally replaces any prior terminal contract with
// inactive navigation state. Under facade dispatch the inactive state is
// staged until the localization response succeeds; direct handlers commit it
// immediately.
func (s *localizationTerminalState) keepOpenForTask(task string) {
	s.armForTask(newLocalizationOpenCompletion(), task)
}

func (s *localizationTerminalState) armRefinementForTask(task, preferredSymbol string, symbols []string, digest *localizationEvidenceDigest) {
	routes := make(map[string]localizationRefinementRoute, len(symbols))
	for _, symbol := range symbols {
		symbol = strings.TrimSpace(symbol)
		if symbol != "" {
			routes[symbol] = localizationRefinementRoute{}
		}
	}
	s.armRefinementRoutesForTask(task, preferredSymbol, symbols, routes, digest)
}

func (s *localizationTerminalState) armRefinementRoutesForTask(
	task, preferredSymbol string,
	symbols []string,
	routes map[string]localizationRefinementRoute,
	digest *localizationEvidenceDigest,
) {
	preferredSymbol = strings.TrimSpace(preferredSymbol)
	seen := make(map[string]struct{}, min(len(symbols), localizationRefinementAllowedSymbolCap))
	refinementSymbols := make([]string, 0, min(len(symbols), localizationRefinementAllowedSymbolCap))
	refinementRoutes := make(map[string]localizationRefinementRoute, min(len(symbols), localizationRefinementAllowedSymbolCap))
	for _, symbol := range symbols {
		symbol = strings.TrimSpace(symbol)
		if symbol == "" {
			continue
		}
		route, authorized := routes[symbol]
		if !authorized {
			continue
		}
		route.implementationSymbol = strings.TrimSpace(route.implementationSymbol)
		if route.implementationSymbol == symbol {
			continue
		}
		if _, duplicate := seen[symbol]; duplicate {
			continue
		}
		seen[symbol] = struct{}{}
		refinementSymbols = append(refinementSymbols, symbol)
		refinementRoutes[symbol] = route
		if len(refinementSymbols) == localizationRefinementAllowedSymbolCap {
			break
		}
	}
	if len(refinementSymbols) == 0 {
		s.keepOpenForTask(task)
		return
	}
	if _, exists := seen[preferredSymbol]; !exists {
		s.keepOpenForTask(task)
		return
	}
	completion := newLocalizationRefinementCompletionForSymbols(preferredSymbol, refinementSymbols)
	completion.refinementRoutes = refinementRoutes
	completion.correctionSymbol, completion.correctionRoute = localizationRankedCorrection(
		preferredSymbol,
		refinementSymbols,
		refinementRoutes,
	)
	// Stashed now, not at promotion: when the permitted read succeeds,
	// finishReservedRead flips this contract to answer_ready and the
	// evidence must already be retained for replay.
	completion.digest = digest
	s.armForTask(completion, task)
}

func (s *localizationTerminalState) commitLocalizationLocked(completion localizationCompletion, fingerprint string) {
	s.generation++
	s.state = completion.State
	s.exactSymbol = completion.ExactSymbol
	s.refinementSymbol = ""
	s.refinementSymbols = nil
	s.refinementRoutes = nil
	s.inFlightImplementationSymbol = ""
	s.inFlightEnforceable = false
	s.inFlightCorrectionSymbol = ""
	s.inFlightRecoveryAnchor = ""
	s.inFlightRecoveryOperation = ""
	s.recoveryOperation = completion.recoveryOperation
	s.recoveryAnchor = completion.recoveryAnchor
	s.exactReadIsCorrection = false
	s.exactReadRoute = localizationRefinementRoute{}
	s.correctionRetriesRemaining = 0
	s.refinementRetriesRemaining = 0
	s.recoveryRetriesRemaining = 0
	s.readReservationToken = 0
	s.readReservationGen = 0
	s.enforceableOnAnswerReady = completion.enforceableOnAnswerReady
	if completion.State == localizationStateAnswerReady {
		s.enforceableOnAnswerReady = completion.Enforceable
	}
	if completion.State == localizationStateNeedsRefinement {
		s.refinementSymbol = completion.refinementSymbol
		s.refinementSymbols = append([]string(nil), completion.refinementSymbols...)
		s.refinementRoutes = cloneLocalizationRefinementRoutes(completion.refinementRoutes)
		s.correctionSymbol = completion.correctionSymbol
		s.correctionRoute = completion.correctionRoute
		s.refinementRetriesRemaining = 1
	} else {
		s.correctionSymbol = ""
		s.correctionRoute = localizationRefinementRoute{}
	}
	if completion.State == localizationStateNeedsRecovery {
		s.recoveryRetriesRemaining = 1
	}
	s.taskFingerprint = fingerprint
	s.taskLead = completion.taskLead
	// The digest follows the contract: an inactive commit (keepOpenForTask)
	// carries nil and clears it; every localize commit replaces it.
	s.digest = completion.digest
}

func (s *localizationTerminalState) completionLocked() localizationCompletion {
	var completion localizationCompletion
	switch s.state {
	case localizationStateNeedsRefinement, localizationStateRefineInFlight:
		completion = newLocalizationRefinementCompletionForSymbols(s.refinementSymbol, s.refinementSymbols)
		completion.refinementRoutes = cloneLocalizationRefinementRoutes(s.refinementRoutes)
		if s.state == localizationStateRefineInFlight {
			completion.State = localizationStateRefineInFlight
			completion.AllowedToolCalls = 0
		}
	case localizationStateNeedsExactRead, localizationStateExactReadInFlight:
		completion = newLocalizationExactReadCompletion(s.exactSymbol, s.exactReadIsCorrection)
	case localizationStateNeedsRecovery, localizationStateRecoveryInFlight:
		completion = newLocalizationRecoveryCompletion()
		if s.recoveryOperation != "" && s.recoveryAnchor != "" {
			completion = newLocalizationPlannedRecoveryCompletion(s.recoveryOperation, s.recoveryAnchor)
		}
		if s.state == localizationStateRecoveryInFlight {
			completion.State = localizationStateRecoveryInFlight
			completion.RequiredAction = "wait"
			completion.Instruction = "The bounded Gortex recovery call is already in progress."
			completion.AllowedToolCalls = 0
		}
	case localizationStateInactive:
		completion = newLocalizationOpenCompletion()
	default:
		completion = newLocalizationCompletion(true, "")
	}
	completion.enforceableOnAnswerReady = s.enforceableOnAnswerReady
	completion.taskLead = s.taskLead
	if completion.State == localizationStateAnswerReady {
		completion.Enforceable = s.enforceableOnAnswerReady
	}
	completion.digest = s.digest
	return localizationCompletionWithDigest(completion, s.digest)
}

// answerReady reports whether the bounded localization transaction has already
// produced its retained result. Facade dispatch uses this to let later coding
// navigation execute, while an identical explore.localize request still replays.
func (s *localizationTerminalState) answerReady() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == localizationStateAnswerReady
}

// interceptAnswerReady is the cheap pre-validation gate used by facade
// dispatch. It makes localization terminality independent of operation
// validity, and consumes an unsupported advisory recovery attempt before a
// schema error can create an unbounded retry loop. Non-navigation facades stay
// untouched.
func (s *localizationTerminalState) interceptAnswerReady(facade, operation string, arguments map[string]any) (*mcpgo.CallToolResult, uint64) {
	if s == nil || !localizationNavigationFacade(facade) {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case localizationStateAnswerReady:
		return localizationTerminalResult(s.completionLocked(), facade, operation), 0
	case localizationStateNeedsRecovery:
		if s.localizationRecoveryPlannedLocked() && !s.localizationRecoveryAllowsLocked(facade, operation, arguments) {
			return localizationPlannedRecoveryMismatchResult(s.completionLocked(), facade, operation), 0
		}
		if s.localizationRecoveryAllowsLocked(facade, operation, arguments) {
			// Carry the task generation through later schema validation. A stale
			// invalid request must never consume a newly committed task's recovery.
			return nil, s.generation
		}
		if localizationRecoveryAllows(facade, operation, arguments) {
			// A concrete search with a weak task correlation has not spent the
			// bounded recovery call. Let the caller correct the anchor once it has
			// better evidence instead of terminalizing the localization session.
			return localizationRecoveryMisalignedResult(s.completionLocked(), facade, operation), 0
		}
		s.state = localizationStateAnswerReady
		s.recoveryRetriesRemaining = 0
		return localizationRecoveryRejectedResult(s.completionLocked(), facade, operation), 0
	default:
		return nil, 0
	}
}

func (s *localizationTerminalState) refinementAllowsLocked(symbol string) bool {
	if symbol == "" {
		return false
	}
	_, authorized := s.refinementRoutes[symbol]
	return authorized
}

func (s *localizationTerminalState) localizationRecoveryPlannedLocked() bool {
	return s.recoveryOperation != "" && s.recoveryAnchor != ""
}

func (s *localizationTerminalState) localizationRecoveryPlanAllowsLocked(facade, operation string, arguments map[string]any) bool {
	return s.localizationRecoveryPlannedLocked() &&
		s.recoveryOperation == facade+"."+operation &&
		s.recoveryAnchor == localizationRecoveryAnchor(facade, operation, arguments)
}

func localizationRecoveryAllows(facade, operation string, arguments map[string]any) bool {
	return localizationRecoveryAnchor(facade, operation, arguments) != ""
}

func localizationRecoveryAnchor(facade, operation string, arguments map[string]any) string {
	switch facade + "." + operation {
	case "search.text", "search.symbols":
		query, _ := arguments["query"].(string)
		return strings.TrimSpace(query)
	case "read.source":
		return exactLocalizationSymbol(arguments)
	default:
		return ""
	}
}

// localizationRecoveryAllowsLocked keeps the one-shot correction anchored to
// the user request. Without this check an agent can invent a nearby generic
// declaration name, receive valid-but-unrelated text hits, and mistake their
// existence for causal evidence. Exact source reads remain available because
// their symbol identity is itself a bounded declaration target.
//
// s.mu must be held by the caller.
func (s *localizationTerminalState) localizationRecoveryAllowsLocked(facade, operation string, arguments map[string]any) bool {
	if s.localizationRecoveryPlannedLocked() {
		return s.localizationRecoveryPlanAllowsLocked(facade, operation, arguments)
	}
	if !localizationRecoveryAllows(facade, operation, arguments) {
		return false
	}
	if facade != "search" {
		return true
	}
	query, _ := arguments["query"].(string)
	if operation == "symbols" && localizationRecoveryConcreteIdentifier(query) {
		// A symbol-shaped recovery query is validated by the scoped graph search
		// result captured for this reservation. An empty typed page restores the
		// allowance; search.text remains anchored to the original task.
		return true
	}
	return localizationRecoveryQueryAligned(s.taskFingerprint, query)
}

func localizationRecoveryQueryAligned(task, query string) bool {
	task = strings.TrimSpace(task)
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	if task == "" {
		// Empty task fingerprints occur only in direct state tests and older
		// sessions. Production recovery is always armed with a localize task.
		return true
	}
	lowerTask := strings.ToLower(task)
	lowerQuery := strings.ToLower(query)
	// Preserve compact quoted literals and command-line flags that are too
	// short for identifier tokenization but occur verbatim in the request.
	trimmedAnchor := strings.Trim(lowerQuery, " \t\r\n`'\"")
	if len(trimmedAnchor) >= 2 && strings.Contains(lowerTask, trimmedAnchor) {
		return true
	}
	taskTerms := exploreTerminalTerms(task)
	for term := range exploreTerminalTerms(query) {
		if _, aligned := taskTerms[term]; aligned {
			return true
		}
	}
	return localizationRecoverySpecificAnchor(query)
}

// localizationRecoveryConcreteIdentifier admits one exact-looking symbol query
// even when the identifier was inferred from preceding graph evidence rather
// than copied from the task. Authorization alone is not success: the scoped,
// typed search page must contain a graph node before the allowance is consumed.
func localizationRecoveryConcreteIdentifier(query string) bool {
	identifier := strings.Trim(strings.TrimSpace(query), "`'\"")
	runes := []rune(identifier)
	if len(runes) < 3 || !unicode.IsLetter(runes[0]) ||
		(!unicode.IsLetter(runes[len(runes)-1]) && !unicode.IsDigit(runes[len(runes)-1])) {
		return false
	}
	hasLower := false
	hasUpper := false
	hasQualifier := false
	hasCaseBoundary := false
	previousLowerOrDigit := false
	for _, r := range runes {
		switch {
		case unicode.IsLetter(r):
			if unicode.IsLower(r) {
				hasLower = true
			} else if unicode.IsUpper(r) {
				hasUpper = true
				if previousLowerOrDigit {
					hasCaseBoundary = true
				}
			}
			previousLowerOrDigit = unicode.IsLower(r)
		case unicode.IsDigit(r):
			previousLowerOrDigit = true
		case r == '_' || r == '$' || r == '.' || r == ':':
			hasQualifier = true
			previousLowerOrDigit = false
		default:
			return false
		}
	}
	return hasQualifier || hasCaseBoundary || (unicode.IsUpper(runes[0]) && hasUpper && hasLower)
}

// localizationTermMatchesAcrossJoin reports whether a candidate term matches a
// requested term set across the one systematic difference between prose and
// code: prose separates what an identifier joins. "DevTools" in a sentence
// tokenizes to [dev tool] while devtoolsImpl tokenizes to [devtool impl], so a
// direct set intersection is empty for the very symbol the request is about.
// Matching a candidate against a term plus its remainder closes that gap
// without loosening what the terms themselves have to be.
func localizationTermMatchesAcrossJoin(candidate string, terms map[string]struct{}) bool {
	if candidate == "" {
		return false
	}
	if _, exact := terms[candidate]; exact {
		return true
	}
	const minJoinPart = 3
	for term := range terms {
		if len(term) < minJoinPart || len(candidate) <= len(term) || !strings.HasPrefix(candidate, term) {
			continue
		}
		remainder := candidate[len(term):]
		if len(remainder) < minJoinPart {
			continue
		}
		if _, joined := terms[remainder]; joined {
			return true
		}
	}
	return false
}

func localizationReservedReadEvidenceAlignedWithLead(task, lead, requested string, rows []localizationDigestRow) bool {
	if strings.TrimSpace(task) == "" || len(rows) == 0 {
		return false
	}
	if localizationTaskCitesConcreteEvidence(task, requested) {
		return true
	}
	if strings.TrimSpace(lead) == "" {
		lead = localizationTaskLead(task)
	}
	taskTerms := exploreTerminalTerms(task)
	leadTerms := exploreTerminalTerms(lead)
	if len(taskTerms) == 0 || len(leadTerms) == 0 {
		return false
	}
	for _, row := range rows {
		values := []string{row.ID, row.Name, row.QualName, row.File, row.Signature}
		for _, value := range values {
			if localizationTaskCitesConcreteEvidence(task, value) {
				return true
			}
		}
		candidateTerms := exploreTerminalTerms(strings.Join(values, " "))
		overallOverlap := 0
		leadOverlap := 0
		for term := range candidateTerms {
			// Both counters cross the same prose/identifier boundary, so both
			// need the join: a request naming its subject in separated words
			// must still match the symbol that spells it as one.
			if localizationTermMatchesAcrossJoin(term, taskTerms) {
				overallOverlap++
			}
			if localizationTermMatchesAcrossJoin(term, leadTerms) {
				leadOverlap++
			}
		}
		if overallOverlap >= 2 && leadOverlap > 0 {
			return true
		}
		if localizationDigestRowHasStrongCompoundLeadMatch(row, taskTerms, leadTerms) {
			return true
		}
	}
	return false
}

// localizationRecoveryEvidenceAlignedWithLead applies the reserved-read
// confidence test to recovery pages while refusing non-explicit long callable
// names that borrow their second semantic hit from source or signature text.
func localizationRecoveryEvidenceAlignedWithLead(task, lead, requested, operation string, rows []localizationDigestRow) bool {
	if strings.TrimSpace(task) == "" || len(rows) == 0 {
		return false
	}
	if localizationTaskCitesConcreteEvidence(task, requested) {
		return true
	}
	if strings.TrimSpace(lead) == "" {
		lead = localizationTaskLead(task)
	}
	taskTerms := exploreTerminalTerms(task)
	leadTerms := exploreTerminalTerms(lead)
	if len(taskTerms) == 0 || len(leadTerms) == 0 {
		return false
	}
	for _, row := range rows {
		values := []string{row.ID, row.Name, row.QualName, row.File, row.Signature}
		for _, value := range values {
			if localizationTaskCitesConcreteEvidence(task, value) {
				return true
			}
		}
		if operation == "search.symbols" && localizationRecoveryAnchorMatchesRow(requested, row) {
			return true
		}
		if localizationDigestRowLacksRecoveryLeadCoverage(row, leadTerms) {
			continue
		}
		if localizationReservedReadEvidenceAlignedWithLead(task, lead, "", []localizationDigestRow{row}) {
			return true
		}
	}
	return false
}

func localizationRecoveryAnchorMatchesRow(requested string, row localizationDigestRow) bool {
	requested = strings.Trim(strings.TrimSpace(requested), "`'\"")
	if requested == "" {
		return false
	}
	for _, value := range []string{row.ID, row.Name, row.QualName} {
		if strings.EqualFold(strings.TrimSpace(value), requested) {
			return true
		}
	}
	if !localizationRecoveryConcreteIdentifier(requested) {
		return false
	}
	lowerRequested := strings.ToLower(requested)
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(row.ID)), "::"+lowerRequested) ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(row.QualName)), "."+lowerRequested)
}

// localizationDigestRowLacksRecoveryLeadCoverage prevents callable source or
// signature text from substituting for a task-aligned identifier. Every
// callable needs one whole filtered issue-lead term in Name; descriptive names
// with more than two identifier segments need two. Citation and exact query
// bypasses are handled before this recovery-only check.
func localizationDigestRowLacksRecoveryLeadCoverage(row localizationDigestRow, leadTerms map[string]struct{}) bool {
	if !localizationDigestRowCallable(row) {
		return false
	}
	required := 1
	if exploreIdentifierSegmentCountBounded(row.Name) > 2 {
		required = 2
	}
	matchedConcepts := 0
	for term := range leadTerms {
		if exploreIdentifierTerminalMatches(row.Name, []string{term}) == 0 &&
			!localizationRowIdentifierJoinsLeadTerm(row.Name, term, leadTerms) {
			continue
		}
		matchedConcepts++
		if matchedConcepts >= required {
			return false
		}
	}
	return true
}

func localizationDigestRowHasStrongCompoundLeadMatch(row localizationDigestRow, taskTerms, leadTerms map[string]struct{}) bool {
	if !localizationDigestRowCallable(row) {
		return false
	}
	segments := 0
	totalLetters := 0
	matchedLetters := 0
	for offset := 0; offset < len(row.Name); {
		start, end, next, ascii := nextExploreASCIIIdentifierToken(row.Name, offset)
		if !ascii {
			return false
		}
		if start < 0 {
			break
		}
		segments++
		segmentLetters := 0
		for index := start; index < end; index++ {
			if exploreASCIILower(row.Name[index]) || exploreASCIIUpper(row.Name[index]) {
				segmentLetters++
			}
		}
		totalLetters += segmentLetters
		for term := range leadTerms {
			if len(term) < 5 {
				continue
			}
			if _, aligned := taskTerms[term]; !aligned {
				continue
			}
			if exploreIdentifierTerminalMatches(row.Name[start:end], []string{term}) != 0 {
				matchedLetters += segmentLetters
				break
			}
		}
		offset = next
	}
	return segments == 2 && totalLetters > 0 && matchedLetters*2 >= totalLetters
}

func localizationDigestRowCallable(row localizationDigestRow) bool {
	switch strings.ToLower(strings.TrimSpace(row.Kind)) {
	case "function", "method":
		return true
	default:
		return false
	}
}

// localizationPlannedRecoveryForWeakRefinement derives one exact symbol-search
// family only when the successful refinement read covers one explicit task
// concept and the retained production evidence contains exactly one family for
// a different, uncovered concept. It never consults source or signature text.
func localizationPlannedRecoveryForWeakRefinement(
	task string,
	current []localizationDigestRow,
	retained *localizationEvidenceDigest,
) (string, string, bool) {
	concepts := localizationExplicitTaskConcepts(task)
	if len(concepts) < 2 || len(current) == 0 || retained == nil || len(retained.Evidence) == 0 {
		return "", "", false
	}
	missing := make(map[string]struct{}, len(concepts))
	covered := false
	for _, concept := range concepts {
		if localizationRowsCoverExplicitConcept(current, concept) {
			covered = true
			continue
		}
		missing[concept] = struct{}{}
	}
	if !covered || len(missing) == 0 {
		return "", "", false
	}

	currentIDs := make(map[string]struct{}, len(current))
	for _, row := range current {
		if id := strings.TrimSpace(row.ID); id != "" {
			currentIDs[id] = struct{}{}
		}
	}
	families := make(map[string]struct{}, 2)
	for _, row := range retained.Evidence {
		if !localizationDigestRowProductionCallable(row) {
			continue
		}
		if _, alreadyRead := currentIDs[strings.TrimSpace(row.ID)]; alreadyRead && strings.TrimSpace(row.ID) != "" {
			continue
		}
		name := localizationDigestCallableName(row)
		for concept := range missing {
			anchor, ok := localizationComplementaryCallableFamily(name, concept)
			if !ok {
				continue
			}
			families[anchor] = struct{}{}
			if len(families) > 1 {
				return "", "", false
			}
		}
	}
	for anchor := range families {
		return "search.symbols", anchor, true
	}
	return "", "", false
}

func localizationExplicitTaskConcepts(task string) []string {
	seen := make(map[string]struct{})
	concepts := make([]string, 0, 4)
	for offset := 0; offset+2 < len(task); {
		relative := strings.Index(task[offset:], "--")
		if relative < 0 {
			break
		}
		start := offset + relative
		if start > 0 && (exploreASCIILower(task[start-1]) || exploreASCIIUpper(task[start-1]) || exploreASCIIDigit(task[start-1]) || task[start-1] == '-') {
			offset = start + 2
			continue
		}
		end := start + 2
		for end < len(task) {
			ch := task[end]
			if !exploreASCIILower(ch) && !exploreASCIIUpper(ch) && !exploreASCIIDigit(ch) && ch != '-' && ch != '_' {
				break
			}
			end++
		}
		concept := localizationNormalizedConcept(task[start+2 : end])
		if len(concept) >= 3 {
			if _, duplicate := seen[concept]; !duplicate {
				seen[concept] = struct{}{}
				concepts = append(concepts, concept)
			}
		}
		if end <= start+2 {
			offset = start + 2
		} else {
			offset = end
		}
	}
	return concepts
}

func localizationNormalizedConcept(value string) string {
	var normalized strings.Builder
	normalized.Grow(len(value))
	for index := 0; index < len(value); index++ {
		ch := value[index]
		switch {
		case exploreASCIIUpper(ch):
			normalized.WriteByte(ch + ('a' - 'A'))
		case exploreASCIILower(ch), exploreASCIIDigit(ch):
			normalized.WriteByte(ch)
		}
	}
	return normalized.String()
}

func localizationRowsCoverExplicitConcept(rows []localizationDigestRow, concept string) bool {
	for _, row := range rows {
		for _, value := range []string{row.Name, localizationDigestCallableName(row)} {
			segments := localizationIdentifierSegments(value)
			if _, _, ok := localizationIdentifierConceptSpan(segments, concept); ok {
				return true
			}
		}
	}
	return false
}

func localizationDigestRowProductionCallable(row localizationDigestRow) bool {
	if !localizationDigestRowCallable(row) {
		return false
	}
	path := "/" + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(row.File), "\\", "/"))
	return !strings.HasSuffix(path, "_test.go") &&
		!strings.Contains(path, "/test/") &&
		!strings.Contains(path, "/tests/") &&
		!strings.Contains(path, ".test.") &&
		!strings.Contains(path, ".spec.")
}

func localizationDigestCallableName(row localizationDigestRow) string {
	if name := strings.TrimSpace(row.Name); name != "" {
		return name
	}
	for _, value := range []string{row.QualName, row.ID} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if index := strings.LastIndex(value, "::"); index >= 0 {
			value = value[index+2:]
		}
		if index := strings.LastIndexByte(value, '.'); index >= 0 {
			value = value[index+1:]
		}
		if name := strings.TrimSpace(value); name != "" {
			return name
		}
	}
	return ""
}

func localizationIdentifierSegments(value string) []string {
	segments := make([]string, 0, 6)
	for offset := 0; offset < len(value); {
		start, end, next, ascii := nextExploreASCIIIdentifierToken(value, offset)
		if !ascii {
			return nil
		}
		if start < 0 {
			break
		}
		segment := localizationNormalizedConcept(value[start:end])
		if segment != "" {
			segments = append(segments, segment)
		}
		if next <= offset {
			break
		}
		offset = next
	}
	return segments
}

func localizationIdentifierConceptSpan(segments []string, concept string) (int, int, bool) {
	concept = localizationNormalizedConcept(concept)
	for start := range segments {
		joined := ""
		for end := start; end < len(segments); end++ {
			joined += segments[end]
			if joined == concept {
				return start, end, true
			}
			if len(joined) >= len(concept) {
				break
			}
		}
	}
	return 0, 0, false
}

func localizationComplementaryCallableFamily(name, concept string) (string, bool) {
	segments := localizationIdentifierSegments(name)
	start, end, ok := localizationIdentifierConceptSpan(segments, concept)
	if !ok || end+2 >= len(segments) || !localizationRecoveryFamilyConnector(segments[end+1]) {
		return "", false
	}
	return strings.Join(segments[start:end+2], "_"), true
}

func localizationRecoveryFamilyConnector(segment string) bool {
	switch segment {
	case "as", "at", "by", "for", "from", "in", "into", "of", "on", "to", "using", "via", "with", "without":
		return true
	default:
		return false
	}
}

func localizationTaskCitesConcreteEvidence(task, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lowerTask := strings.ToLower(task)
	lowerValue := strings.ToLower(value)
	if strings.ContainsAny(value, "/\\.:") && strings.Contains(lowerTask, lowerValue) {
		return true
	}
	if localizationRecoveryConcreteIdentifier(value) && strings.Contains(task, value) {
		return true
	}
	name := value
	if cut := strings.LastIndexAny(name, "/\\.:"); cut >= 0 && cut+1 < len(name) {
		name = name[cut+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if exploreQueryHasCallAnchor(task, name) {
		return true
	}
	lowerName := strings.ToLower(name)
	for _, quote := range []string{"`", "'", "\""} {
		if strings.Contains(lowerTask, quote+lowerName+quote) {
			return true
		}
	}
	return false
}

// localizationRecoverySpecificAnchor admits compact path-like literals that
// are sufficiently concrete to bound a recovery search on their own. This
// covers metadata paths such as `".jj/"` whose semantic class (VCS state) may
// appear in the task without the literal directory name. Generic declaration
// fragments remain subject to task-term alignment.
func localizationRecoverySpecificAnchor(query string) bool {
	anchor := strings.Trim(strings.TrimSpace(query), "`'\"")
	if len(anchor) < 2 || strings.ContainsAny(anchor, " \t\r\n") {
		return false
	}
	return strings.ContainsAny(anchor, "/\\") || strings.HasPrefix(anchor, ".")
}

func localizationRecoveryOperationAllowed(facade, operation string) bool {
	switch facade + "." + operation {
	case "search.text", "search.symbols", "read.source":
		return true
	default:
		return false
	}
}

func (s *localizationTerminalState) consumeInvalidRecovery(facade, operation string, generation uint64) (localizationCompletion, bool) {
	if s == nil || generation == 0 || !localizationRecoveryOperationAllowed(facade, operation) {
		return newLocalizationOpenCompletion(), false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != localizationStateNeedsRecovery || s.generation != generation {
		return newLocalizationOpenCompletion(), false
	}
	s.state = localizationStateAnswerReady
	s.recoveryRetriesRemaining = 0
	return s.completionLocked(), true
}

func localizationPlannedRecoveryMismatchResult(completion localizationCompletion, facade, operation string) *mcpgo.CallToolResult {
	return newStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationComplete,
		Message:   fmt.Sprintf("the planned localization recovery is %s; the recovery allowance is still available", completion.RequiredAction),
		Retriable: true,
		Data: map[string]any{
			"contract":           localizationContractFor(completion),
			"facade":             facade,
			"operation":          operation,
			"allowed_operations": append([]string(nil), completion.AllowedOperations...),
		},
	}, true)
}

func localizationRecoveryMisalignedResult(completion localizationCompletion, facade, operation string) *mcpgo.CallToolResult {
	return newStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationComplete,
		Message:   "the recovery search query is not specific to the localization task; use a task term or a concrete path/literal anchor; the recovery allowance is still available",
		Retriable: true,
		Data: map[string]any{
			"contract":           localizationContractFor(completion),
			"facade":             facade,
			"operation":          operation,
			"allowed_operations": append([]string(nil), localizationRecoveryOperations...),
		},
	}, true)
}

func localizationRecoveryRejectedResult(completion localizationCompletion, facade, operation string) *mcpgo.CallToolResult {
	result := newStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationTerminal,
		Message:   "the one bounded localization recovery call must be search.text or search.symbols with a task-aligned query, or read.source with a concrete symbol; localization is now terminal",
		Retriable: false,
		Data: map[string]any{
			"contract":           localizationContractFor(completion),
			"facade":             facade,
			"operation":          operation,
			"allowed_operations": append([]string(nil), localizationRecoveryOperations...),
		},
	}, true)
	return attachLocalizationHostEnvelope(result, completion, completion.digest)
}

// beginLocalize reserves the only localization handler slot for this session.
// An inactive session admits its first localization without a boundary flag.
// Once a contract exists, only the first explore call for a genuinely new user
// request may cross it, and the caller must say so explicitly. Localize stages
// its returned completion; task stages inactive navigation. The old contract
// remains live until finishLocalize commits the successful replacement.
func (s *localizationTerminalState) beginLocalize(task string, newUserTask bool) (uint64, *mcpgo.CallToolResult) {
	if s == nil {
		return 0, nil
	}
	fingerprint := localizationTaskFingerprint(task)
	if fingerprint == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservation != nil {
		return 0, localizationInProgressResult()
	}
	if s.state != localizationStateInactive && !newUserTask {
		completion := s.completionLocked()
		if s.state == localizationStateNeedsRecovery {
			s.state = localizationStateAnswerReady
			s.recoveryRetriesRemaining = 0
			return 0, localizationRecoveryRejectedResult(s.completionLocked(), "explore", "localize")
		}
		// A repeat localize against a terminal contract gets the same compact,
		// typed non-retriable signal as every other post-terminal navigation
		// call. The original successful result already holds the evidence.
		if s.state == localizationStateAnswerReady {
			return 0, localizationTerminalResult(completion, "explore", "localize")
		}
		return 0, NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeLocalizationComplete,
			Message:   "this user request already has a localization completion contract; follow it instead of starting another localize call",
			Data: map[string]any{
				"completion": completion,
				"facade":     "explore",
				"operation":  "localize",
			},
		})
	}
	s.nextReservation++
	if s.nextReservation == 0 {
		s.nextReservation++
	}
	token := s.nextReservation
	s.reservation = &localizationReservation{token: token, generation: s.generation}
	return token, nil
}

// finishLocalize commits only the completion staged by the matching reservation
// and only if no reset changed its generation. Errors and panics pass success=false
// and leave the prior contract untouched. A stale finisher can never clear or
// overwrite a newer reservation.
func (s *localizationTerminalState) finishLocalize(token uint64, success bool) bool {
	if s == nil || token == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reservation := s.reservation
	if reservation == nil || reservation.token != token {
		return false
	}
	s.reservation = nil
	if !success || !reservation.staged || reservation.generation != s.generation {
		return false
	}
	s.commitLocalizationLocked(reservation.pendingCompletion, reservation.pendingTaskFingerprint)
	return true
}

func localizationInProgressResult() *mcpgo.CallToolResult {
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationComplete,
		Message:   "a localization request is already in progress for this session",
		Data: map[string]any{
			"completion": map[string]any{
				"state": "localization_in_progress", "scope": "localization",
				"required_action": "wait", "allowed_tool_calls": 0,
				"contract_version": localizationTerminalContractV2,
				"enforceable":      false,
			},
			"facade": "explore", "operation": "localize",
		},
	})
}

const localizationTaskLeadMaxRunes = 240

// localizationTaskLead retains only the normalized first non-empty issue line
// (or its leading clause) so terminal confidence can distinguish a report's
// primary claim from supporting body vocabulary.
func localizationTaskLead(task string) string {
	lead := ""
	for _, line := range strings.Split(task, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lead = line
			break
		}
	}
	if lead == "" {
		return ""
	}
	if clause := inlineLeadClause(lead); clause != "" {
		lead = clause
	}
	lead = strings.Join(strings.Fields(lead), " ")
	if runes := []rune(lead); len(runes) > localizationTaskLeadMaxRunes {
		lead = string(runes[:localizationTaskLeadMaxRunes])
	}
	return lead
}

func localizationTaskFingerprint(task string) string {
	return strings.Join(strings.Fields(task), " ")
}

// authorize checks a navigation call and reserves the single permitted
// localization read when applicable. The caller must finish the reservation
// after invocation so the state can restore a prescribed refinement failure or
// terminalize one accepted recovery without silently consuming either event.
func (s *localizationTerminalState) authorize(facade, operation string, arguments map[string]any) (*mcpgo.CallToolResult, bool) {
	blocked, token := s.authorizeWithToken(facade, operation, arguments)
	return blocked, token != 0
}

func (s *localizationTerminalState) authorizeWithToken(facade, operation string, arguments map[string]any) (*mcpgo.CallToolResult, uint64) {
	if s == nil || !localizationNavigationFacade(facade) {
		return nil, 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservation != nil {
		return localizationInProgressResult(), 0
	}
	if s.state == localizationStateInactive {
		return nil, 0
	}
	// The state-level authorizer retains deterministic replay for callers that
	// explicitly choose bounded localization semantics. Facade dispatch bypasses
	// this branch for answer_ready coding continuations.
	if s.state == localizationStateAnswerReady {
		return localizationTerminalResult(s.completionLocked(), facade, operation), 0
	}
	if s.state == localizationStateNeedsRecovery {
		if s.localizationRecoveryPlannedLocked() && !s.localizationRecoveryAllowsLocked(facade, operation, arguments) {
			return localizationPlannedRecoveryMismatchResult(s.completionLocked(), facade, operation), 0
		}
		if s.localizationRecoveryAllowsLocked(facade, operation, arguments) {
			s.inFlightRecoveryAnchor = localizationRecoveryAnchor(facade, operation, arguments)
			s.inFlightRecoveryOperation = facade + "." + operation
			s.state = localizationStateRecoveryInFlight
			return nil, s.beginReadReservationLocked()
		}
		if localizationRecoveryAllows(facade, operation, arguments) {
			return localizationRecoveryMisalignedResult(s.completionLocked(), facade, operation), 0
		}
		s.state = localizationStateAnswerReady
		s.recoveryRetriesRemaining = 0
		return localizationRecoveryRejectedResult(s.completionLocked(), facade, operation), 0
	}
	if s.state == localizationStateNeedsExactRead && facade == "read" && operation == "source" && exactLocalizationSymbol(arguments) == s.exactSymbol {
		s.inFlightImplementationSymbol = s.exactReadRoute.implementationSymbol
		s.inFlightEnforceable = s.exactReadRoute.enforceable
		s.state = localizationStateExactReadInFlight
		return nil, s.beginReadReservationLocked()
	}
	refinementSymbol := exactLocalizationSymbol(arguments)
	if s.state == localizationStateNeedsRefinement && facade == "read" && operation == "source" && s.refinementAllowsLocked(refinementSymbol) {
		route := s.refinementRoutes[refinementSymbol]
		s.inFlightImplementationSymbol = route.implementationSymbol
		s.inFlightEnforceable = route.enforceable
		s.inFlightCorrectionSymbol = ""
		if refinementSymbol == s.refinementSymbol && !route.enforceable && route.implementationSymbol == "" && s.correctionSymbol != "" {
			s.inFlightCorrectionSymbol = s.correctionSymbol
		}
		s.state = localizationStateRefineInFlight
		return nil, s.beginReadReservationLocked()
	}

	completion := s.completionLocked()
	message := "localization is complete; return the existing evidence without another Gortex navigation call"
	switch s.state {
	case localizationStateNeedsExactRead:
		message = fmt.Sprintf("localization needs exactly one read(operation:\"source\") for %q; other navigation calls are blocked", s.exactSymbol)
	case localizationStateExactReadInFlight:
		message = "the permitted exact localization read is already in progress"
	case localizationStateNeedsRefinement:
		message = fmt.Sprintf("localization permits exactly one read(operation:\"source\") for %q; other navigation calls are blocked", s.refinementSymbol)
	case localizationStateRefineInFlight:
		message = "the permitted localization refinement read is already in progress"
	case localizationStateRecoveryInFlight:
		message = "the bounded localization recovery call is already in progress"
	}
	return newStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationComplete,
		Message:   message,
		Retriable: false,
		Data: map[string]any{
			"completion": completion,
			"facade":     facade,
			"operation":  operation,
		},
	}, true), 0
}

func (s *localizationTerminalState) beginReadReservationLocked() uint64 {
	s.nextReadReservation++
	if s.nextReadReservation == 0 {
		s.nextReadReservation++
	}
	s.readReservationToken = s.nextReadReservation
	s.readReservationGen = s.generation
	return s.readReservationToken
}

// finishReservedRead is retained for direct state tests. Production dispatch
// carries the exact token returned by authorizeWithToken so a stale finisher
// can never consume a later task's read.
func (s *localizationTerminalState) finishReservedRead(success bool) localizationCompletion {
	if s == nil {
		return newLocalizationCompletion(true, "")
	}
	s.mu.Lock()
	token := s.readReservationToken
	s.mu.Unlock()
	return s.finishReservedReadToken(token, success)
}

func (s *localizationTerminalState) finishReservedReadToken(token uint64, success bool) localizationCompletion {
	return s.finishReservedReadTokenInternal(token, success, nil, false, false)
}

// finishReservedReadTokenWithDigest is the production finalizer for a reserved
// localization read. Handler-captured evidence is trusted only after the token
// and generation match, and is published only when this transition actually
// reaches answer_ready.
func (s *localizationTerminalState) finishReservedReadTokenWithDigest(
	token uint64,
	success bool,
	currentEvidence []localizationDigestRow,
	evidenceRecorded bool,
) localizationCompletion {
	return s.finishReservedReadTokenInternal(token, success, currentEvidence, evidenceRecorded, true)
}

func (s *localizationTerminalState) finishReservedReadTokenInternal(
	token uint64,
	success bool,
	currentEvidence []localizationDigestRow,
	evidenceRecorded bool,
	captureRequired bool,
) (completion localizationCompletion) {
	if s == nil {
		return newLocalizationOpenCompletion()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == 0 || token != s.readReservationToken || s.readReservationGen != s.generation {
		return newLocalizationOpenCompletion()
	}
	s.readReservationToken = 0
	s.readReservationGen = 0

	wireSuccess := success
	capturedResult := wireSuccess && evidenceRecorded && len(currentEvidence) > 0
	zeroResult := wireSuccess && evidenceRecorded && len(currentEvidence) == 0
	var mergedDigest *localizationEvidenceDigest
	if capturedResult {
		mergedDigest = mergeLocalizationEvidenceDigestForTask(s.taskFingerprint, currentEvidence, s.digest)
	}
	terminalDigest := mergedDigest
	if captureRequired && zeroResult {
		// An explicitly recorded empty typed page is a bounded zero-result, not
		// evidence that can confirm localization. Mark it unsuccessful so an
		// accepted recovery publishes the terminal unconfirmed conclusion. A
		// synthetic handler with no capture record keeps the legacy contract.
		success = false
	}
	defer func() {
		if s.state != localizationStateAnswerReady {
			return
		}
		if terminalDigest != nil {
			s.digest = terminalDigest
		}
		// A recorded empty page or handler failure carries no new evidence, but
		// the retained digest still belongs to this token's generation and task.
		// Stale or cross-task finishers were rejected before reaching this defer.
		completion = s.completionLocked()
	}()

	switch s.state {
	case localizationStateRecoveryInFlight:
		requested := s.inFlightRecoveryAnchor
		operation := s.inFlightRecoveryOperation
		s.inFlightRecoveryAnchor = ""
		s.inFlightRecoveryOperation = ""
		legacyRecovery := !captureRequired || (wireSuccess && !evidenceRecorded) ||
			(operation == "" && strings.TrimSpace(s.taskFingerprint) == "")
		confirmedRecovery := success && (legacyRecovery ||
			(capturedResult && mergedDigest != nil && len(mergedDigest.Evidence) > 0 &&
				localizationRecoveryEvidenceAlignedWithLead(s.taskFingerprint, s.taskLead, requested, operation, currentEvidence)))
		if !confirmedRecovery {
			// Authorization accepted this one bounded recovery. Weak, empty, and
			// failed pages therefore conclude localization from the best retained
			// evidence instead of reproducing the same needs_recovery contract.
			if terminalDigest == nil {
				terminalDigest = s.digest
			}
			terminalDigest = newLocalizationBoundedConclusionCompletion(terminalDigest).digest
		}
		s.recoveryOperation = ""
		s.recoveryAnchor = ""
		s.recoveryRetriesRemaining = 0
		s.state = localizationStateAnswerReady
		return s.completionLocked()
	case localizationStateExactReadInFlight:
		implementationSymbol := s.inFlightImplementationSymbol
		routeEnforceable := s.inFlightEnforceable
		wasCorrection := s.exactReadIsCorrection
		confidentRead := capturedResult && mergedDigest != nil && len(mergedDigest.Evidence) > 0 &&
			localizationReservedReadEvidenceAlignedWithLead(s.taskFingerprint, s.taskLead, s.exactSymbol, currentEvidence)
		s.inFlightImplementationSymbol = ""
		s.inFlightEnforceable = false
		s.inFlightCorrectionSymbol = ""
		if success {
			if routeEnforceable {
				s.enforceableOnAnswerReady = true
			}
			if wasCorrection && implementationSymbol != "" {
				s.state = localizationStateNeedsExactRead
				s.exactSymbol = implementationSymbol
				s.exactReadRoute = localizationRefinementRoute{enforceable: routeEnforceable}
				return s.completionLocked()
			}
			s.exactSymbol = ""
			s.correctionSymbol = ""
			s.correctionRoute = localizationRefinementRoute{}
			s.exactReadIsCorrection = false
			s.exactReadRoute = localizationRefinementRoute{}
			s.correctionRetriesRemaining = 0
			if !wasCorrection && !s.enforceableOnAnswerReady && !confidentRead {
				s.state = localizationStateNeedsRecovery
				s.recoveryRetriesRemaining = 1
				return s.completionLocked()
			}
			s.state = localizationStateAnswerReady
			return s.completionLocked()
		}
		s.enforceableOnAnswerReady = false
		if s.exactReadIsCorrection {
			if s.correctionRetriesRemaining > 0 {
				s.correctionRetriesRemaining--
				s.state = localizationStateNeedsExactRead
				return s.completionLocked()
			}
			s.state = localizationStateAnswerReady
			s.exactSymbol = ""
			s.exactReadIsCorrection = false
			s.exactReadRoute = localizationRefinementRoute{}
			s.correctionRetriesRemaining = 0
			return s.completionLocked()
		}
		s.state = localizationStateNeedsExactRead
	case localizationStateRefineInFlight:
		confidentRead := capturedResult && mergedDigest != nil && len(mergedDigest.Evidence) > 0 &&
			localizationReservedReadEvidenceAlignedWithLead(s.taskFingerprint, s.taskLead, s.refinementSymbol, currentEvidence)
		if success {
			implementationSymbol := s.inFlightImplementationSymbol
			enforceable := s.inFlightEnforceable
			correctionSymbol := s.inFlightCorrectionSymbol
			correctionRoute := s.correctionRoute
			s.inFlightImplementationSymbol = ""
			s.inFlightEnforceable = false
			s.inFlightCorrectionSymbol = ""
			s.enforceableOnAnswerReady = enforceable
			s.refinementSymbol = ""
			s.refinementSymbols = nil
			s.refinementRoutes = nil
			s.correctionSymbol = ""
			s.correctionRoute = localizationRefinementRoute{}
			s.refinementRetriesRemaining = 0
			if implementationSymbol != "" {
				s.state = localizationStateNeedsExactRead
				s.exactSymbol = implementationSymbol
				s.exactReadIsCorrection = true
				s.exactReadRoute = localizationRefinementRoute{enforceable: enforceable}
				s.correctionRetriesRemaining = 1
				return s.completionLocked()
			}
			if correctionSymbol != "" {
				s.state = localizationStateNeedsExactRead
				s.exactSymbol = correctionSymbol
				s.exactReadIsCorrection = true
				s.exactReadRoute = correctionRoute
				s.correctionRetriesRemaining = 1
				return s.completionLocked()
			}
			if !enforceable && !confidentRead {
				s.recoveryOperation = ""
				s.recoveryAnchor = ""
				if operation, anchor, planned := localizationPlannedRecoveryForWeakRefinement(s.taskFingerprint, currentEvidence, s.digest); planned {
					s.recoveryOperation = operation
					s.recoveryAnchor = anchor
				}
				s.state = localizationStateNeedsRecovery
				s.recoveryRetriesRemaining = 1
				return s.completionLocked()
			}
			s.state = localizationStateAnswerReady
			return s.completionLocked()
		}
		s.inFlightImplementationSymbol = ""
		s.inFlightEnforceable = false
		s.inFlightCorrectionSymbol = ""
		s.enforceableOnAnswerReady = false
		if s.refinementRetriesRemaining > 0 {
			s.refinementRetriesRemaining--
			s.state = localizationStateNeedsRefinement
			return s.completionLocked()
		}
		s.state = localizationStateAnswerReady
		s.refinementSymbol = ""
		s.refinementSymbols = nil
		s.refinementRoutes = nil
		s.correctionSymbol = ""
		s.correctionRoute = localizationRefinementRoute{}
	}
	return s.completionLocked()
}

// localizationTerminalResult is the route-neutral successful replay returned
// after a localization response established answer_ready. The facade and
// operation are intentionally ignored so every later navigation call receives
// byte-identical evidence and a ready-to-emit answer.
func localizationTerminalResult(completion localizationCompletion, _, _ string) *mcpgo.CallToolResult {
	return localizationAnswerReadyResult(completion)
}

func cloneLocalizationRefinementRoutes(routes map[string]localizationRefinementRoute) map[string]localizationRefinementRoute {
	if len(routes) == 0 {
		return nil
	}
	cloned := make(map[string]localizationRefinementRoute, len(routes))
	for symbol, route := range routes {
		cloned[symbol] = route
	}
	return cloned
}

// block is retained for direct state checks; production dispatch uses
// authorize so it can finish a reserved exact read after handler completion.
func (s *localizationTerminalState) block(facade, operation string, arguments map[string]any) *mcpgo.CallToolResult {
	blocked, _ := s.authorize(facade, operation, arguments)
	return blocked
}

func localizationNavigationFacade(facade string) bool {
	switch facade {
	case "explore", "search", "read", "relations", "trace", "analyze":
		return true
	default:
		return false
	}
}

func exactLocalizationSymbol(arguments map[string]any) string {
	if target, ok := arguments["target"].(map[string]any); ok {
		return strings.TrimSpace(fmt.Sprint(target["symbol"]))
	}
	return strings.TrimSpace(fmt.Sprint(arguments["symbol"]))
}

func (s *Server) localizationFor(ctx context.Context) *localizationTerminalState {
	id := SessionIDFromContext(ctx)
	if id == "" || s.sessions == nil {
		return s.localization
	}
	return s.sessions.get(id).localization
}

// localizationRowIdentifierJoinsLeadTerm applies the prose/identifier join to a
// row's own name, so a name the request describes in separated words is not
// refused for spelling it as one word.
func localizationRowIdentifierJoinsLeadTerm(name, term string, leadTerms map[string]struct{}) bool {
	if strings.TrimSpace(name) == "" || term == "" {
		return false
	}
	for candidate := range exploreTerminalTerms(name) {
		if !strings.HasPrefix(candidate, term) {
			continue
		}
		if localizationTermMatchesAcrossJoin(candidate, leadTerms) {
			return true
		}
	}
	return false
}
