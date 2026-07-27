package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/search/rerank"
)

// Terminal evidence retention.
//
// The localize handler builds a byte-budgeted evidence envelope once and
// retains a compact projection for host-side fallback and deterministic replay.
// A post-terminal navigation call returns the same successful ready-to-emit
// answer instead of an error that would invite another recovery loop.

const (
	// localizationDigestMaxBytes bounds retained session state independently of
	// the original envelope budget.
	localizationDigestMaxBytes = 4096
	// localizationReplayEvidenceLimit preserves the five strongest direct rows
	// plus at most one graph-validated direct relationship for each. The retained
	// byte cap remains authoritative when long identities cannot all fit.
	localizationReplayEvidenceLimit = 10
	// localizationFinalResponseMaxBytes bounds the ready-to-emit answer that
	// accompanies the retained digest on terminal responses and replays.
	localizationFinalResponseMaxBytes = 4096
	// This canonical envelope is deliberately carried in MCP _meta. Adapting
	// hosts may render its ordered evidence deterministically without exposing
	// retained rows to model-visible text or structuredContent.
	localizationHostMetaKey = "gortex/localization"
)

// localizationEvidenceDigest is the compact, session-retained projection of
// an answer envelope: ranked candidate evidence without source bodies.
type localizationEvidenceDigest struct {
	Files    []string                `json:"files,omitempty"`
	Symbols  []string                `json:"symbols,omitempty"`
	Evidence []localizationDigestRow `json:"evidence,omitempty"`

	// finalResponse is derived from Evidence and excluded from digest JSON so
	// the retained-state byte cap does not count the same identities twice.
	finalResponse string
	// provisionalResponse is the same rows rendered for a state that has not
	// proven its answer yet. A caller can run out of turns at any point, and a
	// session that ends holding nothing is the one outcome with no recovery, so
	// every state carries a page — labelled for what it is.
	provisionalResponse string
}

type localizationDigestRow struct {
	Rank       int      `json:"rank,omitempty"`
	ID         string   `json:"id,omitempty"`
	Name       string   `json:"name,omitempty"`
	QualName   string   `json:"qual_name,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	File       string   `json:"file,omitempty"`
	Line       int      `json:"line,omitempty"`
	Signature  string   `json:"signature,omitempty"`
	Callers    []string `json:"callers,omitempty"`
	Callees    []string `json:"callees,omitempty"`
	Provenance string   `json:"provenance,omitempty"`
}

// newLocalizationEvidenceDigestForTask retains only concrete ranked evidence
// rows. Files and Symbols are rebuilt from those rows, so an item that was shed
// by the replay limit or byte budget cannot survive as an unsupported answer
// candidate. Exact and refinement-authorized rows form a stable protected
// prefix in retained state first. The serialized envelope is then aligned to
// that canonical digest order, so every response mode assigns the same rank to
// the same file/symbol tuple on the first page and every later replay.
// The request is rendered into the ready-to-emit answer, so a page completing
// on its first call presents the same canonical retained rows a later replay would.
func newLocalizationEvidenceDigestForTask(task string, envelope localizationExploreEnvelope) *localizationEvidenceDigest {
	digest := &localizationEvidenceDigest{}
	priorityIDs := localizationDigestPriorityIDs(envelope.Completion, envelope.Evidence)

	seen := make(map[string]struct{}, localizationReplayEvidenceLimit)
	appendRows := func(priority bool) {
		for _, row := range envelope.Evidence {
			if len(digest.Evidence) >= localizationReplayEvidenceLimit {
				return
			}
			if row.ID == "" || row.File == "" {
				continue
			}
			_, prioritized := priorityIDs[row.ID]
			if prioritized != priority {
				continue
			}
			if _, exists := seen[row.ID]; exists {
				continue
			}
			seen[row.ID] = struct{}{}
			digest.Evidence = append(digest.Evidence, localizationDigestRow{
				Rank:       row.Rank,
				ID:         row.ID,
				Name:       row.Name,
				QualName:   row.QualName,
				Kind:       row.Kind,
				File:       row.File,
				Line:       row.Line,
				Signature:  row.Signature,
				Callers:    append([]string(nil), row.Callers...),
				Callees:    append([]string(nil), row.Callees...),
				Provenance: row.Provenance,
			})
		}
	}
	appendRows(true)
	appendRows(false)

	return fitLocalizationEvidenceDigest(digest, task, nil, priorityIDs)
}

// localizationDigestPriorityIDs protects every identity required to justify a
// live authorization, not only the identity the client may read. Dependencies
// still remain subject to the hard byte cap; post-pack reconciliation below
// removes or downgrades any authorization whose complete proof did not fit.
func localizationDigestPriorityIDs(completion localizationCompletion, evidence []localizationEvidence) map[string]struct{} {
	priority := make(map[string]struct{}, 1+len(completion.AllowedSymbols)+len(completion.refinementRoutes)*3)
	add := func(symbol string) {
		if symbol = strings.TrimSpace(symbol); symbol != "" {
			priority[symbol] = struct{}{}
		}
	}
	add(completion.ExactSymbol)
	for _, symbol := range completion.AllowedSymbols {
		add(symbol)
	}
	for symbol, route := range completion.refinementRoutes {
		add(symbol)
		add(route.implementationSymbol)
		add(route.proofSymbol)
	}
	for _, row := range evidence {
		switch row.Provenance {
		case localizationProvenanceSourceLiteralCallee,
			localizationProvenanceDivergentDefault,
			localizationProvenanceDivergentDefaultType,
			localizationProvenanceImplementationRoute,
			localizationProvenanceImplementationTarget:
			add(row.ID)
		}
	}
	return priority
}

func cloneLocalizationDigestRows(rows []localizationDigestRow) []localizationDigestRow {
	if len(rows) == 0 {
		return nil
	}
	cloned := make([]localizationDigestRow, len(rows))
	for index, row := range rows {
		cloned[index] = row
		cloned[index].Callers = append([]string(nil), row.Callers...)
		cloned[index].Callees = append([]string(nil), row.Callees...)
	}
	return cloned
}

// localizationFreshEvidenceReserve bounds how much of the retained digest the
// terminalizing call's own output may claim. A targeted read returns one row and
// deserves to lead; a broad search returns a whole page of its own and would
// otherwise evict every ranked localization row — including a rank-one ground
// truth the caller has already read — leaving an answer built entirely from the
// caller's last query. The localization evidence is what the session exists to
// deliver, so it keeps the majority of the bounded digest.
const localizationFreshEvidenceReserve = 3

// mergeLocalizationEvidenceDigest puts evidence returned by the terminalizing
// permitted call first, then fills the bounded tail from the retained localize
// digest. Files, Symbols, and Evidence are rebuilt from the same rows so their
// ordinal positions can never diverge. Fresh rows lead but cannot crowd the
// retained ranking out of its own answer.
func mergeLocalizationEvidenceDigest(current []localizationDigestRow, retained *localizationEvidenceDigest) *localizationEvidenceDigest {
	digest := &localizationEvidenceDigest{}
	seen := make(map[string]struct{}, localizationReplayEvidenceLimit)
	appendRows := func(rows []localizationDigestRow, limit int) {
		if limit > localizationReplayEvidenceLimit {
			limit = localizationReplayEvidenceLimit
		}
		for _, row := range rows {
			if len(digest.Evidence) >= limit {
				return
			}
			row.ID = strings.TrimSpace(row.ID)
			row.File = strings.TrimSpace(row.File)
			if row.ID == "" || row.File == "" {
				continue
			}
			if _, exists := seen[row.ID]; exists {
				continue
			}
			seen[row.ID] = struct{}{}
			row.Callers = append([]string(nil), row.Callers...)
			row.Callees = append([]string(nil), row.Callees...)
			digest.Evidence = append(digest.Evidence, row)
		}
	}
	freshLead := localizationReplayEvidenceLimit
	if retained != nil && len(retained.Evidence) > 0 {
		freshLead = localizationFreshEvidenceReserve
	}
	appendRows(current, freshLead)
	if retained != nil {
		appendRows(retained.Evidence, localizationReplayEvidenceLimit)
	}
	// Any capacity the retained rows did not need goes back to the fresh page.
	appendRows(current, localizationReplayEvidenceLimit)
	return fitLocalizationEvidenceDigest(digest, "", nil, nil)
}

const localizationSameOwnerEvidenceReserve = 3

// mergeLocalizationEvidenceDigestForTask preserves a small coherent method
// cohort around the terminalizing read before byte shedding considers the
// unrelated tail. It only reorders already-retained rows; cardinality and byte
// limits remain centralized in mergeLocalizationEvidenceDigest.
func mergeLocalizationEvidenceDigestForTask(task string, current []localizationDigestRow, retained *localizationEvidenceDigest) *localizationEvidenceDigest {
	ordered := &localizationEvidenceDigest{Evidence: localizationTaskAwareRetainedRows(task, current, retained)}
	digest := mergeLocalizationEvidenceDigest(current, ordered)
	finalResponse := renderLocalizationFinalResponseForTask(task, current, digest.Evidence)
	if len(finalResponse) <= localizationFinalResponseMaxBytes {
		digest.finalResponse = finalResponse
		digest.provisionalResponse = renderLocalizationProvisionalResponseForTask(task, current, digest.Evidence)
	}
	return digest
}

func localizationTaskAwareRetainedRows(task string, current []localizationDigestRow, retained *localizationEvidenceDigest) []localizationDigestRow {
	if retained == nil || len(retained.Evidence) == 0 {
		return nil
	}
	rows := retained.Evidence
	ordered := make([]localizationDigestRow, 0, len(rows))
	selected := make(map[string]struct{}, len(rows)+len(current))
	ownerKeys := make(map[string]struct{}, len(current))
	seenFiles := make(map[string]struct{}, len(current))
	for _, row := range current {
		if id := strings.TrimSpace(row.ID); id != "" {
			selected[id] = struct{}{}
		}
		if file := strings.TrimSpace(row.File); file != "" {
			seenFiles[file] = struct{}{}
		}
		if key := localizationDigestRowOwnerKey(row); key != "" {
			ownerKeys[key] = struct{}{}
		}
	}
	appendWhere := func(limit int, keep func(localizationDigestRow) bool) {
		added := 0
		for _, row := range rows {
			id := strings.TrimSpace(row.ID)
			if id == "" {
				continue
			}
			if _, exists := selected[id]; exists || !keep(row) {
				continue
			}
			selected[id] = struct{}{}
			ordered = append(ordered, row)
			if file := strings.TrimSpace(row.File); file != "" {
				seenFiles[file] = struct{}{}
			}
			added++
			if limit > 0 && added == limit {
				return
			}
		}
	}
	sameOwner := func(row localizationDigestRow) bool {
		key := localizationDigestRowOwnerKey(row)
		_, exists := ownerKeys[key]
		return key != "" && exists
	}
	appendWhere(0, func(row localizationDigestRow) bool {
		return localizationDigestRowTaskCited(task, row)
	})
	appendWhere(localizationSameOwnerEvidenceReserve, sameOwner)
	taskTerms := exploreTerminalTerms(task)
	appendWhere(0, func(row localizationDigestRow) bool {
		return !sameOwner(row) && localizationDigestRowTaskAligned(task, taskTerms, row)
	})
	appendWhere(0, func(row localizationDigestRow) bool {
		_, seen := seenFiles[strings.TrimSpace(row.File)]
		return !seen
	})
	appendWhere(0, func(localizationDigestRow) bool { return true })
	return ordered
}

func localizationDigestRowTaskCited(task string, row localizationDigestRow) bool {
	for _, value := range []string{row.ID, row.Name, row.QualName, row.File, row.Signature} {
		if localizationTaskCitesConcreteEvidence(task, value) {
			return true
		}
	}
	return false
}

func localizationDigestRowTaskAligned(task string, taskTerms map[string]struct{}, row localizationDigestRow) bool {
	if localizationDigestRowTaskCited(task, row) {
		return true
	}
	values := []string{row.ID, row.Name, row.QualName, row.File, row.Signature}
	for term := range exploreTerminalTerms(strings.Join(values, " ")) {
		if _, aligned := taskTerms[term]; aligned {
			return true
		}
	}
	return false
}

func localizationDigestRowOwnerKey(row localizationDigestRow) string {
	if !strings.EqualFold(strings.TrimSpace(row.Kind), "method") {
		return ""
	}
	file := strings.TrimSpace(row.File)
	owner := strings.TrimSpace(row.QualName)
	if owner == "" {
		owner = strings.TrimSpace(row.ID)
		if prefix := file + "::"; file != "" && strings.HasPrefix(owner, prefix) {
			owner = strings.TrimPrefix(owner, prefix)
		}
	}
	if cut := strings.LastIndex(owner, "."); cut > 0 {
		owner = owner[:cut]
	} else if cut := strings.LastIndex(owner, "::"); cut > 0 {
		owner = owner[:cut]
	} else {
		return ""
	}
	owner = strings.TrimSpace(owner)
	if file == "" || owner == "" {
		return ""
	}
	return file + "\x00" + owner
}

func shedLocalizationEnvelopeRowOptionalFields(row *localizationEvidence) bool {
	if row == nil {
		return false
	}
	if len(row.Callers) > 0 || len(row.Callees) > 0 {
		row.Callers = nil
		row.Callees = nil
		return true
	}
	if row.Signature != "" {
		row.Signature = ""
		return true
	}
	if row.QualName != "" {
		row.QualName = ""
		return true
	}
	if row.Name != "" || row.Kind != "" {
		row.Name = ""
		row.Kind = ""
		return true
	}
	return false
}

func shedLocalizationDigestRowOptionalFields(row *localizationDigestRow) bool {
	if row == nil {
		return false
	}
	if len(row.Callers) > 0 || len(row.Callees) > 0 {
		row.Callers = nil
		row.Callees = nil
		return true
	}
	if row.Signature != "" {
		row.Signature = ""
		return true
	}
	if row.QualName != "" {
		row.QualName = ""
		return true
	}
	if row.Name != "" || row.Kind != "" {
		row.Name = ""
		row.Kind = ""
		return true
	}
	return false
}

const (
	localizationProvenanceTaskComplement         = "task_complement"
	localizationProvenanceTaskRelationComplement = "task_relation_complement"
)

// markLocalizationDigestTaskComplement records the one complementary PRIMARY
// identity before byte packing removes relation lists. A separate marker keeps
// a direct graph complement distinguishable from a weak lexical hint after the
// relation arrays are compacted. Both markers are display-only: neither grants
// navigation authorization or changes evidence order.
func markLocalizationDigestTaskComplement(task string, current []localizationDigestRow, digest *localizationEvidenceDigest) {
	if digest == nil || strings.TrimSpace(task) == "" {
		return
	}
	relationRows := current
	if len(relationRows) == 0 {
		relationRows = digest.Evidence
	}
	relationSeeds := relationRows[:min(localizationFinalResponsePrimaryLimit+2, len(relationRows))]
	for _, item := range localizationFinalResponseRows(task, current, digest.Evidence) {
		if !item.primary || item.row.Rank <= 2 {
			continue
		}
		provenance := localizationProvenanceTaskComplement
		for _, candidate := range relationRows {
			if candidate.ID == item.row.ID && localizationFinalResponseDirectRelation(candidate, relationSeeds) {
				provenance = localizationProvenanceTaskRelationComplement
				break
			}
		}
		for index := range digest.Evidence {
			if digest.Evidence[index].ID != item.row.ID {
				continue
			}
			if strings.TrimSpace(digest.Evidence[index].Provenance) == "" {
				digest.Evidence[index].Provenance = provenance
			}
			return
		}
	}
}

// fitLocalizationEvidenceDigest removes optional detail across the whole page
// before it drops any file/symbol identity. The old tail-only policy could spend
// the entire byte budget on callers and signatures for early rows, then discard
// a late implementation candidate that would fit once those optional fields
// were compacted. Protected authorization identities are dropped only when no
// unprotected identity can satisfy the hard cap.
func fitLocalizationEvidenceDigest(
	digest *localizationEvidenceDigest,
	task string,
	current []localizationDigestRow,
	protectedIDs map[string]struct{},
) *localizationEvidenceDigest {
	markLocalizationDigestTaskComplement(task, current, digest)
	for {
		rebuildLocalizationDigestSkeleton(digest)
		refreshLocalizationDigestResponses(digest, task, current)
		encoded, err := json.Marshal(digest)
		if err == nil && len(encoded) <= localizationDigestMaxBytes && len(digest.finalResponse) <= localizationFinalResponseMaxBytes {
			return digest
		}
		if len(digest.Evidence) == 0 {
			return digest
		}

		shed := false
		for index := len(digest.Evidence) - 1; index >= 0; index-- {
			if shedLocalizationDigestRowOptionalFields(&digest.Evidence[index]) {
				shed = true
			}
		}
		if shed {
			continue
		}

		drop := -1
		for index := len(digest.Evidence) - 1; index >= 0; index-- {
			if _, protected := protectedIDs[digest.Evidence[index].ID]; !protected {
				drop = index
				break
			}
		}
		if drop < 0 {
			drop = len(digest.Evidence) - 1
		}
		if len(digest.Evidence) == 1 {
			// The identity and file are irreducible. A pathological single row
			// cannot escape the byte cap as a truncated, misleading identity.
			digest.Evidence = nil
			continue
		}
		copy(digest.Evidence[drop:], digest.Evidence[drop+1:])
		digest.Evidence = digest.Evidence[:len(digest.Evidence)-1]
	}
}

func rebuildLocalizationDigestSkeleton(digest *localizationEvidenceDigest) {
	digest.Files = digest.Files[:0]
	digest.Symbols = digest.Symbols[:0]
	for index := range digest.Evidence {
		row := &digest.Evidence[index]
		row.Rank = index + 1
		// Keep these arrays positional, including repeated files. A consumer can
		// now pair FILES #N, SYMBOLS #N, and EVIDENCE #N without guessing.
		digest.Files = append(digest.Files, row.File)
		digest.Symbols = append(digest.Symbols, row.ID)
	}
}

// alignLocalizationEnvelopeWithDigest makes the retained digest the one
// canonical positional projection. Source bodies and end lines stay attached
// to their identities, while every visible mode receives the digest's ranks,
// file/symbol order, and provenance labels.
func alignLocalizationEnvelopeWithDigest(envelope *localizationExploreEnvelope, digest *localizationEvidenceDigest) {
	if envelope == nil || digest == nil {
		return
	}

	original := append([]localizationEvidence(nil), envelope.Evidence...)
	rowsByID := make(map[string]localizationEvidence, len(original))
	for _, row := range original {
		if row.ID == "" {
			continue
		}
		if _, exists := rowsByID[row.ID]; !exists {
			rowsByID[row.ID] = row
		}
	}

	files := make([]string, 0, len(original))
	symbols := make([]string, 0, len(original))
	evidence := make([]localizationEvidence, 0, len(original))
	seen := make(map[string]struct{}, len(original))
	appendRow := func(row localizationEvidence) {
		if row.ID == "" {
			return
		}
		if _, exists := seen[row.ID]; exists {
			return
		}
		seen[row.ID] = struct{}{}
		row.Rank = len(evidence) + 1
		files = append(files, row.File)
		symbols = append(symbols, row.ID)
		evidence = append(evidence, row)
	}
	for _, retained := range digest.Evidence {
		row, exists := rowsByID[retained.ID]
		if !exists {
			row = localizationEvidence{
				ID: retained.ID, Name: retained.Name, QualName: retained.QualName,
				Kind: retained.Kind, File: retained.File, Line: retained.Line,
				Signature: retained.Signature,
				Callers:   append([]string(nil), retained.Callers...),
				Callees:   append([]string(nil), retained.Callees...),
			}
		}
		row.ID = retained.ID
		row.File = retained.File
		row.Provenance = retained.Provenance
		appendRow(row)
	}
	// Rows that did not fit the retained replay digest remain useful on the
	// first page. Keep them after the canonical prefix so accuracy does not pay
	// for replay's tighter byte cap and every shared rank still stays aligned.
	for _, row := range original {
		appendRow(row)
	}
	envelope.Files = files
	envelope.Symbols = symbols
	envelope.Evidence = evidence
}

// localizationDigestPackingDropIndex chooses a response-compaction row without
// invalidating the live completion. Authorization dependencies, strong proof
// provenance, and a body that retired its prescribed read are never selected.
func localizationDigestPackingDropIndex(
	digest *localizationEvidenceDigest,
	completion localizationCompletion,
	satisfiedSymbol string,
) int {
	if digest == nil || len(digest.Evidence) == 0 {
		return -1
	}
	evidence := make([]localizationEvidence, 0, len(digest.Evidence))
	for _, row := range digest.Evidence {
		evidence = append(evidence, localizationEvidence{ID: row.ID, Provenance: row.Provenance})
	}
	protected := localizationDigestPriorityIDs(completion, evidence)
	if satisfiedSymbol = strings.TrimSpace(satisfiedSymbol); satisfiedSymbol != "" {
		protected[satisfiedSymbol] = struct{}{}
	}
	for index := len(digest.Evidence) - 1; index >= 0; index-- {
		if _, keep := protected[digest.Evidence[index].ID]; !keep {
			return index
		}
	}
	return -1
}

func localizationFinalResponseField(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

const (
	localizationFinalResponsePrimaryLimit = 3
	// PRIMARY is a role label on the canonical retained order, never a second
	// ranking. Every retained row keeps its EVIDENCE number and later rows remain
	// visible even when one complementary row receives the third PRIMARY slot.
	localizationFinalResponseSupportingLimit = localizationReplayEvidenceLimit - localizationFinalResponsePrimaryLimit
)

type localizationFinalResponseRow struct {
	row     localizationDigestRow
	primary bool
}

type localizationFinalResponseTaskScore struct {
	matched  int
	longest  int
	callable bool
}

func localizationFinalResponsePrimaryProvenance(provenance string) bool {
	switch provenance {
	case localizationProvenanceSourceLiteralCallee,
		localizationProvenanceDivergentDefault,
		localizationProvenanceImplementationTarget,
		localizationProvenanceSyntacticAnchor,
		localizationProvenanceTypedAnchorProjection:
		return true
	default:
		return false
	}
}

func localizationFinalResponseSupportingProvenance(provenance string) bool {
	switch provenance {
	case localizationProvenanceDivergentDefaultType,
		localizationProvenanceImplementationRoute,
		localizationProvenanceTaskComplement,
		localizationProvenanceTaskRelationComplement,
		"direct_caller", "direct_callee":
		return true
	default:
		return false
	}
}

func localizationFinalResponseIdentifierText(row localizationDigestRow) string {
	id := strings.TrimSpace(row.ID)
	if cut := strings.LastIndex(id, "::"); cut >= 0 {
		id = id[cut+2:]
	}
	return strings.ToLower(strings.Join([]string{row.Name, row.QualName, id}, " "))
}

func scoreLocalizationFinalResponseTask(taskTerms map[string]struct{}, row localizationDigestRow) localizationFinalResponseTaskScore {
	score := localizationFinalResponseTaskScore{
		callable: strings.EqualFold(strings.TrimSpace(row.Kind), "function") ||
			strings.EqualFold(strings.TrimSpace(row.Kind), "method"),
	}
	text := localizationFinalResponseIdentifierText(row)
	for term := range taskTerms {
		if !exploreConceptTermPresent(text, term) {
			continue
		}
		score.matched++
		if len(term) > score.longest {
			score.longest = len(term)
		}
	}
	return score
}

func localizationFinalResponseTaskSupportsPrimary(row localizationDigestRow, score localizationFinalResponseTaskScore) bool {
	if score.matched == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(row.Kind)) {
	case "function", "method", "type", "class":
		return true
	default:
		// Constants and enum variants are often incidental flag metadata. Keep
		// them PRIMARY only when the task supplies multiple distinctive terms;
		// an explicit identifier such as BinaryMode.Auto still clears this bar.
		return score.matched >= 2 && score.longest >= 6
	}
}

func localizationFinalResponseBetterTaskScore(left, right localizationFinalResponseTaskScore) bool {
	if left.matched != right.matched {
		return left.matched > right.matched
	}
	if left.callable != right.callable {
		return left.callable
	}
	return left.longest > right.longest
}

func localizationFinalResponseNeighborContains(ids []string, id string) bool {
	for _, candidate := range ids {
		if strings.TrimSpace(candidate) == id {
			return true
		}
	}
	return false
}

func localizationFinalResponseDirectRelation(row localizationDigestRow, primaries []localizationDigestRow) bool {
	id := strings.TrimSpace(row.ID)
	for _, primary := range primaries {
		primaryID := strings.TrimSpace(primary.ID)
		if localizationFinalResponseNeighborContains(primary.Callers, id) ||
			localizationFinalResponseNeighborContains(primary.Callees, id) ||
			localizationFinalResponseNeighborContains(row.Callers, primaryID) ||
			localizationFinalResponseNeighborContains(row.Callees, primaryID) {
			return true
		}
	}
	return false
}

func localizationFinalResponseDirectory(file string) string {
	file = strings.ReplaceAll(strings.TrimSpace(file), "\\", "/")
	index := strings.LastIndex(file, "/")
	if index <= 0 {
		return ""
	}
	return strings.ToLower(file[:index])
}

func localizationFinalResponseSameDirectory(row localizationDigestRow, primaries []localizationDigestRow) bool {
	directory := localizationFinalResponseDirectory(row.File)
	if directory == "" {
		return false
	}
	for _, primary := range primaries {
		if directory == localizationFinalResponseDirectory(primary.File) {
			return true
		}
	}
	return false
}

func localizationFinalResponseOwnerFamily(row localizationDigestRow) string {
	identity := strings.TrimSpace(row.QualName)
	if identity == "" {
		identity = strings.TrimSpace(row.ID)
		if cut := strings.LastIndex(identity, "::"); cut >= 0 {
			identity = identity[cut+2:]
		}
	}
	if cut := strings.LastIndexByte(identity, '.'); cut > 0 {
		identity = identity[:cut]
	}
	tokens := rerank.Tokenize(identity)
	if len(tokens) == 0 {
		return ""
	}
	family := exploreTerminalTermRoot(strings.ToLower(strings.TrimSpace(tokens[len(tokens)-1])))
	if len(family) < 6 {
		return ""
	}
	switch family {
	case "handler", "service", "controller", "manager", "client", "implementation":
		return ""
	default:
		return family
	}
}

func localizationFinalResponseSharesOwnerFamily(row localizationDigestRow, primaries []localizationDigestRow) bool {
	family := localizationFinalResponseOwnerFamily(row)
	if family == "" {
		return false
	}
	for _, primary := range primaries {
		if family == localizationFinalResponseOwnerFamily(primary) {
			return true
		}
	}
	return false
}

// localizationFinalResponseRows renders the retained evidence order directly.
// Filtering only rejects invalid, duplicate, or over-limit rows; it never
// promotes a later row ahead of an earlier Files/Symbols/Evidence position.
// For a task-aware page, two leading rows remain PRIMARY and the third slot may
// label one later complementary implementation row. Label selection never
// changes the row's position or EVIDENCE number.
func localizationFinalResponseRows(task string, _ []localizationDigestRow, rows []localizationDigestRow) []localizationFinalResponseRow {
	presented := make([]localizationFinalResponseRow, 0, min(len(rows), localizationReplayEvidenceLimit))
	seen := make(map[string]struct{}, localizationReplayEvidenceLimit)
	for _, row := range rows {
		row.ID = strings.TrimSpace(row.ID)
		row.File = strings.TrimSpace(row.File)
		if row.ID == "" || row.File == "" {
			continue
		}
		if _, exists := seen[row.ID]; exists {
			continue
		}
		seen[row.ID] = struct{}{}
		row.Rank = len(presented) + 1
		presented = append(presented, localizationFinalResponseRow{row: row})
		if len(presented) == localizationReplayEvidenceLimit {
			break
		}
	}
	for index := 0; index < min(len(presented), localizationFinalResponsePrimaryLimit); index++ {
		presented[index].primary = true
	}
	if strings.TrimSpace(task) == "" || len(presented) <= localizationFinalResponsePrimaryLimit {
		return presented
	}

	seedLimit := min(2, len(presented))
	relationSeedLimit := min(localizationFinalResponsePrimaryLimit+2, len(presented))
	relationSeeds := make([]localizationDigestRow, 0, relationSeedLimit)
	primaryFiles := make(map[string]struct{}, seedLimit)
	for index := 0; index < relationSeedLimit; index++ {
		relationSeeds = append(relationSeeds, presented[index].row)
		if index < seedLimit {
			primaryFiles[strings.ToLower(strings.TrimSpace(presented[index].row.File))] = struct{}{}
		}
	}
	taskTerms := exploreTerminalTerms(task)
	// The complementary PRIMARY slot should add task coverage, not repeat the
	// identifiers already guaranteed by the two leading PRIMARY rows. Score
	// only the remaining task terms; graph/provenance signals still decide ties
	// and cases with no marginal lexical coverage.
	for _, seed := range relationSeeds[:seedLimit] {
		identifierText := localizationFinalResponseIdentifierText(seed)
		for term := range taskTerms {
			if exploreConceptTermPresent(identifierText, term) {
				delete(taskTerms, term)
			}
		}
	}
	taskTermFrequency := make(map[string]int, len(taskTerms))
	for index := seedLimit; index < len(presented); index++ {
		identifierText := localizationFinalResponseIdentifierText(presented[index].row)
		for term := range taskTerms {
			if exploreConceptTermPresent(identifierText, term) {
				taskTermFrequency[term]++
			}
		}
	}
	bestIndex := -1
	bestScore := localizationFinalResponseTaskScore{}
	bestRareTaskCoverage := 0
	bestDirect, bestSameDirectory, bestStrongTaskAlignment := false, false, false
	bestTaskComplement := false
	bestSyntacticAnchor := false
	bestPrimaryProvenance, bestSupportingProvenance := false, false
	for index := seedLimit; index < len(presented); index++ {
		row := presented[index].row
		if _, duplicateFile := primaryFiles[strings.ToLower(strings.TrimSpace(row.File))]; duplicateFile {
			continue
		}
		score := scoreLocalizationFinalResponseTask(taskTerms, row)
		strongTaskAlignment := score.matched >= 2
		rareTaskCoverage := 0
		identifierText := localizationFinalResponseIdentifierText(row)
		for term, frequency := range taskTermFrequency {
			if frequency == 1 && exploreConceptTermPresent(identifierText, term) {
				rareTaskCoverage++
			}
		}
		direct := localizationFinalResponseDirectRelation(row, relationSeeds)
		typeLike := strings.EqualFold(strings.TrimSpace(row.Kind), "type") || strings.EqualFold(strings.TrimSpace(row.Kind), "class")
		sameDirectory := localizationFinalResponseSameDirectory(row, relationSeeds[:seedLimit]) &&
			(typeLike || localizationFinalResponseSharesOwnerFamily(row, relationSeeds[:seedLimit]))
		primaryProvenance := localizationFinalResponsePrimaryProvenance(row.Provenance)
		supportingProvenance := localizationFinalResponseSupportingProvenance(row.Provenance)
		syntacticAnchor := row.Provenance == localizationProvenanceSyntacticAnchor
		// Only a graph-backed marker outranks ordinary lexical scoring. A plain
		// task_complement remains a weak display hint and cannot displace a stronger
		// same-directory or task-aligned artifact.
		taskComplement := row.Provenance == localizationProvenanceTaskRelationComplement
		eligible := direct || supportingProvenance || score.matched >= 2 ||
			(primaryProvenance && score.matched > 0) ||
			(sameDirectory && score.matched > 0)
		if !eligible || (bestSyntacticAnchor && !syntacticAnchor) {
			continue
		}
		better := bestIndex < 0 ||
			(syntacticAnchor && !bestSyntacticAnchor) ||
			(direct && !bestDirect) ||
			(direct == bestDirect && taskComplement && !bestTaskComplement) ||
			(direct == bestDirect && taskComplement == bestTaskComplement && rareTaskCoverage > bestRareTaskCoverage) ||
			(direct == bestDirect && taskComplement == bestTaskComplement && rareTaskCoverage == bestRareTaskCoverage && strongTaskAlignment && !bestStrongTaskAlignment) ||
			(direct == bestDirect && taskComplement == bestTaskComplement && rareTaskCoverage == bestRareTaskCoverage && strongTaskAlignment == bestStrongTaskAlignment && sameDirectory && !bestSameDirectory) ||
			(direct == bestDirect && taskComplement == bestTaskComplement && rareTaskCoverage == bestRareTaskCoverage && strongTaskAlignment == bestStrongTaskAlignment && sameDirectory == bestSameDirectory && supportingProvenance && !bestSupportingProvenance) ||
			(direct == bestDirect && taskComplement == bestTaskComplement && rareTaskCoverage == bestRareTaskCoverage && strongTaskAlignment == bestStrongTaskAlignment && sameDirectory == bestSameDirectory && supportingProvenance == bestSupportingProvenance && localizationFinalResponseBetterTaskScore(score, bestScore)) ||
			(direct == bestDirect && taskComplement == bestTaskComplement && rareTaskCoverage == bestRareTaskCoverage && strongTaskAlignment == bestStrongTaskAlignment && sameDirectory == bestSameDirectory && supportingProvenance == bestSupportingProvenance && score == bestScore && primaryProvenance && !bestPrimaryProvenance)
		if !better {
			continue
		}
		bestIndex = index
		bestScore = score
		bestRareTaskCoverage = rareTaskCoverage
		bestDirect = direct
		bestTaskComplement = taskComplement
		bestSyntacticAnchor = syntacticAnchor
		bestSameDirectory = sameDirectory
		bestStrongTaskAlignment = strongTaskAlignment
		bestPrimaryProvenance = primaryProvenance
		bestSupportingProvenance = supportingProvenance
	}
	if bestIndex >= seedLimit {
		presented[localizationFinalResponsePrimaryLimit-1].primary = false
		presented[bestIndex].primary = true
	}

	// PRIMARY is a confidence claim, not simply a position. Preserve EVIDENCE
	// order, but demote leading rows that have no task-term, graph, or provenance
	// support. This keeps the role labels aligned with the evidence
	// a model is actually being asked to trust.
	allEvidenceRows := make([]localizationDigestRow, 0, len(presented))
	for _, candidate := range presented {
		allEvidenceRows = append(allEvidenceRows, candidate.row)
	}
	primaryTaskTerms := exploreTerminalTerms(task)
	for index := 1; index < len(presented); index++ {
		if !presented[index].primary {
			continue
		}
		row := presented[index].row
		score := scoreLocalizationFinalResponseTask(primaryTaskTerms, row)
		if localizationFinalResponseTaskSupportsPrimary(row, score) ||
			localizationFinalResponseDirectRelation(row, allEvidenceRows) ||
			localizationFinalResponsePrimaryProvenance(row.Provenance) ||
			localizationFinalResponseSupportingProvenance(row.Provenance) {
			continue
		}
		presented[index].primary = false
	}
	return presented
}

func renderLocalizationFinalResponse(rows []localizationDigestRow) string {
	return renderLocalizationFinalResponseForTask("", nil, rows)
}

func renderLocalizationFinalResponseForTask(task string, current, rows []localizationDigestRow) string {
	presented := localizationFinalResponseRows(task, current, rows)
	return renderLocalizationAnswerPage(presented, localizationAnswerHeading,
		"No bounded localization evidence was found.", localizationAnswerReadyDirective)
}

// renderLocalizationProvisionalResponseForTask renders the same rows for a
// state that has not proven its answer yet. The heading and closing line are
// the whole difference: a caller must be able to tell a proven answer from the
// best one available so far, or the page teaches it to trust both equally.
// Unlike the proven page, this one trims its own tail to stay inside the byte
// cap — it is additional payload on responses that previously carried none, so
// it may not push an envelope over budget on its way in.
func renderLocalizationProvisionalResponseForTask(task string, current, rows []localizationDigestRow) string {
	presented := localizationFinalResponseRows(task, current, rows)
	if len(presented) > localizationProvisionalRowLimit {
		presented = presented[:localizationProvisionalRowLimit]
	}
	for {
		page := renderLocalizationAnswerPage(presented, localizationProvisionalHeading,
			"No localization evidence has been retained for this request yet.",
			localizationProvisionalDirective)
		if len(page) <= localizationFinalResponseMaxBytes || len(presented) == 0 {
			return page
		}
		presented = presented[:len(presented)-1]
	}
}

func localizationFinalResponseProvenanceCue(provenance string) string {
	switch strings.TrimSpace(provenance) {
	case localizationProvenanceDivergentDefault:
		return "CAUSAL OWNER — upstream default/state owner"
	case localizationProvenanceDivergentDefaultType:
		return "OWNING TYPE"
	case localizationProvenanceImplementationTarget:
		return "IMPLEMENTATION TARGET"
	case localizationProvenanceSourceLiteralCallee:
		return "LITERAL-RESOLVED CALLEE"
	case localizationProvenanceTaskComplement, localizationProvenanceTaskRelationComplement:
		return "TASK COMPLEMENT"
	default:
		return ""
	}
}

func renderLocalizationAnswerPage(
	presented []localizationFinalResponseRow, heading, empty, directive string,
) string {
	if len(presented) == 0 {
		return heading + "\n" + empty + "\n\n" + directive
	}
	var response strings.Builder
	response.WriteString(heading)
	response.WriteString("\n")
	for _, item := range presented {
		role := "SUPPORTING"
		if item.primary {
			role = "PRIMARY"
		}
		file := localizationFinalResponseField(item.row.File)
		id := localizationFinalResponseField(item.row.ID)
		cue := localizationFinalResponseProvenanceCue(item.row.Provenance)
		if item.row.Line > 0 {
			fmt.Fprintf(&response, "- EVIDENCE #%d — %s — %s:%d — %s\n", item.row.Rank, role, file, item.row.Line, id)
		} else {
			fmt.Fprintf(&response, "- EVIDENCE #%d — %s — %s — %s\n", item.row.Rank, role, file, id)
		}
		if cue != "" {
			fmt.Fprintf(&response, "  PROVENANCE #%d — %s\n", item.row.Rank, cue)
		}
	}
	response.WriteString("\n")
	response.WriteString(directive)
	return response.String()
}

// refreshLocalizationDigestResponses keeps the proven and provisional pages
// derived from one row set. They are rebuilt together at every site that
// reshapes Evidence, so a shed row can never survive in one page after it has
// left the other.
func refreshLocalizationDigestResponses(digest *localizationEvidenceDigest, task string, current []localizationDigestRow) {
	if digest == nil {
		return
	}
	digest.finalResponse = renderLocalizationFinalResponseForTask(task, current, digest.Evidence)
	digest.provisionalResponse = renderLocalizationProvisionalResponseForTask(task, current, digest.Evidence)
}

// The directive is the only instruction the caller sees on a terminal page, so
// it names the one failure mode measurement keeps finding: an answer that
// paraphrases the located identifier into a neighbouring one it inferred from
// the request text, losing the located symbol.
// The conclusion is bounded deliberately. Output is the most expensive token
// class in a session, and asking for the located lines verbatim already moved
// median output up measurably; an unbounded "explain" invites the model to
// restate evidence it has just quoted.
// Wording matters more than it looks. Ordering a caller to reproduce a page it
// judges wrong reads as coercion: measured across 336 sessions, pages carrying
// the word "verbatim" drew an explicit manipulation accusation in 30% of the
// caller's own statements against 2% on pages without it. Ask for the answer,
// name what the answer should carry, and leave the caller free to disagree —
// its disagreement is right more often than not.
const localizationAnswerReadyDirective = "Localization for this task is complete: the bounded search examined the supplied anchors, and this is the full retained result. For a localization-only request, answer now using compact FILES, SYMBOLS, and EVIDENCE sections, preserving each PRIMARY tuple once in EVIDENCE order. Do not call another tool just to gain confidence, repeat, or cross-check this localization: PRIMARY rows are the best-supported answer, and SUPPORTING rows are context, not a signal that the search is unfinished. " + localizationAdvisorySelfCheck + " An identical localize call will replay this result. On a broader coding task, this closes only localization; if the user's request includes diagnosis, implementation, or verification, continue normally, and all tools remain available."

const (
	localizationAnswerHeading      = "LOCALIZATION:"
	localizationProvisionalHeading = "LOCALIZATION (UNCONFIRMED):"
	localizationBoundedHeading     = "LOCALIZATION COMPLETE (BOUNDED EVIDENCE):"
)

// The provisional page exists because a session can stop at any turn: the
// caller may run out of steps, decide it has enough, or give up. Until now
// every state but one handed back no answer at all, so those sessions ended
// with nothing — measurably, one in five of them.
// It is deliberately terse. The envelope budget is 6400 bytes and a real page
// already fills most of it, so a wordy fallback is a fallback that gets shed
// before it ever reaches a caller. Say the three things that matter — these
// are unconfirmed, the prescribed step is still the better move, and stopping
// with them beats stopping with nothing — and stop.
const localizationProvisionalDirective = "Unconfirmed. Prefer the step this response prescribes; if you stop here instead, answer with these candidates and say they are unconfirmed."

// localizationProvisionalRowLimit starts from every retained evidence row.
// The renderer sheds only the canonical tail when the final-response byte cap
// requires it, so the PRIMARY role limit never hides otherwise retained rows.
const localizationProvisionalRowLimit = localizationReplayEvidenceLimit

func localizationDigestRowsByID(digest *localizationEvidenceDigest) map[string]localizationDigestRow {
	retained := make(map[string]localizationDigestRow)
	if digest == nil {
		return retained
	}
	for _, row := range digest.Evidence {
		if symbol := strings.TrimSpace(row.ID); symbol != "" {
			retained[symbol] = row
		}
	}
	return retained
}

func localizationDigestHasProvenance(rows map[string]localizationDigestRow, provenance string) bool {
	for _, row := range rows {
		if row.Provenance == provenance {
			return true
		}
	}
	return false
}

func localizationDigestStrongProofRetained(rows map[string]localizationDigestRow, symbol string) bool {
	row, exists := rows[strings.TrimSpace(symbol)]
	if !exists {
		return false
	}
	switch row.Provenance {
	case localizationProvenanceSourceLiteralCallee:
		return true
	case localizationProvenanceDivergentDefault:
		return localizationDigestHasProvenance(rows, localizationProvenanceDivergentDefaultType)
	case localizationProvenanceImplementationRoute:
		return localizationDigestHasProvenance(rows, localizationProvenanceImplementationTarget)
	case localizationProvenanceImplementationTarget:
		return localizationDigestHasProvenance(rows, localizationProvenanceImplementationRoute)
	default:
		return false
	}
}

func localizationDigestAnyStrongProofRetained(rows map[string]localizationDigestRow) bool {
	for symbol := range rows {
		if localizationDigestStrongProofRetained(rows, symbol) {
			return true
		}
	}
	return false
}

// localizationDigestReconcileRoute preserves a concrete advisory read when its
// optional proof was shed, but never preserves a generic-wrapper hop without
// the concrete implementation that makes the route useful.
func localizationDigestReconcileRoute(
	symbol string,
	route localizationRefinementRoute,
	rows map[string]localizationDigestRow,
) (localizationRefinementRoute, bool) {
	row, retained := rows[symbol]
	if !retained {
		return localizationRefinementRoute{}, false
	}
	if route.implementationSymbol != "" {
		implementation, implementationRetained := rows[route.implementationSymbol]
		if !implementationRetained {
			return localizationRefinementRoute{}, false
		}
		if route.enforceable && (row.Provenance != localizationProvenanceImplementationRoute ||
			implementation.Provenance != localizationProvenanceImplementationTarget) {
			route.enforceable = false
		}
	}
	if route.proofSymbol != "" {
		proof, proofRetained := rows[route.proofSymbol]
		if !proofRetained || proof.Provenance != localizationProvenanceImplementationRoute ||
			row.Provenance != localizationProvenanceImplementationTarget {
			route.proofSymbol = ""
			route.enforceable = false
		}
	}
	if route.enforceable && route.implementationSymbol == "" && route.proofSymbol == "" &&
		row.Provenance != localizationProvenanceSourceLiteralCallee {
		route.enforceable = false
	}
	return route, true
}

func localizationCompletionBoundedByDigest(completion localizationCompletion, digest *localizationEvidenceDigest) localizationCompletion {
	if digest == nil {
		return completion
	}
	retained := localizationDigestRowsByID(digest)
	advisory := func() localizationCompletion {
		bounded := newLocalizationCompletion(true, "")
		bounded.taskLead = completion.taskLead
		return bounded
	}

	switch completion.State {
	case localizationStateAnswerReady:
		if completion.Enforceable && !localizationDigestAnyStrongProofRetained(retained) {
			completion.Enforceable = false
		}
	case localizationStateNeedsExactRead:
		if _, exists := retained[completion.ExactSymbol]; !exists || completion.ExactSymbol == "" {
			return advisory()
		}
		if completion.enforceableOnAnswerReady &&
			!localizationDigestStrongProofRetained(retained, completion.ExactSymbol) {
			completion.enforceableOnAnswerReady = false
		}
	case localizationStateNeedsRefinement:
		allowed := make([]string, 0, len(completion.AllowedSymbols))
		seen := make(map[string]struct{}, len(completion.AllowedSymbols))
		var routes map[string]localizationRefinementRoute
		if len(completion.refinementRoutes) > 0 {
			routes = make(map[string]localizationRefinementRoute, len(completion.refinementRoutes))
		}
		for _, symbol := range completion.AllowedSymbols {
			symbol = strings.TrimSpace(symbol)
			if symbol == "" {
				continue
			}
			if _, exists := retained[symbol]; !exists {
				continue
			}
			if _, duplicate := seen[symbol]; duplicate {
				continue
			}
			if route, exists := completion.refinementRoutes[symbol]; exists {
				reconciled, usable := localizationDigestReconcileRoute(symbol, route, retained)
				if !usable {
					continue
				}
				routes[symbol] = reconciled
			}
			seen[symbol] = struct{}{}
			allowed = append(allowed, symbol)
		}
		if len(allowed) == 0 {
			return advisory()
		}
		preferred := strings.TrimSpace(completion.refinementSymbol)
		if _, exists := seen[preferred]; !exists {
			preferred = allowed[0]
		}
		bounded := newLocalizationRefinementCompletionForSymbols(preferred, allowed)
		bounded.enforceableOnAnswerReady = completion.enforceableOnAnswerReady
		bounded.taskLead = completion.taskLead
		if routes != nil {
			bounded.refinementRoutes = routes
			bounded.correctionSymbol, bounded.correctionRoute = localizationRankedCorrection(
				preferred, allowed, bounded.refinementRoutes,
			)
		}
		return bounded
	}
	return completion
}

// localizationStateCarriesEvidence reports whether a state is inside a
// localization flow that has ranked something. The inactive state is excluded
// deliberately: it rides responses that were never a localization request, and
// a candidate page there is payload with no question to answer.
func localizationStateCarriesEvidence(state string) bool {
	switch state {
	case localizationStateNeedsExactRead, localizationStateExactReadInFlight,
		localizationStateNeedsRefinement, localizationStateRefineInFlight,
		localizationStateNeedsRecovery, localizationStateRecoveryInFlight,
		localizationStateLocalized:
		return true
	default:
		return false
	}
}

func localizationCompletionWithDigest(completion localizationCompletion, digest *localizationEvidenceDigest) localizationCompletion {
	if digest == nil {
		digest = completion.digest
	}
	completion.digest = digest
	if completion.State != localizationStateAnswerReady {
		// The rows are already ranked and packed here; only the confidence to
		// call them an answer is missing. Discarding the page left a caller that
		// stops early — out of steps, or satisfied — with nothing to say, which
		// is strictly worse than a candidate list that admits it is unconfirmed.
		// A state holding no evidence still gets nothing: boilerplate with no
		// identities in it is payload the caller cannot answer from.
		completion.FinalResponse = ""
		if localizationStateCarriesEvidence(completion.State) && digest != nil && len(digest.Evidence) > 0 {
			completion.FinalResponse = digest.provisionalResponse
		}
		return completion
	}
	if completion.Instruction == localizationBoundedConclusionInstruction {
		response := completion.FinalResponse
		if response == "" && digest != nil && digest.provisionalResponse != "" {
			response = digest.provisionalResponse
		} else if response == "" && digest != nil && len(digest.Evidence) > 0 {
			response = renderLocalizationProvisionalResponseForTask("", nil, digest.Evidence)
		}
		response = strings.TrimSpace(response)
		if response != "" {
			switch {
			case strings.HasPrefix(response, localizationAnswerHeading):
				response = localizationBoundedHeading + strings.TrimPrefix(response, localizationAnswerHeading)
			case strings.HasPrefix(response, localizationProvisionalHeading):
				response = localizationBoundedHeading + strings.TrimPrefix(response, localizationProvisionalHeading)
			case !strings.HasPrefix(response, localizationBoundedHeading):
				response = localizationBoundedHeading + "\n" + response
			}
			for {
				stripped := false
				for _, directive := range []string{
					localizationAnswerReadyDirective,
					localizationBoundedConclusionDirective,
					localizationProvisionalDirective,
				} {
					if strings.HasSuffix(response, directive) {
						response = strings.TrimSpace(strings.TrimSuffix(response, directive))
						stripped = true
						break
					}
				}
				if !stripped {
					break
				}
			}
			response += "\n\n" + localizationBoundedConclusionDirective
			completion.FinalResponse = response
		}
		return completion
	}
	if digest != nil && digest.finalResponse != "" {
		completion.FinalResponse = digest.finalResponse
	} else if completion.FinalResponse == "" {
		completion.FinalResponse = renderLocalizationFinalResponse(nil)
	}
	return completion
}

func localizationTerminalStructuredContent(payload any, contract localizationTerminalContract) map[string]any {
	var structured map[string]any
	switch existing := payload.(type) {
	case map[string]any:
		structured = make(map[string]any, len(existing)+4)
		for key, value := range existing {
			structured[key] = value
		}
	case nil:
		structured = make(map[string]any, 4)
	default:
		structured = map[string]any{"payload": existing}
	}
	structured["completion"] = contract.Completion
	structured["terminal"] = contract.Terminal
	if contract.Terminal && contract.Completion.FinalResponse != "" {
		// completion already carries the answer, and the directive is its closing
		// line. A second top-level copy is the same block billed twice at the
		// cache-write rate, which is the most expensive place to repeat oneself:
		// point at it instead.
		structured["directive"] = localizationAnswerReadyDirective
	}
	return structured
}

// localizationHostEnvelope stores each retained row exactly once. Hosts render
// the ordered rows with fallback_format; no prewritten answer or duplicate row
// string crosses the wire.
type localizationHostEnvelope struct {
	Version        int                          `json:"version"`
	FallbackFormat string                       `json:"fallback_format"`
	Evidence       *localizationEvidenceDigest  `json:"evidence"`
	Contract       localizationTerminalContract `json:"contract"`
}

// Initial localization and authorized reads call this only after byte-budget
// packing and evidence-policy finalization, so visible and authoritative host
// contracts always describe the same completion.
func attachLocalizationHostEnvelope(result *mcpgo.CallToolResult, completion localizationCompletion, digest *localizationEvidenceDigest) *mcpgo.CallToolResult {
	if result == nil {
		return result
	}
	completion = localizationCompletionWithDigest(completion, digest)
	contract := localizationContractFor(completion)
	// Preserve preterminal tool payloads byte-for-byte. Only answer_ready adds
	// the terminal host projection; refinement and recovery retain their
	// existing structuredContent shape and visible completion envelope.
	if completion.State == localizationStateAnswerReady {
		base := result.StructuredContent
		if base == nil {
			// A host that renders structured content in preference to text sees
			// only this projection. Replacing a nil payload would therefore erase
			// the tool's own answer — including the source an authorized read was
			// just permitted to fetch — so decode the text payload and keep it
			// underneath the terminal keys.
			if text, ok := singleTextContent(result); ok {
				var decoded map[string]any
				if err := json.Unmarshal([]byte(text), &decoded); err == nil {
					base = decoded
				}
			}
		}
		result.StructuredContent = localizationTerminalStructuredContent(base, contract)
	}
	if result.Meta == nil {
		result.Meta = &mcpgo.Meta{}
	}
	if result.Meta.AdditionalFields == nil {
		result.Meta.AdditionalFields = make(map[string]any)
	}
	result.Meta.AdditionalFields[localizationHostMetaKey] = localizationHostEnvelope{
		Version:        1,
		FallbackFormat: "{file}:{line} — {id} ({signature})",
		Evidence:       digest,
		Contract:       contract,
	}
	return result
}

// localizationAnswerReadyResult is the successful, deterministic evidence
// replay. Hooked hosts may stop before dispatch; every other host receives the
// same ready-to-emit answer and directive on every post-terminal navigation.
func localizationAnswerReadyResult(completion localizationCompletion) *mcpgo.CallToolResult {
	completion = localizationCompletionWithDigest(completion, completion.digest)
	visible := completion.FinalResponse
	// Older retained completions may predate the in-response convergence cue.
	// Preserve their successful replay shape without duplicating the directive
	// for newly rendered terminal evidence.
	if !strings.HasPrefix(visible, localizationBoundedHeading+"\n") &&
		!strings.HasSuffix(visible, localizationAnswerReadyDirective) {
		visible += "\n\n" + localizationAnswerReadyDirective
	}
	result := mcpgo.NewToolResultText(visible)
	return attachLocalizationHostEnvelope(result, completion, completion.digest)
}

// newLocalizationEvidenceDigest retains rows without a request in hand. Callers
// that know the request use newLocalizationEvidenceDigestForTask so retained
// evidence and its response page are built together.
func newLocalizationEvidenceDigest(envelope localizationExploreEnvelope) *localizationEvidenceDigest {
	return newLocalizationEvidenceDigestForTask("", envelope)
}
